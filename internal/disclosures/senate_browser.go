package disclosures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

var ErrSenateAuthorizationRequired = errors.New(
	"Senate eFD browser authorization is required; run `te disclosures senate authorize`",
)

type SenateBrowserOptions struct {
	BaseURL        string
	ProfileDir     string
	ExecutablePath string
	Headless       bool
	Background     bool
	Timeout        time.Duration
}

// SenateChromeTransport keeps eFD requests inside Chrome. This preserves the
// browser's cookies, TLS behavior, and challenge state instead of copying
// credentials into Go's HTTP stack.
type SenateChromeTransport struct {
	opts SenateBrowserOptions

	mu               sync.Mutex
	allocatorContext context.Context
	allocatorCancel  context.CancelFunc
	browserContext   context.Context
	browserCancel    context.CancelFunc
}

type senateBrowserPageState struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	BodyPreview string `json:"bodyPreview"`
	HasCSRF     bool   `json:"hasCSRF"`
}

type senateBrowserResponse struct {
	Status int    `json:"status"`
	URL    string `json:"url"`
	Body   string `json:"body"`
}

type senateStoredCookie struct {
	Name     string                 `json:"name"`
	Value    string                 `json:"value"`
	Domain   string                 `json:"domain"`
	Path     string                 `json:"path"`
	Secure   bool                   `json:"secure"`
	HTTPOnly bool                   `json:"http_only"`
	SameSite network.CookieSameSite `json:"same_site,omitempty"`
}

type senateBrowserSessionState struct {
	Version int                  `json:"version"`
	Cookies []senateStoredCookie `json:"cookies"`
}

func DefaultSenateBrowserProfileDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".tradeengine", "browser", "senate-efd"), nil
}

func NewSenateChromeTransport(opts SenateBrowserOptions) (*SenateChromeTransport, error) {
	opts.BaseURL = strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid Senate browser base URL %q", opts.BaseURL)
	}
	if strings.TrimSpace(opts.ProfileDir) == "" {
		opts.ProfileDir, err = DefaultSenateBrowserProfileDir()
		if err != nil {
			return nil, err
		}
	} else {
		opts.ProfileDir, err = expandSenateProfilePath(opts.ProfileDir)
		if err != nil {
			return nil, err
		}
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	return &SenateChromeTransport{opts: opts}, nil
}

func (t *SenateChromeTransport) ProfileDir() string { return t.opts.ProfileDir }

