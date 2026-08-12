// Package logprovider is the development messaging.Provider: it never dials
// out. Sends are logged and recorded so the simulate harness and local runs
// exercise the full stack without a Zalo bot token.
package logprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/mock"
	"zl-expese-bot/internal/messaging"
)

// Provider logs outbound messages and captures them for inspection.
type Provider struct {
	log *slog.Logger

	mu   sync.Mutex
	sent []messaging.OutboundMessage
}

func New(log *slog.Logger) *Provider {
	return &Provider{log: log}
}

// VerifyWebhook always passes: the log provider is for local flows that
// inject events directly, not for real webhook traffic.
func (p *Provider) VerifyWebhook(context.Context, http.Header, []byte) error {
	return nil
}

// ParseWebhook decodes the provider-neutral local raw shape: one JSON
// events.InboundEvent. The simulate harness stores exactly this in
// provider_messages.raw_payload_json so the receipt worker can recover
// media references through the same code path as real providers. Like the
// Zalo adapter, the raw body and its sha256 are retained on the event —
// without them the provider_messages insert violates NOT NULL.
func (p *Provider) ParseWebhook(_ context.Context, body []byte) ([]events.InboundEvent, error) {
	var ev events.InboundEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, domain.E(domain.CodeValidation, "logprovider decode event", err)
	}
	sum := sha256.Sum256(body)
	ev.RawPayload = body
	ev.RawPayloadHash = hex.EncodeToString(sum[:])
	return []events.InboundEvent{ev}, nil
}

// Send records the message and logs it with the idempotency key.
func (p *Provider) Send(_ context.Context, msg messaging.OutboundMessage) (messaging.ProviderMessageRef, error) {
	p.mu.Lock()
	p.sent = append(p.sent, msg)
	n := len(p.sent)
	p.mu.Unlock()
	p.log.Info("outbound message",
		slog.String("chat_id_hash", hashForLog(msg.ProviderChatID)),
		slog.String("idempotency_key", msg.IdempotencyKey))
	fmt.Printf("\n[BOT → %s]\n%s\n\n", msg.ProviderChatID, msg.Text)
	return messaging.ProviderMessageRef{
		Provider:          "log",
		ProviderMessageID: fmt.Sprintf("log-%d", n),
	}, nil
}

// Sent returns a copy of captured messages (test/harness use).
func (p *Provider) Sent() []messaging.OutboundMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]messaging.OutboundMessage, len(p.sent))
	copy(out, p.sent)
	return out
}

// DownloadMedia serves dev-harness fixtures: a URL of the form
// "MOCK-FIXTURE:<id>" yields the selector bytes themselves, which
// receipt.ValidateAndHash accepts and the mock extractor maps to a corpus
// entry. Anything else is unsupported locally.
func (p *Provider) DownloadMedia(_ context.Context, ref events.MediaReference) (io.ReadCloser, messaging.MediaMetadata, error) {
	if strings.HasPrefix(ref.URL, mock.MockPrefix) {
		body := ref.URL
		return io.NopCloser(strings.NewReader(body)), messaging.MediaMetadata{
			ContentType: "text/plain",
			ByteSize:    int64(len(body)),
		}, nil
	}
	return nil, messaging.MediaMetadata{},
		domain.E(domain.CodeUnsupported, "logprovider only downloads "+mock.MockPrefix+" media", nil)
}

func hashForLog(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return "****" + s[len(s)-4:]
}
