package zalo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/messaging"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func mediaRef(url string) events.MediaReference {
	return events.MediaReference{Provider: string(domain.ProviderZaloBot), URL: url}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "webhook", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseWebhookFixtures(t *testing.T) {
	c := New(Config{})
	tests := []struct {
		fixture string
		check   func(t *testing.T, body []byte, evs []json.RawMessage, err error)
	}{
		{
			fixture: "text.json",
			check: func(t *testing.T, body []byte, _ []json.RawMessage, err error) {
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
			},
		},
	}
	_ = tests
	_ = c

	t.Run("text", func(t *testing.T) {
		body := readFixture(t, "text.json")
		evs, err := c.ParseWebhook(context.Background(), body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		ev := evs[0]
		if ev.EventType != domain.EventTextReceived {
			t.Errorf("EventType = %q, want %q", ev.EventType, domain.EventTextReceived)
		}
		if ev.Provider != string(domain.ProviderZaloBot) {
			t.Errorf("Provider = %q, want zalo_bot", ev.Provider)
		}
		if ev.ProviderUserID != "8891234567890123456" {
			t.Errorf("ProviderUserID = %q", ev.ProviderUserID)
		}
		if ev.ProviderChatID != "7009876543210987654" {
			t.Errorf("ProviderChatID = %q", ev.ProviderChatID)
		}
		if ev.ProviderMessageID != "a1b2c3d4e5f60718293a4b5c" {
			t.Errorf("ProviderMessageID = %q", ev.ProviderMessageID)
		}
		if ev.Text != "cà phê 35k" {
			t.Errorf("Text = %q", ev.Text)
		}
		if !ev.ReceivedAt.Equal(time.Unix(1752854400, 0).UTC()) {
			t.Errorf("ReceivedAt = %v", ev.ReceivedAt)
		}
		sum := sha256.Sum256(body)
		if ev.RawPayloadHash != hex.EncodeToString(sum[:]) {
			t.Errorf("RawPayloadHash = %q", ev.RawPayloadHash)
		}
		if string(ev.RawPayload) != string(body) {
			t.Errorf("RawPayload mismatch")
		}
	})

	t.Run("image", func(t *testing.T) {
		body := readFixture(t, "image.json")
		evs, err := c.ParseWebhook(context.Background(), body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		ev := evs[0]
		if ev.EventType != domain.EventImageReceived {
			t.Errorf("EventType = %q, want %q", ev.EventType, domain.EventImageReceived)
		}
		if len(ev.Media) != 1 {
			t.Fatalf("got %d media, want 1", len(ev.Media))
		}
		m := ev.Media[0]
		if m.Provider != string(domain.ProviderZaloBot) {
			t.Errorf("Media.Provider = %q", m.Provider)
		}
		if m.URL != "https://bot-api.zapps.me/file/bot123456%3AREDACTED/photos/file_1.jpg" {
			t.Errorf("Media.URL = %q", m.URL)
		}
		if m.MimeType != "image/jpeg" {
			t.Errorf("Media.MimeType = %q", m.MimeType)
		}
		if m.Caption != "hoá đơn siêu thị" {
			t.Errorf("Media.Caption = %q", m.Caption)
		}
	})

	t.Run("unsupported", func(t *testing.T) {
		body := readFixture(t, "unsupported.json")
		evs, err := c.ParseWebhook(context.Background(), body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		ev := evs[0]
		if ev.EventType != domain.EventUnsupported {
			t.Errorf("EventType = %q, want %q", ev.EventType, domain.EventUnsupported)
		}
		if ev.ProviderMessageID != "c3d4e5f60718293a4b5c6d7e" {
			t.Errorf("ProviderMessageID = %q", ev.ProviderMessageID)
		}
	})

	t.Run("missing_fields", func(t *testing.T) {
		body := readFixture(t, "missing_fields.json")
		_, err := c.ParseWebhook(context.Background(), body)
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})
}

func TestParseWebhookVariants(t *testing.T) {
	c := New(Config{})
	tests := []struct {
		name      string
		body      string
		wantType  string
		wantUser  string
		wantMsgID string
		wantURL   string
	}{
		{
			name:     "numeric ids",
			body:     `{"event_name":"message.text.received","message":{"message_id":12345,"from":{"id":678},"chat":{"id":901},"date":1752854400,"text":"xin chào"}}`,
			wantType: domain.EventTextReceived, wantUser: "678", wantMsgID: "12345",
		},
		{
			name:     "photos array variant",
			body:     `{"event_name":"message.image.received","message":{"message_id":"m1","from":{"id":"u1"},"chat":{"id":"c1"},"photos":[{"url":"https://example.com/a.jpg"}]}}`,
			wantType: domain.EventImageReceived, wantUser: "u1", wantMsgID: "m1",
			wantURL: "https://example.com/a.jpg",
		},
		{
			name:     "image object variant",
			body:     `{"event_name":"message.image.received","message":{"message_id":"m2","from":{"id":"u2"},"chat":{"id":"c2"},"image":{"url":"https://example.com/b.png","mime_type":"image/png"}}}`,
			wantType: domain.EventImageReceived, wantUser: "u2", wantMsgID: "m2",
			wantURL: "https://example.com/b.png",
		},
		{
			name:     "image string variant",
			body:     `{"event_name":"message.image.received","message":{"message_id":"m3","from":{"id":"u3"},"chat":{"id":"c3"},"image":"https://example.com/c.jpg"}}`,
			wantType: domain.EventImageReceived, wantUser: "u3", wantMsgID: "m3",
			wantURL: "https://example.com/c.jpg",
		},
		{
			name:     "live polling photo_url string",
			body:     `{"event_name":"message.image.received","message":{"message_id":"m-live","from":{"id":"u-live"},"chat":{"id":"c-live"},"date":1786530300000,"message_type":"photo","photo_url":"https://example.com/live.jpg"}}`,
			wantType: domain.EventImageReceived, wantUser: "u-live", wantMsgID: "m-live",
			wantURL: "https://example.com/live.jpg",
		},
		{
			name:     "attachments file_url variant",
			body:     `{"event_name":"message.image.received","message":{"message_id":"m4","from":{"id":"u4"},"chat":{"id":"c4"},"attachments":[{"file_url":"https://example.com/d.jpg"}]}}`,
			wantType: domain.EventImageReceived, wantUser: "u4", wantMsgID: "m4",
			wantURL: "https://example.com/d.jpg",
		},
		{
			name:     "unknown event never errors",
			body:     `{"event_name":"message.video.received","message":{"message_id":"m5","from":{"id":"u5"},"chat":{"id":"c5"}}}`,
			wantType: domain.EventUnsupported, wantUser: "u5", wantMsgID: "m5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evs, err := c.ParseWebhook(context.Background(), []byte(tt.body))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			if len(evs) != 1 {
				t.Fatalf("got %d events, want 1", len(evs))
			}
			ev := evs[0]
			if ev.EventType != tt.wantType {
				t.Errorf("EventType = %q, want %q", ev.EventType, tt.wantType)
			}
			if tt.wantUser != "" && ev.ProviderUserID != tt.wantUser {
				t.Errorf("ProviderUserID = %q, want %q", ev.ProviderUserID, tt.wantUser)
			}
			if tt.wantMsgID != "" && ev.ProviderMessageID != tt.wantMsgID {
				t.Errorf("ProviderMessageID = %q, want %q", ev.ProviderMessageID, tt.wantMsgID)
			}
			if tt.wantURL != "" {
				if len(ev.Media) != 1 || ev.Media[0].URL != tt.wantURL {
					t.Errorf("Media = %+v, want URL %q", ev.Media, tt.wantURL)
				}
			}
			if ev.ReceivedAt.IsZero() {
				t.Errorf("ReceivedAt not set")
			}
		})
	}
}

func TestParseCurrentWebhookEnvelopeAndPhoto(t *testing.T) {
	c := New(Config{})
	body := []byte(`{
		"ok": true,
		"result": {
			"event_name": "message.image.received",
			"message": {
				"message_id": "m-current",
				"from": {"id": "u-current", "display_name": "Test"},
				"chat": {"id": "c-current", "chat_type": "PRIVATE"},
				"date": 1750316131602,
				"photo": "https://files.example.test/current.jpg",
				"caption": "receipt"
			}
		}
	}`)
	evs, err := c.ParseWebhook(context.Background(), body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.ProviderMessageID != "m-current" || ev.ProviderUserID != "u-current" || ev.ProviderChatID != "c-current" {
		t.Errorf("identity fields = %+v", ev)
	}
	if got, want := ev.ReceivedAt, time.UnixMilli(1750316131602).UTC(); !got.Equal(want) {
		t.Errorf("ReceivedAt = %v, want %v", got, want)
	}
	if len(ev.Media) != 1 || ev.Media[0].URL != "https://files.example.test/current.jpg" {
		t.Errorf("Media = %+v", ev.Media)
	}
}

func TestParseWebhookInvalidJSON(t *testing.T) {
	c := New(Config{})
	for _, body := range []string{"{not json", "", `[1,2,3]`, `{}`} {
		if _, err := c.ParseWebhook(context.Background(), []byte(body)); !domain.IsCode(err, domain.CodeValidation) {
			t.Errorf("body %q: err = %v, want CodeValidation", body, err)
		}
	}
}

func TestVerifyWebhook(t *testing.T) {
	c := New(Config{WebhookSecret: "s3cret-token"})
	tests := []struct {
		name   string
		header string
		set    bool
		want   domain.Code
	}{
		{name: "ok", header: "s3cret-token", set: true},
		{name: "ok with padding", header: "  s3cret-token  ", set: true},
		{name: "wrong", header: "nope", set: true, want: domain.CodeForbidden},
		{name: "missing", set: false, want: domain.CodeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.set {
				h.Set("X-Bot-Api-Secret-Token", tt.header)
			}
			err := c.VerifyWebhook(context.Background(), h, []byte(`{"body":"untouched"}`))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("VerifyWebhook: %v", err)
				}
				return
			}
			if !domain.IsCode(err, tt.want) {
				t.Fatalf("err = %v, want %s", err, tt.want)
			}
		})
	}

	t.Run("unconfigured secret", func(t *testing.T) {
		bare := New(Config{})
		h := http.Header{}
		h.Set("X-Bot-Api-Secret-Token", "anything")
		if err := bare.VerifyWebhook(context.Background(), h, nil); !domain.IsCode(err, domain.CodeForbidden) {
			t.Fatalf("err = %v, want CodeForbidden", err)
		}
	})
}

