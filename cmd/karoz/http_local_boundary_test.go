package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestLoopbackListenAddressValidation(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8088", "localhost:8088", "[::1]:8088"} {
		if err := validateStudioListenAddr(addr, false); err != nil {
			t.Fatalf("%s rejected: %v", addr, err)
		}
	}
	for _, addr := range []string{":8088", "0.0.0.0:8088", "192.168.1.20:8088", "not-an-address"} {
		if err := validateStudioListenAddr(addr, false); err == nil {
			t.Fatalf("%s unexpectedly accepted", addr)
		}
	}
}

func TestDockerComposeStartupListenContract(t *testing.T) {
	compose, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	config := string(compose)
	for _, required := range []string{
		`- "127.0.0.1:${KAROZ_PORT:-8088}:8088"`,
		`KAROZ_ADDR: ":8088"`,
		`KAROZ_CONTAINER: "1"`,
	} {
		if !strings.Contains(config, required) {
			t.Fatalf("docker-compose.yml is missing startup contract %q", required)
		}
	}
	if err := validateStudioListenAddr(":8088", true); err != nil {
		t.Fatalf("compose container address rejected at startup: %v", err)
	}
	for _, addr := range []string{"0.0.0.0:8088", "[::]:8088"} {
		if err := validateStudioListenAddr(addr, true); err != nil {
			t.Fatalf("container wildcard %s rejected: %v", addr, err)
		}
	}
	if err := validateStudioListenAddr("192.168.1.20:8088", true); err == nil {
		t.Fatal("container mode unexpectedly accepted a concrete non-loopback address")
	}
}

func TestLocalStudioMutationBoundary(t *testing.T) {
	a, project := newHandlerTestApp(t)
	handler := a.httpHandler()
	base := "/api/projects/" + project.ID

	t.Run("cross-site JSON mutation is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/tasks", strings.NewReader(`{"title":"blocked","type":"feature"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("cross-site status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("mismatched Origin is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/tasks", strings.NewReader(`{"title":"blocked-origin","type":"feature"}`))
		req.Host = "studio.local:8088"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://other.local:8088")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("mismatched Origin status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("text plain JSON mutation is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/tasks", strings.NewReader(`{"title":"wrong-content-type","type":"feature"}`))
		req.Header.Set("Content-Type", "text/plain")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("text/plain status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("same origin JSON works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/tasks", strings.NewReader(`{"title":"same-origin","type":"feature"}`))
		req.Host = "studio.local:8088"
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Origin", "http://studio.local:8088")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("same-origin JSON status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("local CLI JSON works without browser headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/tasks", strings.NewReader(`{"title":"cli-json","type":"feature"}`))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("local CLI JSON status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("agent attachment multipart remains allowed", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("message", "inspect attachment"); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("files", "note.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("fixture")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, base+"/agents/worker-a/messages", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("multipart status = %d body=%s", recorder.Code, recorder.Body.String())
		}
	})
}
