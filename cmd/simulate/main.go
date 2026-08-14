// Command simulate runs the Gate 2 demo loop entirely in-process against
// local PostgreSQL:
//
//	consent → receipt image → mock extraction → confirmation card →
//	confirm → /homnay → duplicate-webhook replay → duplicate-image guard
//
// It always uses the log messaging provider (bot messages print to stdout)
// and the deterministic mock extractor: no Zalo bot token, no network, no
// cloud. The exit code is non-zero when any assertion fails.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

// demoSender/demoChat are the fixed sender identity; reruns reuse the same
// local user, which also exercises returning-user paths.
const (
	demoSender = "demo-sender-1"
	demoChat   = "demo-chat-1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "simulate:", err)
		os.Exit(1)
	}
}

// resetTables mirrors pgtest: user-linked data is wiped so every demo run
// is deterministic, while global seeds (categories, merchants, aliases)
// survive. -keep opts out.
var resetTables = []string{
	"pending_actions",
	"outbound_messages",
	"queue_jobs",
	"usage_counters",
	"insights",
	"user_merchant_rules",
	"corrections",
	"predictions",
	"extracted_fields",
	"transactions",
	"receipt_processing_attempts",
	"receipt_documents",
	"provider_messages",
	"user_identities",
	"users",
}

func run() error {
	keep := flag.Bool("keep", false, "keep existing data (default wipes user-linked tables for a deterministic run)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Demo output is the conversation itself; keep service logs at warn so
	// they only surface when something is wrong.
	log := logging.New("warn")

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
	if !*keep {
		for _, table := range resetTables {
			if _, err := pool.Exec(ctx, "TRUNCATE "+table+" CASCADE"); err != nil {
				return fmt.Errorf("reset %s: %w", table, err)
			}
		}
	}

	st := store.New(pool)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	objects, err := objectstore.NewLocal(cfg.DataDir)
	if err != nil {
		return err
	}
	provider := logprovider.New(log)
	replies := notify.NewEnqueuer(st, q, clk)
	cats := categorisation.NewService(st, clk)
	insights := insight.NewService(st)

	processor := receipt.NewProcessor(st, objects, mock.New(), provider, cats, replies, clk, log)
	processor.ExtractionEnabled = cfg.ExtractionEnabled
	processor.MonthlyOCRLimit = cfg.MonthlyOCRPageLimit

	h := &harness{
		ctx:       ctx,
		pool:      pool,
		handler:   bot.NewHandler(st, q, objects, replies, cats, insights, clk, log, cfg),
		processor: processor,
		sender:    notify.NewSender(st, provider, clk, log, cfg.OutboundEnabled, cfg.ZaloMonthlyMessageLimit),
		provider:  provider,
		q:         q,
	}
	return h.play()
}

// harness wires the in-process demo with the same handler, processor and
// sender the three production binaries use; queues are drained inline so
// every step is fully deterministic.
type harness struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	handler   *bot.Handler
	processor *receipt.Processor
	sender    *notify.Sender
	provider  *logprovider.Provider
	q         queue.Queue

	msgSeq   int
	failures int
}

