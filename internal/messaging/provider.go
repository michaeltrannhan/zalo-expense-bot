// Package messaging holds the provider-neutral messaging contract (plan
// §10.5). Domain packages never import provider types; the Zalo adapter
// lives in internal/messaging/zalo and the local dev adapter in
// internal/messaging/logprovider.
package messaging

import (
	"context"
	"io"
	"net/http"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
)

// OutboundMessage is the provider-neutral send request.
type OutboundMessage struct {
	ProviderChatID string
	Text           string
	// IdempotencyKey dedupes producer retries in the local outbox. Providers
	// may ignore it; callers must not assume provider-side exactly-once send.
	IdempotencyKey string
}

// ProviderMessageRef is the provider's receipt for a sent message.
type ProviderMessageRef struct {
	Provider          string
	ProviderMessageID string
}

// MediaMetadata describes a downloaded media object.
type MediaMetadata struct {
	ContentType string
	ByteSize    int64
}

// Provider abstracts the messaging platform (§10.5).
type Provider interface {
	// VerifyWebhook authenticates the inbound webhook call (constant-time
	// secret comparison over headers; body untouched).
	VerifyWebhook(ctx context.Context, headers http.Header, body []byte) error
	// ParseWebhook converts the raw body into normalised events.
	ParseWebhook(ctx context.Context, body []byte) ([]events.InboundEvent, error)
	// Send delivers one message. Implementations must classify failures
	// into domain codes (transient/permanent/quota).
	Send(ctx context.Context, message OutboundMessage) (ProviderMessageRef, error)
	// DownloadMedia streams provider-hosted media with guards (timeout,
	// byte cap, redirect policy). Caller closes the reader.
	DownloadMedia(ctx context.Context, ref events.MediaReference) (io.ReadCloser, MediaMetadata, error)
}

// Name returns the provider identifier stored on rows.
func Name(p domain.Provider) string { return string(p) }
