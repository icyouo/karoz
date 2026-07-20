package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
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