// play runs the Gate 2 demo script: each step drives one exchange through
// the real handler and asserts the persisted outcome.
func (h *harness) play() error {
	fmt.Println("=== Gate 2 demo: Zalo expense tracker (local, no Zalo, no cloud) ===")

	step(1, "Consent — a new sender starts with /batdau")
	if err := h.say("/batdau"); err != nil {
		return err
	}
	h.check(h.scalar(`SELECT status FROM users`) == "active", "user consented and active")
	h.check(h.count(`SELECT count(*) FROM users`) == 1, "exactly one user exists")
	h.check(strings.Contains(h.lastOutbound(), "Cảm ơn bạn"), "welcome message sent")

	step(2, "Receipt image → queued → extracted → confirmation card")
	image := h.imageEvent("MOCK-FIXTURE:coopmart-clean")
	if err := h.deliver(image); err != nil {
		return err
	}
	h.check(h.count(`SELECT count(*) FROM receipt_documents`) == 1, "one receipt registered")
	h.check(h.scalar(`SELECT status FROM receipt_documents`) == string(domain.ReceiptReviewRequired),
		"receipt awaits review")
	h.check(h.scalar(`SELECT status FROM transactions`) == string(domain.TxAwaitingConfirmation),
		"draft transaction awaits confirmation")
	h.check(h.count(`SELECT count(*) FROM transactions WHERE amount_minor = 325000 AND currency = 'VND'`) == 1,
		"extracted total is 325.000 ₫ VND")
	card := h.lastOutbound()
	// The card shows the merchant as resolved (seed canonical name or, for a
	// first-time sighting, the receipt's own spelling) — match either.
	h.check(strings.Contains(strings.ToLower(card), "co.opmart") && strings.Contains(card, "325.000"),
		"confirmation card shows merchant and total")

	step(3, "Confirm — the user replies “xác nhận”")
	if err := h.say("xác nhận"); err != nil {
		return err
	}
	h.check(h.scalar(`SELECT status FROM transactions`) == string(domain.TxConfirmed), "transaction confirmed")
	h.check(h.scalar(`SELECT status FROM receipt_documents`) == string(domain.ReceiptConfirmed), "receipt confirmed")
	h.check(strings.Contains(h.lastOutbound(), "Đã ghi nhận"), "confirmation acknowledged")
	h.check(h.count(`SELECT count(*) FROM pending_actions`) == 0, "pending action cleared")

	step(4, "Manual entry — “150000 ăn trưa” + confirm")
	if err := h.say("150000 ăn trưa"); err != nil {
		return err
	}
	h.check(strings.Contains(h.lastOutbound(), "150.000"), "manual-entry card shows 150.000 ₫")
	if err := h.say("xác nhận"); err != nil {
		return err
	}
	h.check(h.count(`SELECT count(*) FROM transactions WHERE status = 'confirmed'`) == 2,
		"two confirmed transactions")

	step(5, "/homnay — today's recorded spending")
	if err := h.say("/homnay"); err != nil {
		return err
	}
	summary := h.lastOutbound()
	h.check(strings.Contains(summary, "Hôm nay") && strings.Contains(summary, "150.000"),
		"daily summary includes the confirmed manual entry")

	step(6, "Duplicate webhook replay — same provider message ID")
	before := h.count(`SELECT count(*) FROM provider_messages`)
	if err := h.deliver(image); err != nil {
		return err
	}
	h.check(h.count(`SELECT count(*) FROM provider_messages`) == before, "replayed webhook absorbed, no new row")
	h.check(h.count(`SELECT count(*) FROM receipt_documents`) == 1, "still one receipt")
	h.check(h.count(`SELECT count(*) FROM transactions`) == 2, "still two transactions")

	step(7, "Same image bytes, new message — content duplicate guard")
	if err := h.snap("MOCK-FIXTURE:coopmart-clean"); err != nil {
		return err
	}
	h.check(h.count(`SELECT count(*) FROM receipt_documents`) == 2, "second receipt registered")
	h.check(h.count(`SELECT count(*) FROM receipt_documents WHERE status = 'failed_permanent'`) == 1,
		"duplicate receipt marked failed_permanent")
	h.check(h.count(`SELECT count(*) FROM transactions`) == 2, "no duplicate transaction created")
	h.check(strings.Contains(h.lastOutbound(), "đã xử lý trước đó"), "user told the image is a duplicate")

	step(8, "Stale confirm — nothing pending")
	if err := h.say("xác nhận"); err != nil {
		return err
	}
	h.check(strings.Contains(h.lastOutbound(), "hết hạn"), "stale confirm answered with expired notice")
	h.check(h.count(`SELECT count(*) FROM transactions`) == 2, "transaction count unchanged")

	step(9, "Delete recent — two-step individual deletion")
	if err := h.say("xóa khoản gần nhất"); err != nil {
		return err
	}
	ask := h.lastOutbound()
	h.check(strings.Contains(ask, "Bạn muốn xóa khoản này?") && strings.Contains(ask, "150.000"),
		"delete prompt shows the newest recorded transaction")
	if err := h.say("xác nhận"); err != nil {
		return err
	}
	h.check(strings.Contains(h.lastOutbound(), "Đã xóa: 150.000 ₫ tại ăn trưa"), "manual entry deleted")
	h.check(h.count(`SELECT count(*) FROM transactions WHERE status = 'confirmed'`) == 1,
		"one confirmed transaction remains")
	h.check(h.count(`SELECT count(*) FROM pending_actions`) == 0, "pending action cleared")

	step(10, "Delete recent — cancel keeps the transaction")
	if err := h.say("xóa khoản gần nhất"); err != nil {
		return err
	}
	h.check(strings.Contains(h.lastOutbound(), "Bạn muốn xóa khoản này?"), "second delete prompt shown")
	if err := h.say("bỏ qua"); err != nil {
		return err
	}
	h.check(strings.Contains(h.lastOutbound(), "không xóa"), "cancel acknowledged")
	h.check(h.count(`SELECT count(*) FROM transactions WHERE status = 'confirmed'`) == 1,
		"transaction kept after cancel")

	fmt.Println()
	if h.failures > 0 {
		return fmt.Errorf("Gate 2 demo FAILED: %d assertion(s) failed", h.failures)
	}
	fmt.Println("=== Gate 2 demo: PASS — capture → understand → confirm → summarise, all idempotent ===")
	return nil
}

