package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestProjectAuditExport(t *testing.T) {
	a, project := newHandlerTestApp(t)
	base := "/api/projects/" + project.ID

	seed := serveHTTPRequest(a, http.MethodPost, base+"/tasks", `{"title":"audit seed task","type":"feature"}`)
	if seed.Code != http.StatusOK {
		t.Fatalf("seed task status=%d body=%s", seed.Code, limitString(seed.Body.String(), 200))
	}
	var task Task
	if err := json.Unmarshal(seed.Body.Bytes(), &task); err != nil || task.ID == "" {
		t.Fatalf("seed task decode err=%v id=%q", err, task.ID)
	}

	handoff := AgentInboxMessage{
		ID: "audit-handoff-1", ProjectID: project.ID, SourceAgentID: "karoz", TargetAgentID: "worker-a",
		MessageType: "handoff", Intent: "request", Subject: "Audit subject", Body: "audit body",
		CreatedAt: time.Now().UTC(),
	}
	if err := a.queueInboxMessage(project.ID, handoff); err != nil {
		t.Fatal(err)
	}
	blackboardEntry := a.appendBlackboardEntry(project.ID, Agent{ID: "worker-a", ProjectID: project.ID, Nickname: "Worker A"}, "progress", "audit marker", "audit detail", "")
	now := time.Now().UTC()
	a.memories[projectAgentKey(project.ID, "worker-a")] = []AgentMemoryEntry{{
		ID: "audit-mem-1", ProjectID: project.ID, AgentID: "worker-a", Layer: "fact", State: "active",
		Summary: "audit memory", Detail: "remembered for audit", CreatedAt: now, UpdatedAt: now,
	}}

	recorder := serveHTTPRequest(a, http.MethodGet, base+"/audit", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("audit status = %d body=%s", recorder.Code, limitString(recorder.Body.String(), 200))
	}
	var payload struct {
		ExportedAt string                 `json:"exported_at"`
		Project    Project                `json:"project"`
		Agents     []Agent                `json:"agents"`
		Tasks      []Task                 `json:"tasks"`
		Handoffs   []AgentInboxMessage    `json:"handoffs"`
		Blackboard []AgentBlackboardEntry `json:"blackboard"`
		Memories   []AgentMemoryEntry     `json:"memories"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("audit decode err=%v body=%s", err, limitString(recorder.Body.String(), 400))
	}
	if _, err := time.Parse(time.RFC3339, payload.ExportedAt); err != nil {
		t.Fatalf("exported_at is not RFC3339: %q", payload.ExportedAt)
	}
	if payload.Project.ID != project.ID {
		t.Fatalf("audit project = %+v", payload.Project)
	}
	agentIDs := map[string]bool{}
	for _, agent := range payload.Agents {
		agentIDs[agent.ID] = true
	}
	if !agentIDs["karoz"] || !agentIDs["worker-a"] || !agentIDs["worker-b"] {
		t.Fatalf("audit agents missing seeded agents: %+v", payload.Agents)
	}
	foundTask, foundHandoff, foundBlackboard, foundMemory := false, false, false, false
	for _, item := range payload.Tasks {
		if item.ID == task.ID {
			foundTask = true
		}
	}
	for _, item := range payload.Handoffs {
		if item.ID == handoff.ID {
			foundHandoff = true
		}
	}
	for _, item := range payload.Blackboard {
		if item.ID == blackboardEntry.ID {
			foundBlackboard = true
		}
	}
	for _, item := range payload.Memories {
		if item.ID == "audit-mem-1" {
			foundMemory = true
		}
	}
	if !foundTask || !foundHandoff || !foundBlackboard || !foundMemory {
		t.Fatalf("audit export missing seeded entities task=%v handoff=%v blackboard=%v memory=%v", foundTask, foundHandoff, foundBlackboard, foundMemory)
	}

	if got := serveHTTPRequest(a, http.MethodPost, base+"/audit", `{}`); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("audit POST status = %d, want 405; body=%s", got.Code, limitString(got.Body.String(), 200))
	}
	if got := serveHTTPRequest(a, http.MethodGet, "/api/projects/deadbeef0000/audit", ""); got.Code != http.StatusNotFound {
		t.Fatalf("audit unknown project status = %d, want 404; body=%s", got.Code, limitString(got.Body.String(), 200))
	}
}
