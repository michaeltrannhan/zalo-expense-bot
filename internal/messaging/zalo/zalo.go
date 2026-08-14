// Package zalo is the messaging.Provider adapter for the Zalo Bot API
// (bot-api.zaloplatforms.com). Plain HTTPS only — no SDK, no AWS. Zalo wire types
// never leave this package; everything crossing the boundary is
// events.InboundEvent / messaging.OutboundMessage.
package zalo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/messaging"
)

const (
	defaultAPIBase = "https://bot-api.zaloplatforms.com"
	secretHeader   = "X-Bot-Api-Secret-Token"

	maxMediaBytes = 10 << 20 // 10 MiB hard cap on media downloads
	maxRedirects  = 3

	sendTimeout     = 10 * time.Second
	downloadTimeout = 15 * time.Second
)

// Config carries credentials and transport for the Zalo Bot API. Token may
// be empty only for parse/verify-only usage; Send and GetUpdates then fail
// with domain.CodeValidation.
type Config struct {
	Token         string
	WebhookSecret string
	APIBase       string // defaults to https://bot-api.zaloplatforms.com
	HTTPClient    *http.Client
}

// Client implements messaging.Provider against the Zalo Bot API.
type Client struct {
	token  string
	secret string
	base   string
	http   *http.Client

	// validateMedia, when set, replaces validateMediaURL (tests only).
	validateMedia func(context.Context, string) error
}

var _ messaging.Provider = (*Client)(nil)

// New builds a Client. APIBase defaults to the public Bot API when empty.
func New(cfg Config) *Client {
	base := strings.TrimRight(cfg.APIBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: sendTimeout}
	}
	return &Client{token: cfg.Token, secret: cfg.WebhookSecret, base: base, http: hc}
}

// BotInfo is the non-secret identity metadata returned by getMe.
type BotInfo struct {
	ID            string
	AccountName   string
	AccountType   string
	CanJoinGroups bool
}

