package disclosures

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOGEListFilingsRejectsOversizedIndexResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := make([]byte, 32<<10)
		remaining := maxOGEIndexSize + 1
		for remaining > 0 {
			writeSize := min(len(chunk), remaining)
			if _, err := w.Write(chunk[:writeSize]); err != nil {
				return
			}
			remaining -= writeSize
		}
	}))
	defer server.Close()

	client := NewOGEClient(server.URL, time.Second)
	_, err := client.ListFilingsPage(
		context.Background(), time.Time{}, time.Now(), 0, 100,
	)
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("ListFilingsPage error = %v, want response-size rejection", err)
	}
}

func TestOGEListFilingsSkipsRequestOnly(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":2,"data":[{"type":"<a href='{base}/a.pdf'>278 Transaction</a>","name":"A. Person","agency":"White House","title":"Advisor","docDate":"2026-07-09"},{"type":"Annual (2026) (<a href='{base}/request'>Request this Document</a>)","name":"B","docDate":"2026-07-10"}]}`, "{base}", s.URL)))
	}))
	defer s.Close()
	c := NewOGEClient(s.URL, time.Second)
	got, err := c.ListFilings(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ReportType != "periodic_transaction" {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeOGEReportTypeKeepsStableBaseType(t *testing.T) {
	for _, test := range []struct {
		name, label, amended, want string
	}{
		{"amended label", "Amended Transaction", "no", "periodic_transaction"},
		{"amended field", "278 Transaction", "Yes", "periodic_transaction"},
		{"date amendment marker", "278 Transaction", "2026-07-09", "periodic_transaction"},
		{"corrected annual", "Annual disclosure", "corrected", "annual"},
		{"false-like amendment", "278 Transaction", "false", "periodic_transaction"},
		{"false-like no", "278 Transaction", "no", "periodic_transaction"},
		{"false-like zero", "278 Transaction", "0", "periodic_transaction"},
		{"false-like n/a", "278 Transaction", "n/a", "periodic_transaction"},
		{"false-like na", "278 Transaction", "na", "periodic_transaction"},
		{"false-like not applicable", "278 Transaction", "not applicable", "periodic_transaction"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeOGEReportType(test.label, test.amended); got != test.want {
				t.Fatalf("report type = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOGEAmendedFieldRecognizesCommonFalseLikeValues(t *testing.T) {
	for _, value := range []string{"false", "n/a", "NA", "not applicable"} {
		if ogeAmendedFieldIsAmendment(value) {
			t.Fatalf("amended field %q treated as amendment", value)
		}
	}
}

func TestOGEListFilingsPreservesAmendmentMetadata(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":2,"data":[{"type":"<a href='{base}/amended-label.pdf'>Amended Transaction</a>","amended":"false","name":"Label Amendment","docDate":"2026-07-09"},{"type":"<a href='{base}/amended-field.pdf'>278 Transaction</a>","amended":"yes","name":"Field Amendment","docDate":"2026-07-10"}]}`, "{base}", server.URL)))
	}))
	defer server.Close()

	filings, err := NewOGEClient(server.URL, time.Second).ListFilings(
		context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 2 {
		t.Fatalf("filings = %#v", filings)
	}
	for _, filing := range filings {
		if filing.ReportType != "periodic_transaction" || !filing.IsAmendment {
			t.Fatalf("filing = %#v, want stable type and amendment flag", filing)
		}
		if !strings.Contains(string(filing.RawMetadata), `"amended"`) {
			t.Fatalf("raw metadata lost amendment field: %s", filing.RawMetadata)
		}
	}
}

func TestOGEListFilingsKeepsTimestampedInclusiveEndDate(t *testing.T) {
	var s *httptest.Server
	s = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":2,"data":[{"type":"<a href='{base}/end-date.pdf'>278 Transaction</a>","name":"End Date","docDate":"2026-07-31T17:45:00"},{"type":"<a href='{base}/next-date.pdf'>278 Transaction</a>","name":"Next Date","docDate":"2026-08-01T00:00:01"}]}`, "{base}", s.URL)))
	}))
	defer s.Close()
	c := NewOGEClient(s.URL, time.Second)
	got, err := c.ListFilings(
		context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FilerName != "End Date" {
		t.Fatalf("filings = %#v", got)
	}
}

func TestOGEListFilingsRejectsMalformedDateAtomically(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":2,"data":[{"type":"<a href='{base}/malformed.pdf'>278 Transaction</a>","name":"Malformed","docDate":"not-a-date"},{"type":"<a href='{base}/valid.pdf'>278 Transaction</a>","name":"Valid","docDate":"2026-07-15T17:45:00.123-04:00"}]}`, "{base}", server.URL)))
	}))
	defer server.Close()

	client := NewOGEClient(server.URL, time.Second)
	page, err := client.ListFilingsPage(
		context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 0, 100,
	)
	if err == nil || len(page.Filings) != 0 || page.RowCount != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestOGEListFilingsRejectsUntrustedPDFAtomically(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordsTotal":2,"data":[{"type":"<a href='https://untrusted.example/document.pdf'>278 Transaction</a>","name":"Untrusted","docDate":"2026-07-15"},{"type":"<a href='https://untrusted.example/other.pdf'>278 Transaction</a>","name":"Also Untrusted","docDate":"2026-07-15"}]}`))
	}))
	defer server.Close()

	page, err := NewOGEClient(server.URL, time.Second).ListFilingsPage(context.Background(), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 0, 100)
	if err == nil || len(page.Filings) != 0 || page.RowCount != 0 {
		t.Fatalf("page=%+v err=%v, want atomic untrusted URL rejection", page, err)
	}
}

