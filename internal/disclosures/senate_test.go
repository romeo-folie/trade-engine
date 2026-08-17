package disclosures

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fixtureSenateTransport struct {
	searchBody []byte
	fetchBody  []byte
	fetchErr   error
	closed     bool
}

func (t *fixtureSenateTransport) SearchPTRs(
	context.Context, time.Time, time.Time, int, int,
) ([]byte, error) {
	return t.searchBody, nil
}

func (t *fixtureSenateTransport) FetchPTR(context.Context, string) ([]byte, error) {
	return t.fetchBody, t.fetchErr
}

func (t *fixtureSenateTransport) Close() error {
	t.closed = true
	return nil
}

func TestParseSenatePTRTable(t *testing.T) {
	filing := SenateFiling{
		ExternalID: "a0010f4a-c31a-4824-8b6d-6399b3ccb6f0",
		FilerName:  "Jane Senator", FilingDate: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
		DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/a0010f4a-c31a-4824-8b6d-6399b3ccb6f0/",
	}
	body := `<html><table><thead><tr><th>#</th><th>Transaction Date</th><th>Owner</th><th>Ticker</th><th>Asset Name</th><th>Asset Type</th><th>Transaction Type</th><th>Amount</th><th>Comment</th></tr></thead><tbody><tr><td>1</td><td>03/01/2026</td><td>Spouse</td><td>MSFT</td><td>Microsoft Corporation</td><td>Stock</td><td>Purchase</td><td>$15,001 - $50,000</td><td>Test note</td></tr></tbody></table></html>`
	report, err := ParseSenatePTR(filing, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Transactions) != 1 {
		t.Fatalf("transactions = %+v", report.Transactions)
	}
	item := report.Transactions[0]
	if item.Ticker != "MSFT" || item.Owner != "Spouse" ||
		item.TransactionType != "purchase" || item.AmountMax == nil || *item.AmountMax != 50000 {
		t.Fatalf("unexpected transaction: %+v", item)
	}
}

func TestParseSenateSearchPageRejectsMixedValidAndMalformedRows(t *testing.T) {
	page, err := parseSenateSearchPage("https://efdsearch.senate.gov", []byte(`{"recordsFiltered":2,"data":[["Jane","Senator","Office","<a href='/search/view/ptr/valid/'>PTR</a>","03/05/2026"],["broken"]]}`))
	if err == nil {
		t.Fatal("mixed valid/malformed Senate page was accepted")
	}
	if len(page.Filings) != 0 || page.RowCount != 0 || page.Total != 0 {
		t.Fatalf("partial page = %+v, want zero value", page)
	}
}

func TestSanitizeSenateTicker(t *testing.T) {
	if got := sanitizeSenateTicker("-- AMCR"); got != "AMCR" {
		t.Fatalf("ticker = %q", got)
	}
	if got := sanitizeSenateTicker("N/A"); got != "" {
		t.Fatalf("N/A ticker = %q", got)
	}
}

func TestParseSenatePTRLiveTypeHeaderDoesNotMatchAssetType(t *testing.T) {
	filing := SenateFiling{
		ExternalID: "live-header", FilerName: "Jane Senator",
		FilingDate:  time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/live-header/",
	}
	body := `<html><table><tr><th>#</th><th>Transaction Date</th><th>Owner</th><th>Ticker</th><th>Asset Name</th><th>Asset Type</th><th>Type</th><th>Amount</th><th>Comment</th></tr><tr><td>1</td><td>07/23/2026</td><td>Self</td><td>O</td><td>Realty Income Corporation Common Stock</td><td>Stock</td><td>Purchase</td><td>$1,001 - $15,000</td><td>--</td></tr></table></html>`
	report, err := ParseSenatePTR(filing, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Transactions) != 1 ||
		report.Transactions[0].TransactionType != "purchase" ||
		report.Transactions[0].AssetType != "Stock" {
		t.Fatalf("transactions = %+v", report.Transactions)
	}
}

