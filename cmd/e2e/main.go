// Command e2e provides opt-in, secret-safe checks for the real Zalo →
// Gemini receipt path. It never prints credentials, provider identities,
// message text, receipt fields, or media URLs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	"zl-expese-bot/internal/extraction/gemini"
	"zl-expese-bot/internal/messaging/zalo"
	"zl-expese-bot/internal/platform/postgres"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) < 2 {
		fail(errors.New("usage: e2e fixture|validate-db|preflight|discover|watch"))
	}
	var err error
	switch os.Args[1] {
	case "fixture":
		err = runFixture(os.Args[2:])
	case "validate-db":
		err = runValidateDB(os.Args[2:])
	case "preflight":
		err = runPreflight(ctx, os.Args[2:])
	case "discover":
		err = runDiscover(ctx, os.Args[2:])
	case "watch":
		err = runWatch(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown e2e command %q", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fail(err)
	}
}

func runFixture(args []string) error {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	outPath := fs.String("out", "", "new PNG path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || strings.TrimSpace(*outPath) == "" {
		return errors.New("fixture requires -out and accepts no positional arguments")
	}
	body, err := syntheticReceiptPNG()
	if err != nil {
		return fmt.Errorf("build synthetic receipt: %w", err)
	}
	file, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create synthetic receipt (refusing to overwrite): %w", err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write synthetic receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close synthetic receipt: %w", err)
	}
	fmt.Println("PASS synthetic E2E receipt written")
	return nil
}

const e2eDatabaseName = "zl_expense_e2e"

// runValidateDB resolves the connection string with pgx itself, then checks
// the effective target. This prevents URL query parameters or libpq settings
// from overriding the path/host after a superficial shell-string check.
func runValidateDB(args []string) error {
	fs := flag.NewFlagSet("validate-db", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("validate-db accepts no positional arguments")
	}
	return validateE2EDatabaseURL(os.Getenv("E2E_DATABASE_URL"))
}

func validateE2EDatabaseURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return errors.New("E2E_DATABASE_URL is required")
	}
	cfg, err := pgconn.ParseConfigWithOptions(raw, pgconn.ParseConfigOptions{
		ConnStringAllowedKeys: []string{"host", "port", "user", "password", "dbname", "sslmode"},
	})
	if err != nil {
		// pgx parse errors can include the original connection string, so do
		// not wrap the cause: even local URLs may carry a password.
		return errors.New("E2E_DATABASE_URL is not a valid PostgreSQL connection string")
	}
	if !isLoopbackHost(cfg.Host) {
		return errors.New("E2E_DATABASE_URL must resolve to a loopback host")
	}
	for _, fallback := range cfg.Fallbacks {
		if !isLoopbackHost(fallback.Host) {
			return errors.New("E2E_DATABASE_URL must not contain a non-loopback fallback host")
		}
	}
	if cfg.Database != e2eDatabaseName {
		return fmt.Errorf("E2E_DATABASE_URL must target database %s", e2eDatabaseName)
	}
	return nil
}

func isLoopbackHost(raw string) bool {
	host := strings.TrimSpace(strings.ToLower(raw))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "e2e:", err)
	os.Exit(1)
}

func runPreflight(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.ZaloBotToken == "" {
		return errors.New("ZALO_BOT_TOKEN is required")
	}
	if cfg.ExtractionBackend != "gemini" {
		return fmt.Errorf("EXTRACTOR must be gemini, got %q", cfg.ExtractionBackend)
	}

	zaloClient := zalo.New(zalo.Config{Token: cfg.ZaloBotToken, APIBase: cfg.ZaloAPIBase})
	info, err := zaloClient.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("Zalo getMe check failed: %w", err)
	}
	fmt.Printf("PASS Zalo token: getMe returned bot metadata (account type %s)\n", safeLabel(info.AccountType))

	image, err := syntheticReceiptPNG()
	if err != nil {
		return fmt.Errorf("build synthetic receipt: %w", err)
	}
	ex := gemini.New(gemini.Config{
		APIKey: cfg.GeminiAPIKey, Model: cfg.GeminiModel, APIBase: cfg.GeminiAPIBase,
	})
	result, err := ex.Extract(ctx, bytes.NewReader(image), extraction.Input{ContentType: "image/png"})
	if err != nil {
		return fmt.Errorf("Gemini synthetic OCR check failed: %w", err)
	}
	if err := validateSyntheticResult(result); err != nil {
		return err
	}
	fmt.Printf("PASS Gemini OCR: %s read the synthetic receipt and matched merchant/total/currency/date\n", safeLabel(cfg.GeminiModel))
	return nil
}