func TestOGEListFilingsRejectsNegativeRecordsTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordsTotal":-1,"data":[]}`))
	}))
	defer server.Close()
	page, err := NewOGEClient(server.URL, time.Second).ListFilingsPage(context.Background(), time.Time{}, time.Now(), 0, 100)
	if err == nil || page.RowCount != 0 {
		t.Fatalf("page=%+v err=%v, want recordsTotal rejection", page, err)
	}
}

func TestOGEListFilingsContinuesPastMixedOrderOldRows(t *testing.T) {
	var starts []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("start"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("start") {
		case "0":
			_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":4,"data":[{"type":"<a href='{base}/old.pdf'>278 Transaction</a>","name":"Old","docDate":"2026-06-30"},{"type":"<a href='{base}/in-range-one.pdf'>278 Transaction</a>","name":"In Range One","docDate":"2026-07-10"}]}`, "{base}", server.URL)))
		case "2":
			_, _ = w.Write([]byte(strings.ReplaceAll(`{"recordsTotal":4,"data":[{"type":"<a href='{base}/in-range-two.pdf'>278 Transaction</a>","name":"In Range Two","docDate":"2026-07-11"},{"type":"<a href='{base}/older.pdf'>278 Transaction</a>","name":"Older","docDate":"2026-06-29"}]}`, "{base}", server.URL)))
		default:
			t.Fatalf("unexpected page start %q", r.URL.Query().Get("start"))
		}
	}))
	defer server.Close()

	client := NewOGEClient(server.URL, time.Second)
	filings, err := client.ListFilings(
		context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(starts) != 2 || starts[0] != "0" || starts[1] != "2" {
		t.Fatalf("page starts = %v, want [0 2]", starts)
	}
	if len(filings) != 2 || filings[0].FilerName != "In Range One" || filings[1].FilerName != "In Range Two" {
		t.Fatalf("filings = %#v", filings)
	}
}

func TestOGEFetchPDFRejectsCrossOriginAndRedirectingDocumentURLs(t *testing.T) {
	var destinationHits int
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationHits++
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/documents/redirect.pdf":
			http.Redirect(w, r, destination.URL+"/document.pdf", http.StatusFound)
		case "/documents/ok.pdf":
			_, _ = w.Write([]byte("fixture pdf"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewOGEClient(server.URL, time.Second)
	for _, documentURL := range []string{
		"https://untrusted.example/document.pdf",
		server.URL + "/documents/redirect.pdf",
	} {
		_, err := client.FetchPDF(context.Background(), OGEFiling{DocumentURL: documentURL})
		if err == nil || !strings.Contains(err.Error(), "cross-origin") {
			t.Fatalf("FetchPDF(%q) error = %v, want cross-origin rejection", documentURL, err)
		}
	}
	if _, err := client.FetchPDF(context.Background(), OGEFiling{DocumentURL: server.URL + "/documents/ok.pdf"}); err != nil {
		t.Fatalf("FetchPDF trusted document: %v", err)
	}
	if destinationHits != 0 {
		t.Fatalf("cross-origin redirect destination was requested %d times", destinationHits)
	}
}

func TestParseOGEPDFReconstructsRows(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-test", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{{Text: "07/01/2026", Left: 1, Top: 10}, {Text: "Purchase", Left: 80, Top: 10}, {Text: "Acme Corp (ACME)", Left: 140, Top: 10}, {Text: "$1,001 - $15,000", Left: 260, Top: 10}}}})
	if len(report.Transactions) != 1 || report.Transactions[0].TransactionType != "purchase" || report.Transactions[0].Ticker != "ACME" {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.Transactions[0].TransactionDate.IsZero() || report.Transactions[0].AmountMin == nil {
		t.Fatalf("row fields were not reconstructed: %#v", report.Transactions[0])
	}
	if got, want := report.Transactions[0].AssetName, "Acme Corp (ACME)"; got != want {
		t.Fatalf("asset = %q, want %q", got, want)
	}
	if got, want := report.Transactions[0].AmountText, "$1,001 - $15,000"; got != want {
		t.Fatalf("amount text = %q, want exact amount range %q", got, want)
	}
}

func TestParseOGEPDFDoesNotPromoteLowercaseParentheticalToTicker(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-ticker-case", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{
		{Text: "07/01/2026", Left: 1, Top: 10}, {Text: "Purchase", Left: 80, Top: 10}, {Text: "Acme Fund (common)", Left: 140, Top: 10}, {Text: "$1,001 - $15,000", Left: 260, Top: 10},
		{Text: "07/02/2026", Left: 1, Top: 20}, {Text: "Purchase", Left: 80, Top: 20}, {Text: "Bravo Fund (BRAV)", Left: 140, Top: 20}, {Text: "$1,001 - $15,000", Left: 260, Top: 20},
	}}})
	if len(report.Transactions) != 2 || report.Transactions[0].Ticker != "" || report.Transactions[1].Ticker != "BRAV" {
		t.Fatalf("tickers = %+v, want lowercase descriptor ignored and uppercase symbol retained", report.Transactions)
	}
}

