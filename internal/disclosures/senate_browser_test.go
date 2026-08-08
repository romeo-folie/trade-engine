package disclosures

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	}, "/search/view/ptr/report-id/"); err != nil {
		t.Fatal(err)
	}
	if err := validateSenateBrowserResponse(senateBrowserResponse{
		Status: 200,
		URL:    "https://efdsearch.senate.gov/search/home/",
	}, "/search/report/data/"); !errors.Is(err, ErrSenateAuthorizationRequired) {
		t.Fatalf("error = %v", err)
	}
	if err := validateSenateBrowserResponse(senateBrowserResponse{
		Status: 403,
		URL:    "https://efdsearch.senate.gov/search/report/data/",
	}, "/search/report/data/"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v", err)
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
	if err := os.WriteFile(filepath.Join(profile, "marker"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSenateBrowserProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile still exists: %v", err)
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
