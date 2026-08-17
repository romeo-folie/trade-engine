package disclosures

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTrustedDocumentURLRejectsUnsafePathForms(t *testing.T) {
	base := "https://efdsearch.senate.gov"
	for _, rawURL := range []string{
		"https://user@efdsearch.senate.gov/search/view/ptr/abc/",
		"https://efdsearch.senate.gov/search/view/ptr/../home/",
		"https://efdsearch.senate.gov/search/view/ptr/%2e%2e/home/",
		"https://efdsearch.senate.gov/search/view/ptr%2f../home/",
		"https://efdsearch.senate.gov/search/view/ptr/%5c..%5chome/",
	} {
		if _, err := trustedDocumentURL(base, rawURL, "/search/view/ptr/"); err == nil {
			t.Fatalf("unsafe document URL accepted: %q", rawURL)
		}
	}
	if _, err := trustedDocumentURL(base, base+"/search/view/ptr/abc/", "/search/view/ptr/"); err != nil {
		t.Fatalf("trusted document rejected: %v", err)
	}
}

func TestSameOriginDocumentClientPreservesRedirectPolicyAndCapsHops(t *testing.T) {
	var redirects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loop" {
			http.Redirect(w, r, "/loop", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		redirects.Add(1)
		return errors.New("caller redirect policy")
	}}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sameOriginDocumentClient(client, server.URL, "/").Do(req)
	if err == nil || !strings.Contains(err.Error(), "caller redirect policy") || redirects.Load() != 1 {
		t.Fatalf("caller redirect policy was not preserved: err=%v redirects=%d", err, redirects.Load())
	}
	loopReq, err := http.NewRequest(http.MethodGet, server.URL+"/loop", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sameOriginDocumentClient(&http.Client{}, server.URL, "/").Do(loopReq)
	if err == nil || !strings.Contains(err.Error(), "too many disclosure document redirects") {
		t.Fatalf("redirect hop cap error = %v", err)
	}
}
