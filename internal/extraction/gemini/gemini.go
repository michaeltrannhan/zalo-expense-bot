// Package gemini implements extraction.Extractor over a lightweight vision
// LLM (Google AI Studio, default gemini-3.6-flash).
//
// Product principle (plan §4): the model only READS — it copies raw field
// strings as printed and never computes. The raw strings are re-parsed
// here through the deterministic normalise package (amounts, dates), the
// confidence policy still flags uncertain fields, and the confirmation
// card gates every transaction. Self-reported model confidences are
// capped, because LLM confidence is uncalibrated.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	"zl-expese-bot/internal/extraction/normalise"
)

const (
	// maxImageBytes matches the receipt pipeline cap (10 MiB).
	maxImageBytes = 10 << 20
	// maxConf caps self-reported model confidence (uncalibrated by nature).
	maxConf = 0.95

	defaultTimeout = 45 * time.Second // vision calls are slower than Textract
)

// Extractor implements extraction.Extractor via the Gemini generateContent
// REST API (no SDK — one JSON POST per image).
type Extractor struct {
	key    string
	model  string
	base   string
	http   *http.Client
	prompt string
}

var _ extraction.Extractor = (*Extractor)(nil)

// Config carries credentials and transport. Model defaults to
// gemini-3.6-flash; APIBase exists for tests.
type Config struct {
	APIKey     string
	Model      string
	APIBase    string
	HTTPClient *http.Client
}

// New builds the adapter.
func New(cfg Config) *Extractor {
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Extractor{
		key: cfg.APIKey, model: cfg.Model, base: strings.TrimRight(cfg.APIBase, "/"),
		http: hc, prompt: extractionPrompt,
	}
}

func (e *Extractor) Name() string    { return "gemini-vision" }
func (e *Extractor) Version() string { return e.model }

// Extract sends the image with a strict-JSON prompt and maps the model's
// raw field strings through the deterministic normalisers. A document the
// model rejects as not-a-receipt yields CodeUnsupported.
func (e *Extractor) Extract(ctx context.Context, r io.Reader, in extraction.Input) (events.ExtractionResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxImageBytes+1))
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "reading receipt bytes", err)
	}
	if len(data) == 0 {
		return events.ExtractionResult{}, domain.E(domain.CodeValidation, "empty image", nil)
	}
	if len(data) > maxImageBytes {
		return events.ExtractionResult{}, domain.Ef(domain.CodeValidation, nil, "image exceeds %d bytes", maxImageBytes)
	}
	mime := in.ContentType
	if mime == "" || mime == "text/plain" {
		mime = "image/jpeg"
	}

	body, err := e.buildRequest(data, mime)
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeInternal, "build gemini request", err)
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", e.base, e.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeInternal, "build gemini request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", e.key)

	resp, err := e.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return events.ExtractionResult{}, domain.E(domain.CodeTransient, "gemini call timed out", err)
		}
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "gemini call failed", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "read gemini response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return events.ExtractionResult{}, classifyStatus(resp.StatusCode, respBody)
	}
	return e.mapResponse(respBody)
}

// buildRequest assembles the generateContent payload: image part + prompt
// with JSON response mode for stable machine output.
func (e *Extractor) buildRequest(image []byte, mime string) ([]byte, error) {
	req := generateRequest{
		Contents: []content{{
			Role: "user",
			Parts: []part{
				{Text: e.prompt},
				{InlineData: &inlineData{MimeType: mime, Data: base64.StdEncoding.EncodeToString(image)}},
			},
		}},
		GenerationConfig: generationConfig{
			ResponseMimeType: "application/json",
		},
	}
	return json.Marshal(req)
}

// extractionPrompt is the strict instruction the model answers. It reads
// and copies — it never computes (amounts are re-parsed deterministically).
const extractionPrompt = `You are reading a photo of a receipt, invoice or payment screenshot (Vietnamese or English).

Extract the transaction fields and answer with STRICT JSON only, no markdown, no explanation:
{
  "is_receipt": true or false,
  "merchant": "merchant or store name as printed, or null",
  "total": "the FINAL amount payable, copied exactly as printed (e.g. \"325.000 ₫\"), or null",
  "currency": "ISO 4217 code only if certain from the document, else null",
  "date": "transaction date/time as printed, or null",
  "type_hint": "expense" | "refund" | "transfer" | null,
  "line_items": [{"name": "item", "quantity": 1, "amount": "as printed"}],
  "confidence": {"merchant": 0.0-1.0, "total": 0.0-1.0, "currency": 0.0-1.0, "date": 0.0-1.0}
}

Rules:
- If several totals appear (subtotal, discount, VAT), use the FINAL amount payable.
- NEVER compute, convert or round amounts — copy them exactly as printed.
- Set type_hint only when the document clearly says refund/transfer; otherwise null.
- If the image is not a receipt or payment record, set "is_receipt": false and all other fields null.`

// Wire shapes (generateContent v1beta subset).
type generateRequest struct {
	Contents         []content        `json:"contents"`
	GenerationConfig generationConfig `json:"generationConfig"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text       string      `json:"text,omitempty"`
	InlineData *inlineData `json:"inline_data,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type generationConfig struct {
	ResponseMimeType string `json:"responseMimeType"`
}

type generateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// modelAnswer is the strict-JSON shape the prompt demands.
type modelAnswer struct {
	IsReceipt bool    `json:"is_receipt"`
	Merchant  *string `json:"merchant"`
	Total     *string `json:"total"`
	Currency  *string `json:"currency"`
	Date      *string `json:"date"`
	TypeHint  *string `json:"type_hint"`
	LineItems []struct {
		Name     string `json:"name"`
		Quantity int    `json:"quantity"`
		Amount   string `json:"amount"`
	} `json:"line_items"`
	Confidence map[string]float64 `json:"confidence"`
}

// mapResponse decodes the API response, then the model's JSON answer, and
// converts it into the §10.4 contract via deterministic normalisers.
func (e *Extractor) mapResponse(body []byte) (events.ExtractionResult, error) {
	var gr generateResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "decode gemini response", err)
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "gemini returned no content", nil)
	}
	raw := strings.TrimSpace(gr.Candidates[0].Content.Parts[0].Text)
	raw = strings.Trim(raw, "`")
	raw = strings.TrimPrefix(raw, "json")
	raw = strings.TrimSpace(raw)

	var ans modelAnswer
	if err := json.Unmarshal([]byte(raw), &ans); err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "model answer is not JSON", err)
	}
	if !ans.IsReceipt {
		return events.ExtractionResult{}, domain.E(domain.CodeUnsupported, "image is not a receipt", nil)
	}

	res := events.ExtractionResult{
		SchemaVersion: events.SchemaV1,
		Extractor:     events.ExtractorInfo{Name: e.Name(), Version: e.Version()},
		Fields:        map[string]events.FieldValue{},
		LineItems:     []events.LineItem{},
		Warnings:      []string{},
	}
	conf := func(field string) float64 {
		c := ans.Confidence[field]
		if c > maxConf {
			c = maxConf
		}
		if c < 0 {
			c = 0
		}
		return c
	}

	if ans.Merchant != nil && strings.TrimSpace(*ans.Merchant) != "" {
		v := strings.TrimSpace(*ans.Merchant)
		_, key := normalise.NormaliseMerchant(v)
		res.Fields["merchant"] = events.FieldValue{Raw: v, Normalised: key, Confidence: conf("merchant")}
	}
	if ans.Total != nil && strings.TrimSpace(*ans.Total) != "" {
		v := strings.TrimSpace(*ans.Total)
		declaredHint := ""
		if ans.Currency != nil && strings.TrimSpace(*ans.Currency) != "" {
			declaredHint = strings.ToUpper(strings.TrimSpace(*ans.Currency))
		}
		if minor, currency, err := normalise.ParseAmount(v, declaredHint); err == nil {
			totalConf := conf("total")
			curConf := conf("currency")
			if declaredHint != "" && declaredHint != currency {
				// Keep ParseAmount's currency (hint drove decimal places);
				// lower confidence instead of overwriting without recalc.
				if totalConf > 0.4 {
					totalConf = 0.4
				}
				if curConf > 0.4 {
					curConf = 0.4
				}
				res.Warnings = append(res.Warnings,
					"currency mismatch: declared "+declaredHint+" vs parsed "+currency)
			}
			res.Fields["total_minor"] = events.FieldValue{Raw: v, Normalised: minor, Confidence: totalConf}
			res.Fields["currency"] = events.FieldValue{Raw: v, Normalised: currency, Confidence: curConf}
		}
		// Unparseable total: emit nothing — the pipeline fails permanently
		// rather than inventing a number (product principle).
	}
	if ans.Date != nil && strings.TrimSpace(*ans.Date) != "" {
		v := strings.TrimSpace(*ans.Date)
		if t, err := normalise.ParseDate(v, time.UTC); err == nil {
			res.Fields["occurred_at"] = events.FieldValue{Raw: v, Normalised: t.UTC().Format(time.RFC3339), Confidence: conf("date")}
		} else {
			// Keep the raw date visible at zero confidence; the card asks.
			res.Fields["occurred_at"] = events.FieldValue{Raw: v, Normalised: "", Confidence: 0}
		}
	}
	if ans.TypeHint != nil {
		switch strings.ToLower(strings.TrimSpace(*ans.TypeHint)) {
		case "refund", "transfer":
			res.Fields["type_hint"] = events.FieldValue{
				Raw: *ans.TypeHint, Normalised: strings.ToLower(*ans.TypeHint), Confidence: 0.9,
			}
		}
	}
	for _, li := range ans.LineItems {
		if li.Name == "" && li.Amount == "" {
			continue
		}
		item := events.LineItem{Name: li.Name, Quantity: li.Quantity}
		if minor, _, err := normalise.ParseAmount(li.Amount, ""); err == nil {
			item.AmountMinor = minor
		}
		res.LineItems = append(res.LineItems, item)
	}
	return res, nil
}

// classifyStatus maps HTTP failures onto domain retry classes.
func classifyStatus(status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return domain.Ef(domain.CodeForbidden, nil, "gemini auth rejected: %s", snippet)
	case status == http.StatusRequestTimeout:
		return domain.E(domain.CodeTransient, "gemini request timed out", nil)
	case status == http.StatusTooManyRequests:
		return domain.E(domain.CodeTransient, "gemini rate limited", nil)
	case status >= 400 && status < 500:
		return domain.Ef(domain.CodeValidation, nil, "gemini rejected request: %s", snippet)
	case status >= 500:
		return domain.E(domain.CodeTransient, "gemini server error", nil)
	default:
		return domain.Ef(domain.CodeTransient, nil, "gemini unexpected status %d", status)
	}
}
