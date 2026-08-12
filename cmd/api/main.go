// Command api is the inbound edge of the bot. It serves the Zalo webhook
// (deployed environments) or long-polls the Bot API (-poll, local
// development), verifies and normalises each update, and hands it to the
// bot handler. All slow work — media download, extraction, outbound sends —
// is queued, so acknowledgements stay well inside the P95 < 500 ms budget.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"zl-expese-bot/internal/bot"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/messaging"
	"zl-expese-bot/internal/messaging/logprovider"
	"zl-expese-bot/internal/messaging/zalo"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

// maxWebhookBody caps inbound webhook payloads (plan §17: limit body size).
const maxWebhookBody = 1 << 20 // 1 MiB

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	poll := flag.Bool("poll", false, "long-poll the Zalo Bot API instead of serving the webhook (local development)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	objects, err := objectStore(ctx, cfg)
	if err != nil {
		return err
	}
	provider := messagingProvider(cfg, log)
	replies := notify.NewEnqueuer(st, q, clk)
	cats := categorisation.NewService(st, clk)
	insights := insight.NewService(pool, clk)
	handler := bot.NewHandler(st, pool, q, objects, replies, cats, insights, clk, log, cfg)

	if *poll {
		client, ok := provider.(*zalo.Client)
		if !ok {
			return errors.New("-poll requires ZALO_BOT_TOKEN (nothing to poll without a real bot)")
		}
		return pollLoop(ctx, log, client, handler)
	}
	return serve(ctx, log, cfg, pool, provider, handler)
}

// messagingProvider picks the messaging adapter: the real Zalo Bot API when
// a token is configured, otherwise the log provider that never dials out.
func messagingProvider(cfg config.Config, log *slog.Logger) messaging.Provider {
	if cfg.MessagingMode() == "zalo" {
		return zalo.New(zalo.Config{
			Token:         cfg.ZaloBotToken,
			WebhookSecret: cfg.ZaloWebhookSecret,
			APIBase:       cfg.ZaloAPIBase,
		})
	}
	log.Warn("ZALO_BOT_TOKEN not set: outbound messages are logged, not sent")
	return logprovider.New(log)
}

// objectStore builds the receipt object store: local filesystem by default,
// S3 when OBJECTSTORE=s3 (P3-A02). AWS credentials and region come from the
// standard SDK chain.
func objectStore(ctx context.Context, cfg config.Config) (objectstore.Store, error) {
	if cfg.ObjectStoreBackend != "s3" {
		return objectstore.NewLocal(cfg.DataDir)
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return objectstore.NewS3(awss3.NewFromConfig(awsCfg), cfg.S3Bucket, cfg.S3Prefix)
}

// serve runs the webhook HTTP server until ctx is cancelled, then shuts
// down gracefully so in-flight requests finish.
func serve(ctx context.Context, log *slog.Logger, cfg config.Config, pool *pgxpool.Pool, provider messaging.Provider, handler *bot.Handler) error {
	s := &server{provider: provider, handler: handler, pool: pool, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/zalo", s.webhook)
	mux.HandleFunc("GET /healthz", s.healthz)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("api listening",
		slog.String("addr", cfg.ListenAddr),
		slog.String("messaging", cfg.MessagingMode()))

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// server holds the inbound HTTP dependencies.
type server struct {
	provider messaging.Provider
	handler  *bot.Handler
	pool     *pgxpool.Pool
	log      *slog.Logger
}

// webhook verifies the shared secret, parses the body into normalised
// events and processes them inline: HandleEvent only writes rows and
// enqueues jobs, so the 200 OK is still a fast acknowledgement. A 500 asks
// Zalo to retry; provider_messages dedupe makes retries safe.
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	ctx := logging.WithRequestID(r.Context(), uuid.NewString())
	log := logging.FromContext(ctx, s.log)

	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	if err := s.provider.VerifyWebhook(ctx, r.Header, body); err != nil {
		log.Warn("webhook secret rejected")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	evs, err := s.provider.ParseWebhook(ctx, body)
	if err != nil {
		log.Warn("webhook payload rejected", slog.String("error_class", string(domain.CodeOf(err))))
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	for _, ev := range evs {
		if err := s.handler.HandleEvent(ctx, ev); err != nil {
			log.Error("event handling failed", slog.String("error", err.Error()))
			http.Error(w, "processing failed", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// healthz reports process liveness and database reachability.
func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// pollLoop is the local-development ingress: the Bot API hands out updates
// over getUpdates long polling instead of webhook calls. Events flow
// through the same handler and dedupe path as the deployed webhook.
func pollLoop(ctx context.Context, log *slog.Logger, client *zalo.Client, handler *bot.Handler) error {
	log.Info("long-polling Zalo Bot API for updates")
	var offset int64
	for {
		evs, next, err := client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Error("getUpdates failed", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		offset = next
		for _, ev := range evs {
			evCtx := logging.WithRequestID(ctx, uuid.NewString())
			if err := handler.HandleEvent(evCtx, ev); err != nil {
				log.Error("event handling failed", slog.String("error", err.Error()))
			}
		}
		if len(evs) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
}