// GetMe validates the configured bot token without sending a message or
// consuming an update. It is suitable for startup and E2E preflight checks.
func (c *Client) GetMe(ctx context.Context) (BotInfo, error) {
	if c.token == "" {
		return BotInfo{}, domain.E(domain.CodeValidation, "zalo bot token not configured", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/getMe", nil)
	if err != nil {
		return BotInfo{}, domain.E(domain.CodeValidation, "build getMe request", c.safeCause(err))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return BotInfo{}, domain.E(domain.CodeTransient, "getMe request failed", c.safeCause(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return BotInfo{}, domain.E(domain.CodeTransient, "read getMe response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return BotInfo{}, c.classifyStatus(resp.StatusCode, body)
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			ID            json.RawMessage `json:"id"`
			AccountName   string          `json:"account_name"`
			AccountType   string          `json:"account_type"`
			CanJoinGroups bool            `json:"can_join_groups"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return BotInfo{}, domain.E(domain.CodeTransient, "decode getMe response", err)
	}
	if !out.OK {
		return BotInfo{}, domain.E(domain.CodeValidation, "getMe rejected: "+c.redact(out.Description), nil)
	}
	info := BotInfo{
		ID:            idString(out.Result.ID),
		AccountName:   out.Result.AccountName,
		AccountType:   out.Result.AccountType,
		CanJoinGroups: out.Result.CanJoinGroups,
	}
	if info.ID == "" {
		return BotInfo{}, domain.E(domain.CodeTransient, "getMe response missing bot id", nil)
	}
	return info, nil
}

// Command is one slash-menu entry (no leading slash), matching the
// Telegram-shaped Zalo Bot API setMyCommands payload.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetMyCommands replaces the bot's "/" picker list via
// POST /bot{token}/setMyCommands. Zalo's Bot API is Telegram-compatible for
// this method; a successful call replaces the platform default /xinchao.
// Command names must be 1–32 lowercase letters, digits or underscores.
func (c *Client) SetMyCommands(ctx context.Context, commands []Command) error {
	if c.token == "" {
		return domain.E(domain.CodeValidation, "zalo bot token not configured", nil)
	}
	if len(commands) == 0 {
		return domain.E(domain.CodeValidation, "setMyCommands requires at least one command", nil)
	}
	payload, err := json.Marshal(map[string]any{"commands": commands})
	if err != nil {
		return domain.E(domain.CodeValidation, "encode setMyCommands body", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/bot"+c.token+"/setMyCommands", bytes.NewReader(payload))
	if err != nil {
		return domain.E(domain.CodeValidation, "build setMyCommands request", c.safeCause(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return domain.E(domain.CodeTransient, "setMyCommands request failed", c.safeCause(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return domain.E(domain.CodeTransient, "read setMyCommands response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return c.classifyStatus(resp.StatusCode, body)
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return domain.E(domain.CodeTransient, "decode setMyCommands response", err)
	}
	if !out.OK {
		description := c.redact(strings.TrimSpace(out.Description))
		if description == "" {
			description = "provider returned ok=false"
		}
		return domain.E(domain.CodeValidation, "setMyCommands rejected: "+description, nil)
	}
	return nil
}

// VerifyWebhook authenticates the inbound call by constant-time comparison
// of the shared secret header. The body is never touched.
func (c *Client) VerifyWebhook(_ context.Context, headers http.Header, _ []byte) error {
	if c.secret == "" {
		return domain.E(domain.CodeForbidden, "webhook secret not configured", nil)
	}
	got := strings.TrimSpace(headers.Get(secretHeader))
	want := strings.TrimSpace(c.secret)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return domain.E(domain.CodeForbidden, "invalid webhook secret", nil)
	}
	return nil
}

// ParseWebhook converts one Zalo Bot webhook body into a normalised event.
// Unknown event types are tolerated as domain.EventUnsupported; malformed
// bodies and known events missing message_id/from.id are CodeValidation.
func (c *Client) ParseWebhook(_ context.Context, body []byte) ([]events.InboundEvent, error) {
	var outer providerEnvelope
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, domain.E(domain.CodeValidation, "invalid webhook body", err)
	}
	if outer.OK != nil && !*outer.OK {
		return nil, domain.E(domain.CodeValidation, "webhook envelope was not ok", nil)
	}
	env := envelope{EventName: outer.EventName, Message: outer.Message}
	if len(outer.Result) > 0 && string(outer.Result) != "null" {
		if err := json.Unmarshal(outer.Result, &env); err != nil {
			return nil, domain.E(domain.CodeValidation, "invalid webhook result", err)
		}
	}
	if strings.TrimSpace(env.EventName) == "" {
		return nil, domain.E(domain.CodeValidation, "webhook event_name is required", nil)
	}
	eventType := env.EventName
	switch eventType {
	case domain.EventTextReceived, domain.EventImageReceived:
	default:
		eventType = domain.EventUnsupported
	}
	ev, err := buildEvent(eventType, env.Message, body)
	if err != nil {
		return nil, err
	}
	return []events.InboundEvent{ev}, nil
}

// Send delivers one text message via sendMessage.
func (c *Client) Send(ctx context.Context, msg messaging.OutboundMessage) (messaging.ProviderMessageRef, error) {
	ref := messaging.ProviderMessageRef{Provider: string(domain.ProviderZaloBot)}
	if c.token == "" {
		return ref, domain.E(domain.CodeValidation, "zalo bot token not configured", nil)
	}
	if msg.ProviderChatID == "" {
		return ref, domain.E(domain.CodeValidation, "provider chat id required", nil)
	}
	if strings.TrimSpace(msg.Text) == "" {
		return ref, domain.E(domain.CodeValidation, "message text required", nil)
	}
	if utf8.RuneCountInString(msg.Text) > 2000 {
		return ref, domain.E(domain.CodeValidation, "message text exceeds 2000 characters", nil)
	}
	payload, err := json.Marshal(map[string]string{
		"chat_id": msg.ProviderChatID,
		"text":    msg.Text,
	})
	if err != nil {
		return ref, domain.E(domain.CodeValidation, "encode sendMessage body", err)
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/bot"+c.token+"/sendMessage", bytes.NewReader(payload))
	if err != nil {
		return ref, domain.E(domain.CodeValidation, "build sendMessage request", c.safeCause(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ref, domain.E(domain.CodeTransient, "sendMessage request failed", c.safeCause(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ref, domain.E(domain.CodeTransient, "read sendMessage response", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ref, c.classifyStatus(resp.StatusCode, body)
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID json.RawMessage `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		// The request may have been delivered despite the malformed receipt;
		// let the sender persist an ambiguous outcome instead of counting it sent.
		return ref, domain.E(domain.CodeTransient, "decode sendMessage response", err)
	}
	if !out.OK {
		description := c.redact(strings.TrimSpace(out.Description))
		if description == "" {
			description = "provider returned ok=false"
		}
		return ref, domain.E(domain.CodeValidation, "sendMessage rejected: "+description, nil)
	}
	ref.ProviderMessageID = idString(out.Result.MessageID)
	return ref, nil
}

// DownloadMedia fetches provider-hosted media with a 15s timeout, at most 3
// redirects and a hard 10 MiB cap. The body is buffered (bounded by the cap)
// so overflow is detected before returning. URLs are validated against an
// HTTPS + Zalo CDN allowlist and resolved addresses are checked for SSRF.
func (c *Client) DownloadMedia(ctx context.Context, ref events.MediaReference) (io.ReadCloser, messaging.MediaMetadata, error) {
	var meta messaging.MediaMetadata
	if ref.URL == "" {
		return nil, meta, domain.E(domain.CodeValidation, "media reference has no URL", nil)
	}
	if err := c.checkMediaURL(ctx, ref.URL); err != nil {
		return nil, meta, err
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		return nil, meta, domain.E(domain.CodeValidation, "invalid media URL", c.safeCause(err))
	}
	hc := *c.http
	hc.Timeout = 0 // bounded by ctx instead
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return errors.New("redirect limit exceeded")
		}
		if err := c.checkMediaURL(r.Context(), r.URL.String()); err != nil {
			return err
		}
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && strings.Contains(urlErr.Err.Error(), "redirect limit") {
			return nil, meta, domain.E(domain.CodeValidation, "media redirect limit exceeded", c.safeCause(err))
		}
		if errors.As(err, &urlErr) && domain.IsCode(urlErr.Err, domain.CodeValidation) {
			return nil, meta, urlErr.Err
		}
		var de *domain.Error
		if errors.As(err, &de) {
			return nil, meta, de
		}
		return nil, meta, domain.E(domain.CodeTransient, "media download failed", c.safeCause(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, meta, c.classifyStatus(resp.StatusCode, nil)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		return nil, meta, domain.E(domain.CodeTransient, "read media body", err)
	}
	if len(buf) > maxMediaBytes {
		return nil, meta, domain.Ef(domain.CodeValidation, nil, "media exceeds %d byte cap", maxMediaBytes)
	}
	meta.ContentType = resp.Header.Get("Content-Type")
	if meta.ContentType == "" {
		meta.ContentType = http.DetectContentType(buf[:min(512, len(buf))])
	}
	meta.ByteSize = int64(len(buf))
	return io.NopCloser(bytes.NewReader(buf)), meta, nil
}

func (c *Client) checkMediaURL(ctx context.Context, raw string) error {
	if c.validateMedia != nil {
		return c.validateMedia(ctx, raw)
	}
	return validateMediaURL(ctx, raw)
}

// mediaLookupIP resolves hostnames for media URL SSRF checks. Tests may
// replace it to avoid real DNS.
var mediaLookupIP = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// allowedMediaHost reports whether host is a known Zalo CDN / media domain.
func allowedMediaHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, suffix := range []string{
		"zaloplatforms.com",
		"zaloapp.com",
		"zdn.vn",
		"zadn.vn",
		"zapps.me",
	} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// forbiddenMediaIP reports addresses that must never be fetched (SSRF).
func forbiddenMediaIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// Explicit metadata / link-local ranges (covers IPv4 169.254.0.0/16).
	if ip4 := ip.To4(); ip4 != nil {
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

// validateMediaURL enforces HTTPS, a Zalo CDN host allowlist, and rejects
// resolutions to loopback/private/link-local/multicast/metadata addresses.
func validateMediaURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return domain.E(domain.CodeValidation, "invalid media URL", err)
	}
	if u.Scheme != "https" {
		return domain.E(domain.CodeValidation, "media URL must use https", nil)
	}
	host := u.Hostname()
	if host == "" {
		return domain.E(domain.CodeValidation, "media URL missing host", nil)
	}
	if ip := net.ParseIP(host); ip != nil {
		if forbiddenMediaIP(ip) {
			return domain.E(domain.CodeValidation, "media URL resolves to forbidden address", nil)
		}
		return domain.E(domain.CodeValidation, "media host not allowed", nil)
	}
	if !allowedMediaHost(host) {
		return domain.E(domain.CodeValidation, "media host not allowed", nil)
	}
	addrs, err := mediaLookupIP(ctx, host)
	if err != nil {
		return domain.E(domain.CodeValidation, "media host DNS lookup failed", err)
	}
	if len(addrs) == 0 {
		return domain.E(domain.CodeValidation, "media host DNS lookup returned no addresses", nil)
	}
	for _, a := range addrs {
		if forbiddenMediaIP(a.IP) {
			return domain.E(domain.CodeValidation, "media URL resolves to forbidden address", nil)
		}
	}
	return nil
}

// classifyStatus maps an HTTP failure status onto a domain code: 429/5xx
// are retryable, other 4xx are permanent.
func (c *Client) classifyStatus(status int, body []byte) error {
	snippet := strings.TrimSpace(c.redact(string(body)))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		return domain.Ef(domain.CodeTransient, nil, "zalo api status %d: %s", status, snippet)
	case status >= 400:
		return domain.Ef(domain.CodeValidation, nil, "zalo api status %d: %s", status, snippet)
	default:
		return domain.Ef(domain.CodeTransient, nil, "zalo api unexpected status %d: %s", status, snippet)
	}
}

// safeCause removes request URLs (which contain the bot token) from
// transport errors before they can reach structured logs.
func (c *Client) safeCause(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	return errors.New(c.redact(err.Error()))
}

func (c *Client) redact(value string) string {
	if c.token == "" {
		return value
	}
	out := strings.ReplaceAll(value, c.token, "[REDACTED]")
	out = strings.ReplaceAll(out, url.PathEscape(c.token), "[REDACTED]")
	out = strings.ReplaceAll(out, url.QueryEscape(c.token), "[REDACTED]")
	return out
}

// --- Tolerant wire shapes (package-private; never leak outside zalo) ---

type envelope struct {
	EventName string          `json:"event_name"`
	Message   json.RawMessage `json:"message"`
}

// providerEnvelope accepts both the current {ok,result:{event_name,message}}
// webhook shape and the legacy top-level {event_name,message} shape.
type providerEnvelope struct {
	OK        *bool           `json:"ok"`
	Result    json.RawMessage `json:"result"`
	EventName string          `json:"event_name"`
	Message   json.RawMessage `json:"message"`
}

type zaloMessage struct {
	MessageID   json.RawMessage `json:"message_id"`
	From        *zaloParty      `json:"from"`
	Chat        *zaloParty      `json:"chat"`
	Date        int64           `json:"date"` // unix seconds
	Text        string          `json:"text"`
	Caption     string          `json:"caption"`
	Photo       json.RawMessage `json:"photo"`
	PhotoURL    json.RawMessage `json:"photo_url"`
	Photos      json.RawMessage `json:"photos"`
	Image       json.RawMessage `json:"image"`
	Attachments json.RawMessage `json:"attachments"`
}

type zaloParty struct {
	ID   json.RawMessage `json:"id"`
	Name string          `json:"name"`
}

type zaloMedia struct {
	URL      string `json:"url"`
	FileURL  string `json:"file_url"`
	MimeType string `json:"mime_type"`
	Caption  string `json:"caption"`
}

// idString normalises an ID that may arrive as a JSON string or number.
func idString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// parseMedia accepts a media entry as an object ({url|file_url, ...}) or a
// bare URL string.
func parseMedia(raw json.RawMessage) (zaloMedia, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return zaloMedia{}, false
	}
	var m zaloMedia
	if err := json.Unmarshal(raw, &m); err == nil {
		if m.URL == "" {
			m.URL = m.FileURL
		}
		return m, m.URL != ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return zaloMedia{URL: s}, true
	}
	return zaloMedia{}, false
}

// mediaItems accepts a provider field as either one string/object or an
// array. Zalo's current image webhook documents `photo` as a string while
// older payloads and fixtures used arrays.
func mediaItems(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return []json.RawMessage{raw}
}

// firstMedia picks the first usable media entry across the known field name
// variants. photo_url is emitted by the live getUpdates API even though the
// public webhook example documents photo.
func (m *zaloMessage) firstMedia() (zaloMedia, bool) {
	for _, group := range [][]json.RawMessage{
		mediaItems(m.PhotoURL), mediaItems(m.Photo), mediaItems(m.Photos),
		mediaItems(m.Image), mediaItems(m.Attachments),
	} {
		for _, raw := range group {
			if md, ok := parseMedia(raw); ok {
				return md, true
			}
		}
	}
	return zaloMedia{}, false
}

// buildEvent maps a tolerant message onto the normalised contract. msgRaw
// is the message sub-object; payload is the full raw unit (webhook body or
// update entry) hashed and retained for the idempotency anchor. Every message
// event requires the stable message, sender and chat IDs needed for dedupe,
// ownership and a reply; unknown event names still map to EventUnsupported.
func buildEvent(eventType string, msgRaw, payload []byte) (events.InboundEvent, error) {
	var msg zaloMessage
	if len(msgRaw) > 0 {
		if err := json.Unmarshal(msgRaw, &msg); err != nil {
			return events.InboundEvent{}, domain.E(domain.CodeValidation, "invalid message payload", err)
		}
	}
	sum := sha256.Sum256(payload)
	ev := events.InboundEvent{
		Provider:          string(domain.ProviderZaloBot),
		ProviderMessageID: idString(msg.MessageID),
		EventType:         eventType,
		Text:              msg.Text,
		ReceivedAt:        time.Now().UTC(),
		RawPayloadHash:    hex.EncodeToString(sum[:]),
		RawPayload:        payload,
	}
	if msg.From != nil {
		ev.ProviderUserID = idString(msg.From.ID)
	}
	if msg.Chat != nil {
		ev.ProviderChatID = idString(msg.Chat.ID)
	}
	if msg.Date > 0 {
		ev.ReceivedAt = providerTime(msg.Date)
	}
	if eventType == domain.EventImageReceived {
		if md, ok := msg.firstMedia(); ok {
			caption := msg.Caption
			if caption == "" {
				caption = md.Caption
			}
			ev.Media = []events.MediaReference{{
				Provider: string(domain.ProviderZaloBot),
				URL:      md.URL,
				MimeType: md.MimeType,
				Caption:  caption,
			}}
		}
	}
	if ev.ProviderMessageID == "" {
		return events.InboundEvent{}, domain.E(domain.CodeValidation, "message missing message_id", nil)
	}
	if ev.ProviderUserID == "" {
		return events.InboundEvent{}, domain.E(domain.CodeValidation, "message missing from.id", nil)
	}
	if ev.ProviderChatID == "" {
		return events.InboundEvent{}, domain.E(domain.CodeValidation, "message missing chat.id", nil)
	}
	return ev, nil
}

// providerTime supports legacy Unix-second payloads and the current Zalo
// millisecond timestamps without producing dates thousands of years ahead.
func providerTime(value int64) time.Time {
	const millisecondThreshold = int64(100_000_000_000)
	if value >= millisecondThreshold || value <= -millisecondThreshold {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}
