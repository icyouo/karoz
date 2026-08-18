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

func TestFrontendCSSSharesTaskLogScrollbarAndKeepsCodeBlocksContentSized(t *testing.T) {
	styles, err := staticFS.ReadFile("static/css/app.css")
	if err != nil {
		t.Fatal(err)
	}
	styleSource := string(styles)
	for _, fragment := range []string{
		"*::-webkit-scrollbar { width: 9px; height: 9px; }",
		"*::-webkit-scrollbar-thumb { border: 2px solid transparent; border-radius: 999px;",
		".md-code {\n      margin-top: 12px;\n      min-height: 0;",
		".preview-pane pre:not(.md-code)",
		"--chat-composer-max-width: 920px;",
		".chat-message,\n    .choice-card {\n      width: min(var(--chat-composer-max-width), 100%);",
		".chat-message.user {\n      width: fit-content;\n      max-width: min(720px, 76%);\n      margin-right: max(0px, calc((100% - var(--chat-composer-max-width)) / 2));\n      margin-left: auto;",
		".tool-batch,\n    .tool-group {\n      width: fit-content;\n      max-width: min(880px, 100%);\n      margin-right: auto;\n      margin-left: max(0px, calc((100% - var(--chat-composer-max-width)) / 2));",
		".manage-list button.active::before",
		".manage-list button:hover { background: rgba(255, 255, 255, .04); color: var(--text-primary); }",
	} {
		if !strings.Contains(styleSource, fragment) {
			t.Fatalf("shared scrollbar/code-block styling missing %q", fragment)
		}
	}
	if strings.Contains(styleSource, "pre { min-height: 140px;") {
		t.Fatal("generic pre styling must not force a minimum height on Markdown code blocks")
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

func TestRuntimeStripHidesZeroBadgesAndPreviewUsesIconOnlyControl(t *testing.T) {
	panels, err := staticFS.ReadFile("static/js/panels.js")
	if err != nil {
		t.Fatal(err)
	}
	panelSource := string(panels)
	for _, fragment := range []string{
		"if (total <= 0) return '';",
		"const countMarkup = total > 0 ? '<strong>' + total + '</strong>' : '';",
		"const accessibleLabel = total > 0 ? label + ' ' + total : label;",
		"chip('updates', 'Updates', updatesCount",
		"function renderUpdatesPane(body)",
		"data-updates-view=\"attention\"",
		"previewToggle.classList.toggle('active', open);",
		"previewToggle.setAttribute('aria-pressed', open ? 'true' : 'false');",
	} {
		if !strings.Contains(panelSource, fragment) {
			t.Fatalf("runtime strip zero-badge handling missing %q", fragment)
		}
	}
	if strings.Contains(panelSource, "chip('inbox', 'Inbox'") || strings.Contains(panelSource, "chip('blackboard', 'Activity'") {
		t.Fatal("Inbox and Activity must share the Updates navigation entry")
	}

	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	indexSource := string(index)
	if !strings.Contains(indexSource, `id="togglePreviewPane" class="secondary icon preview-toggle"`) ||
		!strings.Contains(indexSource, `aria-label="Preview"`) {
		t.Fatal("preview control must be an accessible icon-only button")
	}
	if strings.Contains(indexSource, `>Preview</button>`) {
		t.Fatal("preview control still renders a text label")
	}

	bindings, err := staticFS.ReadFile("static/js/bindings.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bindings), "if (state.sidePanel) {") {
		t.Fatal("preview control must close any open side panel")
	}
}

func TestToolBatchFrontendKeepsMessageBoundaries(t *testing.T) {
	renderer, err := staticFS.ReadFile("static/js/chat-render.js")
	if err != nil {
		t.Fatal(err)
	}
	rendererSource := string(renderer)
	for _, fragment := range []string{
		"const previous = output.lastElementChild;",
		"previous && previous.classList.contains('tool-batch')",
		"else if (previous && previous.classList.contains('tool-group'))",
		"batch.className = 'tool-batch';",
	} {
		if !strings.Contains(rendererSource, fragment) {
			t.Fatalf("tool batching boundary guard missing %q", fragment)
		}
	}

	for path, fragments := range map[string][]string{
		"static/js/panels.js":      {"appendToolMessage(m.role, m.intent || 'tool'"},
		"static/js/chat-stream.js": {"appendToolMessage('tool_call', payload.tool", "appendToolMessage('tool_result', payload.tool"},
	} {
		source, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(source), fragment) {
				t.Fatalf("%s no longer uses the shared tool batch renderer", path)
			}
		}
	}
}

func TestContextTokenFrontendTracksTheCurrentStreamTurn(t *testing.T) {
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	contextScript := strings.Index(string(index), "/static/js/context-tokens.js")
	coreScript := strings.Index(string(index), "/static/js/core.js")
	if contextScript < 0 || coreScript < 0 || contextScript > coreScript {
		t.Fatal("context token helpers must load before core.js")
	}

	stream, err := staticFS.ReadFile("static/js/chat-stream.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"beginCurrentContextTurn(contextMessage)",
		"updateCurrentContextAssistant(assistantText)",
		"appendCurrentContextEvent('tool_call'",
		"appendCurrentContextEvent('tool_result'",
		"rehydrateCurrentContextAssistant(replay.text)",
		"await refreshActiveAgentChat();\n        clearCurrentContextTurn();",
	} {
		if !strings.Contains(string(stream), fragment) {
			t.Fatalf("stream context accounting missing %q", fragment)
		}
	}

	contextTokens, err := staticFS.ReadFile("static/js/context-tokens.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contextTokens), "function rehydrateAssistantTurn(body)") {
		t.Fatal("replay token accounting must replace the transient assistant turn")
	}
}

func TestFrontendReconnectsAnActiveRunFromItsSequenceCursor(t *testing.T) {
	for path, fragments := range map[string][]string{
		"static/js/core.js": {
			"activeRunID: ''",
			"activeRunAgentID: ''",
			"lastRunSeq: 0",
			"activeRunReplay: null",
			"let agentRunSyncInFlight = false",
			"let agentRunSyncQueued = false",
		},
		"static/js/chat-stream.js": {
			"KarozRunReplay.accept(state, envelope)",
			"event === 'reset'",
			"function appendActiveRunReplayDelta(delta)",
			"function renderActiveRunReplay()",
			"function appendActiveRunReplayToolStart(payload)",
			"function appendActiveRunReplayToolResult(payload)",
			"messageSeq: payload.message_seq",
			"KarozRunReplay.transientToolEvents(replay, state.chatMessages)",
			"run-replay-message",
		},
		"static/js/run-replay.js": {
			"state.lastRunSeq = Math.max(0, Number(floor || 0));",
			"seq <= Number(state.lastRunSeq || 0)",
			"transientToolEvents(replay, messages)",
		},
		"static/js/agents.js": {
			"async function syncActiveRunEvents()",
			"function requestActiveRunSync()",
			"while (agentRunSyncQueued)",
			"await loadResidentRuntimeState();\n      requestActiveRunSync();",
			"/runs/' + encodeURIComponent(run.id) + '/events?after='",
			"dispatchAgentSSE('event: ' + event.type",
			"onDelta: appendActiveRunReplayDelta",
			"onToolStart: appendActiveRunReplayToolStart",
			"onToolResult: appendActiveRunReplayToolResult",
			"await refreshActiveAgentChat(); clearActiveRunReplay(run.id);",
			"requestActiveRunSync();",
			"if (state.agent && !state.chatStreaming && state.view === 'agent') requestActiveRunSync();",
			"runtimeEvents.addEventListener('snapshot'",
			"requestActiveRunSync();\n          scheduleChatRefresh();",
		},
		"static/js/panels.js": {
			"renderChatMessages();\n      renderActiveRunReplay();",
		},
	} {
		source, err := staticFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(source), fragment) {
				t.Fatalf("%s missing active-run reconnect contract %q", path, fragment)
			}
		}
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
