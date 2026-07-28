package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"
)

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// withLocalStudioMutationGuard is deliberately a small local-browser defense,
// not an authentication scheme. Karoz is a single-user loopback Studio: a
// cross-site page must not be able to drive state-changing local APIs, while a
// same-machine CLI remains usable with ordinary JSON requests.
func withLocalStudioMutationGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChangingMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
			writeError(w, http.StatusForbidden, errors.New("cross-site state-changing requests are not allowed"))
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !requestOriginMatchesRequest(origin, r) {
			writeError(w, http.StatusForbidden, errors.New("request Origin does not match this local Studio"))
			return
		}
		if requestHasBody(r) && !allowsMutationContentType(r) {
			writeError(w, http.StatusUnsupportedMediaType, errors.New("state-changing JSON requests require Content-Type application/json"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isStateChangingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func requestHasBody(r *http.Request) bool {
	return r != nil && r.Body != nil && r.ContentLength != 0
}

func allowsMutationContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	if strings.EqualFold(mediaType, "application/json") {
		return true
	}
	return strings.EqualFold(mediaType, "multipart/form-data") && isAgentMessageUploadRoute(r)
}

func isAgentMessageUploadRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	return len(parts) == 6 && parts[0] == "api" && parts[1] == "projects" && parts[3] == "agents" && parts[5] == "messages" && parts[2] != "" && parts[4] != ""
}

func requestOriginMatchesRequest(rawOrigin string, r *http.Request) bool {
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil {
		return false
	}
	expectedScheme := "http"
	if r != nil && r.TLS != nil {
		expectedScheme = "https"
	}
	return strings.EqualFold(origin.Scheme, expectedScheme) && r != nil && strings.EqualFold(strings.TrimSpace(origin.Host), strings.TrimSpace(r.Host))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// recoveryWriter tracks whether a response has already been committed so
// withRecovery only writes a 500 when nothing was sent yet. Flush is forwarded
// explicitly so SSE handlers keep working through the wrapper.
type recoveryWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *recoveryWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoveryWriter) Write(data []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(data)
}

func (w *recoveryWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &recoveryWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, recovered, debug.Stack())
				if !wrapped.committed {
					wrapped.Header().Set("Content-Type", "application/json")
					wrapped.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(wrapped).Encode(map[string]string{"error": "internal server error"})
				}
			}
		}()
		next.ServeHTTP(wrapped, r)
	})
}