func TestSend(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/botT0K3N/sendMessage" {
				t.Errorf("path = %q, want /botT0K3N/sendMessage", r.URL.Path)
			}
			if r.Method != http.MethodPost {
				t.Errorf("method = %q", r.Method)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			if body["chat_id"] != "c1" || body["text"] != "đã ghi nhận" {
				t.Errorf("body = %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":12345}}`)
		}))
		defer srv.Close()

		c := New(Config{Token: "T0K3N", APIBase: srv.URL})
		ref, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "đã ghi nhận"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if ref.Provider != string(domain.ProviderZaloBot) {
			t.Errorf("ref.Provider = %q", ref.Provider)
		}
		if ref.ProviderMessageID != "12345" {
			t.Errorf("ref.ProviderMessageID = %q, want 12345", ref.ProviderMessageID)
		}
	})

	t.Run("missing message_id tolerated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"ok":true,"result":{}}`)
		}))
		defer srv.Close()
		c := New(Config{Token: "T0K3N", APIBase: srv.URL})
		ref, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "x"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if ref.ProviderMessageID != "" {
			t.Errorf("ref.ProviderMessageID = %q, want empty", ref.ProviderMessageID)
		}
	})

	t.Run("malformed success response is ambiguous", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{not-json`)
		}))
		defer srv.Close()
		c := New(Config{Token: "T0K3N", APIBase: srv.URL})
		_, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "x"})
		if !domain.IsCode(err, domain.CodeTransient) {
			t.Fatalf("err = %v, want CodeTransient", err)
		}
	})

	t.Run("status classification", func(t *testing.T) {
		tests := []struct {
			status int
			want   domain.Code
		}{
			{http.StatusTooManyRequests, domain.CodeTransient},
			{http.StatusInternalServerError, domain.CodeTransient},
			{http.StatusBadGateway, domain.CodeTransient},
			{http.StatusBadRequest, domain.CodeValidation},
			{http.StatusUnauthorized, domain.CodeValidation},
			{http.StatusNotFound, domain.CodeValidation},
		}
		for _, tt := range tests {
			t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tt.status)
					fmt.Fprint(w, `{"ok":false,"description":"boom"}`)
				}))
				defer srv.Close()
				c := New(Config{Token: "T0K3N", APIBase: srv.URL})
				_, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "x"})
				if !domain.IsCode(err, tt.want) {
					t.Fatalf("err = %v, want %s", err, tt.want)
				}
				if domain.Retryable(err) != (tt.want == domain.CodeTransient) {
					t.Errorf("Retryable(%v) mismatch", err)
				}
			})
		}
	})

	t.Run("empty token", func(t *testing.T) {
		c := New(Config{})
		_, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "x"})
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})

	t.Run("validates provider text limit", func(t *testing.T) {
		c := New(Config{Token: "T0K3N"})
		for _, text := range []string{"", "   ", strings.Repeat("x", 2001)} {
			_, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: text})
			if !domain.IsCode(err, domain.CodeValidation) {
				t.Errorf("text length %d: err = %v, want CodeValidation", len(text), err)
			}
		}
	})

	t.Run("transport errors redact token", func(t *testing.T) {
		const token = "secret:bot-token"
		hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial failed for %s", req.URL.String())
		})}
		c := New(Config{Token: token, HTTPClient: hc})
		_, err := c.Send(context.Background(), messaging.OutboundMessage{ProviderChatID: "c1", Text: "hello"})
		if err == nil {
			t.Fatal("Send returned nil error")
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "secret%3Abot-token") {
			t.Fatalf("transport error leaked token: %v", err)
		}
	})
}

func TestGetMe(t *testing.T) {
	t.Run("valid token metadata", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/botT0K3N/getMe" {
				t.Errorf("request = %s %s", r.Method, r.URL.Path)
			}
			fmt.Fprint(w, `{"ok":true,"result":{"id":2948222836272997507,"account_name":"bot.test","account_type":"BASIC","can_join_groups":false}}`)
		}))
		defer srv.Close()
		info, err := New(Config{Token: "T0K3N", APIBase: srv.URL}).GetMe(context.Background())
		if err != nil {
			t.Fatalf("GetMe: %v", err)
		}
		if info.ID != "2948222836272997507" || info.AccountType != "BASIC" || info.AccountName != "bot.test" {
			t.Errorf("info = %+v", info)
		}
	})

	t.Run("missing token", func(t *testing.T) {
		_, err := New(Config{}).GetMe(context.Background())
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})
}

func TestGetUpdatesTimeoutIsEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		fmt.Fprint(w, `{"ok":false,"description":"Request timeout"}`)
	}))
	defer srv.Close()
	c := New(Config{Token: "T0K3N", APIBase: srv.URL})
	evs, next, err := c.GetUpdates(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("evs = %v, want empty", evs)
	}
	if next != 42 {
		t.Errorf("next = %d, want 42", next)
	}
}

func TestGetUpdatesCurrentSingleResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		fmt.Fprint(w, `{"ok":true,"result":{"event_name":"message.text.received","message":{"message_id":"m1","from":{"id":"u1"},"chat":{"id":"c1"},"date":1750316131602,"text":"hello"}}}`)
	}))
	defer srv.Close()
	c := New(Config{Token: "T0K3N", APIBase: srv.URL})
	evs, next, err := c.GetUpdates(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(evs) != 1 || evs[0].ProviderMessageID != "m1" || evs[0].Text != "hello" {
		t.Fatalf("events = %+v", evs)
	}
	if !evs[0].ReceivedAt.Equal(time.UnixMilli(1750316131602).UTC()) {
		t.Errorf("ReceivedAt = %v", evs[0].ReceivedAt)
	}
	if next != 0 {
		t.Errorf("next = %d, want unchanged offset 0 without update_id", next)
	}
}

func TestGetUpdatesNonArrayResult(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"null result", `{"ok":true,"result":null}`},
		{"empty object result", `{"ok":true,"result":{}}`},
		{"empty array result", `{"ok":true,"result":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()
			c := New(Config{Token: "T0K3N", APIBase: srv.URL})
			evs, next, err := c.GetUpdates(context.Background(), 0)
			if err != nil {
				t.Fatalf("GetUpdates: %v", err)
			}
			if len(evs) != 0 {
				t.Errorf("evs = %v, want empty", evs)
			}
			if next != 0 {
				t.Errorf("next = %d, want 0", next)
			}
		})
	}
}

func TestGetUpdatesRealErrorStillFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	defer srv.Close()
	c := New(Config{Token: "BAD", APIBase: srv.URL})
	_, _, err := c.GetUpdates(context.Background(), 0)
	if !domain.IsCode(err, domain.CodeValidation) {
		t.Fatalf("err = %v, want CodeValidation", err)
	}
}

func TestGetUpdatesScalarResultFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ok":true,"result":"unexpected"}`)
	}))
	defer srv.Close()
	c := New(Config{Token: "T0K3N", APIBase: srv.URL})
	_, _, err := c.GetUpdates(context.Background(), 0)
	if !domain.IsCode(err, domain.CodeTransient) {
		t.Fatalf("err = %v, want CodeTransient", err)
	}
}

func TestDownloadMedia(t *testing.T) {
	t.Run("ok with content type", func(t *testing.T) {
		payload := []byte("fake jpeg bytes")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(payload)
		}))
		defer srv.Close()
		c := New(Config{})
		rc, meta, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL+"/f"))
		if err != nil {
			t.Fatalf("DownloadMedia: %v", err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(payload) {
			t.Errorf("body = %q", got)
		}
		if meta.ContentType != "image/jpeg" {
			t.Errorf("ContentType = %q", meta.ContentType)
		}
		if meta.ByteSize != int64(len(payload)) {
			t.Errorf("ByteSize = %d", meta.ByteSize)
		}
	})

	t.Run("sniffs content type", func(t *testing.T) {
		png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Del("Content-Type")
			w.Write(png)
		}))
		defer srv.Close()
		c := New(Config{})
		rc, meta, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL))
		if err != nil {
			t.Fatalf("DownloadMedia: %v", err)
		}
		rc.Close()
		if meta.ContentType != "image/png" {
			t.Errorf("ContentType = %q, want image/png", meta.ContentType)
		}
	})

	t.Run("byte cap", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write(make([]byte, maxMediaBytes+100))
		}))
		defer srv.Close()
		c := New(Config{})
		_, _, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL))
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})

	t.Run("redirects within limit", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
		mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
		mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/final", http.StatusFound) })
		mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := New(Config{})
		rc, _, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL+"/a"))
		if err != nil {
			t.Fatalf("DownloadMedia: %v", err)
		}
		rc.Close()
	})

	t.Run("redirects over limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
		}))
		defer srv.Close()
		c := New(Config{})
		_, _, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL+"/r"))
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		c := New(Config{})
		_, _, err := c.DownloadMedia(context.Background(), mediaRef(srv.URL))
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})

	t.Run("empty url", func(t *testing.T) {
		c := New(Config{})
		_, _, err := c.DownloadMedia(context.Background(), mediaRef(""))
		if !domain.IsCode(err, domain.CodeValidation) {
			t.Fatalf("err = %v, want CodeValidation", err)
		}
	})
}
