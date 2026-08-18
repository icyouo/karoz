package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimedomain "github.com/karoz/karoz/internal/runtime"
)

// m3BrowserFixtureProvider is deliberately deterministic and side-effect free.
// It gives the manual browser gate a long-lived scheduled Run with the same
// delta and tool-event callbacks as a real provider, without spending a model
// request or touching a user project.
type m3BrowserFixtureProvider struct{}

func (m3BrowserFixtureProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

func (m3BrowserFixtureProvider) Stream(ctx context.Context, _ CLI2APIRequest, _ ResidentToolContext, callbacks AgentStreamCallbacks) error {
	if callbacks.OnDelta != nil {
		callbacks.OnDelta("M3 fixture: partial assistant output before the tool result. ")
	}
	call := codexToolCall{ID: "m3-fixture-tool", CallID: "m3-fixture-tool", Name: "repo_list", Arguments: `{"path":".","depth":0,"max_entries":1}`}
	if callbacks.OnToolStart != nil {
		callbacks.OnToolStart(call)
	}
	if !waitForFixturePhase(ctx, 20*time.Second) {
		return ctx.Err()
	}
	if callbacks.OnToolResult != nil {
		callbacks.OnToolResult(call, `{"entries":[{"path":"fixture.txt","type":"file"}]}`, true)
	}
	if callbacks.OnDelta != nil {
		callbacks.OnDelta("The fixture tool result is now available; keeping the Run open for refresh replay.")
	}
	if !waitForFixturePhase(ctx, 20*time.Second) {
		return ctx.Err()
	}
	return nil
}

func waitForFixturePhase(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// TestM3BrowserFixture is an opt-in manual regression harness. It is skipped
// in normal test runs. When enabled, it serves only an isolated temporary
// project and purposely reports its scheduled Run as idle through both the
// agents list and runtime-event snapshots; /run and the Run ledger remain
// authoritative. That reproduces the stale-list state the M3 fix addresses.
func TestM3BrowserFixture(t *testing.T) {
	if os.Getenv("KAROZ_M3_BROWSER_FIXTURE") != "1" {
		t.Skip("manual browser fixture; set KAROZ_M3_BROWSER_FIXTURE=1")
	}
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	root := t.TempDir()
	projectPath := filepath.Join(root, "m3-browser-fixture")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	project := projectFromPath(projectPath, root, "main")
	project.Name = "M3 Browser Fixture"
	agent := Agent{ID: "karoz", ProjectID: project.ID, Name: "Karoz", Nickname: "Karoz", DisplayName: "Karoz", ShortName: "PMO", Role: "fixture replay verifier", Runtime: "resident", State: "idle", StatusMessage: "ready"}
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	a.agentDirectoryLocked().agents[project.ID] = []Agent{agent}
	a.modelProvider = m3BrowserFixtureProvider{}

	const kind = ScheduledRunKind("m3-browser-fixture")
	a.agentRuntimeLocked().schedulerExecutors[kind] = func(ctx context.Context, job ScheduledRun) error {
		out, err := a.runScheduledResidentAgentTurn(ctx, job, project, agent, "M3 browser fixture scheduled Run", "ask")
		if err != nil {
			return err
		}
		return a.commitScheduledRunResult(project, agent, job.ID, "result", out)
	}

	staleAgent := func() Agent {
		copy := agent
		copy.State = "idle"
		copy.StatusMessage = "ready"
		copy.MessageCount = len(a.agentMessagesFor(project.ID, agent.ID))
		return copy
	}
	serveStaleRuntimeEvents := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("streaming is not supported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		ch := make(chan RuntimeEvent, 32)
		a.addRuntimeWatcher(project.ID, ch)
		defer a.removeRuntimeWatcher(project.ID, ch)
		writeSSE(w, "snapshot", map[string]any{"agents": []Agent{staleAgent()}, "backlog": ""})
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case event := <-ch:
				writeSSE(w, "runtime", map[string]any{"event": event, "agents": []Agent{staleAgent()}, "backlog_not_empty": false})
				flusher.Flush()
			}
		}
	}

	var startMu sync.Mutex
	runID := ""
	done := make(chan struct{})
	var stopOnce sync.Once
	fixtureHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/__m3_fixture/start" && r.Method == http.MethodPost:
			startMu.Lock()
			if runID == "" {
				runID = "m3-browser-" + randomID()
				job, err := newScheduledRun(kind, AgentRunInput{RunID: runID, ProjectID: project.ID, AgentID: agent.ID, Trigger: RunTriggerSystem, TurnType: "ask"}, "m3-browser/"+runID, map[string]string{"fixture": "browser replay"}, 2*time.Minute)
				if err == nil {
					_, _ = a.scheduleAgentRun(job)
				}
				if err != nil {
					startMu.Unlock()
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			}
			started := runID
			startMu.Unlock()
			writeJSON(w, map[string]any{"run_id": started, "project_id": project.ID, "agent_id": agent.ID})
		case r.URL.Path == "/__m3_fixture/stop" && r.Method == http.MethodPost:
			stopOnce.Do(func() { close(done) })
			writeJSON(w, map[string]bool{"stopping": true})
		case r.URL.Path == "/api/projects/"+project.ID+"/agents" && r.Method == http.MethodGet:
			writeJSON(w, []Agent{staleAgent()})
		case r.URL.Path == "/api/projects/"+project.ID+"/runtime-events":
			serveStaleRuntimeEvents(w, r)
		default:
			a.httpHandler().ServeHTTP(w, r)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:8088")
	if err != nil {
		t.Fatalf("listen fixture: %v", err)
	}
	server := &http.Server{Handler: withRecovery(fixtureHandler)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
	})
	t.Logf("M3 fixture ready: project=%s start=POST http://127.0.0.1:8088/__m3_fixture/start", project.ID)
	select {
	case <-done:
	case <-time.After(5 * time.Minute):
		t.Fatal("M3 browser fixture timed out waiting for stop")
	case err := <-serveErr:
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			t.Fatalf("M3 browser fixture server: %v", err)
		}
	}
}
