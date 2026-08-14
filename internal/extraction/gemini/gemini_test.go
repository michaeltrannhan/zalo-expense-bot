package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
)

// fakeGemini serves one canned generateContent response and captures the
// last request body for inspection.
func fakeGemini(t *testing.T, status int, answerJSON string) (*Extractor, func() []byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			resp := map[string]any{
				"candidates": []any{
					map[string]any{"content": map[string]any{
						"parts": []any{map[string]any{"text": answerJSON}},
					}},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		_, _ = w.Write([]byte(`{"error": {"message": "boom"}}`))
	}))
	t.Cleanup(srv.Close)
	ex := New(Config{APIKey: "test-key", Model: "gemini-2.5-flash-lite", APIBase: srv.URL})
	return ex, func() []byte { return lastBody }
}

const receiptAnswer = `{
  "is_receipt": true,
  "merchant": "CO.OPMART NGUYỄN TRÃI",
  "total": "325.000 ₫",
  "currency": "VND",
  "date": "15/07/2026 09:24",
  "type_hint": null,
  "line_items": [{"name": "Sữa tươi", "quantity": 2, "amount": "62.000 ₫"}],
  "confidence": {"merchant": 0.99, "total": 0.98, "currency": 0.97, "date": 0.96}
}`

func TestExtractMapsAnswerDeterministically(t *testing.T) {
	ex, reqBody := fakeGemini(t, http.StatusOK, receiptAnswer)
	res, err := ex.Extract(context.Background(), strings.NewReader("fake-jpeg"), extraction.Input{ContentType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// The request carries the base64 image and JSON mode.
	var sent generateRequest
	if err := json.Unmarshal(reqBody(), &sent); err != nil {
		t.Fatalf("decode sent request: %v", err)
	}
	if sent.GenerationConfig.ResponseMimeType != "application/json" {
		t.Errorf("generation config = %+v", sent.GenerationConfig)
	}
	if sent.GenerationConfig.ThinkingConfig == nil || sent.GenerationConfig.ThinkingConfig.ThinkingLevel != "minimal" {
		t.Errorf("thinkingConfig = %+v, want minimal (default medium thinking is the OCR latency)", sent.GenerationConfig.ThinkingConfig)
	}
	if sent.Contents[0].Parts[0].Text == "" {
		t.Fatal("prompt must precede the image part")
	}
	img := sent.Contents[0].Parts[1].InlineData
	if img.MimeType != "image/jpeg" {
		t.Errorf("mime = %q", img.MimeType)
	}
	if decoded, err := base64.StdEncoding.DecodeString(img.Data); err != nil || string(decoded) != "fake-jpeg" {
		t.Errorf("image payload round-trip failed: %v", err)
	}

	if res.Extractor.Name != "gemini-vision" || res.Extractor.Version != "gemini-2.5-flash-lite" {
		t.Errorf("provenance = %+v", res.Extractor)
	}
	if got := res.Fields["merchant"]; got.Normalised != "coopmart nguyen trai" {
		t.Errorf("merchant = %+v", got)
	}
	if got := res.Fields["total_minor"]; got.Normalised != int64(325000) {
		t.Errorf("total_minor = %+v", got)
	}
	if got := res.Fields["currency"]; got.Normalised != "VND" {
		t.Errorf("currency = %+v", got)
	}
	if got := res.Fields["occurred_at"]; got.Normalised != "2026-07-15T09:24:00Z" {
		t.Errorf("occurred_at = %+v", got)
	}
	// Self-reported confidence 0.99 is capped at maxConf.
	if got := res.Fields["merchant"].Confidence; got != maxConf {
		t.Errorf("merchant confidence = %v, want capped %v", got, maxConf)
	}
	if len(res.LineItems) != 1 || res.LineItems[0].AmountMinor != 62000 {
		t.Errorf("line items = %+v", res.LineItems)
	}
}

func TestExtractCurrencyMismatchLowersConfidence(t *testing.T) {
	answer := `{
	  "is_receipt": true,
	  "merchant": "Cafe",
	  "total": "325.000 ₫",
	  "currency": "USD",
	  "date": "15/07/2026",
	  "type_hint": null,
	  "line_items": [],
	  "confidence": {"merchant": 0.9, "total": 0.9, "currency": 0.9, "date": 0.9}
	}`
	ex, _ := fakeGemini(t, http.StatusOK, answer)
	res, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fields["currency"].Normalised != "VND" {
		t.Fatalf("currency = %v, want VND from printed amount", res.Fields["currency"].Normalised)
	}
	if res.Fields["total_minor"].Normalised != int64(325000) {
		t.Fatalf("total = %v, want 325000 VND minor", res.Fields["total_minor"].Normalised)
	}
	if res.Fields["currency"].Confidence > 0.4 || res.Fields["total_minor"].Confidence > 0.4 {
		t.Fatalf("mismatch confidences = total %v currency %v, want <= 0.4",
			res.Fields["total_minor"].Confidence, res.Fields["currency"].Confidence)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "currency mismatch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want currency mismatch", res.Warnings)
	}
}

func TestExtractNotAReceipt(t *testing.T) {
	ex, _ := fakeGemini(t, http.StatusOK, `{"is_receipt": false}`)
	_, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if !domain.IsCode(err, domain.CodeUnsupported) {
		t.Errorf("is_receipt=false = %v, want CodeUnsupported", err)
	}
}

func TestExtractRefundHintSurvives(t *testing.T) {
	answer := `{"is_receipt": true, "merchant": "SHOPEE VN", "total": "89.000đ",
		"currency": "VND", "date": "13/07/2026", "type_hint": "refund",
		"line_items": [], "confidence": {"merchant": 0.9, "total": 0.9, "currency": 0.9, "date": 0.9}}`
	ex, _ := fakeGemini(t, http.StatusOK, answer)
	res, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	hint, ok := res.Fields["type_hint"]
	if !ok || hint.Normalised != "refund" {
		t.Fatalf("type_hint = %+v", hint)
	}
	if !extraction.TypeNeedsConfirmation(domain.TxType(hint.Normalised.(string))) {
		t.Errorf("refund hint must require explicit confirmation")
	}
}

func TestExtractMarkdownFencedJSON(t *testing.T) {
	ex, _ := fakeGemini(t, http.StatusOK, "```json\n"+receiptAnswer+"\n```")
	res, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if err != nil {
		t.Fatalf("Extract fenced: %v", err)
	}
	if res.Fields["total_minor"].Normalised != int64(325000) {
		t.Errorf("fenced answer not parsed: %+v", res.Fields["total_minor"])
	}
}

func TestExtractUnparseableTotalDropped(t *testing.T) {
	answer := `{"is_receipt": true, "merchant": "X", "total": "not a number",
		"currency": null, "date": null, "type_hint": null, "line_items": [], "confidence": {}}`
	ex, _ := fakeGemini(t, http.StatusOK, answer)
	res, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if _, ok := res.Fields["total_minor"]; ok {
		t.Errorf("unparseable total must be dropped, never invented")
	}
}

func TestExtractHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   domain.Code
	}{
		{http.StatusTooManyRequests, domain.CodeTransient},
		{http.StatusRequestTimeout, domain.CodeTransient},
		{http.StatusForbidden, domain.CodeForbidden},
		{http.StatusUnauthorized, domain.CodeForbidden},
		{http.StatusBadRequest, domain.CodeValidation},
		{http.StatusNotFound, domain.CodeValidation},
		{http.StatusInternalServerError, domain.CodeTransient},
		{http.StatusTeapot, domain.CodeValidation},
	}
	for _, c := range cases {
		ex, _ := fakeGemini(t, c.status, "")
		_, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
		if got := domain.CodeOf(err); got != c.want {
			t.Errorf("status %d: got %s, want %s", c.status, got, c.want)
		}
	}
}