func step(n int, title string) {
	fmt.Printf("\n── Step %d: %s\n", n, title)
}

// say delivers one text message through the full inbound path and drains
// all queued work it triggers.
func (h *harness) say(text string) error {
	fmt.Printf("\n[USER → bot] %s\n", text)
	return h.deliver(h.textEvent(text))
}

// snap delivers one image message; the URL selects the mock corpus entry.
func (h *harness) snap(imageURL string) error {
	fmt.Printf("\n[USER → bot] 📷 %s\n", imageURL)
	return h.deliver(h.imageEvent(imageURL))
}

// deliver runs one event through HandleEvent and drains the queues.
func (h *harness) deliver(ev events.InboundEvent) error {
	if err := h.handler.HandleEvent(h.ctx, ev); err != nil {
		return fmt.Errorf("handle %s %s: %w", ev.EventType, ev.ProviderMessageID, err)
	}
	return worker.Drain(h.ctx, h.q, []worker.DrainStep{
		{Kind: domain.JobReceiptProcess, Handle: h.processor.Handle},
		{Kind: domain.JobOutboundSend, Handle: h.sender.Handle},
	})
}

// textEvent builds a normalised inbound text event.
func (h *harness) textEvent(text string) events.InboundEvent {
	return h.event(domain.EventTextReceived, text, nil)
}

// imageEvent builds a normalised inbound image event. The media URL uses
// the mock fixture scheme, which the log provider "downloads" as bytes.
func (h *harness) imageEvent(imageURL string) events.InboundEvent {
	return h.event(domain.EventImageReceived, "", []events.MediaReference{{
		Provider: string(domain.ProviderZaloBot),
		URL:      imageURL,
		MimeType: "image/jpeg",
	}})
}

// event stamps a deterministic provider message ID and retains the raw
// payload in the log-provider shape so the receipt worker can recover the
// media reference exactly as it would from a real webhook payload.
func (h *harness) event(eventType, text string, media []events.MediaReference) events.InboundEvent {
	h.msgSeq++
	ev := events.InboundEvent{
		Provider:          string(domain.ProviderZaloBot),
		ProviderUserID:    demoSender,
		ProviderChatID:    demoChat,
		ProviderMessageID: fmt.Sprintf("sim-%04d", h.msgSeq),
		EventType:         eventType,
		Text:              text,
		Media:             media,
		ReceivedAt:        time.Now().UTC(),
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		panic(err) // static struct; cannot fail
	}
	sum := sha256.Sum256(raw)
	ev.RawPayload = raw
	ev.RawPayloadHash = hex.EncodeToString(sum[:])
	return ev
}

// check records one assertion.
func (h *harness) check(ok bool, format string, args ...any) {
	mark := "✓"
	if !ok {
		mark = "✗"
		h.failures++
	}
	fmt.Printf("  %s %s\n", mark, fmt.Sprintf(format, args...))
}

// lastOutbound returns the most recent message the bot "sent".
func (h *harness) lastOutbound() string {
	sent := h.provider.Sent()
	if len(sent) == 0 {
		return ""
	}
	return sent[len(sent)-1].Text
}

// count runs a COUNT-style scalar query; a query failure counts as a demo
// failure and returns -1.
func (h *harness) count(query string) int64 {
	var n int64
	if err := h.pool.QueryRow(h.ctx, query).Scan(&n); err != nil {
		h.failures++
		fmt.Printf("  ✗ query failed: %v\n", err)
		return -1
	}
	return n
}

// scalar runs a single-row string scalar query.
func (h *harness) scalar(query string) string {
	var s string
	if err := h.pool.QueryRow(h.ctx, query).Scan(&s); err != nil {
		h.failures++
		fmt.Printf("  ✗ query failed: %v\n", err)
		return ""
	}
	return s
}