func TestParseSenatePTRRejectsMalformedCanonicalTablesAtomically(t *testing.T) {
	filing := SenateFiling{ExternalID: "strict", FilerName: "Jane Senator", FilingDate: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC), DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/strict/"}
	header := `<tr><th>Asset Name</th><th>Transaction Type</th><th>Transaction Date</th><th>Amount</th></tr>`
	valid := `<tr><td>Microsoft</td><td>Purchase</td><td>07/23/2026</td><td>$1,001 - $15,000</td></tr>`
	for name, body := range map[string]string{
		"blank amount":         `<table>` + header + `<tr><td>Microsoft</td><td>Purchase</td><td>07/23/2026</td><td></td></tr></table>`,
		"malformed middle row": `<table>` + header + valid + `<tr><td>Apple</td><td>Unknown</td><td>07/24/2026</td><td>$1,001</td></tr>` + valid + `</table>`,
		"multiple tables":      `<table>` + header + valid + `</table><table>` + header + valid + `</table>`,
		"duplicate header":     `<table><tr><th>Asset Name</th><th>Asset</th><th>Transaction Type</th><th>Transaction Date</th><th>Amount</th></tr>` + valid + `</table>`,
	} {
		t.Run(name, func(t *testing.T) {
			report, err := ParseSenatePTR(filing, []byte("<html>"+body+"</html>"))
			if err == nil || len(report.Transactions) != 0 {
				t.Fatalf("report=%+v err=%v, want atomic parse rejection", report, err)
			}
		})
	}
}

func TestSenateClientConsentAndSearchFlow(t *testing.T) {
	var calls []string
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls = append(calls, req.Method+" "+req.URL.Path)
		if req.Header.Get("User-Agent") != "Fixture Browser/3.0" {
			t.Fatalf("User-Agent = %q", req.Header.Get("User-Agent"))
		}
		switch len(calls) {
		case 1:
			return response(req, 200, `<input type="hidden" name="csrfmiddlewaretoken" value="form-token">`, nil), nil
		case 2:
			body, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(body), "prohibition_agreement=1") {
				t.Fatalf("agreement payload = %s", body)
			}
			return response(req, 200, "", &http.Cookie{Name: "csrftoken", Value: "cookie-token"}), nil
		case 3:
			body, _ := io.ReadAll(req.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("report_types") != "[11]" || req.Header.Get("X-CSRFToken") != "cookie-token" {
				t.Fatalf("search request form=%v csrf=%q", values, req.Header.Get("X-CSRFToken"))
			}
			return response(req, 200, `{"data":[["Jane","Senator","Office","<a href='/search/view/ptr/abc-123/'>PTR</a>","03/05/2026"]]}`, nil), nil
		default:
			t.Fatalf("unexpected request %s", req.URL)
			return nil, nil
		}
	})
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = transport
	client.UserAgent = "Fixture Browser/3.0"
	filings, err := client.ListPTRs(context.Background(),
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 || filings[0].ExternalID != "abc-123" || filings[0].FilerName != "Jane Senator" {
		t.Fatalf("filings = %+v", filings)
	}
}

