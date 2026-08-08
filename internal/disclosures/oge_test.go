package disclosures

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