func TestParseOGEPDFUsesTransactionColumnWhenAssetMentionsPurchase(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-test-sale", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{
		{Text: "07/01/2026", Left: 1, Top: 10},
		{Text: "Sale", Left: 80, Top: 10},
		{Text: "Purchase Corp (ACME)", Left: 140, Top: 10},
		{Text: "$15,001 - $50,000", Left: 260, Top: 10},
	}}})
	if len(report.Transactions) != 1 || report.Transactions[0].TransactionType != "sale" || report.Transactions[0].Ticker != "ACME" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestParseOGEPDFReconstructsCompactActionCodes(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-test-compact", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{
		{Text: "07/01/2026", Left: 1, Top: 10},
		{Text: "P", Left: 80, Top: 10},
		{Text: "Compact Corp (CMPT)", Left: 140, Top: 10},
		{Text: "$1,001 - $15,000", Left: 260, Top: 10},
	}}})
	if len(report.Transactions) != 1 || report.Transactions[0].TransactionType != "purchase" || report.Transactions[0].Ticker != "CMPT" {
		t.Fatalf("unexpected compact report: %#v", report)
	}
}

func TestParseOGEPDFReconstructsEveryCompactActionCode(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-compact-codes", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	tests := []struct {
		code, want string
	}{
		{code: "P", want: "purchase"},
		{code: "S", want: "sale"},
		{code: "E", want: "exchange"},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{
				{Text: "07/01/2026", Left: 1, Top: 10},
				{Text: test.code, Left: 80, Top: 10},
				{Text: "Compact Corp (CMPT)", Left: 140, Top: 10},
				{Text: "$1,001 - $15,000", Left: 260, Top: 10},
			}}})
			if len(report.Transactions) != 1 || report.Transactions[0].TransactionType != test.want {
				t.Fatalf("compact code %q report=%+v", test.code, report)
			}
		})
	}
}

func TestParseOGEPDFKeepsAssetActionWordsAndAttachesWrappedFields(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-wrapped", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 2, Boxes: []TextBox{
		{Text: "Transaction Date", Left: 1, Top: 1},
		{Text: "Transaction Type", Left: 80, Top: 1},
		{Text: "Asset", Left: 140, Top: 1},
		{Text: "Amount", Left: 260, Top: 1},
		{Text: "07/01/2026", Left: 1, Top: 10},
		{Text: "P", Left: 80, Top: 10},
		{Text: "Purchase Preference Corp", Left: 140, Top: 10},
		{Text: "(PPC)", Left: 140, Top: 14},
		{Text: "$1,001 -", Left: 260, Top: 10},
		{Text: "$15,000", Left: 260, Top: 14},
		// The next dated transaction is deliberately close: neither its asset
		// nor its amount may become a continuation of the first row.
		{Text: "07/02/2026", Left: 1, Top: 20},
		{Text: "S", Left: 80, Top: 20},
		{Text: "Second Corp (NEXT)", Left: 140, Top: 20},
		{Text: "$15,001 - $50,000", Left: 260, Top: 20},
	}}})
	if report.ExtractionStatus != "completed" || report.RetryableFailure || len(report.Transactions) != 2 {
		t.Fatalf("report=%+v", report)
	}
	first := report.Transactions[0]
	if first.TransactionType != "purchase" || first.AssetName != "Purchase Preference Corp (PPC)" ||
		first.Ticker != "PPC" || first.AmountText != "$1,001 - $15,000" ||
		first.AmountMin == nil || *first.AmountMin != 1001 || first.AmountMax == nil || *first.AmountMax != 15000 {
		t.Fatalf("first transaction=%+v", first)
	}
	second := report.Transactions[1]
	if second.TransactionType != "sale" || second.AssetName != "Second Corp (NEXT)" ||
		second.AmountText != "$15,001 - $50,000" {
		t.Fatalf("second transaction=%+v", second)
	}
}

