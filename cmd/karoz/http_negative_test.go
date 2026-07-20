package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveHTTPRequest drives the real mux (a.httpHandler) so routing, project
// lookup, and handler method checks are all exercised end to end. It never runs
// through withRecovery on purpose: a panic in any handler must fail the test
// instead of being converted into a 500.
func serveHTTPRequest(a *app, method, target, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	a.httpHandler().ServeHTTP(recorder, req)
	return recorder
}

func TestHTTPNegativeUnknownPathsReturn404(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID
	cases := []struct{ name, method, path string }{
		{"unknown api namespace", http.MethodGet, "/api/does-not-exist"},
		{"unknown api namespace post", http.MethodPost, "/api/does-not-exist"},
		{"unknown top-level path", http.MethodGet, "/no/such/page"},
		{"projects trailing slash", http.MethodGet, "/api/projects/"},
		{"unknown project section", http.MethodGet, base + "/no-such-section"},
		{"typo project section", http.MethodGet, base + "/task"},
		{"unknown agent subresource", http.MethodGet, base + "/agents/worker-a/no-such-sub"},
		{"unknown deep agent subresource", http.MethodGet, base + "/agents/worker-a/messages/extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveHTTPRequest(a, tc.method, tc.path, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404; body=%s", tc.method, tc.path, recorder.Code, limitString(recorder.Body.String(), 200))
			}
		})
	}
}

func TestHTTPNegativeWrongMethodStatuses(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID

	// Seed a task so resource-shaped wrong-method checks hit a real resource
	// instead of the missing-resource 404 path.
	seed := serveHTTPRequest(a, http.MethodPost, base+"/tasks", `{"title":"seed task","type":"feature"}`)
	if seed.Code != http.StatusOK {
		t.Fatalf("seed task status=%d body=%s", seed.Code, limitString(seed.Body.String(), 200))
	}
	var task Task
	if err := json.Unmarshal(seed.Body.Bytes(), &task); err != nil || task.ID == "" {
		t.Fatalf("seed task decode err=%v id=%q", err, task.ID)
	}

	// Collection and singleton endpoints reject unsupported methods with an
	// explicit 405 from the handler's default branch.
	methodNotAllowed := []struct{ name, method, path string }{
		{"settings delete", http.MethodDelete, "/api/settings"},
		{"agent templates post", http.MethodPost, "/api/agent-templates"},
		{"agent team templates post", http.MethodPost, "/api/agent-team-templates"},
		{"diagnostics post", http.MethodPost, "/api/diagnostics"},
		{"runtime providers post", http.MethodPost, "/api/runtime/providers"},
		{"cli2api get", http.MethodGet, "/api/cli2api"},
		{"folder dialog get", http.MethodGet, "/api/folder-dialog"},
		{"projects patch", http.MethodPatch, "/api/projects"},
		{"blackboard post", http.MethodPost, base + "/agent-blackboard"},
		{"skills post", http.MethodPost, base + "/skills"},
		{"agents delete", http.MethodDelete, base + "/agents"},
		{"agent routes delete", http.MethodDelete, base + "/agent-routes"},
		{"agent teams get", http.MethodGet, base + "/agent-teams"},
		{"tasks delete", http.MethodDelete, base + "/tasks"},
		{"runtime events post", http.MethodPost, base + "/runtime-events"},
	}
	for _, tc := range methodNotAllowed {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveHTTPRequest(a, tc.method, tc.path, "")
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d, want 405; body=%s", tc.method, tc.path, recorder.Code, limitString(recorder.Body.String(), 200))
			}
		})
	}

	// Resource-shaped routes that only register specific methods fall through
	// to 404 ("no such route") rather than 405. This documents the router's
	// actual intended behavior instead of forcing uniformity.
	notFound := []struct{ name, method, path string }{
		{"agent run post", http.MethodPost, base + "/agents/worker-a/run"},
		{"agent put", http.MethodPut, base + "/agents/worker-a"},
		{"task item post", http.MethodPost, base + "/tasks/" + task.ID},
		{"group inbox post", http.MethodPost, base + "/groups/some-group/inbox"},
	}
	for _, tc := range notFound {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveHTTPRequest(a, tc.method, tc.path, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404; body=%s", tc.method, tc.path, recorder.Code, limitString(recorder.Body.String(), 200))
			}
		})
	}
}