func validateSyntheticResult(result events.ExtractionResult) error {
	if got, ok := fieldInt64(result, "total_minor"); !ok || got != syntheticTotalMinor {
		return errors.New("Gemini OCR check returned an unexpected total")
	}
	if got, ok := fieldString(result, "currency"); !ok || got != syntheticCurrency {
		return errors.New("Gemini OCR check returned an unexpected currency")
	}
	if got, ok := fieldString(result, "merchant"); !ok || got != syntheticMerchant {
		return errors.New("Gemini OCR check returned an unexpected merchant")
	}
	if got, ok := fieldString(result, "occurred_at"); !ok || got != syntheticOccurredAt {
		return errors.New("Gemini OCR check returned an unexpected transaction date")
	}
	return nil
}

func fieldInt64(result events.ExtractionResult, name string) (int64, bool) {
	field, ok := result.Fields[name]
	if !ok {
		return 0, false
	}
	switch value := field.Normalised.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		return int64(value), value == float64(int64(value))
	case json.Number:
		n, err := value.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func fieldString(result events.ExtractionResult, name string) (string, bool) {
	field, ok := result.Fields[name]
	if !ok {
		return "", false
	}
	value, ok := field.Normalised.(string)
	return value, ok && strings.TrimSpace(value) != ""
}

func safeLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == ' ' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return "configured value"
		}
	}
	return value
}

type discoveredIdentity struct {
	ProviderUserID string `json:"provider_user_id"`
	ProviderChatID string `json:"provider_chat_id"`
}

var providerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

func runDiscover(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	challenge := fs.String("challenge", "", "exact one-time text to match")
	identityFile := fs.String("identity-file", "", "0600 output file for the matched identity")
	timeout := fs.Duration("timeout", 10*time.Minute, "maximum discovery wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*challenge) == "" || *identityFile == "" {
		return errors.New("-challenge and -identity-file are required")
	}
	token := strings.TrimSpace(os.Getenv("ZALO_BOT_TOKEN"))
	if token == "" {
		return errors.New("ZALO_BOT_TOKEN is required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	client := zalo.New(zalo.Config{Token: token, APIBase: os.Getenv("ZALO_API_BASE")})
	var offset int64
	for {
		events, next, err := client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("identity discovery timed out: %w", ctx.Err())
			}
			if !domain.Retryable(err) {
				return fmt.Errorf("identity discovery poll failed: %w", err)
			}
			fmt.Fprintln(os.Stderr, "WAIT transient Zalo polling error; retrying without exposing request details")
			if err := waitContext(ctx, 2*time.Second); err != nil {
				return fmt.Errorf("identity discovery stopped: %w", err)
			}
			continue
		}
		offset = next
		for _, event := range events {
			if event.EventType != domain.EventTextReceived || strings.TrimSpace(event.Text) != strings.TrimSpace(*challenge) {
				continue
			}
			if !providerIDPattern.MatchString(event.ProviderUserID) || !providerIDPattern.MatchString(event.ProviderChatID) {
				return errors.New("matched update carried an invalid provider identity")
			}
			if err := writeIdentity(*identityFile, discoveredIdentity{
				ProviderUserID: event.ProviderUserID,
				ProviderChatID: event.ProviderChatID,
			}); err != nil {
				return err
			}
			fmt.Println("PASS challenge matched; the E2E run is pinned to that sender (identity redacted)")
			return nil
		}
	}
}

func writeIdentity(path string, identity discoveredIdentity) error {
	body := []byte(identity.ProviderUserID + "\n" + identity.ProviderChatID + "\n")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open identity file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("protect identity file: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write identity file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close identity file: %w", err)
	}
	return nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type receiptState struct {
	ID     uuid.UUID
	Status domain.ReceiptStatus
}

type transactionState struct {
	ID       uuid.UUID
	Status   domain.TxStatus
	Amount   int64
	Currency string
	Merchant string
}

