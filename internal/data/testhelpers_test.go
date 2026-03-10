package data

import (
	"net/http"
	"strings"
)

// urlRewriteTransport rewrites all outbound requests so that any request
// to a specific host prefix is redirected to a test server URL.
// This allows testing clients that hardcode a base URL (Finnhub, Kalshi, etc.)
// by replacing their httpClient with a rewriting transport.
type urlRewriteTransport struct {
	// serverURL is the test server base (e.g. "http://127.0.0.1:PORT").
	serverURL string
	inner     http.RoundTripper
}

func (t *urlRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// Extract host from serverURL (strip scheme).
	host := t.serverURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	clone.URL.Scheme = "http"
	clone.URL.Host = host
	return t.inner.RoundTrip(clone)
}

// newHTTPClientWithTransport builds an *http.Client that redirects all requests
// to the given test server URL.
func newHTTPClientWithTransport(serverURL string) *http.Client {
	return &http.Client{
		Transport: &urlRewriteTransport{
			serverURL: serverURL,
			inner:     http.DefaultTransport,
		},
	}
}
