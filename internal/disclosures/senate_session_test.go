package disclosures

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseSenateBrowserCookiesJSON(t *testing.T) {
	cookies, err := ParseSenateBrowserCookies([]byte(`[
		{"domain":".senate.gov","name":"csrftoken","value":"csrf-value","path":"/","secure":true},
		{"domain":"efdsearch.senate.gov","name":"sessionid","value":"session-value","path":"/search/","httpOnly":true},
		{"domain":"example.com","name":"ignored","value":"secret","path":"/"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 2 {
		t.Fatalf("cookies = %+v", cookies)
	}
	if cookies[0].Value != "csrf-value" || cookies[1].Value != "session-value" {
		t.Fatal("cookie values were not retained")
	}
}

func TestParseSenateBrowserCookiesNetscape(t *testing.T) {
	data := strings.Join([]string{
		"# Netscape HTTP Cookie File",
		".senate.gov\tTRUE\t/\tTRUE\t0\tcsrftoken\tcsrf-value",
		"#HttpOnly_efdsearch.senate.gov\tFALSE\t/search/\tTRUE\t0\tsessionid\tsession-value",
	}, "\n")
	cookies, err := ParseSenateBrowserCookies([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 2 || !cookies[1].HttpOnly {
		t.Fatalf("cookies = %+v", cookies)
	}
}

func TestParseSenateBrowserCookiesRequiresSessionAndCSRF(t *testing.T) {
	_, err := ParseSenateBrowserCookies([]byte(`[
		{"domain":"efdsearch.senate.gov","name":"csrftoken","value":"csrf-value","path":"/"}
	]`))
	if err == nil || !strings.Contains(err.Error(), "sessionid") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseSenateBrowserCopiedRequestHeaders(t *testing.T) {
	session, err := ParseSenateBrowserSession([]byte(strings.Join([]string{
		"accept: text/html",
		"cookie: ak_bmsc=edge-value; csrftoken=csrf-value; sessionid=session-value",
		"user-agent: Fixture Browser/2.0",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if session.UserAgent != "Fixture Browser/2.0" || len(session.Cookies) != 3 {
		t.Fatalf("session = %+v", session)
	}
}

func TestImportSenateBrowserSessionRejectsExpiredSessionCookie(t *testing.T) {
	client, err := NewSenateClient("https://efdsearch.senate.gov", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.ImportBrowserSession([]*http.Cookie{
		{Name: "csrftoken", Value: "csrf-value", Domain: "efdsearch.senate.gov", Path: "/", Secure: true},
		{Name: "sessionid", Value: "expired-value", Domain: "efdsearch.senate.gov", Path: "/", Secure: true, Expires: time.Now().Add(-time.Hour)},
	}, "Fixture Browser/2.0")
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %v", err)
	}
}
