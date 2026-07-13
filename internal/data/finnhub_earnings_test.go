package data

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchEarningsCalendar_ParsesAndSkipsBadDates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("symbol"); got != "NVDA" {
			t.Errorf("symbol param = %q; want NVDA", got)
		}
		fmt.Fprint(w, `{"earningsCalendar":[
			{"symbol":"NVDA","date":"2024-08-28","hour":"amc"},
			{"symbol":"NVDA","date":"not-a-date","hour":"bmo"},
			{"symbol":"NVDA","date":"2024-11-20","hour":""}
		]}`)
	}))
	defer srv.Close()

	c := newTestFinnhubClient(srv.URL)
	got, err := c.FetchEarningsCalendar(context.Background(), "NVDA",
		time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchEarningsCalendar: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 parsed dates (bad one skipped), got %d: %+v", len(got), got)
	}
	if got[0].Date.Format("2006-01-02") != "2024-08-28" || got[0].Hour != "amc" {
		t.Errorf("first = %+v; want 2024-08-28/amc", got[0])
	}
	if got[1].Date.Format("2006-01-02") != "2024-11-20" {
		t.Errorf("second date = %s; want 2024-11-20", got[1].Date.Format("2006-01-02"))
	}
}

func TestFetchEarningsCalendar_EmptyIsNotAnError(t *testing.T) {
	// Free tier returns an empty list for historical windows — must be (nil, nil), not an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"earningsCalendar":[]}`)
	}))
	defer srv.Close()

	c := newTestFinnhubClient(srv.URL)
	got, err := c.FetchEarningsCalendar(context.Background(), "NVDA",
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("empty calendar must not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 releases, got %d", len(got))
	}
}
