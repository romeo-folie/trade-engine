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
