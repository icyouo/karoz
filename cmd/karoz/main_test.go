package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResidentToolsMemoryArchiveAndSendTo(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	t.Setenv("KAROZ_TASK_AUTO_RUN", "0")
	root := t.TempDir()
	dataDir := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: dataDir, ProjectsRoot: root},
		tasks:         map[string][]Task{},
		agents:        map[string][]Agent{},
		archives:      map[string][]AgentArchiveMessage{},
		memories:      map[string][]AgentMemoryEntry{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
		taskHooks:     map[string][]TaskRuntimeHook{},
		agentRoutes:   map[string][]AgentRoute{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
	}
	agents := a.projectAgents(project)
	if len(agents) != 1 || agents[0].ID != "karoz" {
		t.Fatalf("default agents = %+v", agents)
	}
	if agents[0].Nickname != "Karoz" {
		t.Fatalf("default agent nickname = %q", agents[0].Nickname)
	}
	build, err := a.createProjectAgent(project, AgentCreateRequest{TemplateID: "implementation-lead", Nickname: "Build"})
	if err != nil {
		t.Fatal(err)
	}
	toolCtx := ResidentToolContext{Project: project, Agent: agents[0], Workdir: projectPath}
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "remember_fact", Arguments: `{"summary":"fact","detail":"detail"}`}); err != nil {
		t.Fatal(err)
	}
	if got := a.activeMemoriesFor(project.ID, "karoz", "fact", 10); len(got) != 1 {
		t.Fatalf("fact memories = %+v", got)
	}
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "report_activity", Arguments: `{"activity_kind":"progress","summary":"working","detail":"details"}`}); err != nil {
		t.Fatal(err)
	}
	choiceResult, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "request_choice", Arguments: `{"question":"Proceed?","mode":"yes_no"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(choiceResult, `"kind":"choice_request"`) || !strings.Contains(choiceResult, `"id":"yes"`) || !strings.Contains(choiceResult, `"id":"no"`) {
		t.Fatalf("choice result = %s", choiceResult)
	}
	if got := a.blackboardFor(project.ID, 10); len(got) != 1 || got[0].Summary != "working" {
		t.Fatalf("blackboard = %+v", got)
	}
	args, _ := json.Marshal(map[string]any{"target_agent_id": build.ID, "body": "please review", "subject": "handoff"})
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "send_to", Arguments: string(args)}); err != nil {
		t.Fatal(err)
	}
	buildInbox := a.pendingInboxFor(project.ID, build.ID, 10)
	if len(buildInbox) != 1 || buildInbox[0].Body != "please review" || buildInbox[0].MessageType != "handoff" {
		t.Fatalf("inbox = %+v", buildInbox)
	}
	buildCtx := ResidentToolContext{Project: project, Agent: build, Workdir: projectPath}
	reportDoneArgs, _ := json.Marshal(map[string]any{"inbox_message_id": buildInbox[0].ID, "activity_kind": "done", "summary": "review complete"})
	_, err = a.executeResidentTool(context.Background(), buildCtx, codexToolCall{Name: "report_activity", Arguments: string(reportDoneArgs)})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.pendingInboxFor(project.ID, build.ID, 10); len(got) != 0 {
		t.Fatalf("inbox = %+v", got)
	}
	karozInbox := a.pendingInboxFor(project.ID, "karoz", 10)
	if len(karozInbox) != 0 {
		t.Fatalf("terminal reply left pending inbox = %+v", karozInbox)
	}
	original, _ := a.inboxMessage(project.ID, build.ID, buildInbox[0].ID)
	if original.Status != HandoffClosed || original.Result != "review complete" || original.ReportedAt == nil {
		t.Fatalf("original handoff = %+v", original)
	}
	if _, err := a.updateAgentRoutes(project, []AgentRoute{{FromAgentID: build.ID, ToAgentID: "karoz", Intent: "request", Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	allowed, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "send_to", Arguments: string(args)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(allowed, "route_denied") {
		t.Fatalf("karoz coordinator send_to should be allowed, got = %s", allowed)
	}
	resultToKarozArgs, _ := json.Marshal(map[string]any{"target_agent_id": "karoz", "intent": "result", "body": "direct result", "subject": "result"})
	resultToKaroz, err := a.executeResidentTool(context.Background(), buildCtx, codexToolCall{Name: "send_to", Arguments: string(resultToKarozArgs)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resultToKaroz, "route_denied") {
		t.Fatalf("inbound to karoz should be allowed, got = %s", resultToKaroz)
	}
	reportInbox := a.pendingInboxFor(project.ID, build.ID, 10)
	if len(reportInbox) != 1 {
		t.Fatalf("expected report inbox, got %+v", reportInbox)
	}
	reportArgs, _ := json.Marshal(map[string]any{"activity_kind": "progress", "summary": "reported progress", "inbox_message_id": reportInbox[0].ID})
	if _, err := a.executeResidentTool(context.Background(), buildCtx, codexToolCall{Name: "report_activity", Arguments: string(reportArgs)}); err != nil {
		t.Fatal(err)
	}
	if got := a.pendingInboxFor(project.ID, build.ID, 10); len(got) != 1 || got[0].ID != reportInbox[0].ID {
		t.Fatalf("report_activity must not close handoff, got %+v", got)
	}
	reportAckArgs, _ := json.Marshal(map[string]any{"inbox_message_id": reportInbox[0].ID, "note": "reported separately"})
	if _, err := a.executeResidentTool(context.Background(), buildCtx, codexToolCall{Name: "ack_inbox", Arguments: string(reportAckArgs)}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "write_workspace_file", Arguments: `{"path":"mockup.html","content":"<!doctype html><html><body>hi</body></html>"}`}); err != nil {
		t.Fatal(err)
	}
	files, err := a.listWorkspaceFiles(project.ID, "karoz")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Filename != "mockup.html" {
		t.Fatalf("workspace files = %+v", files)
	}
	preview, err := a.getWorkspaceFilePreview(project.ID, "karoz", "mockup.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Content, "hi") {
		t.Fatalf("preview = %+v", preview)
	}
	taskResult, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "create_task", Arguments: `{"title":"Build feature","description":"Implement feature","type":"feature"}`})
	if err != nil {
		t.Fatal(err)
	}
	var taskPayload struct {
		TaskID string `json:"task_id"`
		HookID string `json:"hook_id"`
	}
	if err := json.Unmarshal([]byte(taskResult), &taskPayload); err != nil {
		t.Fatal(err)
	}
	if taskPayload.TaskID == "" || taskPayload.HookID == "" {
		t.Fatalf("create_task payload = %s", taskResult)
	}
	task, ok := a.findTask(project.ID, taskPayload.TaskID)
	if !ok || task.Type != "feature" {
		t.Fatalf("created task = %+v ok=%v", task, ok)
	}
	if _, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "update_task_status", Arguments: `{"task_id":"` + taskPayload.TaskID + `","status":"done","result":"completed"}`}); err != nil {
		t.Fatal(err)
	}
	messages := a.agentMessagesFor(project.ID, "karoz")
	if len(messages) == 0 || messages[len(messages)-1].Intent != "task_hook" {
		t.Fatalf("expected task_hook message, got %+v", messages)
	}
	for i := 0; i < 55; i++ {
		a.appendAgentMessage(project.ID, "karoz", "user", "question", "message")
	}
	if got := a.archives[agentMessageKey(project.ID, "karoz")]; len(got) == 0 {
		t.Fatalf("expected archived messages after checkpoint")
	}
}

func TestCreateAgentTeamCreatesGroupedAgentsAndRoutes(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: dataDir, ProjectsRoot: root},
		tasks:         map[string][]Task{},
		agents:        map[string][]Agent{},
		agentRoutes:   map[string][]AgentRoute{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
	}
	resp, err := a.createAgentTeam(project, AgentTeamCreateRequest{TemplateID: "build-lane", Instance: "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GroupID != "build-lane-ship" || resp.Created != 3 || len(resp.Agents) != 3 {
		t.Fatalf("team response = %+v", resp)
	}
	for _, agent := range resp.Agents {
		if agent.GroupID != "build-lane-ship" || agent.GroupRole == "" {
			t.Fatalf("agent missing group tag: %+v", agent)
		}
	}
	if len(resp.Routes) != 7 {
		t.Fatalf("routes = %+v", resp.Routes)
	}
}

func TestKarozPromptRequiresSendToForAgentCoordination(t *testing.T) {
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agents:        map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Nickname: "Karoz"}, {ID: "product-a", ProjectID: "p1", Nickname: "Product A", Role: "product"}, {ID: "product-b", ProjectID: "p1", Nickname: "Product B", Role: "product"}}},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		memories:      map[string][]AgentMemoryEntry{},
		archives:      map[string][]AgentArchiveMessage{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
	}

	prompt := a.buildResidentAgentPrompt(project, a.agents["p1"][0], "让两个产品输出最后的 prd", "ask")
	if !strings.Contains(prompt, "must call send_to") {
		t.Fatalf("karoz prompt does not require send_to coordination:\n%s", prompt)
	}
	if !strings.Contains(prompt, "product-a") || !strings.Contains(prompt, "product-b") {
		t.Fatalf("karoz prompt missing teammate routing context:\n%s", prompt)
	}
}

func TestAutoHandoffFallbackClosesCollaborationLoop(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	root := t.TempDir()
	dataDir := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: "p1", Name: "demo", Path: projectPath, DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: dataDir, ProjectsRoot: root},
		agents:        map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Nickname: "Karoz"}, {ID: "worker", ProjectID: "p1", Nickname: "Worker"}}},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
	}
	msg := AgentInboxMessage{
		ID:            "inbox-1",
		ProjectID:     "p1",
		SourceAgentID: "karoz",
		TargetAgentID: "worker",
		MessageType:   "handoff",
		Intent:        "request",
		Subject:       "Need result",
		Body:          "Please answer",
		ThreadKey:     "thread-1",
		Status:        "pending",
		CreatedAt:     time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, msg); err != nil {
		t.Fatal(err)
	}
	a.completeUnhandledInboxAfterAutoResponse(project, a.agents["p1"][1], msg, "Here is the result")
	workerInbox, ok := a.inboxMessage(project.ID, "worker", msg.ID)
	if !ok || workerInbox.Status != HandoffClosed || workerInbox.ReportedAt == nil || workerInbox.ClosedAt == nil || workerInbox.Result != "Here is the result" {
		t.Fatalf("worker inbox = %+v ok=%v", workerInbox, ok)
	}
	karozPending := a.pendingInboxFor(project.ID, "karoz", 10)
	if len(karozPending) != 0 {
		t.Fatalf("karoz pending reply = %+v", karozPending)
	}
	entries := a.blackboardFor(project.ID, 10)
	if len(entries) != 2 {
		t.Fatalf("blackboard = %+v", entries)
	}
	foundProjection, foundReport := false, false
	for _, entry := range entries {
		if entry.Derived && entry.SourceType == blackboardSourceHandoff && entry.SourceInboxMessageID == msg.ID && entry.Status == HandoffClosed {
			foundProjection = true
		}
		if !entry.Derived && entry.SourceType == blackboardSourceAgentReport && entry.SourceInboxMessageID == msg.ID && entry.ActivityKind == "done" {
			foundReport = true
		}
	}
	if !foundProjection || !foundReport {
		t.Fatalf("blackboard missing projection/report = %+v", entries)
	}
	peer := Agent{ID: "reviewer", ProjectID: "p1", Nickname: "Reviewer"}
	a.agents["p1"] = append(a.agents["p1"], peer)
	peerMsg := AgentInboxMessage{
		ID:            "inbox-2",
		ProjectID:     "p1",
		SourceAgentID: "reviewer",
		TargetAgentID: "worker",
		MessageType:   "handoff",
		Intent:        "request",
		Subject:       "Question",
		Body:          "Please answer",
		ThreadKey:     "thread-2",
		Status:        "pending",
		CreatedAt:     time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, peerMsg); err != nil {
		t.Fatal(err)
	}
	a.completeUnhandledInboxAfterAutoResponse(project, a.agents["p1"][1], peerMsg, "Peer answer")
	if got := a.pendingInboxFor(project.ID, "reviewer", 10); len(got) != 0 {
		t.Fatalf("reviewer reply = %+v", got)
	}
	if entries := a.blackboardFor(project.ID, 10); len(entries) != 3 {
		t.Fatalf("blackboard should contain one projection per original handoff, got %+v", entries)
	}
}

func TestProjectWorkspacesScanMainAndExtraCreateInMain(t *testing.T) {
	mainRoot := t.TempDir()
	extraRoot := t.TempDir()
	dataDir := t.TempDir()
	for _, path := range []string{
		filepath.Join(mainRoot, "main-app", ".git"),
		filepath.Join(extraRoot, "imported-app", ".git"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{
		settings: Settings{
			DataDir:            dataDir,
			ProjectsRoot:       mainRoot,
			ExtraProjectsRoots: []string{extraRoot},
		},
		tasks:          map[string][]Task{},
		projectAliases: map[string]string{},
	}
	projects, err := a.scanProjects()
	if err != nil {
		t.Fatal(err)
	}
	var sawMain, sawExtra bool
	for _, project := range projects {
		switch project.Name {
		case "main-app":
			sawMain = project.WorkspaceType == "main" && project.WorkspaceRoot == filepath.Clean(mainRoot)
		case "imported-app":
			sawExtra = project.WorkspaceType == "extra" && project.WorkspaceRoot == filepath.Clean(extraRoot)
		}
	}
	if !sawMain || !sawExtra {
		t.Fatalf("projects = %+v", projects)
	}
	created, err := a.createProject(ProjectCreateRequest{Name: "new-app"})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkspaceType != "main" || created.WorkspaceRoot != filepath.Clean(mainRoot) || !strings.HasPrefix(created.Path, filepath.Clean(mainRoot)+string(os.PathSeparator)) {
		t.Fatalf("created project = %+v", created)
	}
	externalRoot := t.TempDir()
	externalProject := filepath.Join(externalRoot, "existing-app")
	if err := os.MkdirAll(filepath.Join(externalProject, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	imported, err := a.createProject(ProjectCreateRequest{Mode: "import", Path: externalProject, Name: "Imported Name"})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Name != "Imported Name" || imported.Path != filepath.Clean(externalProject) || imported.WorkspaceType != "extra" || imported.WorkspaceRoot != filepath.Clean(externalProject) {
		t.Fatalf("imported project = %+v", imported)
	}
	scanned, err := a.scanProjects()
	if err != nil {
		t.Fatal(err)
	}
	var sawImported bool
	for _, project := range scanned {
		if project.ID == imported.ID && project.Name == "Imported Name" {
			sawImported = true
		}
	}
	if !sawImported {
		t.Fatalf("scanned projects after import = %+v", scanned)
	}
}

func TestSkillsDiscoveryToolsAndPromptInjection(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	skillDir := filepath.Join(projectPath, ".agents", "skills", "demo")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: demo-skill\ndescription: Demo skill for tests\nshort-description: Demo short\n---\nFollow demo instructions.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir(), ProjectsRoot: root},
		agents:        map[string][]Agent{},
		archives:      map[string][]AgentArchiveMessage{},
		memories:      map[string][]AgentMemoryEntry{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		inbox:         map[string][]AgentInboxMessage{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
	}
	skills := a.discoverSkills(project)
	var sawDemo bool
	for _, skill := range skills {
		if skill.Name == "demo-skill" && skill.Path == skillPath {
			sawDemo = true
		}
	}
	if !sawDemo {
		t.Fatalf("skills = %+v", skills)
	}
	toolCtx := ResidentToolContext{Project: project, Agent: Agent{ID: "karoz", Nickname: "Karoz"}, Workdir: projectPath}
	listed, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "list_skills", Arguments: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, "demo-skill") {
		t.Fatalf("list_skills = %s", listed)
	}
	read, err := a.executeResidentTool(context.Background(), toolCtx, codexToolCall{Name: "read_skill", Arguments: `{"name":"demo-skill"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read, "Follow demo instructions") {
		t.Fatalf("read_skill = %s", read)
	}
	prompt := a.buildResidentAgentPrompt(project, toolCtx.Agent, "use $demo-skill now", "ask")
	if !strings.Contains(prompt, "### Available skills") || !strings.Contains(prompt, "<skill name=\"demo-skill\"") {
		t.Fatalf("prompt did not include skill context:\n%s", prompt)
	}
	slashPrompt := a.buildResidentAgentPrompt(project, toolCtx.Agent, "use /demo-skill now", "ask")
	if !strings.Contains(slashPrompt, "<skill name=\"demo-skill\"") {
		t.Fatalf("slash skill mention did not inject skill:\n%s", slashPrompt)
	}
}

