package textract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/textract"
	"github.com/aws/aws-sdk-go-v2/service/textract/types"
	"github.com/aws/smithy-go"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
)

type fakeAPI struct {
	out *textract.AnalyzeExpenseOutput
	err error
}

func (f fakeAPI) AnalyzeExpense(context.Context, *textract.AnalyzeExpenseInput, ...func(*textract.Options)) (*textract.AnalyzeExpenseOutput, error) {
	return f.out, f.err
}

func expenseField(typeText, value string, conf float32) types.ExpenseField {
	return types.ExpenseField{
		Type:           &types.ExpenseType{Text: aws.String(typeText)},
		ValueDetection: &types.ExpenseDetection{Text: aws.String(value), Confidence: aws.Float32(conf)},
	}
}

func fullReceipt() *textract.AnalyzeExpenseOutput {
	return &textract.AnalyzeExpenseOutput{
		ExpenseDocuments: []types.ExpenseDocument{{
			SummaryFields: []types.ExpenseField{
				expenseField("VENDOR_NAME", "CO.OPMART NGUYỄN TRÃI", 98.5),
				expenseField("TOTAL", "325.000 ₫", 99.2),
				expenseField("INVOICE_RECEIPT_DATE", "15/07/2026", 97.0),
			},
			LineItemGroups: []types.LineItemGroup{{
				LineItems: []types.LineItemFields{{
					LineItemExpenseFields: []types.ExpenseField{
						expenseField("ITEM", "Sữa tươi", 90),
						expenseField("QUANTITY", "2", 95),
						expenseField("PRICE", "62.000 ₫", 88),
					},
				}},
			}},
		}},
	}
}

func TestExtractMapsFields(t *testing.T) {
	ex := New(fakeAPI{out: fullReceipt()})
	res, err := ex.Extract(context.Background(), strings.NewReader("fake-jpeg"), extraction.Input{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.SchemaVersion != 1 || res.Extractor.Name != "textract-analyze-expense" || res.Extractor.Version != "v1" {
		t.Errorf("extractor provenance wrong: %+v", res.Extractor)
	}

	m := res.Fields["merchant"]
	if m.Raw != "CO.OPMART NGUYỄN TRÃI" || m.Normalised != "coopmart nguyen trai" {
		t.Errorf("merchant = %+v", m)
	}
	if m.Confidence < 0.984 || m.Confidence > 0.986 {
		t.Errorf("merchant confidence = %v, want ~0.985", m.Confidence)
	}

	total := res.Fields["total_minor"]
	if total.Normalised != int64(325000) {
		t.Errorf("total_minor = %+v", total)
	}
	cur := res.Fields["currency"]
	if cur.Normalised != "VND" {
		t.Errorf("currency = %+v", cur)
	}

	date := res.Fields["occurred_at"]
	if date.Normalised != "2026-07-15T00:00:00Z" {
		t.Errorf("occurred_at = %+v", date)
	}

	if len(res.LineItems) != 1 {
		t.Fatalf("line items = %d, want 1", len(res.LineItems))
	}
	li := res.LineItems[0]
	if li.Name != "Sữa tươi" || li.Quantity != 2 || li.AmountMinor != 62000 {
		t.Errorf("line item = %+v", li)
	}
}

func TestExtractMultipleTotalsWarns(t *testing.T) {
	out := &textract.AnalyzeExpenseOutput{
		ExpenseDocuments: []types.ExpenseDocument{{
			SummaryFields: []types.ExpenseField{
				expenseField("TOTAL", "325.000 ₫", 90),
				expenseField("TOTAL", "32.500 ₫", 85),
			},
		}},
	}
	ex := New(fakeAPI{out: out})
	res, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Fields["total_minor"].Normalised != int64(325000) {
		t.Errorf("first total wins: %+v", res.Fields["total_minor"])
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != "multiple_totals_detected" {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestExtractUnsupportedImage(t *testing.T) {
	ex := New(fakeAPI{out: &textract.AnalyzeExpenseOutput{}})
	_, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
	if !domain.IsCode(err, domain.CodeUnsupported) {
		t.Errorf("empty expense documents = %v, want CodeUnsupported", err)
	}
}

func TestExtractClassifiesErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domain.Code
	}{
		{"throttle", &smithy.GenericAPIError{Code: "ThrottlingException", Message: "slow down"}, domain.CodeTransient},
		{"throughput", &smithy.GenericAPIError{Code: "ProvisionedThroughputExceededException", Message: "x"}, domain.CodeTransient},
		{"bad document", &smithy.GenericAPIError{Code: "UnsupportedDocumentException", Message: "x"}, domain.CodeValidation},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "x"}, domain.CodeForbidden},
		{"unknown aws", &smithy.GenericAPIError{Code: "SomethingNew", Message: "x"}, domain.CodeTransient},
		{"plain error", errors.New("boom"), domain.CodeTransient},
	}
	for _, c := range cases {
		ex := New(fakeAPI{err: c.err})
		_, err := ex.Extract(context.Background(), strings.NewReader("img"), extraction.Input{})
		if got := domain.CodeOf(err); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestExtractRejectsOversize(t *testing.T) {
	ex := New(fakeAPI{out: fullReceipt()})
	_, err := ex.Extract(context.Background(), strings.NewReader(strings.Repeat("x", maxDocBytes+1)), extraction.Input{})
	if !domain.IsCode(err, domain.CodeValidation) {
		t.Errorf("oversize = %v, want CodeValidation", err)
	}
}

func TestParseTextractDate(t *testing.T) {
	for s, want := range map[string]string{
		"15/07/2026":       "2026-07-15T00:00:00Z",
		"2026-07-15":       "2026-07-15T00:00:00Z",
		"Jul 15, 2026":     "2026-07-15T00:00:00Z",
		"15 Jul 2026":      "2026-07-15T00:00:00Z",
		"15/07/2026 09:24": "2026-07-15T09:24:00Z",
	} {
		got, ok := parseTextractDate(s)
		if !ok {
			t.Errorf("%q: not parsed", s)
			continue
		}
		if got.Format("2006-01-02T15:04:05Z") != want {
			t.Errorf("%q = %s, want %s", s, got.Format("2006-01-02T15:04:05Z"), want)
		}
	}
	if _, ok := parseTextractDate("không phải ngày"); ok {
		t.Errorf("garbage date must fail")
	}
}
