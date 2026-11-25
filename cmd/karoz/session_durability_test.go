package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	runtimedomain "github.com/karoz/karoz/internal/runtime"
)

// Force a real atomic-store failure while retaining the last durable file.
func blockSessionEventWrites(t *testing.T, a *app) func() {
	t.Helper()
	path := filepath.Join(a.settings.DataDir, "agent-session-events.json")
	backup := path + ".test-backup"
	hadFile := true
	if err := os.Rename(path, backup); os.IsNotExist(err) {
		hadFile = false
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if hadFile {
				if err := os.Rename(backup, path); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

func TestSessionWriteFailureLeavesAllConversationProjectionsUnchanged(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: "project", AgentID: "agent", RunID: "run"})
	if !started {
		t.Fatal("Run did not start")
	}
	key := projectAgentKey(run.ProjectID, run.AgentID)
	wantEvents := a.conversation.Snapshot()
	wantMessages := a.conversation.MessageSnapshot()
	wantSessions := a.conversation.SessionSnapshot()
	wantTranscript := a.conversation.TranscriptsFor(key)
	restore := blockSessionEventWrites(t, a)
	if msg, ok := a.appendAgentMessageForRun(run.ProjectID, run.AgentID, run.ID, "user", "ask", "not durable"); ok || msg.ID != "" {
		t.Fatal("failed message append reported success")
	}
	if msg := a.appendAgentMessage(run.ProjectID, run.AgentID, "system", "note", "not durable"); msg.ID != "" {
		t.Fatal("unscoped append reported success")
	}
	if item, ok, err := a.appendAgentModelOnlyTranscriptForRun(run.ProjectID, run.AgentID, run.ID, "scheduled_input", "not durable"); err == nil || ok || item.ID != "" {
		t.Fatalf("hidden input reported success: %+v %t %v", item, ok, err)
	}
	checkpoint := a.agentSessionState(run.ProjectID, run.AgentID)
	checkpoint.ResidentSummary = "not durable"
	checkpoint.LongTermVersion++
	a.updateAgentSessionState(checkpoint)
	if !reflect.DeepEqual(a.conversation.Snapshot(), wantEvents) ||
		!reflect.DeepEqual(a.conversation.MessageSnapshot(), wantMessages) ||
		!reflect.DeepEqual(a.conversation.SessionSnapshot(), wantSessions) ||
		!reflect.DeepEqual(a.conversation.TranscriptsFor(key), wantTranscript) {
		t.Fatal("failed write changed a conversation projection")
	}
	restore()
	message, ok := a.appendAgentMessageForRun(run.ProjectID, run.AgentID, run.ID, "user", "ask", "durable retry")
	if !ok || message.Seq != 1 {
		t.Fatalf("failed writes consumed conversation sequence: %+v %t", message, ok)
	}
	reloaded := newApp(Settings{DataDir: a.settings.DataDir})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.conversation.Snapshot(), a.conversation.Snapshot()) {
		t.Fatal("successful retry cannot be replayed exactly")
	}
}

func TestSessionResultWriteFailureNeverPublishesDone(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	project, agent := Project{ID: "project"}, Agent{ID: "agent"}
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: agent.ID, RunID: "run"})
	if !started {
		t.Fatal("Run did not start")
	}
	ctx, bound := a.claimAndBindAgentRunWorkerContext(context.Background(), project.ID, agent.ID, run.ID)
	if !bound {
		t.Fatal("worker was not bound")
	}
	ledger := a.createRunLedger(run.ID)
	want := a.conversation.Snapshot()
	restore := blockSessionEventWrites(t, a)
	if err := a.commitScheduledRunResult(project, agent, run.ID, "result", "must not appear"); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("persistence fault was not returned as an error: %v", err)
	}
	if ledgerHasTerminalType(ledger, "done") || !ledgerHasTerminalType(ledger, "error") {
		t.Fatal("failed persistence did not produce an error-only terminal delivery")
	}
	if _, active := a.activeAgentRun(project.ID, agent.ID); active || ctx.Err() == nil {
		t.Fatal("failed result left execution ownership active")
	}
	if !reflect.DeepEqual(a.conversation.Snapshot(), want) || len(a.agentMessagesFor(project.ID, agent.ID)) != 0 {
		t.Fatal("failed result or terminal event leaked into projections")
	}
	restore()
	reloaded := newApp(Settings{DataDir: a.settings.DataDir})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.conversation.Snapshot(), want) {
		t.Fatal("failed result changed the durable log")
	}
}