func TestMCPToolSpecsAndCall(t *testing.T) {
	if os.Getenv("KAROZ_FAKE_MCP_SERVER") == "1" {
		runFakeMCPServer(t)
		return
	}
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}
	a := &app{
		settings: Settings{
			DataDir:      t.TempDir(),
			ProjectsRoot: root,
			MCPServers: map[string]MCPServerConfig{
				"fake": {
					Command: os.Args[0],
					Args:    []string{"-test.run=TestMCPToolSpecsAndCall"},
					Env:     map[string]string{"KAROZ_FAKE_MCP_SERVER": "1"},
				},
			},
		},
	}
	specs := a.mcpToolSpecs(context.Background(), projectPath)
	if len(specs) != 1 || specs[0]["name"] != "mcp__fake__echo" {
		t.Fatalf("mcp specs = %+v", specs)
	}
	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{
		Project: Project{ID: "demo", Name: "demo", Path: projectPath},
		Agent:   Agent{ID: "karoz"},
		Workdir: projectPath,
	}, codexToolCall{Name: "mcp__fake__echo", Arguments: `{"text":"hello"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "echo:hello") {
		t.Fatalf("mcp call result = %s", result)
	}
}

func TestProjectMCPConfigSSETool(t *testing.T) {
	responses := make(chan map[string]any, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("response writer cannot flush")
			}
			_, _ = io.WriteString(w, "event: endpoint\ndata: /message?session=test\n\n")
			flusher.Flush()
			for {
				select {
				case msg := <-responses:
					data, _ := json.Marshal(msg)
					_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		case "/message":
			var req struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.ID == nil {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			switch req.Method {
			case "initialize":
				responses <- map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}}}
			case "tools/list":
				responses <- map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{
					"name":        "get_design_context",
					"description": "Read Figma design context",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"fileKey": map[string]any{"type": "string"}}},
				}}}}
			case "tools/call":
				responses <- map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"content": []map[string]any{{"type": "text", "text": "figma ok"}}, "isError": false}}
			default:
				responses <- map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"message": "unknown method"}}
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	projectPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectPath, ".mcp.json"), []byte(`{"mcpServers":{"figma":{"url":"`+server.URL+`/sse"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	a := &app{settings: Settings{DataDir: t.TempDir(), ProjectsRoot: t.TempDir()}}
	specs := a.mcpToolSpecs(context.Background(), projectPath)
	if len(specs) != 1 || specs[0]["name"] != "mcp__figma__get_design_context" {
		t.Fatalf("mcp specs = %+v", specs)
	}
	result, err := a.callMCPTool(context.Background(), projectPath, "mcp__figma__get_design_context", `{"fileKey":"abc"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "figma ok") {
		t.Fatalf("mcp call result = %s", result)
	}
}

func TestStreamCodexResponseUsesCompletedTextWithoutDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"最终回复"}]}]}}`+"\n\n")
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	calls, err := streamCodexResponse(req, func(delta string) {
		out.WriteString(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("tool calls = %d", len(calls))
	}
	if out.String() != "最终回复" {
		t.Fatalf("streamed text = %q", out.String())
	}
}

func TestAgentPromptDeltaCompactsLargeToolResults(t *testing.T) {
	largeImage := strings.Repeat("A", 5000)
	largeStdout := strings.Repeat("line\n", 2000)
	messages := []AgentMessage{
		{Role: "tool_result", Intent: "mcp__figma__get_screenshot", Body: `{"result":{"content":[{"type":"image","data":"` + largeImage + `"}]}}`},
		{Role: "tool_result", Intent: "bash", Body: `{"ok":true,"stdout":"` + largeStdout + `"}`},
		{Role: "assistant", Body: "done"},
	}

	lines := renderAgentPromptDelta(messages, 50, 24000)
	var prompt strings.Builder
	for _, line := range lines {
		prompt.WriteString(line.Body)
	}
	got := prompt.String()
	if strings.Contains(got, largeImage[:100]) {
		t.Fatalf("prompt leaked raw base64 data")
	}
	if !strings.Contains(got, "omitted 5000 chars of data") {
		t.Fatalf("prompt did not mark omitted data: %s", got)
	}
	if len(got) > 6000 {
		t.Fatalf("prompt too large: %d", len(got))
	}
}

func TestLimitToolResultForModelCapsCurrentLoopOutput(t *testing.T) {
	result := strings.Repeat("x", maxCodexToolOutputChars+1000)
	got := limitToolResultForModel(result)

	if len(got) > maxCodexToolOutputChars {
		t.Fatalf("tool result was not capped: %d", len(got))
	}
	if !strings.Contains(got, "karoz truncated tool result") {
		t.Fatalf("tool result missing truncation notice")
	}
	if !strings.Contains(got, "original_chars=901000") {
		t.Fatalf("tool result missing original size: %s", got[len(got)-160:])
	}
}

func TestGetArchivedMessagesCompactsToolResults(t *testing.T) {
	projectID := "p1"
	agentID := "karoz"
	key := agentMessageKey(projectID, agentID)
	largeStdout := strings.Repeat("line\n", 5000)
	a := &app{
		agentMessages: map[string][]AgentMessage{
			key: {{Seq: 1, Role: "tool_result", Intent: "bash", Body: `{"ok":true,"stdout":"` + largeStdout + `"}`, CreatedAt: time.Now().UTC()}},
		},
		archives: map[string][]AgentArchiveMessage{},
	}

	got := a.getArchivedMessages(projectID, agentID, 1, 1, 40)
	if strings.Contains(got, largeStdout[:100]) {
		t.Fatalf("get_messages leaked raw tool result")
	}
	if !strings.Contains(got, `"original_chars"`) {
		t.Fatalf("get_messages missing original size metadata: %s", got)
	}
	if len(got) > 5000 {
		t.Fatalf("get_messages result too large: %d", len(got))
	}
}

func TestAgentMessagesPageForDisplayReturnsLatestThenEarlier(t *testing.T) {
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
	}
	for i := 1; i <= 5; i++ {
		a.appendAgentMessage("p1", "karoz", "assistant", "", fmt.Sprintf("message-%d", i))
	}

	latest := a.agentMessagesPageForDisplay("p1", "karoz", 0, 2)
	if len(latest.Messages) != 2 || latest.Messages[0].Body != "message-4" || latest.Messages[1].Body != "message-5" {
		t.Fatalf("latest page = %+v", latest.Messages)
	}
	if !latest.HasMore || latest.NextBeforeSeq != latest.Messages[0].Seq {
		t.Fatalf("latest page metadata = %+v", latest)
	}

	earlier := a.agentMessagesPageForDisplay("p1", "karoz", latest.NextBeforeSeq, 2)
	if len(earlier.Messages) != 2 || earlier.Messages[0].Body != "message-2" || earlier.Messages[1].Body != "message-3" {
		t.Fatalf("earlier page = %+v", earlier.Messages)
	}
	if !earlier.HasMore || earlier.NextBeforeSeq != earlier.Messages[0].Seq {
		t.Fatalf("earlier page metadata = %+v", earlier)
	}

	first := a.agentMessagesPageForDisplay("p1", "karoz", earlier.NextBeforeSeq, 2)
	if len(first.Messages) != 1 || first.Messages[0].Body != "message-1" || first.HasMore {
		t.Fatalf("first page = %+v hasMore=%v", first.Messages, first.HasMore)
	}
}

func TestReadAgentMessageRequestSavesMultipartAttachments(t *testing.T) {
	dataDir := t.TempDir()
	a := &app{settings: Settings{DataDir: dataDir}}
	project := Project{ID: "p1", Name: "demo"}
	agent := Agent{ID: "karoz"}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("message", "please inspect"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("type", "ask"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("files", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("hello attachment")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/messages", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	parsed, attachments, err := a.readAgentMessageRequest(req, project, agent)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Message != "please inspect" || parsed.Type != "ask" {
		t.Fatalf("request = %+v", parsed)
	}
	if len(attachments) != 1 {
		t.Fatalf("attachments = %+v", attachments)
	}
	if _, err := os.Stat(attachments[0].Path); err != nil {
		t.Fatal(err)
	}
	rendered := messageTextWithAttachments(parsed.Message, attachments)
	if !strings.Contains(rendered, "Attachments:") || !strings.Contains(rendered, attachments[0].Path) {
		t.Fatalf("rendered message = %s", rendered)
	}
}

func TestAgentInterruptQueueDrainsForModelInjection(t *testing.T) {
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		agentRuns:     map[string]AgentRun{},
	}
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: "p1", AgentID: "frontend", Trigger: RunTriggerUserDirect, TurnType: "dev"})
	if !started {
		t.Fatal("first run should start")
	}
	if run.Trigger != RunTriggerUserDirect || run.State != RunStatePreparingContext || run.TurnType != "dev" {
		t.Fatalf("run = %+v", run)
	}
	if _, started := a.beginAgentRun(AgentRunInput{ProjectID: "p1", AgentID: "frontend", Trigger: RunTriggerHandoff}); started {
		t.Fatal("second run should be rejected while active")
	}
	msg := a.appendAgentMessage("p1", "frontend", "user", "interrupt", "请先改按钮布局")
	if _, queued := a.enqueueAgentInterrupt("p1", "frontend", msg, "dev"); !queued {
		t.Fatal("interrupt should attach to active run")
	}
	items := a.drainAgentInterrupts("p1", "frontend")
	if len(items) != 1 || items[0].Body != "请先改按钮布局" {
		t.Fatalf("interrupts = %+v", items)
	}
	rendered := renderAgentInterruptsForModel(items)
	if !strings.Contains(rendered, "latest user input") || !strings.Contains(rendered, "请先改按钮布局") {
		t.Fatalf("rendered interrupts = %s", rendered)
	}
	if got := a.drainAgentInterrupts("p1", "frontend"); len(got) != 0 {
		t.Fatalf("interrupts should drain once: %+v", got)
	}
	finished, ok := a.finishAgentRun("p1", "frontend", run.ID, RunStateDone, nil)
	if !ok || finished.State != RunStateDone || finished.EndedAt == nil {
		t.Fatalf("finished run = %+v ok=%v", finished, ok)
	}
	if _, started := a.beginAgentRun(AgentRunInput{ProjectID: "p1", AgentID: "frontend", Trigger: RunTriggerHandoff, SourceID: "karoz"}); !started {
		t.Fatal("run should start after end")
	}
}

func TestAgentRunControllerTracksTriggerTransitionsAndFailure(t *testing.T) {
	events := make(chan RuntimeEvent, 8)
	a := &app{
		agentRuns:       map[string]AgentRun{},
		runtimeHooks:    map[string]bool{},
		runtimeWatchers: map[string]map[chan RuntimeEvent]bool{"p1": {events: true}},
		tasks:           map[string][]Task{},
		inbox:           map[string][]AgentInboxMessage{},
		blackboard:      map[string][]AgentBlackboardEntry{},
		memories:        map[string][]AgentMemoryEntry{},
	}
	run, started := a.beginAgentRun(AgentRunInput{
		ProjectID: "p1",
		AgentID:   "designer",
		Trigger:   RunTriggerHandoff,
		TurnType:  "plan",
		SourceID:  "product",
		MessageID: "handoff-1",
	})
	if !started || run.Trigger != RunTriggerHandoff || run.SourceID != "product" || run.MessageID != "handoff-1" {
		t.Fatalf("started run = %+v started=%v", run, started)
	}
	startedEvent := <-events
	if startedEvent.RunID != run.ID || startedEvent.Trigger != string(RunTriggerHandoff) || startedEvent.To != string(RunStatePreparingContext) {
		t.Fatalf("started event = %+v", startedEvent)
	}
	if current, ok := a.transitionAgentRun("p1", "designer", RunStateExecutingTool); !ok || current.State != RunStateExecutingTool {
		t.Fatalf("tool transition = %+v ok=%v", current, ok)
	}
	toolEvent := <-events
	if toolEvent.RunID != run.ID || toolEvent.To != string(RunStateExecutingTool) {
		t.Fatalf("tool event = %+v", toolEvent)
	}
	if current, ok := a.activeAgentRun("p1", "designer"); !ok || current.State != RunStateExecutingTool {
		t.Fatalf("active run = %+v ok=%v", current, ok)
	}
	failure := fmt.Errorf("provider failed")
	finished, ok := a.finishAgentRun("p1", "designer", run.ID, RunStateFailed, failure)
	if !ok || finished.State != RunStateFailed || finished.Error != failure.Error() || finished.EndedAt == nil {
		t.Fatalf("failed run = %+v ok=%v", finished, ok)
	}
	failedEvent := <-events
	if failedEvent.RunID != run.ID || failedEvent.To != string(RunStateFailed) {
		t.Fatalf("failed event = %+v", failedEvent)
	}
	if _, ok := a.activeAgentRun("p1", "designer"); ok {
		t.Fatal("finished run remained active")
	}
}

func TestAgentRunQueryAndCancelAPI(t *testing.T) {
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agents:        map[string][]Agent{"p1": {{ID: "designer", ProjectID: "p1", Name: "product-designer", Nickname: "Designer"}}},
		agentRuns:     map[string]AgentRun{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		tasks:         map[string][]Task{},
		inbox:         map[string][]AgentInboxMessage{},
		blackboard:    map[string][]AgentBlackboardEntry{},
		memories:      map[string][]AgentMemoryEntry{},
	}
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerUserDirect, TurnType: "plan"})
	if !started {
		t.Fatal("run did not start")
	}
	runCtx, bound := a.bindAgentRunContext(context.Background(), "p1", "designer")
	if !bound {
		t.Fatal("run context did not bind")
	}

	queryRecorder := httptest.NewRecorder()
	a.handleAgents(queryRecorder, httptest.NewRequest(http.MethodGet, "/api/projects/p1/agents/designer/run", nil), project, []string{"designer", "run"})
	if queryRecorder.Code != http.StatusOK || !strings.Contains(queryRecorder.Body.String(), run.ID) || !strings.Contains(queryRecorder.Body.String(), `"active":true`) {
		t.Fatalf("query response code=%d body=%s", queryRecorder.Code, queryRecorder.Body.String())
	}

	cancelRecorder := httptest.NewRecorder()
	a.handleAgents(cancelRecorder, httptest.NewRequest(http.MethodPost, "/api/projects/p1/agents/designer/run/cancel", nil), project, []string{"designer", "run", "cancel"})
	if cancelRecorder.Code != http.StatusOK || !strings.Contains(cancelRecorder.Body.String(), `"cancelled":true`) || !strings.Contains(cancelRecorder.Body.String(), string(RunStateCancelled)) {
		t.Fatalf("cancel response code=%d body=%s", cancelRecorder.Code, cancelRecorder.Body.String())
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("cancel API did not cancel the bound run context")
	}
	if _, active := a.activeAgentRun("p1", "designer"); active {
		t.Fatal("cancelled run remained active")
	}
}

func TestAgentRunSchedulerSerializesAndDeduplicatesPerAgent(t *testing.T) {
	testKind := ScheduledRunKind("test")
	a := &app{
		settings:           Settings{DataDir: t.TempDir()},
		agentRuns:          map[string]AgentRun{},
		agentRunCancels:    map[string]context.CancelFunc{},
		schedulerExecutors: map[ScheduledRunKind]ScheduledRunExecutor{},
		runtimeHooks:       map[string]bool{},
		tasks:              map[string][]Task{},
		inbox:              map[string][]AgentInboxMessage{},
		blackboard:         map[string][]AgentBlackboardEntry{},
		memories:           map[string][]AgentMemoryEntry{},
	}
	started := make(chan string, 2)
	releaseFirst := make(chan struct{})
	a.schedulerExecutors[testKind] = func(ctx context.Context, job ScheduledRun) error {
		switch job.SourceID {
		case "product":
			started <- "first"
			select {
			case <-releaseFirst:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case "task-1":
			run, active := a.activeAgentRun("p1", "designer")
			if !active || run.Trigger != RunTriggerTaskEvent {
				return fmt.Errorf("unexpected active run: %+v active=%v", run, active)
			}
			started <- "second"
			return nil
		default:
			return fmt.Errorf("unexpected source id %q", job.SourceID)
		}
	}
	first, err := newScheduledRun(testKind, AgentRunInput{ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerHandoff, SourceID: "product"}, "handoff/first", map[string]any{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newScheduledRun(testKind, AgentRunInput{ProjectID: "p1", AgentID: "designer", Trigger: RunTriggerTaskEvent, SourceID: "task-1"}, "task/second", map[string]any{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := second
	duplicate.ID = randomID()
	if _, scheduled := a.scheduleAgentRun(first); !scheduled {
		t.Fatal("first job was not scheduled")
	}
	if _, scheduled := a.scheduleAgentRun(second); !scheduled {
		t.Fatal("second job was not scheduled")
	}
	if _, scheduled := a.scheduleAgentRun(duplicate); scheduled {
		t.Fatal("duplicate job was scheduled")
	}
	select {
	case got := <-started:
		if got != "first" {
			t.Fatalf("first execution = %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduled job did not start")
	}
	if queued := a.scheduledAgentRunCount("p1", "designer"); queued != 1 {
		t.Fatalf("queued jobs = %d", queued)
	}
	close(releaseFirst)
	select {
	case got := <-started:
		if got != "second" {
			t.Fatalf("second execution = %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second scheduled job did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.agentRunActive("p1", "designer") || a.scheduledAgentRunCount("p1", "designer") > 0 || a.scheduledAgentWorkerActive("p1", "designer") {
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not become idle")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTaskCompletionSchedulesTaskEventRun(t *testing.T) {
	t.Setenv("KAROZ_AGENT_PROVIDER", "stub")
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "1")
	root := t.TempDir()
	projectPath := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(projectPath), Name: "demo", Path: projectPath, WorkspaceRoot: root, WorkspaceType: "main", DefaultBranch: "main"}
	hook := TaskRuntimeHook{ID: "hook-1", TaskID: "task-1", ProjectID: project.ID, AgentID: "designer", HookType: "resident_task_completion", Status: "pending"}
	hookKey := project.ID + "/task-1"
	a := &app{
		settings:           Settings{DataDir: t.TempDir(), ProjectsRoot: root},
		agents:             map[string][]Agent{project.ID: {{ID: "designer", ProjectID: project.ID, Name: "product-designer", Nickname: "Designer"}}},
		agentMessages:      map[string][]AgentMessage{},
		agentSessions:      map[string]AgentSessionState{},
		agentRuns:          map[string]AgentRun{},
		agentRunCancels:    map[string]context.CancelFunc{},
		schedulerExecutors: map[ScheduledRunKind]ScheduledRunExecutor{},
		taskHooks:          map[string][]TaskRuntimeHook{hookKey: {hook}},
		tasks:              map[string][]Task{project.ID: {{ID: "task-1", ProjectID: project.ID, Status: "done", Result: "mockup implemented"}}},
		inbox:              map[string][]AgentInboxMessage{},
		blackboard:         map[string][]AgentBlackboardEntry{},
		memories:           map[string][]AgentMemoryEntry{},
		archives:           map[string][]AgentArchiveMessage{},
	}
	task := Task{ID: "task-1", ProjectID: project.ID, Status: "done", Result: "mockup implemented"}
	a.notifyTaskRuntimeHooks(project, task)
	deadline := time.Now().Add(2 * time.Second)
	for {
		messages := a.agentMessagesFor(project.ID, "designer")
		found := false
		for _, message := range messages {
			if message.Intent == "task_result" {
				found = true
				break
			}
		}
		if found {
			if !a.scheduledAgentWorkerActive(project.ID, "designer") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("task event did not produce an agent result: %+v", messages)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hooks := a.taskHooks[hookKey]; len(hooks) != 1 || hooks[0].Status != "delivered" {
		t.Fatalf("task hooks = %+v", hooks)
	}
}

func TestRuntimeIdleStateConsidersAgentsTasksAndHooks(t *testing.T) {
	a := &app{
		tasks:        map[string][]Task{"p1": {{ID: "t1", ProjectID: "p1", Status: "done"}}},
		agentRuns:    map[string]AgentRun{},
		runtimeHooks: map[string]bool{},
	}
	if !a.projectRuntimeIdle("p1") {
		t.Fatal("done task with no agents should be idle")
	}
	a.tasks["p1"] = []Task{{ID: "t1", ProjectID: "p1", Status: "pending"}}
	if !a.projectRuntimeIdle("p1") {
		t.Fatal("pending task should not block runtime quiescence")
	}
	if !a.projectBacklogNotEmpty("p1") {
		t.Fatal("pending task should be backlog")
	}
	a.tasks["p1"] = []Task{{ID: "t1", ProjectID: "p1", Status: "running"}}
	if a.projectRuntimeIdle("p1") {
		t.Fatal("running task should block runtime quiescence")
	}
	a.tasks["p1"] = nil
	a.agentRuns[agentMessageKey("p1", "worker")] = AgentRun{ID: "r1", ProjectID: "p1", AgentID: "worker", State: RunStateInvokingModel}
	if a.projectRuntimeIdle("p1") {
		t.Fatal("active agent should block idle reconciliation")
	}
	a.agentRuns = map[string]AgentRun{}
	a.runtimeHooks["p1/"+karozIdleReconcileHook] = true
	if a.projectRuntimeIdle("p1") {
		t.Fatal("active runtime hook should block normal idle")
	}
	if !a.projectRuntimeIdleIgnoringHook("p1", karozIdleReconcileHook) {
		t.Fatal("current hook should be ignored by hook-local idle recheck")
	}
}

func TestBlackboardEntryActionableRules(t *testing.T) {
	if !blackboardEntryActionable(AgentBlackboardEntry{ActivityKind: "blocker", Summary: "blocked", Status: "active"}) {
		t.Fatal("blocker should be actionable")
	}
	if blackboardEntryActionable(AgentBlackboardEntry{ActivityKind: "progress", Summary: "working", Status: "active"}) {
		t.Fatal("plain progress should not be actionable")
	}
	if !blackboardEntryActionable(AgentBlackboardEntry{ActivityKind: "progress", Summary: "需要产品协调下一步", Status: "active"}) {
		t.Fatal("action marker should make entry actionable")
	}
	if blackboardEntryActionable(AgentBlackboardEntry{ActivityKind: "blocker", Summary: "old", Status: "done"}) {
		t.Fatal("done entry should not be actionable")
	}
	now := time.Now().UTC()
	if blackboardEntryActionable(AgentBlackboardEntry{ActivityKind: "blocker", Summary: "handled", Status: "active", HandledAt: &now}) {
		t.Fatal("handled entry should not be actionable")
	}
}

func TestMarkBlackboardActivityConsumesSignal(t *testing.T) {
	projectID := "p1"
	a := &app{
		settings:   Settings{DataDir: t.TempDir()},
		blackboard: map[string][]AgentBlackboardEntry{},
	}
	agent := Agent{ID: "karoz", ProjectID: projectID, Name: "Karoz"}
	entry := a.appendBlackboardEntry(projectID, agent, "blocker", "需要协调", "details", "")
	if !a.projectBacklogNotEmpty(projectID) {
		t.Fatal("actionable blackboard entry should be backlog")
	}
	got := a.markBlackboardActivity(projectID, agent, map[string]any{
		"activity_id":        entry.ID,
		"handling_result":    "routed_to_inbox",
		"routed_to_agent_id": "worker",
	})
	if strings.Contains(got, `"error"`) {
		t.Fatalf("mark activity failed: %s", got)
	}
	if a.projectBacklogNotEmpty(projectID) {
		t.Fatal("handled blackboard signal should no longer be backlog")
	}
}

func TestDeleteProjectAgentRemovesAgentAndRoutes(t *testing.T) {
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a := &app{
		settings:    Settings{DataDir: t.TempDir()},
		agents:      map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Name: "Karoz"}, {ID: "frontend", ProjectID: "p1", Name: "Frontend"}}},
		agentRoutes: map[string][]AgentRoute{"p1": {{ID: "r1", ProjectID: "p1", FromAgentID: "karoz", ToAgentID: "frontend", Intent: "request"}}},
		agentRuns:   map[string]AgentRun{agentMessageKey("p1", "frontend"): {ID: "r1", ProjectID: "p1", AgentID: "frontend", State: RunStateExecutingTool, Interrupts: []AgentInterrupt{{AgentID: "frontend", Body: "pending"}}}},
	}
	if err := a.deleteProjectAgent(project, "karoz"); err == nil {
		t.Fatal("default agent delete should fail")
	}
	if err := a.deleteProjectAgent(project, "frontend"); err != nil {
		t.Fatal(err)
	}
	if len(a.agents["p1"]) != 1 || a.agents["p1"][0].ID != "karoz" {
		t.Fatalf("agents = %+v", a.agents["p1"])
	}
	if len(a.agentRoutes["p1"]) != 0 {
		t.Fatalf("routes = %+v", a.agentRoutes["p1"])
	}
	if _, ok := a.agentRuns[agentMessageKey("p1", "frontend")]; ok {
		t.Fatal("agent run state was not cleared")
	}
}

func TestKarozOnlyAgentManagementTools(t *testing.T) {
	project := Project{ID: "p1", Name: "demo", Path: t.TempDir(), DefaultBranch: "main"}
	a := &app{
		settings:      Settings{DataDir: t.TempDir()},
		agents:        map[string][]Agent{"p1": {{ID: "karoz", ProjectID: "p1", Name: "Karoz"}}},
		agentRoutes:   map[string][]AgentRoute{},
		agentMessages: map[string][]AgentMessage{},
		agentSessions: map[string]AgentSessionState{},
		agentRuns:     map[string]AgentRun{},
	}
	karozSpecs := toolSpecNames(a.residentToolSpecsForContext(context.Background(), project.Path, Agent{ID: "karoz"}))
	if !karozSpecs["list_agent_templates"] || !karozSpecs["add_agent"] || !karozSpecs["create_agent_team"] || !karozSpecs["delete_agent"] {
		t.Fatalf("karoz management tools missing: %+v", karozSpecs)
	}
	frontendSpecs := toolSpecNames(a.residentToolSpecsForContext(context.Background(), project.Path, Agent{ID: "frontend"}))
	if frontendSpecs["list_agent_templates"] || frontendSpecs["add_agent"] || frontendSpecs["create_agent_team"] || frontendSpecs["delete_agent"] {
		t.Fatalf("non-karoz should not see management tools: %+v", frontendSpecs)
	}
	result := a.listAgentTemplatesFromResidentTool(Agent{ID: "karoz"}, map[string]any{"query": "product"})
	if !strings.Contains(result, "product-strategist") || !strings.Contains(result, "product-discovery") {
		t.Fatalf("template list result = %s", result)
	}
	result = a.addAgentFromResidentTool(project, Agent{ID: "frontend"}, map[string]any{"template_id": "frontend-specialist"})
	if !strings.Contains(result, "forbidden") {
		t.Fatalf("non-karoz add result = %s", result)
	}
	result = a.addAgentFromResidentTool(project, Agent{ID: "karoz"}, map[string]any{"template_id": "frontend-specialist", "nickname": "Frontend"})
	if !strings.Contains(result, `"agent"`) || len(a.agents["p1"]) != 2 {
		t.Fatalf("karoz add result = %s agents=%+v", result, a.agents["p1"])
	}
	result = a.deleteAgentFromResidentTool(project, Agent{ID: "frontend"}, map[string]any{"agent_id": "frontend"})
	if !strings.Contains(result, "forbidden") {
		t.Fatalf("non-karoz delete result = %s", result)
	}
	result = a.deleteAgentFromResidentTool(project, Agent{ID: "karoz"}, map[string]any{"agent_id": "frontend"})
	if !strings.Contains(result, `"deleted":true`) || len(a.agents["p1"]) != 1 {
		t.Fatalf("karoz delete result = %s agents=%+v", result, a.agents["p1"])
	}
	result = a.createAgentTeamFromResidentTool(project, Agent{ID: "karoz"}, map[string]any{"template_id": "product-discovery", "instance": "discovery"})
	if !strings.Contains(result, `"created":3`) || len(a.agents["p1"]) != 4 {
		t.Fatalf("karoz create team result = %s agents=%+v", result, a.agents["p1"])
	}
	routes := a.routesForProject("p1")
	foundKarozRoute := false
	for _, route := range routes {
		if route.FromAgentID == "karoz" && strings.HasSuffix(route.ToAgentID, "-facilitator") && route.Intent == "request" {
			foundKarozRoute = true
			break
		}
	}
	if !foundKarozRoute {
		t.Fatalf("expected karoz request route to product discovery facilitator, routes=%+v", routes)
	}
}

func TestAgentCapabilitiesKeepAllResidentsDirectAndKarozPrivileged(t *testing.T) {
	ordinary := capabilitiesForAgent(Agent{ID: "frontend", TemplateID: "frontend-specialist", Role: "frontend"})
	if !ordinary.CanDirectChat || !ordinary.CanCreateTasks || !ordinary.CanDelegate || !ordinary.CanCreateArtifacts {
		t.Fatalf("ordinary resident capabilities = %+v", ordinary)
	}
	if ordinary.CanManageAgents || ordinary.CanManageRoutes || ordinary.CanInspectProjectWide || ordinary.CanReconcileBacklog {
		t.Fatalf("ordinary resident received project-wide capabilities = %+v", ordinary)
	}

	karoz := capabilitiesForAgent(Agent{ID: "karoz", TemplateID: "karoz", Role: "coordinator"})
	if !karoz.CanDirectChat || !karoz.CanManageAgents || !karoz.CanManageRoutes || !karoz.CanInspectProjectWide || !karoz.CanReconcileBacklog {
		t.Fatalf("karoz capabilities = %+v", karoz)
	}
}

func TestDefaultKarozAndProductDesignerTemplates(t *testing.T) {
	karoz := defaultKarozAgentTemplate()
	if karoz.ID != "karoz" || karoz.DisplayName != "Karoz" {
		t.Fatalf("default karoz template = %+v", karoz)
	}
	if _, ok := residentAgentTemplateByID("karoz"); ok {
		t.Fatal("karoz must not be exposed as a cloneable resident template")
	}
	designerTemplate, ok := residentAgentTemplateByID("product-designer")
	if !ok {
		t.Fatal("product designer template is missing")
	}
	designer := newAgentFromTemplate(Project{ID: "p1"}, designerTemplate, "designer", "Designer")
	capabilities := capabilitiesForAgent(designer)
	if !capabilities.CanDirectChat || !capabilities.CanCreateArtifacts || !capabilities.CanDesignArtifacts {
		t.Fatalf("designer capabilities = %+v", capabilities)
	}
	designPrompt := strings.ToLower(designer.SystemPrompt + residentDesignAgentPrompt())
	if !strings.Contains(designPrompt, "concrete") || !strings.Contains(designPrompt, "artifact") {
		t.Fatalf("designer prompt does not require concrete artifacts: %s", designPrompt)
	}
}

func toolSpecNames(specs []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, spec := range specs {
		name, _ := spec["name"].(string)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func TestResidentToolRegistryCoversStaticDefinitions(t *testing.T) {
	a := &app{}
	expected := toolSpecNames(append(residentToolSpecs(), residentAgentManagementToolSpecs()...))
	delete(expected, "bash")
	definitions := a.residentToolRegistry().Definitions()
	if len(definitions) != len(expected) {
		t.Fatalf("registry definitions = %d, expected %d", len(definitions), len(expected))
	}
	for _, definition := range definitions {
		if !expected[definition.Name] {
			t.Fatalf("unexpected registry definition %q", definition.Name)
		}
		delete(expected, definition.Name)
	}
	if len(expected) != 0 {
		t.Fatalf("registry definitions missing: %+v", expected)
	}

	result, err := a.executeResidentTool(context.Background(), ResidentToolContext{}, codexToolCall{Name: "does_not_exist", Arguments: `{}`})
	if err != nil || !strings.Contains(result, `"error":"unknown_tool"`) {
		t.Fatalf("unknown tool result = %s, err = %v", result, err)
	}
}

func TestWebSearchAndFetchTools(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<html><body><a class="result__a" href="/l/?uddg=%s">Example Result</a><div class="result__snippet">Useful snippet</div></body></html>`, urlQueryEscape(serverURL+"/page"))
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<html><head><title>Example Page</title><style>.x{}</style></head><body><h1>Hello</h1><p>Readable text.</p><script>bad()</script></body></html>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	t.Setenv("KAROZ_WEB_SEARCH_ENDPOINT", server.URL+"/search")

	search := webSearchTool(context.Background(), map[string]any{"query": "example", "limit": float64(3)})
	if !strings.Contains(search, "Example Result") || !strings.Contains(search, server.URL+"/page") || !strings.Contains(search, "Useful snippet") {
		t.Fatalf("web_search result = %s", search)
	}
	fetched := webFetchTool(context.Background(), map[string]any{"url": server.URL + "/page", "max_chars": float64(2000)})
	if !strings.Contains(fetched, "Example Page") || !strings.Contains(fetched, "Readable text") || strings.Contains(fetched, "bad()") {
		t.Fatalf("web_fetch result = %s", fetched)
	}
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, ":", "%3A"), "/", "%2F")
}

func runFakeMCPServer(t *testing.T) {
	t.Helper()
	reader := bufio.NewReader(os.Stdin)
	for {
		msg, err := readFakeMCPMessage(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			t.Fatal(err)
		}
		var req struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(msg, &req); err != nil {
			t.Fatal(err)
		}
		if req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			writeFakeMCPMessage(t, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "test"},
			}})
		case "tools/list":
			writeFakeMCPMessage(t, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echo text",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
						"required": []string{"text"},
					},
				}},
			}})
		case "tools/call":
			arguments, _ := req.Params["arguments"].(map[string]any)
			text, _ := arguments["text"].(string)
			writeFakeMCPMessage(t, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo:" + text}},
				"isError": false,
			}})
		default:
			writeFakeMCPMessage(t, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"message": "unknown method"}})
		}
	}
}

func readFakeMCPMessage(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	data := make([]byte, contentLength)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func writeFakeMCPMessage(t *testing.T, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
		t.Fatal(err)
	}
}
