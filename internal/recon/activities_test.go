package recon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// activityJSON renders n synthetic FILL activities with sequential ids starting
// at start, matching Alpaca's string-typed qty/price wire format.
func activityJSON(start, n int) string {
	var b strings.Builder
	b.WriteString("[")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"id-%06d","activity_type":"FILL","symbol":"AAA","side":"buy",`+
			`"qty":"1","price":"10","transaction_time":"2026-07-01T14:00:00Z"}`, start+i)
	}
	b.WriteString("]")
	return b.String()
}

func fetcherFor(srv *httptest.Server) ActivityFetcher {
	return ActivityFetcher{BaseURL: srv.URL, APIKey: "k", Secret: "s", Client: srv.Client()}
}

func wholeWindow() (time.Time, time.Time) {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
}

// Pagination must walk by page_token and must not repeat or drop rows.
func TestFetchFillsPaginatesWithoutDuplicates(t *testing.T) {
	var seenTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("page_token")
		seenTokens = append(seenTokens, token)
		switch token {
		case "":
			fmt.Fprint(w, activityJSON(0, activityPageSize))
		case "id-000099":
			fmt.Fprint(w, activityJSON(100, activityPageSize))
		case "id-000199":
			fmt.Fprint(w, activityJSON(200, 7)) // short page ends the walk
		default:
			t.Errorf("unexpected page_token %q", token)
			fmt.Fprint(w, "[]")
		}
	}))
	defer srv.Close()

	after, until := wholeWindow()
	fills, err := fetcherFor(srv).FetchFills(context.Background(), after, until)
	if err != nil {
		t.Fatalf("FetchFills: %v", err)
	}
	if len(fills) != 207 {
		t.Fatalf("got %d fills, want 207 (100+100+7)", len(fills))
	}
	ids := map[string]bool{}
	for _, f := range fills {
		if ids[f.ID] {
			t.Fatalf("duplicate fill %s — page_token walk repeated a page", f.ID)
		}
		ids[f.ID] = true
	}
	want := []string{"", "id-000099", "id-000199"}
	if len(seenTokens) != len(want) {
		t.Fatalf("tokens = %v, want %v", seenTokens, want)
	}
	for i, tok := range want {
		if seenTokens[i] != tok {
			t.Errorf("request %d used token %q, want %q", i, seenTokens[i], tok)
		}
	}
}

// A server that always returns a full page must produce an error, never a
// truncated history reported as complete. Because the walk is ascending, the
// rows lost would be the newest — silently erasing open positions.
func TestFetchFillsRefusesToTruncate(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, activityJSON(page*activityPageSize, activityPageSize))
		page++
	}))
	defer srv.Close()

	after, until := wholeWindow()
	fills, err := fetcherFor(srv).FetchFills(context.Background(), after, until)
	if err == nil {
		t.Fatalf("got %d fills and nil error; want an error rather than a silently truncated history", len(fills))
	}
	if fills != nil {
		t.Error("partial fills returned alongside the error; callers must not be able to use them")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error = %q, want it to name page exhaustion", err)
	}
}

// An empty id on a full page would drop page_token from the next request and
// restart at page zero, looping until the cap with a nil error.
func TestFetchFillsRejectsUnusablePageToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("[")
		for i := range activityPageSize {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"id":"","activity_type":"FILL","symbol":"AAA","side":"buy",` +
				`"qty":"1","price":"10","transaction_time":"2026-07-01T14:00:00Z"}`)
		}
		b.WriteString("]")
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()

	after, until := wholeWindow()
	if _, err := fetcherFor(srv).FetchFills(context.Background(), after, until); err == nil {
		t.Fatal("want an error for an unusable page token, got nil")
	}
}

func TestFetchFillsSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"forbidden"}`)
	}))
	defer srv.Close()

	after, until := wholeWindow()
	_, err := fetcherFor(srv).FetchFills(context.Background(), after, until)
	if err == nil {
		t.Fatal("want an error on HTTP 403, got nil")
	}
	// The status must win over the decode failure a JSON object would cause,
	// or a permissions problem reads as a parser bug.
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, want it to name the HTTP status", err)
	}
}

func TestFetchFillsRejectsMalformedNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"x","activity_type":"FILL","symbol":"AAA","side":"buy",`+
			`"qty":"not-a-number","price":"10","transaction_time":"2026-07-01T14:00:00Z"}]`)
	}))
	defer srv.Close()

	after, until := wholeWindow()
	_, err := fetcherFor(srv).FetchFills(context.Background(), after, until)
	if err == nil {
		t.Fatal("want an error for an unparseable qty, got nil — a skipped fill corrupts the basis silently")
	}
}

// The window must reach the API, or the caller's --since is decorative.
func TestFetchFillsSendsWindowAndOrder(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{
			"after":     r.URL.Query().Get("after"),
			"until":     r.URL.Query().Get("until"),
			"direction": r.URL.Query().Get("direction"),
		}
		fmt.Fprint(w, "[]")
	}))
	defer srv.Close()

	after := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := fetcherFor(srv).FetchFills(context.Background(), after, until); err != nil {
		t.Fatalf("FetchFills: %v", err)
	}
	if !strings.HasPrefix(got["after"], "2026-06-01") || !strings.HasPrefix(got["until"], "2026-07-01") {
		t.Errorf("window not forwarded: %v", got)
	}
	if got["direction"] != "asc" {
		t.Errorf("direction = %q, want asc — the walk assumes chronological order", got["direction"])
	}
}

// Non-FILL rows and rows without a symbol must not enter the walk.
func TestFetchFillsSkipsNonFillActivities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rows := []map[string]string{
			{"id": "1", "activity_type": "FEE", "symbol": "", "side": "", "qty": "0", "price": "0",
				"transaction_time": "2026-07-01T14:00:00Z"},
			{"id": "2", "activity_type": "FILL", "symbol": "AAA", "side": "buy", "qty": "3", "price": "9",
				"transaction_time": "2026-07-01T14:00:00Z"},
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()

	after, until := wholeWindow()
	fills, err := fetcherFor(srv).FetchFills(context.Background(), after, until)
	if err != nil {
		t.Fatalf("FetchFills: %v", err)
	}
	if len(fills) != 1 || fills[0].Symbol != "AAA" {
		t.Fatalf("fills = %+v, want only the FILL row", fills)
	}
}