func TestSessionRunAdmissionAndTransitionFailClosed(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	restore := blockSessionEventWrites(t, a)
	if run, started := a.beginAgentRun(AgentRunInput{ProjectID: "project", AgentID: "agent", RunID: "run"}); started || run.ID != "" {
		t.Fatal("unpersisted Run was admitted")
	}
	if len(a.conversation.Snapshot()) != 0 || len(a.conversation.SessionSnapshot()) != 0 {
		t.Fatal("failed Run admission published a session")
	}
	restore()
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: "project", AgentID: "agent", RunID: "run"})
	if !started {
		t.Fatal("Run admission did not recover")
	}
	blockSessionEventWrites(t, a)
	if _, changed := a.transitionAgentRun(run.ProjectID, run.AgentID, run.ID, RunStateInvokingModel); changed {
		t.Fatal("failed transition reported success")
	}
	if current, active := a.activeAgentRun(run.ProjectID, run.AgentID); !active || current.State != RunStatePreparingContext {
		t.Fatalf("failed transition changed active state: %+v %t", current, active)
	}
}

func TestSessionMessageHTTPRejectsUnpersistedRun(t *testing.T) {
	a, project := newHandlerTestApp(t)
	blockSessionEventWrites(t, a)
	response := serveHTTPRequest(a, http.MethodPost, "/api/projects/"+project.ID+"/agents/karoz/messages", `{"message":"hello","type":"ask"}`)
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "event: done") {
		t.Fatalf("failed admission response=%d %s", response.Code, response.Body.String())
	}
	if _, active := a.activeAgentRun(project.ID, "karoz"); active {
		t.Fatal("failed HTTP admission started a worker")
	}
}