func runWatch(parent context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	identityFile := fs.String("identity-file", "", "identity file written by discover")
	sinceRaw := fs.String("since", "", "RFC3339 lower bound for this run")
	timeout := fs.Duration("timeout", 10*time.Minute, "maximum E2E wait")
	model := fs.String("model", "gemini-3.6-flash", "expected extractor version")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *identityFile == "" || *sinceRaw == "" {
		return errors.New("-identity-file and -since are required")
	}
	identity, err := readIdentity(*identityFile)
	if err != nil {
		return err
	}
	since, err := time.Parse(time.RFC3339, *sinceRaw)
	if err != nil {
		return fmt.Errorf("parse -since: %w", err)
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	pool, err := postgres.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var userID uuid.UUID
	onboarded := false
	extracted := false
	lastStage := "waiting for /batdau"
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("timed out while %s: %w", lastStage, ctx.Err())
		}
		if dead, err := hasDeadJob(ctx, pool, since); err != nil {
			return err
		} else if dead {
			return errors.New("a queue job entered dead state during the E2E run")
		}

		if userID == uuid.Nil {
			id, status, found, err := findUser(ctx, pool, identity.ProviderUserID)
			if err != nil {
				return err
			}
			if found {
				userID = id
				if status != domain.UserActive {
					lastStage = "waiting for consent via /batdau"
				}
			}
		}
		if userID != uuid.Nil {
			bad, err := hasBadOutbound(ctx, pool, userID, since)
			if err != nil {
				return err
			}
			if bad {
				return errors.New("an outbound message failed or became ambiguous during the E2E run")
			}
		}
		if userID != uuid.Nil && !onboarded {
			status, err := currentUserStatus(ctx, pool, userID)
			if err != nil {
				return err
			}
			sent, err := outboundSent(ctx, pool, userID, "welcome:%", since)
			if err != nil {
				return err
			}
			if status == domain.UserActive && sent {
				onboarded = true
				lastStage = "waiting for a receipt image"
				fmt.Println("PASS onboarding reply delivered. Send one clear synthetic or sanitized receipt photo now.")
			}
		}
		if onboarded {
			receipt, found, err := latestReceipt(ctx, pool, userID, since)
			if err != nil {
				return err
			}
			if found {
				switch receipt.Status {
				case domain.ReceiptFailedPermanent:
					return attemptFailure(ctx, pool, receipt.ID, "receipt processing failed permanently")
				case domain.ReceiptFailedTransient:
					lastStage = "waiting for a transient receipt retry"
				case domain.ReceiptReviewRequired, domain.ReceiptConfirmed:
					attemptOK, err := successfulGeminiAttempt(ctx, pool, receipt.ID, *model)
					if err != nil {
						return err
					}
					tx, txFound, err := receiptTransaction(ctx, pool, receipt.ID)
					if err != nil {
						return err
					}
					fieldsOK, err := requiredExtractionFields(ctx, pool, receipt.ID)
					if err != nil {
						return err
					}
					cardSent := false
					if txFound {
						cardSent, err = outboundSent(ctx, pool, userID, "card:"+tx.ID.String()+":%", since)
						if err != nil {
							return err
						}
					}
					if attemptOK && txFound && fieldsOK && cardSent {
						if tx.Amount <= 0 || len(tx.Currency) != 3 || strings.TrimSpace(tx.Merchant) == "" {
							return errors.New("extracted draft failed financial field invariants")
						}
						if !extracted {
							extracted = true
							lastStage = "waiting for confirmation"
							fmt.Printf("PASS receipt downloaded, Gemini OCR (%s) persisted provenance, and the review card was delivered. Send \"xác nhận\" now.\n", safeLabel(*model))
						}
						if tx.Status == domain.TxConfirmed && receipt.Status == domain.ReceiptConfirmed {
							confirmed, err := outboundSent(ctx, pool, userID, "confirm:"+tx.ID.String(), since)
							if err != nil {
								return err
							}
							if confirmed {
								fmt.Println("PASS E2E: real Zalo ingress → media download → Gemini OCR → database draft → Zalo review card → user confirmation")
								return nil
							}
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out while %s: %w", lastStage, ctx.Err())
		case <-ticker.C:
		}
	}
}

func readIdentity(path string) (discoveredIdentity, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return discoveredIdentity{}, fmt.Errorf("read identity file: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		return discoveredIdentity{}, errors.New("identity file has an invalid shape")
	}
	identity := discoveredIdentity{ProviderUserID: lines[0], ProviderChatID: lines[1]}
	if !providerIDPattern.MatchString(identity.ProviderUserID) || !providerIDPattern.MatchString(identity.ProviderChatID) {
		return discoveredIdentity{}, errors.New("identity file has invalid provider ids")
	}
	return identity, nil
}

func findUser(ctx context.Context, pool *pgxpool.Pool, subject string) (uuid.UUID, domain.UserStatus, bool, error) {
	var id uuid.UUID
	var status domain.UserStatus
	err := pool.QueryRow(ctx, `
		SELECT u.id, u.status
		FROM user_identities i
		JOIN users u ON u.id = i.user_id
		WHERE i.provider = 'zalo_bot' AND i.provider_subject = $1
		  AND i.provider_scope = 'zalo_bot'`, subject).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", false, fmt.Errorf("query E2E user: %w", err)
	}
	return id, status, true, nil
}

func currentUserStatus(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) (domain.UserStatus, error) {
	var status domain.UserStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		return "", fmt.Errorf("query E2E user status: %w", err)
	}
	return status, nil
}

func latestReceipt(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, since time.Time) (receiptState, bool, error) {
	var receipt receiptState
	err := pool.QueryRow(ctx, `
		SELECT id, status FROM receipt_documents
		WHERE user_id = $1 AND created_at >= $2
		ORDER BY created_at DESC LIMIT 1`, userID, since).Scan(&receipt.ID, &receipt.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return receiptState{}, false, nil
	}
	if err != nil {
		return receiptState{}, false, fmt.Errorf("query E2E receipt: %w", err)
	}
	return receipt, true, nil
}

func receiptTransaction(ctx context.Context, pool *pgxpool.Pool, receiptID uuid.UUID) (transactionState, bool, error) {
	var tx transactionState
	err := pool.QueryRow(ctx, `
		SELECT id, status, amount_minor, currency, merchant_name
		FROM transactions WHERE receipt_document_id = $1
		ORDER BY created_at DESC LIMIT 1`, receiptID).
		Scan(&tx.ID, &tx.Status, &tx.Amount, &tx.Currency, &tx.Merchant)
	if errors.Is(err, pgx.ErrNoRows) {
		return transactionState{}, false, nil
	}
	if err != nil {
		return transactionState{}, false, fmt.Errorf("query E2E transaction: %w", err)
	}
	return tx, true, nil
}

func successfulGeminiAttempt(ctx context.Context, pool *pgxpool.Pool, receiptID uuid.UUID, model string) (bool, error) {
	var processor, version, status string
	err := pool.QueryRow(ctx, `
		SELECT processor_name, processor_version, status
		FROM receipt_processing_attempts
		WHERE receipt_id = $1
		ORDER BY attempt_number DESC LIMIT 1`, receiptID).Scan(&processor, &version, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query E2E extraction attempt: %w", err)
	}
	if status == "failed" {
		return false, errors.New("Gemini extraction attempt is marked failed")
	}
	if status != "succeeded" {
		return false, nil
	}
	if processor != "gemini-vision" || version != model {
		return false, fmt.Errorf("unexpected extractor provenance %s/%s", safeLabel(processor), safeLabel(version))
	}
	return true, nil
}

func requiredExtractionFields(ctx context.Context, pool *pgxpool.Pool, receiptID uuid.UUID) (bool, error) {
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT field_name)
		FROM extracted_fields
		WHERE receipt_document_id = $1
		  AND field_name IN ('merchant', 'total_minor', 'currency')`, receiptID).Scan(&count); err != nil {
		return false, fmt.Errorf("query E2E extraction fields: %w", err)
	}
	return count == 3, nil
}

func outboundSent(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, pattern string, since time.Time) (bool, error) {
	var sent bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM outbound_messages
			WHERE user_id = $1 AND idempotency_key LIKE $2
			  AND status = 'sent' AND created_at >= $3
		)`, userID, pattern, since).Scan(&sent); err != nil {
		return false, fmt.Errorf("query E2E outbound status: %w", err)
	}
	return sent, nil
}

