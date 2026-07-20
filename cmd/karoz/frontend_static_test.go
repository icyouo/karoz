package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func frontendGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	a := &app{}
	recorder := httptest.NewRecorder()
	a.handleIndex(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func TestFrontendServesEmbeddedCSS(t *testing.T) {
	response := frontendGet(t, "/static/css/app.css")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 for /static/css/app.css, got %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/css") {
		t.Fatalf("expected text/css content type, got %q", contentType)
	}
	if response.Body.Len() == 0 {
		t.Fatal("expected non-empty css body")
	}
}

func TestFrontendServesEmbeddedJS(t *testing.T) {
	response := frontendGet(t, "/static/js/core.js")
	if response.Code != http.StatusOK {
		t.Fatalf("expected 200 for /static/js/core.js, got %d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "javascript") {
		t.Fatalf("expected javascript content type, got %q", contentType)
	}
	if response.Body.Len() == 0 {
		t.Fatal("expected non-empty js body")
	}
}

func TestFrontendStaticRejectsTraversal(t *testing.T) {
	for _, path := range []string{"/static/../main.go", "/static/%2e%2e/main.go", "/static/%2E%2E/main.go"} {
		response := frontendGet(t, path)
		if response.Code != http.StatusNotFound && response.Code != http.StatusBadRequest {
			t.Fatalf("expected 404/400 for %q, got %d", path, response.Code)
		}
	}
}

func TestFrontendMissingAssetNotFound(t *testing.T) {
	for _, path := range []string{"/nonexistent", "/static/js/nope.js"} {
		response := frontendGet(t, path)
		if response.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for %q, got %d", path, response.Code)
		}
	}
}