func (t *SenateChromeTransport) SearchPTRs(
	ctx context.Context, since, until time.Time, maxReports int,
) ([]byte, error) {
	if maxReports <= 0 {
		return nil, fmt.Errorf("Senate browser report limit must be positive")
	}
	form := url.Values{
		"start":                {"0"},
		"length":               {strconv.Itoa(maxReports)},
		"report_types":         {"[11]"},
		"filer_types":          {"[]"},
		"submitted_start_date": {since.Format("01/02/2006") + " 00:00:00"},
		"submitted_end_date":   {until.Format("01/02/2006") + " 23:59:59"},
		"candidate_state":      {""},
		"senator_state":        {""},
		"office_id":            {""},
		"first_name":           {""},
		"last_name":            {""},
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.startLocked(); err != nil {
		return nil, err
	}
	if _, err := t.ensureAuthorizedLocked(ctx); err != nil {
		if errors.Is(err, ErrSenateAuthorizationRequired) {
			_ = t.stopLocked()
		}
		return nil, err
	}
	encodedForm, err := json.Marshal(form.Encode())
	if err != nil {
		return nil, err
	}
	expression := fmt.Sprintf(`(async () => {
		const csrfMatch = document.cookie.match(/(?:^|;\s*)(?:csrftoken|csrf)=([^;]+)/i);
		if (!csrfMatch) return {status: 0, url: location.href, body: "missing csrf cookie"};
		const body = %s + "&csrfmiddlewaretoken=" + encodeURIComponent(decodeURIComponent(csrfMatch[1]));
		const response = await fetch("/search/report/data/", {
			method: "POST", credentials: "same-origin", redirect: "follow",
			headers: {
				"Accept": "application/json, text/javascript, */*; q=0.01",
				"Content-Type": "application/x-www-form-urlencoded",
				"X-CSRFToken": decodeURIComponent(csrfMatch[1]),
				"X-Requested-With": "XMLHttpRequest"
			},
			body
		});
		const text = await response.text();
		return {status: response.status, url: response.url, body: text.slice(0, 10485760)};
	})()`, string(encodedForm))
	var response senateBrowserResponse
	if err := t.runLocked(ctx, chromedp.Evaluate(
		expression, &response, awaitSenateBrowserPromise,
	)); err != nil {
		return nil, fmt.Errorf("search Senate eFD in Chrome: %w", err)
	}
	if err := validateSenateBrowserResponse(response, "/search/report/data/"); err != nil {
		return nil, err
	}
	if err := t.saveSessionLocked(ctx); err != nil {
		return nil, err
	}
	return []byte(response.Body), nil
}

func (t *SenateChromeTransport) FetchPTR(ctx context.Context, documentURL string) ([]byte, error) {
	document, err := url.Parse(documentURL)
	if err != nil {
		return nil, fmt.Errorf("parse Senate PTR URL: %w", err)
	}
	base, _ := url.Parse(t.opts.BaseURL)
	if !strings.EqualFold(document.Scheme, base.Scheme) ||
		!strings.EqualFold(document.Host, base.Host) ||
		!strings.HasPrefix(document.Path, "/search/view/ptr/") {
		return nil, fmt.Errorf("refusing non-eFD Senate PTR URL %q", documentURL)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.startLocked(); err != nil {
		return nil, err
	}
	if _, err := t.ensureAuthorizedLocked(ctx); err != nil {
		if errors.Is(err, ErrSenateAuthorizationRequired) {
			_ = t.stopLocked()
		}
		return nil, err
	}
	encodedURL, err := json.Marshal(document.String())
	if err != nil {
		return nil, err
	}
	expression := fmt.Sprintf(`(async () => {
		const response = await fetch(%s, {credentials: "same-origin", redirect: "follow"});
		const text = await response.text();
		return {status: response.status, url: response.url, body: text.slice(0, 10485760)};
	})()`, string(encodedURL))
	var response senateBrowserResponse
	if err := t.runLocked(ctx, chromedp.Evaluate(
		expression, &response, awaitSenateBrowserPromise,
	)); err != nil {
		return nil, fmt.Errorf("fetch Senate PTR in Chrome: %w", err)
	}
	if err := validateSenateBrowserResponse(response, document.Path); err != nil {
		return nil, err
	}
	if err := t.saveSessionLocked(ctx); err != nil {
		return nil, err
	}
	return []byte(response.Body), nil
}

// AuthorizationStatus checks whether the persistent profile reaches the eFD
// search page with a CSRF cookie. It does not extract any cookie value.
func (t *SenateChromeTransport) AuthorizationStatus(ctx context.Context) (bool, error) {
	if _, err := os.Stat(t.opts.ProfileDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.startLocked(); err != nil {
		return false, err
	}
	_, err := t.ensureAuthorizedLocked(ctx)
	if errors.Is(err, ErrSenateAuthorizationRequired) {
		_ = t.stopLocked()
		return false, err
	}
	return err == nil, err
}

// Authorize opens a visible dedicated Chrome profile and waits for the user to
// accept the eFD agreement. The profile itself owns all resulting credentials.
func (t *SenateChromeTransport) Authorize(ctx context.Context) error {
	if t.opts.Headless {
		return fmt.Errorf("Senate authorization requires visible Chrome")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.startLocked(); err != nil {
		return err
	}
	if err := t.runLocked(ctx, chromedp.Navigate(t.opts.BaseURL+"/search/home/")); err != nil {
		return fmt.Errorf("open Senate eFD authorization page: %w", err)
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := t.pageStateLocked(ctx)
		if err == nil && senatePageAuthorized(state) {
			return t.saveSessionLocked(ctx)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for Senate eFD authorization: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (t *SenateChromeTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopLocked()
}

func (t *SenateChromeTransport) stopLocked() error {
	var closeErr error
	if t.browserContext != nil {
		closeErr = chromedp.Cancel(t.browserContext)
	}
	if t.browserCancel != nil {
		t.browserCancel()
	}
	if t.allocatorCancel != nil {
		t.allocatorCancel()
	}
	t.allocatorContext, t.allocatorCancel = nil, nil
	t.browserContext, t.browserCancel = nil, nil
	return closeErr
}

func (t *SenateChromeTransport) startLocked() error {
	if t.browserContext != nil {
		return nil
	}
	if err := os.MkdirAll(t.opts.ProfileDir, 0o700); err != nil {
		return fmt.Errorf("create Senate Chrome profile: %w", err)
	}
	if err := os.Chmod(t.opts.ProfileDir, 0o700); err != nil {
		return fmt.Errorf("secure Senate Chrome profile: %w", err)
	}
	allocatorOptions := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(t.opts.ProfileDir),
		chromedp.WSURLReadTimeout(t.opts.Timeout),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-background-networking", true),
	}
	if t.opts.Headless {
		allocatorOptions = append(allocatorOptions,
			chromedp.Flag("headless", "new"),
			chromedp.Flag("window-size", "1280,900"),
		)
	} else if t.opts.Background {
		// Senate's edge currently rejects Chrome's headless fingerprint. Keep a
		// genuine headed window out of the operator's workspace during sync.
		allocatorOptions = append(allocatorOptions,
			chromedp.Flag("window-position", "-32000,-32000"),
			chromedp.Flag("window-size", "1,1"),
		)
	}
	if strings.TrimSpace(t.opts.ExecutablePath) != "" {
		allocatorOptions = append(allocatorOptions, chromedp.ExecPath(t.opts.ExecutablePath))
	}
	t.allocatorContext, t.allocatorCancel = chromedp.NewExecAllocator(
		context.Background(), allocatorOptions...,
	)
	t.browserContext, t.browserCancel = chromedp.NewContext(t.allocatorContext)
	// The first Run allocates Chrome with this context. It must use the
	// persistent browser context; a short-lived child would own and terminate
	// the Chrome process when its request timeout is cancelled.
	if err := chromedp.Run(t.browserContext); err != nil {
		t.browserCancel()
		t.allocatorCancel()
		t.allocatorContext, t.allocatorCancel = nil, nil
		t.browserContext, t.browserCancel = nil, nil
		return fmt.Errorf("start Senate Chrome profile: %w", err)
	}
	if err := t.restoreSessionLocked(); err != nil {
		_ = chromedp.Cancel(t.browserContext)
		t.browserCancel()
		t.allocatorCancel()
		t.allocatorContext, t.allocatorCancel = nil, nil
		t.browserContext, t.browserCancel = nil, nil
		return err
	}
	return nil
}

func (t *SenateChromeTransport) sessionPath() string {
	return filepath.Join(t.opts.ProfileDir, ".tradeengine-senate-session.json")
}

func (t *SenateChromeTransport) restoreSessionLocked() error {
	state, err := readSenateBrowserSessionState(t.sessionPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cookies := make([]*network.CookieParam, 0, len(state.Cookies))
	base, _ := url.Parse(t.opts.BaseURL)
	for _, cookie := range state.Cookies {
		if !senateCookieDomainAllowed(base.Hostname(), cookie.Domain) ||
			cookie.Name == "" || strings.ContainsAny(cookie.Name+cookie.Value, "\r\n") {
			continue
		}
		path := cookie.Path
		if path == "" {
			path = "/"
		}
		cookies = append(cookies, &network.CookieParam{
			Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain,
			Path: path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly,
			SameSite: cookie.SameSite,
		})
	}
	if len(cookies) == 0 {
		return nil
	}
	if err := t.runLocked(context.Background(), chromedp.ActionFunc(func(ctx context.Context) error {
		return network.SetCookies(cookies).Do(ctx)
	})); err != nil {
		return fmt.Errorf("restore Senate Chrome session: %w", err)
	}
	return nil
}

func (t *SenateChromeTransport) saveSessionLocked(ctx context.Context) error {
	var browserCookies []*network.Cookie
	if err := t.runLocked(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		browserCookies, err = network.GetCookies().WithURLs([]string{
			t.opts.BaseURL + "/search/",
		}).Do(ctx)
		return err
	})); err != nil {
		return fmt.Errorf("read Senate Chrome session: %w", err)
	}
	base, _ := url.Parse(t.opts.BaseURL)
	state := senateBrowserSessionState{Version: 1}
	for _, cookie := range browserCookies {
		if cookie == nil || !senateCookieDomainAllowed(base.Hostname(), cookie.Domain) {
			continue
		}
		state.Cookies = append(state.Cookies, senateStoredCookie{
			Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain,
			Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly,
			SameSite: cookie.SameSite,
		})
	}
	if len(state.Cookies) == 0 {
		return fmt.Errorf("Senate Chrome profile contains no eFD cookies")
	}
	return writeSenateBrowserSessionState(t.sessionPath(), state)
}

func (t *SenateChromeTransport) ensureAuthorizedLocked(
	ctx context.Context,
) (senateBrowserPageState, error) {
	if err := t.runLocked(ctx, chromedp.Navigate(t.opts.BaseURL+"/search/")); err != nil {
		return senateBrowserPageState{}, fmt.Errorf("open Senate eFD search in Chrome: %w", err)
	}
	state, err := t.pageStateLocked(ctx)
	if err != nil {
		return state, err
	}
	if !senatePageAuthorized(state) {
		pageURL, _ := url.Parse(state.URL)
		return state, fmt.Errorf(
			"%w (profile: %s; landing_path: %s; title: %q; csrf_cookie: %t)",
			ErrSenateAuthorizationRequired, t.opts.ProfileDir, pageURL.Path,
			state.Title, state.HasCSRF,
		)
	}
	return state, nil
}

func (t *SenateChromeTransport) pageStateLocked(ctx context.Context) (senateBrowserPageState, error) {
	var state senateBrowserPageState
	const expression = `(() => ({
		url: location.href,
		title: document.title || "",
		bodyPreview: (document.body && document.body.innerText || "").slice(0, 1000),
		hasCSRF: /(?:^|;\s*)(?:csrftoken|csrf)=/i.test(document.cookie)
	}))()`
	if err := t.runLocked(ctx, chromedp.Evaluate(expression, &state)); err != nil {
		return state, fmt.Errorf("inspect Senate eFD browser page: %w", err)
	}
	return state, nil
}

func (t *SenateChromeTransport) runLocked(ctx context.Context, actions ...chromedp.Action) error {
	taskContext, cancel := context.WithTimeout(t.browserContext, t.opts.Timeout)
	stop := context.AfterFunc(ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	return chromedp.Run(taskContext, actions...)
}

func senatePageAuthorized(state senateBrowserPageState) bool {
	pageURL, err := url.Parse(state.URL)
	if err != nil || !strings.HasPrefix(pageURL.Path, "/search/") ||
		pageURL.Path == "/search/home/" || !state.HasCSRF {
		return false
	}
	preview := strings.ToLower(state.Title + " " + state.BodyPreview)
	return !strings.Contains(preview, "access denied") &&
		!strings.Contains(preview, "reference #")
}

func awaitSenateBrowserPromise(params *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
	return params.WithAwaitPromise(true)
}

func validateSenateBrowserResponse(response senateBrowserResponse, expectedPath string) error {
	responseURL, err := url.Parse(response.URL)
	if err != nil {
		return fmt.Errorf("Senate eFD browser returned an invalid URL")
	}
	if responseURL.Path == "/search/home/" {
		return ErrSenateAuthorizationRequired
	}
	if response.Status == 0 {
		reason := strings.TrimSpace(response.Body)
		if reason == "" {
			reason = "browser script returned no response"
		}
		return fmt.Errorf("Senate eFD browser request %s: %s (url: %s)",
			expectedPath, reason, response.URL)
	}
	if response.Status < 200 || response.Status >= 300 {
		return fmt.Errorf("Senate eFD browser request %s: HTTP %d", expectedPath, response.Status)
	}
	if expectedPath != "/search/report/data/" && responseURL.Path != expectedPath {
		return fmt.Errorf("Senate eFD browser redirected %s to %s", expectedPath, responseURL.Path)
	}
	return nil
}

func expandSenateProfilePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve Senate Chrome profile: %w", err)
	}
	return filepath.Clean(abs), nil
}

func senateCookieDomainAllowed(host, domain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	return domain == host || (host == "efdsearch.senate.gov" && domain == "senate.gov")
}

func readSenateBrowserSessionState(path string) (senateBrowserSessionState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return senateBrowserSessionState{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return senateBrowserSessionState{}, fmt.Errorf(
			"Senate browser session file permissions must be 0600: %s", path,
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return senateBrowserSessionState{}, err
	}
	var state senateBrowserSessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode Senate browser session state: %w", err)
	}
	if state.Version != 1 {
		return state, fmt.Errorf("unsupported Senate browser session state version %d", state.Version)
	}
	return state, nil
}

func writeSenateBrowserSessionState(path string, state senateBrowserSessionState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode Senate browser session state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".senate-session-*")
	if err != nil {
		return fmt.Errorf("create Senate browser session state: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace Senate browser session state: %w", err)
	}
	return nil
}

func RemoveSenateBrowserProfile(path string) error {
	resolved, err := expandSenateProfilePath(path)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	if resolved == string(filepath.Separator) || resolved == "." || resolved == filepath.Clean(home) {
		return fmt.Errorf("refusing unsafe Senate Chrome profile path %q", resolved)
	}
	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("remove Senate Chrome profile: %w", err)
	}
	return nil
}
