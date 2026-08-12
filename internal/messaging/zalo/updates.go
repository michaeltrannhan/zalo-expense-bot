package zalo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
)

const (
	pollAPITimeoutSeconds = 25               // server-side long-poll hold
	pollClientTimeout     = 30 * time.Second // long poll + slack
)

// update is one getUpdates result entry.
type update struct {
	UpdateID  int64           `json:"update_id"`
	EventName string          `json:"event_name"`
	Message   json.RawMessage `json:"message"`
}

// GetUpdates long-polls the Bot API and normalises each update with the
// same mapping as ParseWebhook. nextOffset is max(update_id)+1, or the
// incoming offset when the result is empty. A long-poll "Request timeout"
// from the API is normal (no updates within the hold window) and is
// treated as an empty result, not an error.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]events.InboundEvent, int64, error) {
	if c.token == "" {
		return nil, offset, domain.E(domain.CodeValidation, "zalo bot token not configured", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, pollClientTimeout)
	defer cancel()
	u := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=%d",
		c.base, c.token, offset, pollAPITimeoutSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return nil, offset, domain.E(domain.CodeValidation, "build getUpdates request", c.safeCause(err))
	}
	hc := *c.http
	hc.Timeout = 0 // bounded by ctx instead; default client timeout is 10s
	resp, err := hc.Do(req)
	if err != nil {
		return nil, offset, domain.E(domain.CodeTransient, "getUpdates request failed", c.safeCause(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, offset, domain.E(domain.CodeTransient, "read getUpdates response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, offset, c.classifyStatus(resp.StatusCode, body)
	}
	// Parse the envelope with a flexible Result field — Zalo sometimes
	// returns result as an object {} or null instead of an array.
	var raw struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, offset, domain.E(domain.CodeTransient, "decode getUpdates response", err)
	}
	if !raw.OK {
		// Long-poll hold window expired with no updates — normal, keep polling.
		if strings.Contains(strings.ToLower(raw.Description), "timeout") {
			return nil, offset, nil
		}
		return nil, offset, domain.Ef(domain.CodeValidation, nil, "getUpdates rejected: %s", c.redact(raw.Description))
	}
	// Normalise result into a slice of update entries. Zalo sometimes
	// returns result as null, an object {}, or a bare value instead of
	// an array; only treat actual arrays and non-empty single objects
	// as updates.
	var updates []json.RawMessage
	if len(raw.Result) > 0 && string(raw.Result) != "null" {
		if err := json.Unmarshal(raw.Result, &updates); err != nil {
			// Not an array — try as a single update entry, but skip
			// empty objects {} which carry no data.
			var probe map[string]any
			if serr := json.Unmarshal(raw.Result, &probe); serr == nil && len(probe) > 0 {
				updates = []json.RawMessage{raw.Result}
			} else if serr != nil {
				return nil, offset, domain.E(domain.CodeTransient, "unexpected getUpdates result shape", nil)
			}
		}
	}
	evs := make([]events.InboundEvent, 0, len(updates))
	next := offset
	for _, raw := range updates {
		var u update
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, offset, domain.E(domain.CodeValidation, "invalid update entry", err)
		}
		eventType := u.EventName
		if eventType == "" {
			eventType = inferEventType(u.Message)
		} else if eventType != domain.EventTextReceived && eventType != domain.EventImageReceived {
			eventType = domain.EventUnsupported
		}
		ev, err := buildEvent(eventType, u.Message, raw)
		if err != nil {
			return nil, offset, err
		}
		evs = append(evs, ev)
		if u.UpdateID > 0 && u.UpdateID >= next {
			next = u.UpdateID + 1
		}
	}
	return evs, next, nil
}

// inferEventType derives the event type from message content when the
// update carries no explicit event_name.
func inferEventType(msgRaw json.RawMessage) string {
	var msg zaloMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return domain.EventUnsupported
	}
	if msg.Text != "" {
		return domain.EventTextReceived
	}
	if _, ok := msg.firstMedia(); ok {
		return domain.EventImageReceived
	}
	return domain.EventUnsupported
}
