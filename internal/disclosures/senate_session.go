package disclosures

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type browserCookieRecord struct {
	Name           string  `json:"name"`
	Value          string  `json:"value"`
	Domain         string  `json:"domain"`
	Path           string  `json:"path"`
	Secure         bool    `json:"secure"`
	HTTPOnly       bool    `json:"httpOnly"`
	ExpirationDate float64 `json:"expirationDate"`
	Expires        float64 `json:"expires"`
}

type SenateBrowserSession struct {
	Cookies   []*http.Cookie
	UserAgent string
}

// ParseSenateBrowserCookies accepts either a Cookie-Editor/DevTools-style JSON
// export (an array or {"cookies": [...]}) or a Netscape cookie-jar file. Cookie
// values remain in memory and are never logged by the disclosure client.
func ParseSenateBrowserCookies(data []byte) ([]*http.Cookie, error) {
	session, err := ParseSenateBrowserSession(data)
	if err != nil {
		return nil, err
	}
	return session.Cookies, nil
}

// ParseSenateBrowserSession also accepts request headers copied from browser
// developer tools. That format carries both the Cookie and User-Agent headers,
// allowing the CLI to reproduce the authenticated request without echoing
// either value to logs or command-line history.
func ParseSenateBrowserSession(data []byte) (SenateBrowserSession, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return SenateBrowserSession{}, fmt.Errorf("senate browser session file is empty")
	}
	if trimmed[0] == '[' || trimmed[0] == '{' {
		cookies, err := parseSenateJSONCookies(trimmed)
		return SenateBrowserSession{Cookies: cookies}, err
	}
	if bytes.Contains(trimmed, []byte("\t")) {
		cookies, err := parseSenateNetscapeCookies(trimmed)
		return SenateBrowserSession{Cookies: cookies}, err
	}
	return parseSenateRequestHeaders(trimmed)
}

func parseSenateRequestHeaders(data []byte) (SenateBrowserSession, error) {
	var cookieHeader, userAgent string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "cookie":
			cookieHeader = strings.TrimSpace(value)
		case "user-agent":
			userAgent = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return SenateBrowserSession{}, err
	}
	if cookieHeader == "" {
		return SenateBrowserSession{}, fmt.Errorf("browser request headers contain no Cookie header")
	}
	var cookies []*http.Cookie
	for _, field := range strings.Split(cookieHeader, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			continue
		}
		cookie, accepted := senateBrowserCookie(
			name, value, "efdsearch.senate.gov", "/", true, false, 0,
		)
		if accepted {
			cookies = append(cookies, cookie)
		}
	}
	cookies, err := validateSenateBrowserCookies(cookies)
	if err != nil {
		return SenateBrowserSession{}, err
	}
	return SenateBrowserSession{Cookies: cookies, UserAgent: userAgent}, nil
}

func parseSenateJSONCookies(data []byte) ([]*http.Cookie, error) {
	var records []browserCookieRecord
	if data[0] == '[' {
		if err := json.Unmarshal(data, &records); err != nil {
			return nil, fmt.Errorf("parse Senate browser cookie JSON: %w", err)
		}
	} else {
		var envelope struct {
			Cookies []browserCookieRecord `json:"cookies"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, fmt.Errorf("parse Senate browser cookie JSON: %w", err)
		}
		records = envelope.Cookies
	}
	cookies := make([]*http.Cookie, 0, len(records))
	for _, record := range records {
		expires := record.ExpirationDate
		if expires == 0 {
			expires = record.Expires
		}
		cookie, ok := senateBrowserCookie(
			record.Name, record.Value, record.Domain, record.Path,
			record.Secure, record.HTTPOnly, int64(expires),
		)
		if ok {
			cookies = append(cookies, cookie)
		}
	}
	return validateSenateBrowserCookies(cookies)
}

func parseSenateNetscapeCookies(data []byte) ([]*http.Cookie, error) {
	var cookies []*http.Cookie
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		httpOnly := false
		if strings.HasPrefix(line, "#HttpOnly_") {
			httpOnly = true
			line = strings.TrimPrefix(line, "#HttpOnly_")
		} else if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 7 {
			return nil, fmt.Errorf("invalid Netscape cookie row with %d fields", len(fields))
		}
		expires, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Netscape cookie expiry: %w", err)
		}
		cookie, ok := senateBrowserCookie(
			fields[5], fields[6], fields[0], fields[2],
			strings.EqualFold(fields[3], "TRUE"), httpOnly, expires,
		)
		if ok {
			cookies = append(cookies, cookie)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return validateSenateBrowserCookies(cookies)
}

func senateBrowserCookie(
	name, value, domain, path string, secure, httpOnly bool, expires int64,
) (*http.Cookie, bool) {
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	if domain != "efdsearch.senate.gov" && domain != "senate.gov" {
		return nil, false
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name+value, "\r\n") {
		return nil, false
	}
	if path == "" {
		path = "/"
	}
	cookie := &http.Cookie{
		Name: name, Value: value, Domain: domain, Path: path,
		Secure: secure, HttpOnly: httpOnly,
	}
	if expires > 0 {
		cookie.Expires = time.Unix(expires, 0)
	}
	return cookie, true
}

func validateSenateBrowserCookies(cookies []*http.Cookie) ([]*http.Cookie, error) {
	if len(cookies) == 0 {
		return nil, fmt.Errorf("cookie file contained no Senate eFD cookies")
	}
	csrf, session := false, false
	for _, cookie := range cookies {
		switch strings.ToLower(cookie.Name) {
		case "csrftoken", "csrf":
			csrf = csrf || cookie.Value != ""
		case "sessionid":
			session = session || cookie.Value != ""
		}
	}
	if !csrf || !session {
		return nil, fmt.Errorf("senate cookie file must contain csrftoken and sessionid")
	}
	return cookies, nil
}

func (c *SenateClient) ImportBrowserSession(
	cookies []*http.Cookie, userAgent string,
) error {
	if c == nil || c.HTTP == nil || c.HTTP.Jar == nil {
		return fmt.Errorf("senate client cookie jar is unavailable")
	}
	if _, err := validateSenateBrowserCookies(cookies); err != nil {
		return err
	}
	if c.Transport != nil {
		if err := c.Transport.Close(); err != nil {
			return fmt.Errorf("close Senate browser transport: %w", err)
		}
		c.Transport = nil
	}
	endpoint, err := url.Parse(c.BaseURL + "/search/")
	if err != nil {
		return err
	}
	c.HTTP.Jar.SetCookies(endpoint, cookies)
	c.UserAgent = strings.TrimSpace(userAgent)
	c.UseImportedSession = true
	if c.csrfCookie() == "" || c.sessionCookie() == "" {
		return fmt.Errorf("senate browser cookies were rejected by the cookie jar")
	}
	return nil
}

func (c *SenateClient) sessionCookie() string {
	endpoint, err := url.Parse(c.BaseURL + "/search/")
	if err != nil || c.HTTP == nil || c.HTTP.Jar == nil {
		return ""
	}
	for _, cookie := range c.HTTP.Jar.Cookies(endpoint) {
		if strings.EqualFold(cookie.Name, "sessionid") {
			return cookie.Value
		}
	}
	return ""
}
