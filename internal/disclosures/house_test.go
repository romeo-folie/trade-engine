package disclosures

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHouseFetchPDFRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(maxHouseResponseSize+1))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &HouseClient{BaseURL: server.URL, HTTP: server.Client()}
	_, _, err := client.FetchPDF(context.Background(), HouseFiling{Year: 2026, DocumentID: "oversized"})
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("FetchPDF error = %v, want response-size rejection", err)
	}
}

func TestPDFiumHousePTRFixture(t *testing.T) {
	path := os.Getenv("HOUSE_PTR_FIXTURE")
	if path == "" {
		t.Skip("HOUSE_PTR_FIXTURE is not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	extractor, err := NewPDFiumExtractor()
	if err != nil {
		t.Fatal(err)
	}
	defer extractor.Close() //nolint:errcheck
	pages, err := extractor.Extract(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	report := ParseHousePTR(HouseFiling{
		Prefix: "Hon.", First: "David", Last: "Taylor", Year: 2026,
		FilingDate: time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),
		DocumentID: "20034138", StateDistrict: "OH02",
	}, "fixture", data, pages)
	if report.ExtractionStatus != "completed" || len(report.Transactions) < 20 {
		t.Fatalf("PDFium reconstructed only %d transactions: %s", len(report.Transactions), report.ExtractionError)
	}
	wanted := map[string]bool{"GOOGL": false, "IBM": false, "MSFT": false}
	var gotTickers []string
	var missingTickerAssets []string
	for _, item := range report.Transactions {
		gotTickers = append(gotTickers, item.Ticker)
		if item.Ticker == "" {
			missingTickerAssets = append(missingTickerAssets, item.AssetName)
		}
		if _, ok := wanted[item.Ticker]; ok {
			wanted[item.Ticker] = true
		}
	}
	for ticker, found := range wanted {
		if !found {
			t.Errorf("PDFium fixture did not reconstruct %s; got %v; missing assets %v", ticker, gotTickers, missingTickerAssets)
		}
	}
}

func TestParseHouseIndexKeepsPTRs(t *testing.T) {
	data := strings.Join([]string{
		"Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID",
		"Hon.\tTaylor\tDavid\tJ.\tP\tOH02\t2026\t03/06/2026\t20034138",
		"\tExample\tJane\t\tA\tCA01\t2026\t03/06/2026\tignored",
	}, "\n")
	filings, err := ParseHouseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 {
		t.Fatalf("got %d filings, want 1", len(filings))
	}
	if got := filings[0].FilerName(); got != "Hon. David Taylor J." {
		t.Fatalf("filer name = %q", got)
	}
	if filings[0].DocumentID != "20034138" || filings[0].StateDistrict != "OH02" {
		t.Fatalf("unexpected filing: %+v", filings[0])
	}
}

func TestParseHouseIndexAllowsOfficialBareQuotesInNames(t *testing.T) {
	data := strings.Join([]string{
		"Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID",
		"Hon.\tO\"Example\tJane\t\tP\tCA01\t2026\t06/10/2026\t20039999",
	}, "\n")
	filings, err := ParseHouseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 || filings[0].Last != `O"Example` {
		t.Fatalf("filings = %+v", filings)
	}
}

func TestParseHouseIndexSkipsMalformedRowsWithoutDiscardingYear(t *testing.T) {
	data := strings.Join([]string{
		"Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID",
		"Hon.\tGood\tJane\t\tP\tCA01\t2026\t08/10/2026\tgood",
		"Hon.\tBadYear\tBob\t\tP\tCA01\tnot-a-year\t08/10/2026\tbad-year",
		"Hon.\tBadDate\tSue\t\tP\tCA01\t2026\tnot-a-date\tbad-date",
	}, "\n")
	filings, err := ParseHouseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 || filings[0].DocumentID != "good" {
		t.Fatalf("filings = %+v, want only valid PTR row", filings)
	}
}

func TestParseHouseIndexSkipsIncompleteIdentityRows(t *testing.T) {
	data := strings.Join([]string{
		"Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID",
		"Hon.\tValid\tJane\t\tP\tCA01\t2026\t08/10/2026\tvalid",
		"Hon.\tMissingID\tJane\t\tP\tCA01\t2026\t08/10/2026\t",
		"\t\t\t\tP\tCA01\t2026\t08/10/2026\tmissing-name",
		"too\tshort",
		"Hon.\tLater\tPat\t\tP\tCA01\t2026\t08/10/2026\tlater-valid",
	}, "\n")
	filings, err := ParseHouseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 2 || filings[0].DocumentID != "valid" || filings[1].DocumentID != "later-valid" {
		t.Fatalf("filings = %+v, want both complete identities", filings)
	}
}

func TestParseHouseIndexAllowsUnpaddedOfficialDates(t *testing.T) {
	data := strings.Join([]string{
		"Prefix\tLast\tFirst\tSuffix\tFilingType\tStateDst\tYear\tFilingDate\tDocID",
		"Hon.\tExample\tJane\t\tP\tCA01\t2026\t6/1/2026\t20039999",
	}, "\n")
	filings, err := ParseHouseIndex([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 || filings[0].FilingDate.Month() != time.June ||
		filings[0].FilingDate.Day() != 1 {
		t.Fatalf("filings = %+v", filings)
	}
}

func TestNormalizeHousePartialTransactionAbbreviations(t *testing.T) {
	if got := normalizeTransactionType("S (partial)"); got != "sale" {
		t.Fatalf("sale abbreviation = %q", got)
	}
	if got := normalizeTransactionType("P (partial)"); got != "purchase" {
		t.Fatalf("purchase abbreviation = %q", got)
	}
}

func TestParseHousePTRDoesNotPromoteLowercaseParentheticalToTicker(t *testing.T) {
	filing := HouseFiling{First: "Jane", Last: "Member", Year: 2026, FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentID: "ticker-case", StateDistrict: "CA01"}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "Self", Left: 24, Top: 100},
		{Text: "Acme Fund (common)", Left: 104, Top: 100},
		{Text: "P", Left: 262, Top: 100},
		{Text: "08/01/2026", Left: 326, Top: 100},
		{Text: "08/05/2026", Left: 382, Top: 100},
		{Text: "$1,001 - $15,000", Left: 447, Top: 100},
		{Text: "Self", Left: 24, Top: 150},
		{Text: "Bravo Fund (BRAV)", Left: 104, Top: 150},
		{Text: "P", Left: 262, Top: 150},
		{Text: "08/02/2026", Left: 326, Top: 150},
		{Text: "08/06/2026", Left: 382, Top: 150},
		{Text: "$1,001 - $15,000", Left: 447, Top: 150},
	}}}
	report := ParseHousePTR(filing, "https://example.test/ticker-case.pdf", []byte("pdf"), pages)
	if len(report.Transactions) != 2 || report.Transactions[0].Ticker != "" || report.Transactions[1].Ticker != "BRAV" {
		t.Fatalf("tickers = %+v, want lowercase descriptor ignored and uppercase symbol retained", report.Transactions)
	}
}

