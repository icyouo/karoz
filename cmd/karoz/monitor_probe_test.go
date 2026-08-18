//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

func TestMonitorProbeApprovalClaimDryRunFireAndTamper(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := Project{
		ID: projectID(path), Name: "project", Path: path,
	}
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	t.Cleanup(a.shutdownMonitorProbes)
	owner := Agent{
		ID: "owner", ProjectID: project.ID, Name: "Owner",
		CreatedAt: time.Now().UTC(),
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}

	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID:    owner.ID,
			Language:   "shell",
			Workdir:    path,
			Source:     "printf '{\"matched\":true,\"detail\":\"ready\"}\\r\\n'\r\n",
			IntervalMS: minimumMonitorProbeInterval,
			TimeoutMS:  1_000,
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(challenge["source"].(string), "\r\n") {
		t.Fatalf("probe source was not normalized once: %q", challenge["source"])
	}
	receiptView, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := receiptView["id"].(string)
	monitor, err := a.claimScriptProbeMonitor(
		project,
		owner,
		Monitor{
			ID:    challenge["monitor_id"].(string),
			Name:  "ready",
			State: monitordomain.StateActive,
			Trigger: monitordomain.Trigger{
				Kind:              monitordomain.TriggerScriptProbe,
				ApprovalReceiptID: receiptID,
			},
			Action: monitordomain.Action{
				Kind:     monitordomain.ActionBlackboard,
				Revision: 1,
				Topic:    "probe",
				Template: "probe ready",
			},
		},
		receiptID,
		"mutation-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(dataDir, "monitors.json")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.runMonitorProbe(
		a.monitorCtx,
		project.ID,
		monitor.ID,
		time.Now().UTC(),
		true,
	)
	if err != nil || !result.Matched || result.Detail != "ready" {
		t.Fatalf("dry run result=%+v err=%v", result, err)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry run mutated the durable monitor registry")
	}
	if _, err := a.runMonitorProbe(
		a.monitorCtx,
		project.ID,
		monitor.ID,
		time.Now().UTC(),
		false,
	); err != nil {
		t.Fatal(err)
	}
	got := a.monitorsForProject(project.ID)[0]
	if got.TriggerCount != 1 || got.LastCheckedAt == nil {
		t.Fatalf("probe result was not committed: %+v", got)
	}
	if entries := a.blackboardFor(project.ID, 10); len(entries) != 1 {
		t.Fatalf("probe action was not dispatched: %+v", entries)
	}
	public, err := json.Marshal(publicMonitor(got))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "printf") ||
		strings.Contains(string(public), receiptID) ||
		strings.Contains(string(public), "monitor-probes") {
		t.Fatalf("public monitor leaked probe authorization: %s", public)
	}

	receipt := a.monitorProbeReceipts[receiptID]
	if err := os.WriteFile(
		filepath.Join(dataDir, receipt.SnapshotPath),
		[]byte("printf '{\"matched\":false}'\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := a.runMonitorProbe(
		a.monitorCtx,
		project.ID,
		monitor.ID,
		time.Now().UTC(),
		false,
	); err == nil {
		t.Fatal("tampered probe snapshot executed")
	}
	got = a.monitorsForProject(project.ID)[0]
	if got.State != monitordomain.StateError ||
		got.ErrorCode != "probe_authorization" {
		t.Fatalf("tampered probe did not fail closed: %+v", got)
	}
}

func TestMonitorProbeReceiptCannotCrossMonitorRevisionOrMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := Project{ID: projectID(path), Name: "project", Path: path}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	t.Cleanup(a.shutdownMonitorProbes)
	owner := Agent{ID: "owner", ProjectID: project.ID, CreatedAt: time.Now().UTC()}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell", Workdir: path,
			Source: "echo '{\"matched\":false}'",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := view["id"].(string)
	base := Monitor{
		Name:    "probe",
		Trigger: monitordomain.Trigger{Kind: monitordomain.TriggerScriptProbe},
		Action: monitordomain.Action{
			Kind:     monitordomain.ActionBlackboard,
			Revision: 1,
			Topic:    "probe",
		},
	}
	wrong := base
	wrong.ID = "another-monitor"
	if _, err := a.claimScriptProbeMonitor(
		project, owner, wrong, receiptID, "mutation",
	); err == nil {
		t.Fatal("receipt crossed monitor identity")
	}
	base.ID = challenge["monitor_id"].(string)
	if _, err := a.claimScriptProbeMonitor(
		project, owner, base, receiptID, "mutation",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := a.claimScriptProbeMonitor(
		project, owner, base, receiptID, "different-mutation",
	); err == nil {
		t.Fatal("claimed receipt replayed under another mutation")
	}
}

