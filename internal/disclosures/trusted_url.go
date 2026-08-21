package disclosures

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const maxTrustedDocumentRedirects = 10

// trustedDocumentURL only accepts documents served from the configured source
// origin and expected path. This keeps listing data from becoming an SSRF
// vector and, for Senate's HTTP fallback, keeps imported browser cookies on
// the official eFD origin.
func trustedDocumentURL(baseURL, rawURL, pathPrefix string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil {
		return nil, permanentDisclosureError(fmt.Errorf("invalid disclosure base URL %q", baseURL))
	}
	document, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || document.Scheme == "" || document.Host == "" || document.User != nil {
		return nil, permanentDisclosureError(fmt.Errorf("invalid disclosure document URL %q", rawURL))
	}
	if (document.Scheme != "https" && document.Scheme != "http") ||
		document.Scheme != base.Scheme || !sameURLHost(document, base) {
		return nil, permanentDisclosureError(fmt.Errorf("refusing cross-origin disclosure document URL %q", rawURL))
	}
	escapedPath := strings.ToLower(document.EscapedPath())
	cleanPath := path.Clean(document.Path)
	if strings.HasSuffix(document.Path, "/") && cleanPath != "/" {
		cleanPath += "/"
	}
	if strings.Contains(escapedPath, "%2f") || strings.Contains(escapedPath, "%5c") ||
		strings.Contains(escapedPath, "%2e") || cleanPath != document.Path {
		return nil, permanentDisclosureError(fmt.Errorf("refusing non-clean disclosure document URL %q", rawURL))
	}
	if !strings.HasPrefix(document.Path, pathPrefix) {
		return nil, permanentDisclosureError(fmt.Errorf("refusing disclosure document outside %q: %q", pathPrefix, rawURL))
	}
	return document, nil
}

func sameURLHost(left, right *url.URL) bool {
	return strings.EqualFold(left.Hostname(), right.Hostname()) && left.Port() == right.Port()
}

func sameOriginDocumentClient(client *http.Client, baseURL, pathPrefix string) *http.Client {
	clone := *client
	previous := client.CheckRedirect
	clone.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxTrustedDocumentRedirects {
			return permanentDisclosureError(errors.New("too many disclosure document redirects"))
		}
		_, err := trustedDocumentURL(baseURL, request.URL.String(), pathPrefix)
		if err != nil {
			return err
		}
		if previous != nil {
			return previous(request, via)
		}
		return nil
	}
	return &clone
}

func ogeDocumentPathPrefix(baseURL string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "/"
	}
	parts := strings.Split(strings.Trim(base.EscapedPath(), "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "/"
	}
	return "/" + parts[0] + "/" + parts[1] + "/"
}