func TestHTTPNegativeMalformedJSONReturns400(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID
	const malformed = `{"title": "broken", `
	cases := []struct{ name, method, path string }{
		{"create project", http.MethodPost, "/api/projects"},
		{"update settings", http.MethodPut, "/api/settings"},
		{"cli2api", http.MethodPost, "/api/cli2api"},
		{"create agent", http.MethodPost, base + "/agents"},
		{"create task", http.MethodPost, base + "/tasks"},
		{"update agent", http.MethodPatch, base + "/agents/worker-a"},
		{"update agent routes", http.MethodPut, base + "/agent-routes"},
		{"create agent team", http.MethodPost, base + "/agent-teams"},
		{"post agent message", http.MethodPost, base + "/agents/worker-a/messages"},
		{"create plan", http.MethodPost, base + "/plans"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveHTTPRequest(a, tc.method, tc.path, malformed)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s %s status = %d, want 400; body=%s", tc.method, tc.path, recorder.Code, limitString(recorder.Body.String(), 200))
			}
			if !strings.Contains(recorder.Body.String(), `"error"`) {
				t.Fatalf("%s %s body missing error field: %s", tc.method, tc.path, limitString(recorder.Body.String(), 200))
			}
		})
	}

	t.Run("wrong field types", func(t *testing.T) {
		recorder := serveHTTPRequest(a, http.MethodPost, base+"/tasks", `{"title": 42, "artifact_ids": "not-a-slice"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("wrong-typed task body status = %d, want 400; body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
	})

	t.Run("multipart form parse error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, base+"/agents/worker-a/messages", strings.NewReader("not a multipart body"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=zzz")
		recorder := httptest.NewRecorder()
		a.httpHandler().ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("malformed multipart status = %d, want 400; body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
	})

	t.Run("empty message rejected", func(t *testing.T) {
		recorder := serveHTTPRequest(a, http.MethodPost, base+"/agents/worker-a/messages", `{"message":"  "}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("empty message status = %d, want 400; body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
	})

	// Documented behavior: an empty body is not treated as malformed JSON
	// (readJSON no-ops on it), so endpoints fall through to their own
	// validation instead of failing at the decode step.
	t.Run("empty body create project", func(t *testing.T) {
		recorder := serveHTTPRequest(a, http.MethodPost, "/api/projects", "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("empty-body create project status = %d, want 400 from name validation; body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
	})
}

func TestHTTPNegativeNonexistentIDsReturn404(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID
	const missingProject = "/api/projects/deadbeef0000"
	cases := []struct{ name, method, path string }{
		{"project", http.MethodGet, missingProject},
		{"project agents", http.MethodGet, missingProject + "/agents"},
		{"project tasks", http.MethodGet, missingProject + "/tasks"},
		{"project blackboard", http.MethodGet, missingProject + "/agent-blackboard"},
		{"agent", http.MethodGet, base + "/agents/ghost"},
		{"agent messages", http.MethodGet, base + "/agents/ghost/messages"},
		{"agent inbox", http.MethodGet, base + "/agents/ghost/inbox"},
		{"agent run", http.MethodGet, base + "/agents/ghost/run"},
		{"agent patch", http.MethodPatch, base + "/agents/ghost"},
		{"agent message post", http.MethodPost, base + "/agents/ghost/messages"},
		{"agent run cancel", http.MethodPost, base + "/agents/ghost/run/cancel"},
		{"agent workspace files", http.MethodGet, base + "/agents/ghost/workspace/files"},
		{"task", http.MethodGet, base + "/tasks/no-such-task"},
		{"task run", http.MethodPost, base + "/tasks/no-such-task/run"},
		{"task logs", http.MethodGet, base + "/tasks/no-such-task/logs"},
		{"artifact", http.MethodGet, base + "/artifacts/no-such-artifact"},
		{"plan", http.MethodGet, base + "/plans/no-such-plan"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serveHTTPRequest(a, tc.method, tc.path, "")
			if recorder.Code == http.StatusInternalServerError {
				t.Fatalf("%s %s returned 500 for a missing resource; body=%s", tc.method, tc.path, limitString(recorder.Body.String(), 200))
			}
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404; body=%s", tc.method, tc.path, recorder.Code, limitString(recorder.Body.String(), 200))
			}
		})
	}

	// Known intentional exceptions to the 404 convention, asserted as-is:
	t.Run("unknown group inbox lists empty", func(t *testing.T) {
		recorder := serveHTTPRequest(a, http.MethodGet, base+"/groups/no-such-group/inbox", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("unknown group inbox status = %d, want 200 (filters instead of 404); body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
		var payload struct {
			Messages []GroupInboxMessage `json:"messages"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unknown group inbox decode: %v", err)
		}
		if len(payload.Messages) != 0 {
			t.Fatalf("unknown group inbox messages = %+v, want empty", payload.Messages)
		}
	})

	t.Run("plan action on missing plan conflicts", func(t *testing.T) {
		recorder := serveHTTPRequest(a, http.MethodPost, base+"/plans/no-such-plan/submit", `{"expected_version":1}`)
		if recorder.Code == http.StatusInternalServerError {
			t.Fatalf("missing plan submit returned 500; body=%s", limitString(recorder.Body.String(), 200))
		}
		if recorder.Code != http.StatusConflict {
			t.Fatalf("missing plan submit status = %d, want 409 (plan domain errors map to conflict); body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
		}
	})
}

func TestHTTPNegativeWorkspacePathTraversalRejected(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID + "/agents/worker-a/workspace/file?path="
	cases := []string{"../escape.txt", "..%2F..%2Fescape", "/etc/passwd", ""}
	for _, path := range cases {
		recorder := serveHTTPRequest(a, http.MethodGet, base+path, "")
		if recorder.Code == http.StatusInternalServerError {
			t.Fatalf("workspace preview path=%q returned 500; body=%s", path, limitString(recorder.Body.String(), 200))
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("workspace preview path=%q status = %d, want 400; body=%s", path, recorder.Code, limitString(recorder.Body.String(), 200))
		}
	}
}

func TestHTTPNegativeRecoveryMiddleware(t *testing.T) {
	a, _ := newHandlerTestApp(t)

	t.Run("panic becomes json 500", func(t *testing.T) {
		handler := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("panic status = %d, want 500", recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("panic content type = %q, want application/json", got)
		}
		if !strings.Contains(recorder.Body.String(), "internal server error") {
			t.Fatalf("panic body = %s", limitString(recorder.Body.String(), 200))
		}
	})

	t.Run("normal error passes through", func(t *testing.T) {
		handler := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusTeapot, errors.New("teapot"))
		}))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/teapot", nil))
		if recorder.Code != http.StatusTeapot {
			t.Fatalf("passthrough status = %d, want 418", recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "teapot") {
			t.Fatalf("passthrough body = %s", limitString(recorder.Body.String(), 200))
		}
	})

	t.Run("panic after commit does not rewrite", func(t *testing.T) {
		handler := withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusConflict, errors.New("first response"))
			panic("late boom")
		}))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/late-panic", nil))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("late panic status = %d, want 409 (response was already committed)", recorder.Code)
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "first response") || strings.Contains(body, "internal server error") {
			t.Fatalf("late panic body = %s", limitString(body, 200))
		}
	})

	t.Run("real stack keeps 400 and 404 clean", func(t *testing.T) {
		handler := withRecovery(a.httpHandler())
		badReq := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader("{bad json"))
		badReq.Header.Set("Content-Type", "application/json")
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, badReq)
		if badRec.Code != http.StatusBadRequest {
			t.Fatalf("malformed JSON through recovery status = %d, want 400; body=%s", badRec.Code, limitString(badRec.Body.String(), 200))
		}
		missingRec := httptest.NewRecorder()
		handler.ServeHTTP(missingRec, httptest.NewRequest(http.MethodGet, "/api/projects/deadbeef0000/tasks", nil))
		if missingRec.Code != http.StatusNotFound {
			t.Fatalf("missing project through recovery status = %d, want 404; body=%s", missingRec.Code, limitString(missingRec.Body.String(), 200))
		}
	})
}
