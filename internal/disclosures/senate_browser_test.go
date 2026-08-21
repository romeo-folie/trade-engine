package disclosures

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/dop251/goja"
)

func TestBuildSenateSearchPTRExpressionEscapesFormAndParses(t *testing.T) {
	form := url.Values{
		"first_name": {`Renée "Zoë" \`},
		"last_name":  {"李 & O'Neil"},
		"start":      {"0"},
	}
	const encodedForm = "first_name=Ren%C3%A9e+%22Zo%C3%AB%22+%5C&last_name=%E6%9D%8E+%26+O%27Neil&start=0"
	if got := form.Encode(); got != encodedForm {
		t.Fatalf("encoded form = %q, want %q", got, encodedForm)
	}

	got, err := buildSenateSearchPTRExpression(form, 123)
	if err != nil {
		t.Fatal(err)
	}
	const want = `(async () => {
		const csrfMatch = document.cookie.match(/(?:^|;\s*)(?:csrftoken|csrf)=([^;]+)/i);
		if (!csrfMatch) return {status: 0, url: location.href, body: "missing csrf cookie"};
		const requestBody = "first_name=Ren%C3%A9e+%22Zo%C3%AB%22+%5C\u0026last_name=%E6%9D%8E+%26+O%27Neil\u0026start=0" + "&csrfmiddlewaretoken=" + encodeURIComponent(decodeURIComponent(csrfMatch[1]));
		const response = await fetch("/search/report/data/", {
			method: "POST", credentials: "same-origin", redirect: "follow",
			headers: {
				"Accept": "application/json, text/javascript, */*; q=0.01",
				"Content-Type": "application/x-www-form-urlencoded",
				"X-CSRFToken": decodeURIComponent(csrfMatch[1]),
				"X-Requested-With": "XMLHttpRequest"
			},
			body: requestBody
		});
		const limit = 123;
		const reader = response.body ? response.body.getReader() : null;
		if (!reader) {
			const text = await response.text();
			const bodyBytes = new TextEncoder().encode(text).byteLength;
			const oversized = bodyBytes > limit;
			return {status: response.status, url: response.url, body: oversized ? "" : text, bodyBytes, oversized};
		}
		const chunks = [];
		let bodyBytes = 0;
		let oversized = false;
		while (true) {
			const {done, value} = await reader.read();
			if (done) break;
			bodyBytes += value.byteLength;
			if (bodyBytes > limit) {
				oversized = true;
				await reader.cancel();
				break;
			}
			chunks.push(value);
		}
		const responseBody = oversized ? "" : new TextDecoder().decode(
			chunks.length ? await new Blob(chunks).arrayBuffer() : new ArrayBuffer(0)
		);
		return {status: response.status, url: response.url, body: responseBody, bodyBytes, oversized};
	})()`
	if got != want {
		t.Fatalf("generated Senate search expression changed unexpectedly:\n%s", got)
	}
	if _, err := goja.Parse("senate-search-ptr.js", got); err != nil {
		t.Fatalf("generated Senate search expression does not parse: %v", err)
	}
}

func TestSenatePageAuthorized(t *testing.T) {
	tests := []struct {
		name  string
		state senateBrowserPageState
		want  bool
	}{
		{
			name: "search page",
			state: senateBrowserPageState{
				URL: "https://efdsearch.senate.gov/search/", HasCSRF: true,
			},
			want: true,
		},
		{
			name: "agreement page",
			state: senateBrowserPageState{
				URL: "https://efdsearch.senate.gov/search/home/", HasCSRF: true,
			},
		},
		{
			name: "access denied",
			state: senateBrowserPageState{
				URL: "https://efdsearch.senate.gov/search/", HasCSRF: true,
				BodyPreview: "Access Denied Reference #123",
			},
		},
		{
			name: "wrong path",
			state: senateBrowserPageState{
				URL: "https://efdsearch.senate.gov/other/", HasCSRF: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := senatePageAuthorized(test.state); got != test.want {
				t.Fatalf("authorized = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateSenateBrowserResponse(t *testing.T) {
	if err := validateSenateBrowserResponse(senateBrowserResponse{
		Status: 200,
		URL:    "https://efdsearch.senate.gov/search/view/ptr/report-id/",
	}, "https://efdsearch.senate.gov", "/search/view/ptr/report-id/", maxSenatePTRResponseSize); err != nil {
		t.Fatal(err)
	}
	if err := validateSenateBrowserResponse(senateBrowserResponse{
		Status: 200,
		URL:    "https://efdsearch.senate.gov/search/home/",
	}, "https://efdsearch.senate.gov", "/search/report/data/", maxSenateSearchResponseSize); !errors.Is(err, ErrSenateAuthorizationRequired) {
		t.Fatalf("error = %v", err)
	}
	for _, test := range []struct {
		status int
		retry  bool
	}{
		{status: http.StatusBadRequest, retry: false},
		{status: http.StatusNotFound, retry: false},
		{status: http.StatusRequestTimeout, retry: true},
		{status: http.StatusTooManyRequests, retry: true},
		{status: http.StatusInternalServerError, retry: true},
		{status: http.StatusServiceUnavailable, retry: true},
		{status: 599, retry: true},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			err := validateSenateBrowserResponse(senateBrowserResponse{
				Status: test.status,
				URL:    "https://efdsearch.senate.gov/search/report/data/",
			}, "https://efdsearch.senate.gov", "/search/report/data/", maxSenateSearchResponseSize)
			var statusErr *disclosureHTTPError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != test.status {
				t.Fatalf("error = %T %v, want typed HTTP status %d", err, err, test.status)
			}
			if got := shouldRetryDisclosureFailure(err); got != test.retry {
				t.Fatalf("retryable = %v, want %v for HTTP %d", got, test.retry, test.status)
			}
			if test.status == 599 && statusErr.Status != "599" {
				t.Fatalf("unknown HTTP status = %q, want numeric status without trailing space", statusErr.Status)
			}
		})
	}
}

func TestValidateSenateBrowserResponseRejectsOversizedBody(t *testing.T) {
	base := senateBrowserResponse{
		Status:    http.StatusOK,
		URL:       "https://efdsearch.senate.gov/search/view/ptr/report-id/",
		BodyBytes: maxSenatePTRResponseSize,
	}
	if err := validateSenateBrowserResponse(base, "https://efdsearch.senate.gov", "/search/view/ptr/report-id/", maxSenatePTRResponseSize); err != nil {
		t.Fatalf("exact limit error = %v", err)
	}

	base.BodyBytes++
	base.Oversized = true
	if err := validateSenateBrowserResponse(base, "https://efdsearch.senate.gov", "/search/view/ptr/report-id/", maxSenatePTRResponseSize); err == nil ||
		!strings.Contains(err.Error(), "response exceeds") {
		t.Fatalf("oversized response error = %v, want size rejection", err)
	}
}

func TestValidateSenateBrowserResponseUsesResourceSpecificLimit(t *testing.T) {
	response := senateBrowserResponse{Status: http.StatusOK, URL: "https://efdsearch.senate.gov/search/report/data/", BodyBytes: 11}
	if err := validateSenateBrowserResponse(response, "https://efdsearch.senate.gov", "/search/report/data/", 10); err == nil || !strings.Contains(err.Error(), "10-byte") {
		t.Fatalf("resource-specific limit error=%v", err)
	}
}

func TestValidateSenateBrowserResponseTrustsOnlyExpectedSameOriginPath(t *testing.T) {
	base := "https://efdsearch.senate.gov"
	for _, responseURL := range []string{
		"https://evil.example/search/report/data/",
		"http://efdsearch.senate.gov/search/report/data/",
		"https://efdsearch.senate.gov:444/search/report/data/",
		"https://user@efdsearch.senate.gov/search/report/data/",
		"https://efdsearch.senate.gov/search/%2freport/data/",
		"https://efdsearch.senate.gov/search/../search/report/data/",
		"https://efdsearch.senate.gov/search/other/",
	} {
		err := validateSenateBrowserResponse(senateBrowserResponse{Status: http.StatusOK, URL: responseURL}, base, "/search/report/data/", maxSenateSearchResponseSize)
		if err == nil || errors.Is(err, ErrSenateAuthorizationRequired) {
			t.Fatalf("validate(%q) error = %v, want trust/path rejection", responseURL, err)
		}
	}

	if err := validateSenateBrowserResponse(senateBrowserResponse{Status: http.StatusOK, URL: "https://evil.example/search/home/"}, base, "/search/report/data/", maxSenateSearchResponseSize); err == nil || errors.Is(err, ErrSenateAuthorizationRequired) {
		t.Fatalf("cross-origin home error = %v, want trust rejection", err)
	}
	if err := validateSenateBrowserResponse(senateBrowserResponse{Status: http.StatusOK, URL: base + "/search/home/"}, base, "/search/report/data/", maxSenateSearchResponseSize); !errors.Is(err, ErrSenateAuthorizationRequired) {
		t.Fatalf("same-origin home error = %v, want authorization required", err)
	}
}

func TestSenateBrowserProfileLifecycle(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "senate-profile")
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{
		BaseURL: "https://efdsearch.senate.gov", ProfileDir: profile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if browser.ProfileDir() != profile {
		t.Fatalf("profile = %q", browser.ProfileDir())
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, ".tradeengine-senate-profile"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSenateBrowserProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile still exists: %v", err)
	}
}

func TestSenateBrowserStartupIsBoundedByRequestContext(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "senate-profile")
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{
		BaseURL: "https://efdsearch.senate.gov", ProfileDir: profile,
		Timeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A startLockedWithContext call is not practical without a browser, but the
	// context wiring must cancel the startup context when the request cancels.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = browser.startLockedWithContext(ctx)
	if err == nil {
		t.Fatal("startup with canceled request context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error = %v, want context.Canceled", err)
	}
}

func TestSenateBrowserStartupUsesPersistentContext(t *testing.T) {
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{
		BaseURL: "https://efdsearch.senate.gov", ProfileDir: filepath.Join(t.TempDir(), "profile"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var startupContext context.Context
	gracefulCancels := 0
	browser.runBrowser = func(ctx context.Context, _ ...chromedp.Action) error {
		startupContext = ctx
		return nil
	}
	browser.gracefulCancel = func(context.Context) error {
		gracefulCancels++
		return nil
	}
	request, cancel := context.WithCancel(context.Background())
	if err := browser.startLockedWithContext(request); err != nil {
		t.Fatal(err)
	}
	cancel()
	if startupContext != browser.browserContext {
		t.Fatal("Chrome startup did not use the persistent browser context")
	}
	if err := browser.browserContext.Err(); err != nil {
		t.Fatalf("request cancellation stopped persistent browser: %v", err)
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	if gracefulCancels != 1 {
		t.Fatalf("graceful close calls = %d, want 1", gracefulCancels)
	}
	if err := browser.Close(); err != nil {
		t.Fatal(err)
	}
	if gracefulCancels != 1 {
		t.Fatalf("repeated graceful close calls = %d, want 1", gracefulCancels)
	}
}

func TestSenateBrowserCanceledOperationKeepsTransportRunning(t *testing.T) {
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{
		BaseURL: "https://efdsearch.senate.gov", ProfileDir: filepath.Join(t.TempDir(), "profile"),
	})
	if err != nil {
		t.Fatal(err)
	}
	browser.browserContext, browser.browserCancel = context.WithCancel(context.Background())
	defer browser.Close()
	browser.runBrowser = func(ctx context.Context, _ ...chromedp.Action) error {
		<-ctx.Done()
		return ctx.Err()
	}
	request, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- browser.runLocked(request) }()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled browser operation error = %v", err)
	}
	if err := browser.browserContext.Err(); err != nil {
		t.Fatalf("canceled operation stopped transport: %v", err)
	}
}

func TestSenateBrowserAbortedStartupCleansUp(t *testing.T) {
	for _, test := range []struct {
		name             string
		timeout          time.Duration
		cancelRequest    bool
		wantCancellation error
	}{
		{name: "request cancellation", timeout: time.Second, cancelRequest: true, wantCancellation: context.Canceled},
		{name: "startup timeout", timeout: 10 * time.Millisecond, wantCancellation: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			browser, err := NewSenateChromeTransport(SenateBrowserOptions{
				BaseURL: "https://efdsearch.senate.gov", ProfileDir: filepath.Join(t.TempDir(), "profile"),
				Timeout: test.timeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			var startupContext context.Context
			gracefulCancels := 0
			browser.gracefulCancel = func(context.Context) error {
				gracefulCancels++
				return nil
			}
			browser.runBrowser = func(ctx context.Context, _ ...chromedp.Action) error {
				startupContext = ctx
				close(started)
				<-ctx.Done()
				return ctx.Err()
			}
			request, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- browser.startLockedWithContext(request) }()
			<-started
			if test.cancelRequest {
				cancel()
			}
			err = <-result
			if !errors.Is(err, test.wantCancellation) {
				t.Fatalf("startup error = %v, want %v", err, test.wantCancellation)
			}
			if browser.allocatorContext != nil || browser.allocatorCancel != nil ||
				browser.browserContext != nil || browser.browserCancel != nil {
				t.Fatal("aborted startup left browser state behind")
			}
			if gracefulCancels != 0 {
				t.Fatalf("aborted startup graceful close calls = %d, want 0", gracefulCancels)
			}
			if !errors.Is(startupContext.Err(), context.Canceled) {
				t.Fatalf("aborted startup lifetime error = %v, want context.Canceled", startupContext.Err())
			}
		})
	}
}

func TestSenateBrowserRestoreFailureCleansUp(t *testing.T) {
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{
		BaseURL: "https://efdsearch.senate.gov", ProfileDir: filepath.Join(t.TempDir(), "profile"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(browser.ProfileDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browser.ProfileDir(), ".tradeengine-senate-profile"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSenateBrowserSessionState(browser.sessionPath(), senateBrowserSessionState{Version: 1, Cookies: []senateStoredCookie{{
		Name: "sessionid", Value: "value", Domain: "efdsearch.senate.gov", Path: "/",
	}}}); err != nil {
		t.Fatal(err)
	}
	restoreErr := errors.New("restore failed")
	calls := 0
	gracefulCancels := 0
	browser.gracefulCancel = func(context.Context) error {
		gracefulCancels++
		return nil
	}
	browser.runBrowser = func(context.Context, ...chromedp.Action) error {
		calls++
		if calls == 2 {
			return restoreErr
		}
		return nil
	}
	err = browser.startLockedWithContext(context.Background())
	if !errors.Is(err, restoreErr) {
		t.Fatalf("start error = %v, want restore failure", err)
	}
	if browser.allocatorContext != nil || browser.allocatorCancel != nil ||
		browser.browserContext != nil || browser.browserCancel != nil {
		t.Fatal("restore failure left browser state behind")
	}
	if gracefulCancels != 1 {
		t.Fatalf("restore failure graceful close calls = %d, want 1", gracefulCancels)
	}
}

func TestSenateBrowserRejectsUnmarkedExistingProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "existing-chrome-profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	personal := filepath.Join(profile, "Bookmarks")
	if err := os.WriteFile(personal, []byte("personal"), 0o600); err != nil {
		t.Fatal(err)
	}
	browser, err := NewSenateChromeTransport(SenateBrowserOptions{BaseURL: "https://efdsearch.senate.gov", ProfileDir: profile})
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.startLocked(); err == nil || !strings.Contains(err.Error(), "unmarked pre-existing") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(personal); err != nil {
		t.Fatalf("pre-existing profile was modified: %v", err)
	}
	if _, err := os.Stat(filepath.Join(profile, ".tradeengine-senate-profile")); !os.IsNotExist(err) {
		t.Fatalf("marker unexpectedly created: %v", err)
	}
}

func TestSenateBrowserSessionStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	want := senateBrowserSessionState{Version: 1, Cookies: []senateStoredCookie{{
		Name: "sessionid", Value: "secret-value", Domain: "efdsearch.senate.gov",
		Path: "/", Secure: true, HTTPOnly: true,
	}}}
	if err := writeSenateBrowserSessionState(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, err := readSenateBrowserSessionState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "secret-value" {
		t.Fatalf("state = %+v", got)
	}
}

func TestReadSenateBrowserSessionStateRejectsLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"cookies":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSenateBrowserSessionState(path); err == nil ||
		!strings.Contains(err.Error(), "0600") {
		t.Fatalf("error = %v", err)
	}
}

func TestSenateCookieDomainAllowed(t *testing.T) {
	if !senateCookieDomainAllowed("efdsearch.senate.gov", ".senate.gov") ||
		!senateCookieDomainAllowed("efdsearch.senate.gov", "efdsearch.senate.gov") ||
		senateCookieDomainAllowed("efdsearch.senate.gov", "example.com") {
		t.Fatal("unexpected Senate cookie domain decision")
	}
}

func TestRemoveSenateBrowserProfileRejectsRoot(t *testing.T) {
	if err := RemoveSenateBrowserProfile(string(filepath.Separator)); err == nil {
		t.Fatal("root profile path was accepted")
	}
}