func TestWrappedHouseAssetRequiresOriginalUppercaseTicker(t *testing.T) {
	boxes := []TextBox{{Text: "Fund description (common)", Left: 104, Top: 90}}
	if got := wrappedHouseAsset(boxes, 100, -1<<62); got != "" {
		t.Fatalf("lowercase parenthetical wrapped asset = %q, want ignored", got)
	}
	boxes[0].Text = "Fund description (BRAV)"
	if got := wrappedHouseAsset(boxes, 100, -1<<62); got != "Fund description (BRAV)" {
		t.Fatalf("uppercase ticker wrapped asset = %q", got)
	}
}

func TestParseHousePTRUsesPositionedColumnsAndWrappedLines(t *testing.T) {
	filing := HouseFiling{
		Prefix: "Hon.", First: "David", Last: "Taylor", Year: 2026,
		FilingDate: time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),
		DocumentID: "20034138", StateDistrict: "OH02",
	}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "SP", Left: 24, Top: 100},
		{Text: "Stock (GOOGL) [ST]", Left: 104, Top: 90},
		{Text: "Alphabet Inc. - Class A Common", Left: 104, Top: 100},
		{Text: "P", Left: 262, Top: 100},
		{Text: "02/26/2026", Left: 326, Top: 100},
		{Text: "03/05/2026", Left: 382, Top: 100},
		{Text: "$1,001 - $15,000", Left: 447, Top: 100},
		{Text: "$50,000", Left: 447, Top: 140},
		{Text: "DC", Left: 24, Top: 150},
		{Text: "(IBM) [ST]", Left: 104, Top: 140},
		{Text: "International Business Machines Common Stock", Left: 104, Top: 150},
		{Text: "S (partial)", Left: 262, Top: 150},
		{Text: "02/26/2026", Left: 326, Top: 150},
		{Text: "03/05/2026", Left: 382, Top: 150},
		{Text: "$15,001 -", Left: 447, Top: 150},
	}}}
	report := ParseHousePTR(filing, "https://example.test/20034138.pdf", []byte("pdf"), pages)
	if report.ExtractionStatus != "completed" || report.RetryableFailure || len(report.Transactions) != 2 {
		t.Fatalf("unexpected report: status=%s transactions=%+v", report.ExtractionStatus, report.Transactions)
	}
	first := report.Transactions[0]
	if first.Ticker != "GOOGL" || first.TransactionType != "purchase" || first.Owner != "Spouse" ||
		first.AmountMin == nil || *first.AmountMin != 1001 ||
		first.AmountMax == nil || *first.AmountMax != 15000 {
		t.Fatalf("unexpected first transaction: %+v", first)
	}
	second := report.Transactions[1]
	if second.Ticker != "IBM" || second.TransactionType != "sale" || second.Owner != "Dependent Child" ||
		second.AmountMin == nil || *second.AmountMin != 15001 ||
		second.AmountMax == nil || *second.AmountMax != 50000 {
		t.Fatalf("unexpected second transaction: %+v", second)
	}
}