func TestSessionLogCorruptionPreservesSourceAndExistingProjections(t *testing.T) {
	for _, source := range []string{`{broken`, `null`, `{"project/agent":[{"id":"bad","type":"unknown"}]}`} {
		t.Run(source, func(t *testing.T) {
			a := newApp(Settings{DataDir: t.TempDir()})
			if message := a.appendAgentMessage("project", "agent", "user", "ask", "existing history"); message.ID == "" {
				t.Fatal("failed to seed history")
			}
			want := a.conversation.Snapshot()
			path := filepath.Join(a.settings.DataDir, "agent-session-events.json")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.loadAgentSessionEvents(); err == nil {
				t.Fatal("corrupt authority was accepted")
			}
			if !reflect.DeepEqual(a.conversation.Snapshot(), want) {
				t.Fatal("failed recovery discarded existing projections")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != source {
				t.Fatalf("corrupt source was changed: %q %v", got, err)
			}
			matches, err := filepath.Glob(path + ".corrupt-*")
			if err != nil || len(matches) != 0 {
				t.Fatalf("authoritative log was quarantined: %v %v", matches, err)
			}
		})
	}
}

type sessionFaultToolProvider struct {
	t    *testing.T
	app  *app
	runs int
}

func (*sessionFaultToolProvider) Capabilities(CLI2APIRequest) runtimedomain.ProviderCapabilities {
	return runtimedomain.ProviderCapabilities{Streaming: true, Tools: true, Interrupts: true}
}

func (provider *sessionFaultToolProvider) Stream(ctx context.Context, _ CLI2APIRequest, toolCtx ResidentToolContext, callbacks AgentStreamCallbacks) error {
	provider.runs++
	// Fail the tool-call append after its Run transition commits successfully.
	if _, ok := provider.app.transitionAgentRun(toolCtx.Project.ID, toolCtx.Agent.ID, toolCtx.RunID, RunStateExecutingTool); !ok {
		provider.t.Fatal("tool transition did not commit")
	}
	blockSessionEventWrites(provider.t, provider.app)
	call := codexToolCall{ID: "call", Name: "bash", Arguments: `{"command":"touch should-not-exist"}`}
	callbacks.OnToolStart(call)
	_, err := provider.app.executeResidentTool(ctx, toolCtx, call)
	return err
}

func TestSessionToolCallPersistenceFaultStopsSideEffects(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	project := Project{ID: "project", Path: t.TempDir()}
	agent := Agent{ID: "agent", ProjectID: project.ID, Provider: "codex-direct"}
	a.agentDirectory.agents[project.ID] = []Agent{agent}
	provider := &sessionFaultToolProvider{t: t, app: a}
	a.modelProvider = provider
	runner := &recordingCommandRunner{}
	a.commandRunner = runner
	run, started := a.beginAgentRun(AgentRunInput{ProjectID: project.ID, AgentID: agent.ID, RunID: "run", TurnType: "dev"})
	if !started {
		t.Fatal("Run did not start")
	}
	message, ok := a.appendAgentMessageForRun(project.ID, agent.ID, run.ID, "user", "dev", "run a command")
	if !ok {
		t.Fatal("input was not persisted")
	}
	_, err := a.runResidentAgentTurnWithCurrentInput(context.Background(), project, agent, message.Body, "dev", AgentTranscriptItem{ID: message.ID, Seq: message.Seq}, nil)
	if err == nil || !strings.Contains(err.Error(), "persist resident tool call") || provider.runs != 1 {
		t.Fatalf("tool persistence error=%v provider calls=%d", err, provider.runs)
	}
	for _, request := range runner.requests {
		if request.Name == "bash" {
			t.Fatal("unrecorded tool call executed a host command")
		}
	}
	for _, item := range a.agentTranscriptForModel(project.ID, agent.ID) {
		if item.Kind == "tool_call" {
			t.Fatal("failed tool call entered model history")
		}
	}
}

func TestSessionCheckpointDiskFailureDoesNotAdvanceSummaryOrWatermark(t *testing.T) {
	a := newApp(Settings{DataDir: t.TempDir()})
	message := a.appendAgentMessage("project", "agent", "user", "ask", "source for compaction")
	if message.ID == "" {
		t.Fatal("source did not persist")
	}
	state := a.agentSessionState("project", "agent")
	claim := agentCheckpointClaim{
		ProjectID: "project", AgentID: "agent", SessionID: state.SessionID,
		CapturedVersion: state.LongTermVersion, CapturedCoveredSeqEnd: state.CoveredSeqEnd,
		SeqStart: message.Seq, SeqEnd: message.Seq, Messages: []AgentMessage{message},
	}
	a.agentRuntime.checkpointClaims[checkpointClaimKey(claim)] = claim
	wantEvents := a.conversation.Snapshot()
	wantSessions := a.conversation.SessionSnapshot()
	restore := blockSessionEventWrites(t, a)
	if status := a.commitAgentCheckpoint(claim, "uncommitted summary"); status != "save_failed" {
		t.Fatalf("checkpoint status=%s", status)
	}
	if !reflect.DeepEqual(a.conversation.Snapshot(), wantEvents) || !reflect.DeepEqual(a.conversation.SessionSnapshot(), wantSessions) {
		t.Fatal("checkpoint disk failure advanced a projection")
	}
	restore()
	a.agentRuntime.checkpointClaims[checkpointClaimKey(claim)] = claim
	if status := a.commitAgentCheckpoint(claim, "durable summary"); status != "success" {
		t.Fatalf("checkpoint retry status=%s", status)
	}
	reloaded := newApp(Settings{DataDir: a.settings.DataDir})
	if err := reloaded.loadAgentSessionEvents(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.conversation.SessionSnapshot(), a.conversation.SessionSnapshot()) {
		t.Fatal("checkpoint retry cannot be reconstructed")
	}
}

func TestSessionApprovalWriteFailuresDoNotAuthorizeHostCommands(t *testing.T) {
	for _, phase := range []string{"request", "approve", "consume"} {
		t.Run(phase, func(t *testing.T) {
			a := newApp(Settings{DataDir: t.TempDir()})
			project := Project{ID: "project", Path: t.TempDir()}
			agent := Agent{ID: "agent", ProjectID: project.ID}
			toolCtx := ResidentToolContext{Project: project, Agent: agent, Workdir: project.Path, RunID: "run", TurnType: "ask"}
			command := "echo approved"
			if phase == "request" {
				blockSessionEventWrites(t, a)
				raw := a.requestResidentBashApproval(toolCtx, command)
				if !strings.Contains(raw, "save_failed") || len(a.agentRuntime.residentBashApprovals) != 0 {
					t.Fatalf("failed approval request retained a token: %s", raw)
				}
				return
			}
			raw := a.requestResidentBashApproval(toolCtx, command)
			choiceID := bashChoiceID(t, raw, residentBashApprovePrefix)
			if phase == "approve" {
				blockSessionEventWrites(t, a)
				if recognized, err := a.resolveResidentBashChoice(project.ID, agent.ID, toolCtx.RunID, choiceID); !recognized || err == nil {
					t.Fatalf("failed approval returned success: %t %v", recognized, err)
				}
				for _, token := range a.agentRuntime.residentBashApprovals {
					if token.State != residentBashApprovalPending {
						t.Fatal("failed approval granted a host-command token")
					}
				}
				return
			}
			if _, err := a.resolveResidentBashChoice(project.ID, agent.ID, toolCtx.RunID, choiceID); err != nil {
				t.Fatal(err)
			}
			blockSessionEventWrites(t, a)
			subject, err := newResidentBashSubject(residentBashOperationForeground, project.ID, agent.ID, project.Path, command, "")
			if err != nil {
				t.Fatal(err)
			}
			if a.consumeResidentBashApprovalSubject(toolCtx.RunID, subject) {
				t.Fatal("unrecorded consumption authorized a host command")
			}
			if len(a.agentRuntime.residentBashApprovals) != 0 {
				t.Fatal("failed consumption left a reusable token")
			}
		})
	}
}

func TestSessionTaskHookWriteFailureRetainsPendingDelivery(t *testing.T) {
	t.Setenv("KAROZ_AGENT_AUTO_RESPOND", "0")
	a := newApp(Settings{DataDir: t.TempDir()})
	project := Project{ID: "project"}
	task := Task{ID: "task", ProjectID: project.ID, Status: "done", Result: "completed"}
	hook := a.registerTaskRuntimeHook(project.ID, "agent", task.ID, nil)
	restore := blockSessionEventWrites(t, a)
	a.notifyTaskRuntimeHooks(project, task)
	key := project.ID + "/" + task.ID
	if got := a.projectTasks.hooks[key][0]; got.Status != "pending" || got.DeliveredAt != nil {
		t.Fatalf("unpersisted task hook was acknowledged: %+v", got)
	}
	restore()
	a.notifyTaskRuntimeHooks(project, task)
	if got := a.projectTasks.hooks[key][0]; got.Status != "delivered" || got.ID != hook.ID {
		t.Fatalf("pending hook did not recover: %+v", got)
	}
	if messages := a.agentMessagesFor(project.ID, "agent"); len(messages) != 1 || messages[0].Intent != "task_hook" {
		t.Fatalf("notification retry duplicated or lost the message: %+v", messages)
	}
}