func TestMonitorProbeAgentChoiceClaimIsExactAndIdempotent(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	toolCtx := ResidentToolContext{
		Project:  project,
		Agent:    owner,
		RunID:    "request-run",
		TurnType: "ask",
		Workdir:  project.Path,
	}
	raw := a.prepareMonitorProbeFromTool(toolCtx, map[string]any{
		"language": "shell",
		"source":   "printf '{\"matched\":false}'\n",
	})
	var choice struct {
		Kind    string `json:"kind"`
		Choices []struct {
			ID string `json:"id"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &choice); err != nil {
		t.Fatal(err)
	}
	if choice.Kind != "choice_request" || len(choice.Choices) != 2 {
		t.Fatalf("unexpected approval response: %s", raw)
	}
	recognized, err := a.resolveMonitorProbeChoice(
		project.ID,
		owner.ID,
		"approval-run",
		choice.Choices[0].ID,
	)
	if err != nil || !recognized {
		t.Fatalf("resolve choice recognized=%v err=%v", recognized, err)
	}
	var receipt monitordomain.ProbeApprovalReceipt
	for _, candidate := range a.monitorProbeReceipts {
		receipt = candidate
	}
	if receipt.ApprovalFlow != "agent_choice" ||
		receipt.ApprovalRunID != "approval-run" ||
		receipt.ChoiceRequestID == "" {
		t.Fatalf("approval provenance not bound: %+v", receipt)
	}
	item := Monitor{
		ID: receipt.MonitorID, Name: "agent choice",
		State: monitordomain.StateDisabled,
		Action: monitordomain.Action{
			Kind:     monitordomain.ActionBlackboard,
			Revision: 1,
			Topic:    "probe",
		},
	}
	created, err := a.claimScriptProbeMonitor(
		project,
		owner,
		item,
		receipt.ID,
		"exact-mutation",
	)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := a.claimScriptProbeMonitor(
		project,
		owner,
		item,
		receipt.ID,
		"exact-mutation",
	)
	if err != nil || retried.ID != created.ID ||
		len(a.monitorsForProject(project.ID)) != 1 {
		t.Fatalf("idempotent retry=%+v err=%v", retried, err)
	}
}

func TestMonitorProbeConcurrentUIConfirmationIsIdempotent(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":true,\"detail\":\"ui-concurrent\"}'\n",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	start := make(chan struct{})
	views := make([]map[string]any, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			views[index], errs[index] = a.confirmMonitorProbeApproval(
				project,
				challenge["challenge_id"].(string),
				session,
			)
		}(index)
	}
	close(start)
	wait.Wait()
	receiptID := ""
	for index := 0; index < callers; index++ {
		if errs[index] != nil {
			t.Fatalf("confirm %d: %v", index, errs[index])
		}
		currentID, _ := views[index]["id"].(string)
		if currentID == "" {
			t.Fatalf("confirm %d returned no receipt: %+v", index, views[index])
		}
		if receiptID == "" {
			receiptID = currentID
		} else if currentID != receiptID {
			t.Fatalf("confirm identities differ: %s != %s", currentID, receiptID)
		}
	}
	claimed := claimConcurrentProbeReceiptForTest(
		t,
		a,
		project,
		owner,
		challenge["monitor_id"].(string),
		receiptID,
		"ui-concurrent-mutation",
	)
	result, err := a.runMonitorProbe(
		context.Background(),
		project.ID,
		claimed.ID,
		time.Now().UTC(),
		true,
	)
	if err != nil || !result.Matched || result.Detail != "ui-concurrent" {
		t.Fatalf("confirmed UI receipt result=%+v err=%v", result, err)
	}
}

func TestMonitorProbeConcurrentAgentConfirmationIsIdempotent(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	raw := a.prepareMonitorProbeFromTool(
		ResidentToolContext{
			Project: project, Agent: owner,
			RunID: "request-run", TurnType: "ask",
			Workdir: project.Path,
		},
		map[string]any{
			"language": "shell",
			"source":   "printf '{\"matched\":true,\"detail\":\"agent-concurrent\"}'\n",
		},
	)
	var choice struct {
		MonitorID string `json:"monitor_id"`
		Choices   []struct {
			ID string `json:"id"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &choice); err != nil {
		t.Fatal(err)
	}
	if choice.MonitorID == "" || len(choice.Choices) == 0 {
		t.Fatalf("unexpected approval response: %s", raw)
	}
	const callers = 16
	start := make(chan struct{})
	recognized := make([]bool, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			recognized[index], errs[index] = a.resolveMonitorProbeChoice(
				project.ID,
				owner.ID,
				"approval-run",
				choice.Choices[0].ID,
			)
		}(index)
	}
	close(start)
	wait.Wait()
	for index := 0; index < callers; index++ {
		if errs[index] != nil || !recognized[index] {
			t.Fatalf(
				"agent confirm %d recognized=%v err=%v",
				index,
				recognized[index],
				errs[index],
			)
		}
	}
	if len(a.monitorProbeReceipts) != 1 {
		t.Fatalf("concurrent agent confirm minted %d receipts", len(a.monitorProbeReceipts))
	}
	var receiptID string
	for id := range a.monitorProbeReceipts {
		receiptID = id
	}
	claimed := claimConcurrentProbeReceiptForTest(
		t,
		a,
		project,
		owner,
		choice.MonitorID,
		receiptID,
		"agent-concurrent-mutation",
	)
	result, err := a.runMonitorProbe(
		context.Background(),
		project.ID,
		claimed.ID,
		time.Now().UTC(),
		true,
	)
	if err != nil || !result.Matched || result.Detail != "agent-concurrent" {
		t.Fatalf("confirmed agent receipt result=%+v err=%v", result, err)
	}
}