func TestParseHousePTRAssignsAdjacentWrappedTickerAtMidpointToLaterRow(t *testing.T) {
	filing := HouseFiling{First: "Jane", Last: "Member", Year: 2026, FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentID: "adjacent", StateDistrict: "CA01"}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "First asset", Left: 104, Top: 100}, {Text: "First (AAAA)", Left: 104, Top: 95},
		{Text: "P", Left: 262, Top: 100}, {Text: "08/01/2026", Left: 326, Top: 100}, {Text: "08/02/2026", Left: 382, Top: 100}, {Text: "$1,001 - $15,000", Left: 447, Top: 100},
		{Text: "Second asset", Left: 104, Top: 120}, {Text: "Second (BBBB)", Left: 104, Top: 110},
		{Text: "S", Left: 262, Top: 120}, {Text: "08/03/2026", Left: 326, Top: 120}, {Text: "08/04/2026", Left: 382, Top: 120}, {Text: "$1,001 - $15,000", Left: 447, Top: 120},
	}}}
	report := ParseHousePTR(filing, "fixture", []byte("pdf"), pages)
	if len(report.Transactions) != 2 || report.Transactions[0].Ticker != "AAAA" || report.Transactions[1].Ticker != "BBBB" {
		t.Fatalf("tickers = %+v, want adjacent wrapped tickers owned by their rows", report.Transactions)
	}
}

func TestParseHousePTRMalformedDatedRowBoundsFollowingWrappedAsset(t *testing.T) {
	filing := HouseFiling{First: "Jane", Last: "Member", Year: 2026, FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentID: "malformed-anchor", StateDistrict: "CA01"}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "First asset", Left: 104, Top: 100}, {Text: "P", Left: 262, Top: 100}, {Text: "08/01/2026", Left: 326, Top: 100}, {Text: "08/02/2026", Left: 382, Top: 100}, {Text: "$1,001 - $15,000", Left: 447, Top: 100},
		{Text: "Malformed (BAD)", Left: 104, Top: 135}, {Text: "not a transaction", Left: 262, Top: 130}, {Text: "08/03/2026", Left: 326, Top: 130},
		{Text: "Current asset", Left: 104, Top: 160}, {Text: "Current (TARG)", Left: 104, Top: 150},
		{Text: "P", Left: 262, Top: 160}, {Text: "08/04/2026", Left: 326, Top: 160}, {Text: "08/05/2026", Left: 382, Top: 160}, {Text: "$1,001 - $15,000", Left: 447, Top: 160},
	}}}
	report := ParseHousePTR(filing, "fixture", []byte("pdf"), pages)
	if len(report.Transactions) != 2 || report.Transactions[1].Ticker != "TARG" {
		t.Fatalf("transactions = %+v, want malformed dated row to retain its wrapped asset", report.Transactions)
	}
}

func TestParseHousePTRDoesNotBorrowPriorRowAmountContinuation(t *testing.T) {
	filing := HouseFiling{First: "Jane", Last: "Member", Year: 2026, FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), DocumentID: "amount-boundary", StateDistrict: "CA01"}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "First", Left: 104, Top: 100}, {Text: "P", Left: 262, Top: 100}, {Text: "08/01/2026", Left: 326, Top: 100}, {Text: "08/02/2026", Left: 382, Top: 100}, {Text: "$1,001 -", Left: 447, Top: 100}, {Text: "$10,000", Left: 447, Top: 95},
		{Text: "Second", Left: 104, Top: 120}, {Text: "S", Left: 262, Top: 120}, {Text: "08/03/2026", Left: 326, Top: 120}, {Text: "08/04/2026", Left: 382, Top: 120}, {Text: "$15,001 -", Left: 447, Top: 120},
	}}}
	report := ParseHousePTR(filing, "fixture", []byte("pdf"), pages)
	if len(report.Transactions) != 2 || report.Transactions[0].AmountMax == nil || *report.Transactions[0].AmountMax != 10000 ||
		report.Transactions[1].AmountMin == nil || *report.Transactions[1].AmountMin != 15001 || report.Transactions[1].AmountMax != nil {
		t.Fatalf("transactions = %+v, want each amount continuation isolated to its row", report.Transactions)
	}
}

