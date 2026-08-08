package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestFrontendHandlerServesPlaceholderWithoutBuild(t *testing.T) {
	root := fstest.MapFS{
		"dist/.gitkeep": &fstest.MapFile{},
	}

	recorder := httptest.NewRecorder()
	frontendHandler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !strings.Contains(recorder.Body.String(), "Frontend not built yet") {
		t.Fatalf("body does not contain the frontend build placeholder: %q", recorder.Body.String())
	}
}

func TestFrontendPlaceholderPreservesNonFrontend404s(t *testing.T) {
	root := fstest.MapFS{
		"dist/.gitkeep": &fstest.MapFile{},
	}
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/"},
		{method: http.MethodGet, path: "/api/typo"},
		{method: http.MethodGet, path: "/ws/typo"},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			frontendHandler(root).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d; body=%q",
					recorder.Code, http.StatusNotFound, recorder.Body.String())
			}
		})
	}
}

func TestFrontendHandlerServesBuiltIndex(t *testing.T) {
	root := fstest.MapFS{
		"dist/index.html": &fstest.MapFile{Data: []byte("<h1>built frontend</h1>")},
	}

	recorder := httptest.NewRecorder()
	frontendHandler(root).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if body := recorder.Body.String(); body != "<h1>built frontend</h1>" {
		t.Fatalf("body = %q, want built index", body)
	}
}
