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
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordsTotal":2,"data":[{"type":"<a href='https://example.test/a.pdf'>278 Transaction</a>","name":"A. Person","agency":"White House","title":"Advisor","docDate":"2026-07-09"},{"type":"Annual (2026) (<a href='https://example.test/request'>Request this Document</a>)","name":"B","docDate":"2026-07-10"}]}`))
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

func TestOGEListFilingsKeepsTimestampedInclusiveEndDate(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordsTotal":2,"data":[{"type":"<a href='https://example.test/end-date.pdf'>278 Transaction</a>","name":"End Date","docDate":"2026-07-31T17:45:00"},{"type":"<a href='https://example.test/next-date.pdf'>278 Transaction</a>","name":"Next Date","docDate":"2026-08-01T00:00:01"}]}`))
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

func TestOGEListFilingsSkipsMalformedDateWithoutEndingPageRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordsTotal":2,"data":[{"type":"<a href='https://example.test/malformed.pdf'>278 Transaction</a>","name":"Malformed","docDate":"not-a-date"},{"type":"<a href='https://example.test/valid.pdf'>278 Transaction</a>","name":"Valid","docDate":"2026-07-15T17:45:00.123-04:00"}]}`))
	}))
	defer server.Close()

	client := NewOGEClient(server.URL, time.Second)
	page, err := client.ListFilingsPage(
		context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 0, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.BeforeRange || len(page.Filings) != 1 || page.Filings[0].FilerName != "Valid" {
		t.Fatalf("page = %+v", page)
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