func TestMonitorProbeNoOverlapAndThreeErrorsDisable(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	slow := claimMonitorProbeForTest(
		t,
		a,
		project,
		owner,
		"sleep 0.25\nprintf '{\"matched\":false}'\n",
		1_000,
	)
	var wait sync.WaitGroup
	wait.Add(1)
	var firstErr error
	go func() {
		defer wait.Done()
		_, firstErr = a.runMonitorProbe(
			a.monitorCtx,
			project.ID,
			slow.ID,
			time.Now().UTC(),
			true,
		)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		item, ok := a.monitorByID(project.ID, slow.ID)
		if ok && item.ProbeRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first probe did not enter running state")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := a.runMonitorProbe(
		a.monitorCtx,
		project.ID,
		slow.ID,
		time.Now().UTC(),
		true,
	); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("overlapping probe was not rejected: %v", err)
	}
	wait.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}

	broken := claimMonitorProbeForTest(
		t,
		a,
		project,
		owner,
		"printf '{\"matched\":false,\"unexpected\":\"ignored\"}'\n",
		1_000,
	)
	for index := 0; index < 3; index++ {
		if _, err := a.runMonitorProbe(
			a.monitorCtx,
			project.ID,
			broken.ID,
			time.Now().UTC(),
			false,
		); err == nil {
			t.Fatalf("malformed result %d unexpectedly succeeded", index)
		} else if !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("malformed result %d returned unexpected error: %v", index, err)
		}
	}
	got, _ := a.monitorByID(project.ID, broken.ID)
	if got.State != monitordomain.StateError ||
		got.ErrorCode != "probe_error" ||
		got.ConsecutiveProbeErrors != 3 {
		t.Fatalf("three errors did not disable probe: %+v", got)
	}
}