func TestSenateClientImportedBrowserSessionSkipsConsent(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/search/report/data/" {
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		if req.Header.Get("User-Agent") != "Fixture Browser/1.0" ||
			req.Header.Get("X-CSRFToken") != "csrf-value" {
			t.Fatalf("headers = %v", req.Header)
		}
		session, err := req.Cookie("sessionid")
		if err != nil || session.Value != "session-value" {
			t.Fatalf("session cookie = %+v err=%v", session, err)
		}
		return response(req, 200, `{"data":[["Jane","Senator","Office","<a href='/search/view/ptr/imported-123/'>PTR</a>","07/05/2026"]]}`, nil), nil
	})
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = transport
	cookies, err := ParseSenateBrowserCookies([]byte(`[
		{"domain":"efdsearch.senate.gov","name":"csrftoken","value":"csrf-value","path":"/","secure":true},
		{"domain":"efdsearch.senate.gov","name":"sessionid","value":"session-value","path":"/","secure":true}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ImportBrowserSession(cookies, "Fixture Browser/1.0"); err != nil {
		t.Fatal(err)
	}
	filings, err := client.ListPTRs(context.Background(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filings) != 1 || filings[0].ExternalID != "imported-123" {
		t.Fatalf("filings = %+v", filings)
	}
}

func TestSenateImportedSessionAuthorizationFailuresAreSourceBlocked(t *testing.T) {
	t.Run("missing csrf", func(t *testing.T) {
		client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		client.UseImportedSession = true
		home, _ := url.Parse(client.BaseURL)
		client.HTTP.Jar.SetCookies(home, []*http.Cookie{{Name: "sessionid", Value: "session-value", Path: "/"}})
		_, err = client.ListPTRs(context.Background(), time.Now().AddDate(0, 0, -1), time.Now(), 1)
		if !errors.Is(err, ErrSenateAuthorizationRequired) {
			t.Fatalf("missing csrf error=%v, want source block", err)
		}
	})
	t.Run("search redirect", func(t *testing.T) {
		client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := response(req, http.StatusOK, "", nil)
			redirected := req.Clone(req.Context())
			redirected.URL, _ = url.Parse("https://efdsearch.senate.gov/search/home/")
			resp.Request = redirected
			return resp, nil
		})
		cookies, err := ParseSenateBrowserCookies([]byte(`[{"domain":"efdsearch.senate.gov","name":"csrftoken","value":"csrf","path":"/","secure":true},{"domain":"efdsearch.senate.gov","name":"sessionid","value":"session","path":"/","secure":true}]`))
		if err != nil {
			t.Fatal(err)
		}
		if err := client.ImportBrowserSession(cookies, "Fixture"); err != nil {
			t.Fatal(err)
		}
		_, err = client.ListPTRs(context.Background(), time.Now().AddDate(0, 0, -1), time.Now(), 1)
		if !errors.Is(err, ErrSenateAuthorizationRequired) {
			t.Fatalf("redirect error=%v, want source block", err)
		}
	})
}

func TestSenateClientImportedSessionPagePreservesServerTotal(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("start") != "100" || values.Get("length") != "50" {
			t.Fatalf("pagination form = %v", values)
		}
		return response(req, 200, `{"recordsFiltered":250,"data":[["Jane","Senator","Office","<a href='/search/view/ptr/page-123/'>PTR</a>","07/05/2026"]]}`, nil), nil
	})
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = transport
	cookies, err := ParseSenateBrowserCookies([]byte(`[
		{"domain":"efdsearch.senate.gov","name":"csrftoken","value":"csrf-value","path":"/","secure":true},
		{"domain":"efdsearch.senate.gov","name":"sessionid","value":"session-value","path":"/","secure":true}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ImportBrowserSession(cookies, "Fixture Browser/1.0"); err != nil {
		t.Fatal(err)
	}
	page, err := client.ListPTRPage(
		context.Background(), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC), 100, 50,
	)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 250 || page.RowCount != 1 || len(page.Filings) != 1 ||
		page.Filings[0].ExternalID != "page-123" {
		t.Fatalf("page = %+v", page)
	}
}

func TestSenateClientUsesBrowserTransport(t *testing.T) {
	transport := &fixtureSenateTransport{
		searchBody: []byte(`{"data":[["Jane","Senator","Office","<a href='/search/view/ptr/browser-123/'>PTR</a>","07/05/2026"]]}`),
		fetchBody:  []byte(`<html><table><tr><th>Transaction Date</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>07/01/2026</td><td>Microsoft (MSFT)</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table></html>`),
	}
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.Transport = transport
	filings, err := client.ListPTRs(context.Background(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || len(filings) != 1 {
		t.Fatalf("filings=%+v err=%v", filings, err)
	}
	report, err := client.FetchPTR(context.Background(), filings[0])
	if err != nil || len(report.Transactions) != 1 || report.Transactions[0].Ticker != "MSFT" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if err := client.Close(); err != nil || !transport.closed {
		t.Fatalf("closed=%v err=%v", transport.closed, err)
	}
}

func TestSenateFetchPTRRejectsCrossOriginAndRedirectingDocumentURLs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/view/ptr/redirect/":
			http.Redirect(w, r, "https://untrusted.example/search/view/ptr/abc/", http.StatusFound)
		case "/search/view/ptr/ok/":
			_, _ = w.Write([]byte(`<table><tr><th>Transaction Date</th><th>Asset Name</th><th>Transaction Type</th><th>Amount</th></tr><tr><td>07/01/2026</td><td>Microsoft (MSFT)</td><td>Purchase</td><td>$1,001 - $15,000</td></tr></table>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewSenateClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, documentURL := range []string{
		"https://untrusted.example/search/view/ptr/abc/",
		server.URL + "/search/view/ptr/redirect/",
	} {
		_, err := client.FetchPTR(context.Background(), SenateFiling{
			ExternalID: "fixture", FilerName: "Fixture", FilingDate: time.Now(), DocumentURL: documentURL,
		})
		if err == nil || !strings.Contains(err.Error(), "cross-origin") {
			t.Fatalf("FetchPTR(%q) error = %v, want cross-origin rejection", documentURL, err)
		}
	}
	if _, err := client.FetchPTR(context.Background(), SenateFiling{
		ExternalID: "ok", FilerName: "Fixture", FilingDate: time.Now(), DocumentURL: server.URL + "/search/view/ptr/ok/",
	}); err != nil {
		t.Fatalf("FetchPTR trusted document: %v", err)
	}
}

func TestSenateFetchPTRRejectsOversizedHTTPResponse(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			ContentLength: maxSenatePTRResponseSize + 1,
			Request:       req,
			Header:        make(http.Header),
			Body:          io.NopCloser(strings.NewReader("not read")),
		}, nil
	})
	_, err = client.FetchPTR(context.Background(), SenateFiling{
		ExternalID: "oversized", FilerName: "Fixture", FilingDate: time.Now(),
		DocumentURL: "https://efdsearch.senate.gov/search/view/ptr/oversized/",
	})
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("FetchPTR error = %v, want size rejection", err)
	}
}

func TestReadBoundedSenateResponseAcceptsExactLimit(t *testing.T) {
	body := strings.Repeat("x", maxSenatePTRResponseSize)
	got, err := readBoundedSenateResponse(strings.NewReader(body), -1, maxSenatePTRResponseSize, "PTR")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxSenatePTRResponseSize {
		t.Fatalf("body length = %d, want %d", len(got), maxSenatePTRResponseSize)
	}
	_, err = readBoundedSenateResponse(strings.NewReader(body+"x"), -1, maxSenatePTRResponseSize, "PTR")
	if err == nil || !strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("limit+1 error = %v, want size rejection", err)
	}
}

func TestParseSenateSearchRowRejectsCrossOriginDocumentURL(t *testing.T) {
	filing, ok := parseSenateSearchRow("https://efdsearch.senate.gov", []string{
		"Jane", "Senator", "Office",
		`<a href="https://evil.example/search/view/ptr/abc/">PTR</a>`, "07/05/2026",
	})
	if ok || filing.DocumentURL != "" {
		t.Fatalf("cross-origin filing accepted: %+v", filing)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(req *http.Request, status int, body string, cookie *http.Cookie) *http.Response {
	resp := &http.Response{
		StatusCode: status, Status: http.StatusText(status), Request: req,
		Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)),
	}
	if cookie != nil {
		resp.Header.Add("Set-Cookie", cookie.String())
	}
	return resp
}
