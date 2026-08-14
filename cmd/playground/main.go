// Command playground runs a loopback-only browser chat for local product
// testing. It uses the real bot handler, PostgreSQL queue, receipt processor,
// mock extractor and notification sender in one deterministic process; no
// Zalo or cloud credentials are read by any adapter.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/db"
	"zl-expese-bot/internal/bot"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/mock"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/messaging/logprovider"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/migrate"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/receipt"
	"zl-expese-bot/internal/store"
	"zl-expese-bot/internal/worker"
)

//go:embed static/*
var staticFiles embed.FS

var fixtureIDs = map[string]bool{
	"coopmart-clean":          true,
	"highlands-clean":         true,
	"petrolimex-clean":        true,
	"guardian-low-total":      true,
	"shopee-refund":           true,
	"vietcombank-transfer":    true,
	"woolworths-aud":          true,
	"coffeehouse-multi-total": true,
	"grab-mid":                true,
	"not-a-receipt":           true,
}

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "playground:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8090", "loopback address for the local playground")
	flag.Parse()
	if err := requireLoopback(*addr); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.AppEnv != config.EnvDevelopment && cfg.AppEnv != config.EnvTest {
		return fmt.Errorf("APP_ENV must be development or test for the local playground")
	}

	log := logging.New(cfg.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := migrate.Up(ctx, pool, db.MigrationsFS, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	st := store.New(pool)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	objects, err := objectstore.NewLocal(filepath.Join(cfg.DataDir, "playground"))
	if err != nil {
		return err
	}
	provider := logprovider.New(log)
	replies := notify.NewEnqueuer(st, q, clk)
	cats := categorisation.NewService(st, clk)
	insights := insight.NewService(st)
	processor := receipt.NewProcessor(st, objects, mock.New(), provider, cats, replies, clk, log)
	processor.ExtractionEnabled = true
	processor.MonthlyOCRLimit = 0 // the embedded mock has no external cost
	sender := notify.NewSender(st, provider, clk, log, true, 0)
	play := &playgroundServer{
		pool: pool, st: st, q: q, provider: provider, processor: processor, sender: sender,
		handler: bot.NewHandler(st, q, objects, replies, cats, insights, clk, log, cfg),
	}

	assets, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", play.chat)
	mux.HandleFunc("GET /api/health", play.health)
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	srv := &http.Server{
		Addr: *addr, Handler: securityHeaders(mux), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("local playground ready", slog.String("url", "http://"+*addr), slog.String("extractor", "mock"))
	fmt.Printf("\nLocal bot playground: http://%s\nPress Ctrl-C to stop.\n\n", *addr)
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

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("playground address must be host:port: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("playground must listen on loopback, got %q", host)
	}
	return nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

type playgroundServer struct {
	pool      *pgxpool.Pool
	st        *store.Store
	q         queue.Queue
	provider  *logprovider.Provider
	processor *receipt.Processor
	sender    *notify.Sender
	handler   *bot.Handler
	mu        sync.Mutex // deterministic queue draining across browser sessions
}

type chatRequest struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text,omitempty"`
	Fixture   string `json:"fixture,omitempty"`
}

type chatResponse struct {
	Replies []string     `json:"replies"`
	Profile *profileView `json:"profile,omitempty"`
}

type profileView struct {
	Status          string         `json:"status"`
	Timezone        string         `json:"timezone"`
	DefaultCurrency string         `json:"default_currency"`
	Locale          string         `json:"locale"`
	Schedules       []scheduleView `json:"schedules"`
}

type scheduleView struct {
	Frequency string `json:"frequency"`
	Time      string `json:"time"`
	Enabled   bool   `json:"enabled"`
}

func (s *playgroundServer) chat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req chatRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Yêu cầu không hợp lệ: "+err.Error())
		return
	}
	if err := ensureJSONEnd(dec); err != nil {
		writeError(w, http.StatusBadRequest, "Yêu cầu chỉ được chứa một đối tượng JSON.")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Text = strings.TrimSpace(req.Text)
	req.Fixture = strings.TrimSpace(req.Fixture)
	if !validSessionID(req.SessionID) {
		writeError(w, http.StatusBadRequest, "Mã phiên thử không hợp lệ.")
		return
	}
	if (req.Text == "") == (req.Fixture == "") {
		writeError(w, http.StatusBadRequest, "Hãy gửi một tin nhắn hoặc chọn một hóa đơn thử.")
		return
	}
	if len(req.Text) > 2000 {
		writeError(w, http.StatusBadRequest, "Tin nhắn thử tối đa 2.000 ký tự.")
		return
	}
	if req.Fixture != "" && !fixtureIDs[req.Fixture] {
		writeError(w, http.StatusBadRequest, "Hóa đơn thử không tồn tại.")
		return
	}

	chatID := "playground-" + req.SessionID
	ev := events.InboundEvent{
		Provider: string(domain.ProviderZaloBot), ProviderUserID: chatID,
		ProviderChatID: chatID, ProviderMessageID: "local-" + uuid.NewString(),
		EventType: domain.EventTextReceived, Text: req.Text, ReceivedAt: time.Now().UTC(),
	}
	if req.Fixture != "" {
		ev.EventType = domain.EventImageReceived
		ev.Media = []events.MediaReference{{
			Provider: string(domain.ProviderZaloBot), URL: mock.MockPrefix + req.Fixture, MimeType: "image/jpeg",
		}}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Không thể tạo tin nhắn thử.")
		return
	}
	sum := sha256.Sum256(raw)
	ev.RawPayload, ev.RawPayloadHash = raw, hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.provider.Sent())
	if err := s.handler.HandleEvent(r.Context(), ev); err != nil {
		writeError(w, http.StatusInternalServerError, "Bot xử lý thất bại: "+err.Error())
		return
	}
	if err := worker.Drain(r.Context(), s.q, []worker.DrainStep{
		{Kind: domain.JobReceiptProcess, Handle: s.processor.Handle},
		{Kind: domain.JobOutboundSend, Handle: s.sender.Handle},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Hàng đợi xử lý thất bại: "+err.Error())
		return
	}
	sent := s.provider.Sent()
	replies := make([]string, 0, len(sent)-before)
	for _, message := range sent[before:] {
		if message.ProviderChatID == chatID {
			replies = append(replies, message.Text)
		}
	}
	profile, err := s.profile(r.Context(), chatID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Không thể đọc cài đặt: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, chatResponse{Replies: replies, Profile: profile})
}

func (s *playgroundServer) profile(ctx context.Context, subject string) (*profileView, error) {
	user, err := s.st.GetUserByIdentity(ctx, domain.ProviderZaloBot, subject, string(domain.ProviderZaloBot))
	if domain.IsCode(err, domain.CodeNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	preferences, err := s.st.ListSummarySchedules(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	view := &profileView{
		Status: string(user.Status), Timezone: user.Timezone,
		DefaultCurrency: user.DefaultCurrency, Locale: user.Locale,
		Schedules: make([]scheduleView, 0, len(preferences)),
	}
	for _, preference := range preferences {
		view.Schedules = append(view.Schedules, scheduleView{
			Frequency: string(preference.Frequency),
			Time:      fmt.Sprintf("%02d:%02d", preference.DeliveryMinute/60, preference.DeliveryMinute%60),
			Enabled:   preference.Enabled,
		})
	}
	return view, nil
}

func (s *playgroundServer) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "PostgreSQL chưa sẵn sàng.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "extractor": "mock"})
}

func validSessionID(value string) bool {
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func ensureJSONEnd(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