func TestExtractMalformedModelJSON(t *testing.T) {
	ex, _ := fakeGemini(t, http.StatusOK, "this is not json")
	_, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if !domain.IsCode(err, domain.CodeTransient) {
		t.Errorf("malformed model JSON = %v, want CodeTransient", err)
	}
}

func TestDownscaleForOCRShrinksLargeJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3200, 2400))
	src.Set(0, 0, color.RGBA{R: 255, A: 255})
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	out, mime, ok := downscaleForOCR(raw.Bytes())
	if !ok {
		t.Fatalf("downscaleForOCR skipped a %dx%d JPEG (%d bytes)", src.Bounds().Dx(), src.Bounds().Dy(), raw.Len())
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q", mime)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	w, h := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if w > ocrMaxEdge || h > ocrMaxEdge {
		t.Errorf("scaled %dx%d exceeds max edge %d", w, h, ocrMaxEdge)
	}
	if w < ocrMaxEdge && h < ocrMaxEdge {
		t.Errorf("scaled %dx%d should keep one edge at %d", w, h, ocrMaxEdge)
	}
	if len(out) >= raw.Len() {
		t.Errorf("scaled payload %d bytes was not smaller than original %d", len(out), raw.Len())
	}
}

func TestDownscaleForOCRLeavesUndecodableBytes(t *testing.T) {
	if _, _, ok := downscaleForOCR([]byte("not an image")); ok {
		t.Fatal("undecodable bytes must pass through unchanged")
	}
}

func TestExtractSendsDownscaledImage(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3000, 2000))
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	ex, reqBody := fakeGemini(t, http.StatusOK, receiptAnswer)
	_, err := ex.Extract(context.Background(), bytes.NewReader(raw.Bytes()), extraction.Input{ContentType: "image/jpeg"})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var sent generateRequest
	if err := json.Unmarshal(reqBody(), &sent); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(sent.Contents[0].Parts[1].InlineData.Data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("gemini payload is not jpeg: %v", err)
	}
	if img.Bounds().Dx() > ocrMaxEdge || img.Bounds().Dy() > ocrMaxEdge {
		t.Errorf("uploaded image %v exceeds OCR max edge", img.Bounds())
	}
}
