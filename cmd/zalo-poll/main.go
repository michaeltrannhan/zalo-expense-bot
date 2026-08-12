// Command zalo-poll long-polls the Zalo Bot API and prints each normalised
// inbound event as one JSON line. It persists nothing; it exists to eyeball
// real bot traffic locally. Message content is redacted unless -show-text
// is passed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/messaging/zalo"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "zalo-poll:", err)
		os.Exit(1)
	}
}

func run() error {
	showText := flag.Bool("show-text", false, "include message text and media references in output")
	flag.Parse()

	token := os.Getenv("ZALO_BOT_TOKEN")
	if token == "" {
		return errors.New("ZALO_BOT_TOKEN is required")
	}
	client := zalo.New(zalo.Config{
		Token:   token,
		APIBase: os.Getenv("ZALO_API_BASE"),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	enc := json.NewEncoder(os.Stdout)
	var offset int64
	for {
		evs, next, err := client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(os.Stderr, "zalo-poll: poll error:", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		offset = next
		for _, ev := range evs {
			if err := enc.Encode(redact(ev, *showText)); err != nil {
				return fmt.Errorf("write event: %w", err)
			}
		}
	}
}

// line is the printed shape; content fields stay empty unless -show-text.
type line struct {
	Provider          string                  `json:"provider"`
	ProviderUserID    string                  `json:"provider_user_id"`
	ProviderChatID    string                  `json:"provider_chat_id"`
	ProviderMessageID string                  `json:"provider_message_id"`
	EventType         string                  `json:"event_type"`
	ReceivedAt        time.Time               `json:"received_at"`
	MediaCount        int                     `json:"media_count,omitempty"`
	Text              string                  `json:"text,omitempty"`
	Media             []events.MediaReference `json:"media,omitempty"`
}

func redact(ev events.InboundEvent, showText bool) line {
	l := line{
		Provider:          ev.Provider,
		ProviderUserID:    ev.ProviderUserID,
		ProviderChatID:    ev.ProviderChatID,
		ProviderMessageID: ev.ProviderMessageID,
		EventType:         ev.EventType,
		ReceivedAt:        ev.ReceivedAt,
		MediaCount:        len(ev.Media),
	}
	if showText {
		l.Text = ev.Text
		l.Media = ev.Media
	}
	return l
}
