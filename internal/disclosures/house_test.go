package disclosures

import (
	"os"
	"strings"
	"testing"
	"time"
)

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
	pages, err := extractor.Extract(data)
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

func TestParseHousePTRUsesPositionedColumnsAndWrappedLines(t *testing.T) {
	filing := HouseFiling{
		Prefix: "Hon.", First: "David", Last: "Taylor", Year: 2026,
		FilingDate: time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC),
		DocumentID: "20034138", StateDistrict: "OH02",
	}
	pages := []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "Stock (GOOGL) [ST]", Left: 104, Top: 90},
		{Text: "Alphabet Inc. - Class A Common", Left: 104, Top: 100},
		{Text: "P", Left: 262, Top: 100},
		{Text: "02/26/2026", Left: 326, Top: 100},
		{Text: "03/05/2026", Left: 382, Top: 100},
		{Text: "$1,001 - $15,000", Left: 447, Top: 100},
		{Text: "$50,000", Left: 447, Top: 140},
		{Text: "(IBM) [ST]", Left: 104, Top: 140},
		{Text: "International Business Machines Common Stock", Left: 104, Top: 150},
		{Text: "S (partial)", Left: 262, Top: 150},
		{Text: "02/26/2026", Left: 326, Top: 150},
		{Text: "03/05/2026", Left: 382, Top: 150},
		{Text: "$15,001 -", Left: 447, Top: 150},
	}}}
	report := ParseHousePTR(filing, "https://example.test/20034138.pdf", []byte("pdf"), pages)
	if report.ExtractionStatus != "completed" || len(report.Transactions) != 2 {
		t.Fatalf("unexpected report: status=%s transactions=%+v", report.ExtractionStatus, report.Transactions)
	}
	first := report.Transactions[0]
	if first.Ticker != "GOOGL" || first.TransactionType != "purchase" ||
		first.AmountMin == nil || *first.AmountMin != 1001 ||
		first.AmountMax == nil || *first.AmountMax != 15000 {
		t.Fatalf("unexpected first transaction: %+v", first)
	}
	second := report.Transactions[1]
	if second.Ticker != "IBM" || second.TransactionType != "sale" ||
		second.AmountMin == nil || *second.AmountMin != 15001 ||
		second.AmountMax == nil || *second.AmountMax != 50000 {
		t.Fatalf("unexpected second transaction: %+v", second)
	}
}