func hasBadOutbound(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, since time.Time) (bool, error) {
	var bad bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM outbound_messages
			WHERE user_id = $1 AND created_at >= $2
			  AND status IN ('failed', 'ambiguous')
		)`, userID, since).Scan(&bad); err != nil {
		return false, fmt.Errorf("query failed E2E outbounds: %w", err)
	}
	return bad, nil
}

func hasDeadJob(ctx context.Context, pool *pgxpool.Pool, since time.Time) (bool, error) {
	var dead bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM queue_jobs WHERE status = 'dead' AND created_at >= $1
		)`, since).Scan(&dead); err != nil {
		return false, fmt.Errorf("query dead E2E jobs: %w", err)
	}
	return dead, nil
}

func attemptFailure(ctx context.Context, pool *pgxpool.Pool, receiptID uuid.UUID, prefix string) error {
	var class, code string
	err := pool.QueryRow(ctx, `
		SELECT error_class, error_code
		FROM receipt_processing_attempts
		WHERE receipt_id = $1
		ORDER BY attempt_number DESC LIMIT 1`, receiptID).Scan(&class, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New(prefix)
	}
	if err != nil {
		return fmt.Errorf("query failed E2E attempt: %w", err)
	}
	return fmt.Errorf("%s (class=%s code=%s)", prefix, safeLabel(class), safeLabel(code))
}