func claimConcurrentProbeReceiptForTest(
	t *testing.T,
	a *app,
	project Project,
	owner Agent,
	monitorID, receiptID, mutationID string,
) Monitor {
	t.Helper()
	item, err := a.claimScriptProbeMonitor(
		project,
		owner,
		Monitor{
			ID: monitorID, Name: "concurrent",
			State: monitordomain.StateDisabled,
			Action: monitordomain.Action{
				Kind:     monitordomain.ActionBlackboard,
				Revision: 1,
				Topic:    "probe",
			},
		},
		receiptID,
		mutationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestMonitorProbeRestartPreservesReceiptWithoutCatchup(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := Project{
		ID: projectID(projectPath), Name: "project", Path: projectPath,
	}
	dataDir := t.TempDir()
	first := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	owner := Agent{
		ID: "owner", ProjectID: project.ID,
		CreatedAt: time.Now().UTC(),
	}
	first.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	item := claimMonitorProbeForTest(
		t,
		first,
		project,
		owner,
		"printf '{\"matched\":true,\"detail\":\"restart\"}'\n",
		1_000,
	)
	first.shutdownMonitorProbes()

	restarted := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	t.Cleanup(restarted.shutdownMonitorProbes)
	if err := restarted.loadMonitors(); err != nil {
		t.Fatal(err)
	}
	loaded, ok := restarted.monitorByID(project.ID, item.ID)
	if !ok || loaded.TriggerCount != 0 || loaded.LastCheckedAt != nil {
		t.Fatalf("restart replayed historical probe: %+v", loaded)
	}
	result, err := restarted.runMonitorProbe(
		context.Background(),
		project.ID,
		item.ID,
		time.Now().UTC(),
		true,
	)
	if err != nil || !result.Matched || result.Detail != "restart" {
		t.Fatalf("restarted dry run result=%+v err=%v", result, err)
	}
}

func TestMonitorProbeHTTPOnlyPreparesAndConfirms(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	body := `{"agent_id":"owner","language":"shell","source":"printf '{\"matched\":false}'\n"}`
	url := "/api/projects/" + project.ID + "/monitor-probe-approvals/prepare"
	untrusted := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	untrusted.Header.Set("Content-Type", "application/json")
	untrusted.Host = "127.0.0.1:43110"
	rejected := httptest.NewRecorder()
	a.handleProjectScoped(rejected, untrusted)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("missing Origin status=%d body=%s", rejected.Code, rejected.Body.String())
	}

	prepare := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
	prepare.Header.Set("Content-Type", "application/json")
	prepare.Header.Set("Origin", "http://127.0.0.1:43110")
	prepare.Host = "127.0.0.1:43110"
	prepared := httptest.NewRecorder()
	a.handleProjectScoped(prepared, prepare)
	if prepared.Code != http.StatusOK {
		t.Fatalf("prepare status=%d body=%s", prepared.Code, prepared.Body.String())
	}
	var response struct {
		Challenge map[string]any `json:"challenge"`
	}
	if err := json.Unmarshal(prepared.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	challengeID, _ := response.Challenge["challenge_id"].(string)
	if challengeID == "" || len(a.monitorsForProject(project.ID)) != 0 {
		t.Fatalf("prepare created a monitor: %+v", response)
	}
	cookies := prepared.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("approval session cookie is not hardened: %+v", cookies)
	}
	confirmURL := "/api/projects/" + project.ID +
		"/monitor-probe-approvals/" + challengeID + "/confirm"
	confirm := httptest.NewRequest(http.MethodPost, confirmURL, strings.NewReader(`{}`))
	confirm.Header.Set("Content-Type", "application/json")
	confirm.Header.Set("Origin", "http://127.0.0.1:43110")
	confirm.Host = "127.0.0.1:43110"
	confirm.AddCookie(cookies[0])
	confirmed := httptest.NewRecorder()
	a.handleProjectScoped(confirmed, confirm)
	if confirmed.Code != http.StatusOK ||
		len(a.monitorsForProject(project.ID)) != 0 ||
		!strings.Contains(confirmed.Body.String(), `"dev_turn_required":true`) {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	if strings.Contains(confirmed.Body.String(), "printf") ||
		strings.Contains(confirmed.Body.String(), "monitor-probes") {
		t.Fatalf("confirm leaked probe source/path: %s", confirmed.Body.String())
	}

	direct := httptest.NewRequest(
		http.MethodPost,
		"/api/projects/"+project.ID+"/monitors",
		strings.NewReader(`{"agent_id":"`+owner.ID+`","trigger":{"kind":"script_probe"}}`),
	)
	direct.Header.Set("Content-Type", "application/json")
	direct.Header.Set("Origin", "http://127.0.0.1:43110")
	direct.Host = "127.0.0.1:43110"
	directResult := httptest.NewRecorder()
	a.handleProjectScoped(directResult, direct)
	if directResult.Code != http.StatusBadRequest ||
		len(a.monitorsForProject(project.ID)) != 0 {
		t.Fatalf("HTTP created probe status=%d body=%s", directResult.Code, directResult.Body.String())
	}
}

func TestMonitorProbeTimeoutKillsDescendantProcessGroup(t *testing.T) {
	execution, _, err := executeMonitorProbe(
		context.Background(),
		"shell",
		t.TempDir(),
		[]byte("sleep 30 &\nprintf '%s\\n' \"$!\"\nwait\n"),
		100*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !execution.TimedOut {
		t.Fatalf("probe did not time out: %+v", execution)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(execution.Stdout)))
	if err != nil || pid <= 0 {
		t.Fatalf("missing descendant pid stdout=%q err=%v", execution.Stdout, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed-out probe descendant %d remains: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMonitorProbeInterruptedStagingRetriesSameReceipt(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	challengeView, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":false}'\n",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	challengeID := challengeView["challenge_id"].(string)
	receiptID := randomID()
	a.mu.Lock()
	challenge := a.monitorProbeChallenges[challengeID]
	challenge.State = "staging"
	challenge.StagingReceiptID = receiptID
	challenge.StagingPath = filepath.Join(
		"monitor-probes",
		monitordomain.SafeProjectKey(project.ID),
		"receipts",
		receiptID,
		challenge.SourceSHA256+".sh",
	)
	a.monitorProbeChallenges[challengeID] = challenge
	if err := a.saveMonitorsLocked(); err != nil {
		a.mu.Unlock()
		t.Fatal(err)
	}
	a.mu.Unlock()
	view, err := a.confirmMonitorProbeApproval(
		project,
		challengeID,
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	if view["id"] != receiptID ||
		a.monitorProbeChallenges[challengeID].ConsumedReceiptID != receiptID {
		t.Fatalf("staging retry changed identity: %+v", view)
	}
	retry, err := a.confirmMonitorProbeApproval(
		project,
		challengeID,
		session,
	)
	if err != nil || retry["id"] != receiptID {
		t.Fatalf("consumed retry=%+v err=%v", retry, err)
	}
}

func TestMonitorProbeOwnerIdentityAndRegistryCorruptionFailClosed(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":false}'\n",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	recreated := owner
	recreated.CreatedAt = owner.CreatedAt.Add(time.Second)
	a.mu.Lock()
	a.agentDirectoryLocked().agents[project.ID] = []Agent{recreated}
	a.mu.Unlock()
	if _, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	); err == nil {
		t.Fatal("recreated same-ID owner consumed stale approval")
	}

	dataDir := t.TempDir()
	registryPath := filepath.Join(dataDir, "monitors.json")
	corrupt := []byte("{broken")
	if err := os.WriteFile(registryPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	if err := reloaded.loadMonitors(); err == nil {
		t.Fatal("corrupt monitor registry loaded as empty")
	}
	got, err := os.ReadFile(registryPath)
	if err != nil || string(got) != string(corrupt) {
		t.Fatalf("corrupt registry was rewritten/quarantined: %q err=%v", got, err)
	}
}

func TestMonitorProbeProjectCapsFailBeforeMutation(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":false}'\n",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := view["id"].(string)
	a.mu.Lock()
	for index := 0; index < maximumEnabledProbes; index++ {
		a.monitors[project.ID] = append(a.monitors[project.ID], Monitor{
			ID:        "existing-" + strconv.Itoa(index),
			ProjectID: project.ID,
			AgentID:   owner.ID,
			State:     monitordomain.StateActive,
			Trigger: monitordomain.Trigger{
				Kind: monitordomain.TriggerScriptProbe,
			},
		})
	}
	a.mu.Unlock()
	if _, err := a.claimScriptProbeMonitor(
		project,
		owner,
		Monitor{
			ID:    challenge["monitor_id"].(string),
			State: monitordomain.StateActive,
			Action: monitordomain.Action{
				Kind:     monitordomain.ActionBlackboard,
				Revision: 1,
				Topic:    "probe",
			},
		},
		receiptID,
		"capacity-mutation",
	); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("enabled cap error=%v", err)
	}
	if a.monitorProbeReceipts[receiptID].ClaimedMutationID != "" {
		t.Fatal("enabled-cap rejection consumed receipt")
	}

	a.mu.Lock()
	a.monitors[project.ID] = nil
	a.mu.Unlock()
	created, err := a.claimScriptProbeMonitor(
		project,
		owner,
		Monitor{
			ID:    challenge["monitor_id"].(string),
			State: monitordomain.StateActive,
			Action: monitordomain.Action{
				Kind:     monitordomain.ActionBlackboard,
				Revision: 1,
				Topic:    "probe",
			},
		},
		receiptID,
		"capacity-mutation",
	)
	if err != nil {
		t.Fatal(err)
	}
	slot := a.monitorProbeProjectSlot(project.ID)
	for index := 0; index < maximumProbeConcurrency; index++ {
		slot <- struct{}{}
	}
	defer func() {
		for index := 0; index < maximumProbeConcurrency; index++ {
			<-slot
		}
	}()
	registryPath := filepath.Join(a.settings.DataDir, "monitors.json")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.runMonitorProbe(
		context.Background(),
		project.ID,
		created.ID,
		time.Now().UTC(),
		true,
	); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("concurrency cap error=%v", err)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("dry-run concurrency rejection mutated registry")
	}
}

func TestMonitorProbeClaimSaveFailureRollsBackAndRetries(t *testing.T) {
	a, project, owner := newMonitorProbeTestApp(t)
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":false}'\n",
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptID := view["id"].(string)
	registryPath := filepath.Join(a.settings.DataDir, "monitors.json")
	backupPath := filepath.Join(a.settings.DataDir, "monitors.backup")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(registryPath, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	item := Monitor{
		ID:    challenge["monitor_id"].(string),
		State: monitordomain.StateDisabled,
		Action: monitordomain.Action{
			Kind:     monitordomain.ActionBlackboard,
			Revision: 1,
			Topic:    "probe",
		},
	}
	if _, err := a.claimScriptProbeMonitor(
		project,
		owner,
		item,
		receiptID,
		"retryable-mutation",
	); err == nil {
		t.Fatal("claim unexpectedly survived registry save failure")
	}
	if len(a.monitorsForProject(project.ID)) != 0 ||
		a.monitorProbeReceipts[receiptID].ClaimedMutationID != "" {
		t.Fatal("failed claim left a live monitor or consumed receipt")
	}
	if err := os.Remove(registryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backupPath, registryPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed claim changed durable bytes: err=%v", err)
	}
	if _, err := a.claimScriptProbeMonitor(
		project,
		owner,
		item,
		receiptID,
		"retryable-mutation",
	); err != nil {
		t.Fatalf("claim retry failed: %v", err)
	}
}

func newMonitorProbeTestApp(t *testing.T) (*app, Project, Agent) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := Project{
		ID: projectID(path), Name: "project", Path: path,
	}
	a := newApp(Settings{DataDir: t.TempDir(), ProjectsRoot: root})
	t.Cleanup(a.shutdownMonitorProbes)
	owner := Agent{
		ID: "owner", ProjectID: project.ID,
		CreatedAt: time.Now().UTC(),
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	return a, project, owner
}

func claimMonitorProbeForTest(
	t *testing.T,
	a *app,
	project Project,
	owner Agent,
	source string,
	timeoutMS int64,
) Monitor {
	t.Helper()
	challenge, session, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path, Source: source,
			IntervalMS: minimumMonitorProbeInterval,
			TimeoutMS:  timeoutMS,
		},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.confirmMonitorProbeApproval(
		project,
		challenge["challenge_id"].(string),
		session,
	)
	if err != nil {
		t.Fatal(err)
	}
	item, err := a.claimScriptProbeMonitor(
		project,
		owner,
		Monitor{
			ID:    challenge["monitor_id"].(string),
			Name:  "probe",
			State: monitordomain.StateActive,
			Action: monitordomain.Action{
				Kind:     monitordomain.ActionBlackboard,
				Revision: 1,
				Topic:    "probe",
			},
		},
		view["id"].(string),
		randomID(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return item
}