func TestParseOGEPDFHeaderDateBandDoesNotSplitAssetDates(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-header-dates", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "Transaction Date", Left: 10, Top: 1}, {Text: "Action", Left: 80, Top: 1}, {Text: "Asset", Left: 140, Top: 1}, {Text: "Amount", Left: 260, Top: 1},
		{Text: "07/01/2026", Left: 10, Top: 10}, {Text: "P", Left: 80, Top: 10}, {Text: "Acme 07/02/2026 Fund (ACME)", Left: 140, Top: 10}, {Text: "$1,001 - $15,000", Left: 260, Top: 10},
		{Text: "07/03/2026", Left: 10, Top: 20}, {Text: "S", Left: 80, Top: 20}, {Text: "Bravo 07/04/2026 Fund (BRAV)", Left: 140, Top: 20}, {Text: "$15,001 - $50,000", Left: 260, Top: 20},
	}}})
	if len(report.Transactions) != 2 {
		t.Fatalf("transactions=%+v, want two unsplit rows", report.Transactions)
	}
	if report.Transactions[0].AssetName != "Acme 07/02/2026 Fund (ACME)" || report.Transactions[1].AssetName != "Bravo 07/04/2026 Fund (BRAV)" {
		t.Fatalf("asset dates were split from their rows: %+v", report.Transactions)
	}
}

func TestParseOGEPDFInfersHeaderlessDateBandWithoutAssetDateAnchors(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-headerless-dates", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	report := ParseOGEPDF(filing, []byte("pdf"), []PageText{{Number: 1, Boxes: []TextBox{
		{Text: "07/01/2026", Left: 10, Top: 10}, {Text: "P", Left: 80, Top: 10}, {Text: "Acme 07/02/2026 Fund (ACME)", Left: 140, Top: 10}, {Text: "$1,001 - $15,000", Left: 260, Top: 10},
		{Text: "07/03/2026", Left: 10, Top: 20}, {Text: "S", Left: 80, Top: 20}, {Text: "Bravo 07/04/2026 Fund (BRAV)", Left: 140, Top: 20}, {Text: "$15,001 - $50,000", Left: 260, Top: 20},
	}}})
	if len(report.Transactions) != 2 {
		t.Fatalf("transactions=%+v, want two unsplit rows", report.Transactions)
	}
	if report.Transactions[0].TransactionDate.Format("01/02/2006") != "07/01/2026" || report.Transactions[1].TransactionDate.Format("01/02/2006") != "07/03/2026" {
		t.Fatalf("transaction dates=%+v, want true date column", report.Transactions)
	}
}

func TestParseOGEPDFClassifiesNoTextAndParserMiss(t *testing.T) {
	filing := OGEFiling{ExternalID: "oge-status", ReportType: "periodic_transaction", FilingDate: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	tests := []struct {
		name      string
		pages     []PageText
		status    string
		retryable bool
		errorText string
	}{
		{name: "empty is terminal OCR", status: "no_text", errorText: "requires OCR"},
		{name: "whitespace is terminal OCR", pages: []PageText{{Number: 1, Boxes: []TextBox{{Text: " \t\n "}}}}, status: "no_text", errorText: "requires OCR"},
		{name: "text parser miss retries", pages: []PageText{{Number: 1, Boxes: []TextBox{{Text: "transaction table heading"}}}}, status: "failed", retryable: true, errorText: "no transaction rows were reconstructed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := ParseOGEPDF(filing, []byte("pdf"), test.pages)
			if report.ExtractionStatus != test.status || report.RetryableFailure != test.retryable || !strings.Contains(report.ExtractionError, test.errorText) {
				t.Fatalf("report=%+v, want status=%q retryable=%v error containing %q", report, test.status, test.retryable, test.errorText)
			}
		})
	}
}