func TestHousePTRTransactionDateAnchorsDedupeNearDuplicateDates(t *testing.T) {
	anchors := housePTRTransactionDateAnchors([]TextBox{
		{Text: "08/01/2026", Left: 326, Top: 100},
		{Text: "08/01/2026", Left: 326, Top: 101},
		{Text: "08/02/2026", Left: 326, Top: 110},
	})
	if len(anchors) != 2 || anchors[0].Top != 100 || anchors[1].Top != 110 {
		t.Fatalf("anchors = %+v, want near-duplicate dates deduplicated", anchors)
	}
}

func TestParseHousePTRClassifiesEmptyAndUnreconstructedText(t *testing.T) {
	filing := HouseFiling{
		First: "Jane", Last: "Member", Year: 2026,
		FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		DocumentID: "empty", StateDistrict: "CA01",
	}
	tests := []struct {
		name      string
		pages     []PageText
		status    string
		retryable bool
		errorText string
	}{
		{
			name:      "no pages requires OCR but is terminal",
			status:    "no_text",
			errorText: "requires OCR",
		},
		{
			name: "no meaningful text requires OCR but is terminal",
			pages: []PageText{{Number: 1, Boxes: []TextBox{
				{Text: " \t\n "},
			}}},
			status: "no_text", errorText: "requires OCR",
		},
		{
			name: "extracted text without reconstructable rows retries",
			pages: []PageText{{Number: 1, Boxes: []TextBox{
				{Text: "Transaction Date Ticker Asset"},
			}}},
			status: "failed", retryable: true, errorText: "no transaction rows were reconstructed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := ParseHousePTR(filing, "https://example.test/empty.pdf", []byte("pdf"), test.pages)
			if report.ExtractionStatus != test.status || report.RetryableFailure != test.retryable ||
				!strings.Contains(report.ExtractionError, test.errorText) {
				t.Fatalf("report=%+v, want status=%q retryable=%v error containing %q", report, test.status, test.retryable, test.errorText)
			}
		})
	}
}

func TestParseHousePTRSkipsEmptyAssetsAfterWrappedContinuation(t *testing.T) {
	filing := HouseFiling{
		First: "Jane", Last: "Member", Year: 2026,
		FilingDate: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		DocumentID: "empty-assets", StateDistrict: "CA01",
	}
	row := func(top float64, asset string) []TextBox {
		return []TextBox{
			{Text: "Self", Left: 24, Top: top}, {Text: asset, Left: 104, Top: top},
			{Text: "P", Left: 262, Top: top}, {Text: "08/01/2026", Left: 326, Top: top},
			{Text: "08/02/2026", Left: 382, Top: top}, {Text: "$1,001 - $15,000", Left: 447, Top: top},
		}
	}
	t.Run("only whitespace assets are retryable", func(t *testing.T) {
		pages := []PageText{{Number: 1, Boxes: row(100, " \t\n ")}}
		report := ParseHousePTR(filing, "fixture", []byte("pdf"), pages)
		if report.ExtractionStatus != "failed" || !report.RetryableFailure || len(report.Transactions) != 0 {
			t.Fatalf("report = %+v, want retryable empty reconstruction", report)
		}
	})
	t.Run("mixed rows retain only complete assets", func(t *testing.T) {
		boxes := append(row(100, "  Acme   Fund (ACME) [ST]  "), row(120, " \t")...)
		report := ParseHousePTR(filing, "fixture", []byte("pdf"), []PageText{{Number: 1, Boxes: boxes}})
		if report.ExtractionStatus != "completed" || len(report.Transactions) != 1 ||
			report.Transactions[0].AssetName != "Acme Fund (ACME) [ST]" ||
			report.Transactions[0].Ticker != "ACME" || report.Transactions[0].AssetType != "ST" {
			t.Fatalf("report = %+v, want normalized valid asset only", report)
		}
	})
	t.Run("wrapped-only asset is retained", func(t *testing.T) {
		boxes := row(100, "")
		boxes = append(boxes, TextBox{Text: "Wrapped Fund (WRAP) [ST]", Left: 104, Top: 95})
		report := ParseHousePTR(filing, "fixture", []byte("pdf"), []PageText{{Number: 1, Boxes: boxes}})
		if report.ExtractionStatus != "completed" || len(report.Transactions) != 1 ||
			report.Transactions[0].AssetName != "Wrapped Fund (WRAP) [ST]" ||
			report.Transactions[0].Ticker != "WRAP" || report.Transactions[0].AssetType != "ST" {
			t.Fatalf("report = %+v, want wrapped asset", report)
		}
	})
}
