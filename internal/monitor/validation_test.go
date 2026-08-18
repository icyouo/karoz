package monitor

import (
	"testing"
	"time"
)

func validMonitorFixture() Monitor {
	return Monitor{
		ID: "monitor-1", ProjectID: "project-1", AgentID: "agent-1",
		Name: "watch tasks", Revision: 1, State: StateActive,
		CooldownMS: MinimumCooldownMS,
		Trigger: Trigger{
			Kind: TriggerRuntimeEvent, Revision: 1,
			EventKinds: []string{"task_changed"},
		},
		Action: Action{
			Revision: 1, Kind: ActionBlackboard, Topic: "task-status",
		},
	}
}

func TestValidateActionTriggerAndMonitor(t *testing.T) {
	item := validMonitorFixture()
	if err := ValidateMonitor(item); err != nil {
		t.Fatal(err)
	}
	badActions := []Action{
		{Revision: 1, Kind: ActionKind("command")},
		{Revision: 1, Kind: ActionNotifyAgent, AgentID: "agent-1", TurnType: "dev"},
		{Revision: 1, Kind: ActionNotifyAgent, TurnType: "ask"},
		{Revision: 1, Kind: ActionBlackboard},
	}
	for _, action := range badActions {
		if ValidateAction(action) == nil {
			t.Fatalf("invalid action accepted: %+v", action)
		}
	}
	probe := Trigger{
		Kind: TriggerScriptProbe, Revision: 2, ProbeLanguage: "shell",
		ProbePath: "/runtime/probe", ProbeSHA256: sha256Hex([]byte("echo ok")),
		IntervalMS: 60_000, TimeoutMS: 5_000, ApprovalReceiptID: "receipt-1",
	}
	if err := ValidateTrigger(probe); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Trigger){
		func(value *Trigger) { value.ApprovalReceiptID = "" },
		func(value *Trigger) { value.ProbePath = "" },
		func(value *Trigger) { value.ProbeSHA256 = "bad" },
		func(value *Trigger) { value.IntervalMS = 9_999 },
		func(value *Trigger) { value.TimeoutMS = 30_001 },
		func(value *Trigger) { value.EventKinds = []string{"task_changed"} },
	} {
		candidate := probe
		mutate(&candidate)
		if ValidateTrigger(candidate) == nil {
			t.Fatalf("incomplete/unsafe probe accepted: %+v", candidate)
		}
	}
	item.Action.Kind = ActionKind("unknown")
	if ValidateMonitor(item) == nil {
		t.Fatal("monitor accepted invalid action")
	}
}

func TestProbeAuthorizedExactClaimedReceipt(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	source := []byte("echo ok")
	digest := sha256Hex(source)
	item := Monitor{
		ID: "monitor-1", ProjectID: "project-1", AgentID: "agent-1",
		Trigger: Trigger{
			Kind: TriggerScriptProbe, Revision: 2, ProbeLanguage: "shell",
			ProbePath: "/runtime/probe", ProbeSHA256: digest,
			IntervalMS: 60_000, TimeoutMS: 5_000, ApprovalReceiptID: "receipt-1",
		},
	}
	receipt := ProbeApprovalReceipt{
		ID: "receipt-1", ProjectID: item.ProjectID, AgentID: item.AgentID,
		MonitorID: item.ID, TriggerRevision: item.Trigger.Revision,
		ApprovalFlow: "agent_choice", ApprovalRunID: "run-1", ChoiceRequestID: "choice-1",
		CanonicalWorkdir: "/project", Language: "shell",
		SnapshotPath: item.Trigger.ProbePath, SnapshotDevice: 10, SnapshotInode: 20,
		NormalizedSource: source, SourceSHA256: digest,
		IntervalMS: item.Trigger.IntervalMS, TimeoutMS: item.Trigger.TimeoutMS,
		ApprovedBy: "local_operator:test", ApprovedAt: now,
		ExpiresAt:         now.Add(10 * time.Minute),
		ClaimedMutationID: "mutation-1", ClaimedAt: timePointer(now),
	}
	check := ProbeAuthorizationCheck{
		MutationID: "mutation-1", CanonicalWorkdir: "/project",
		SnapshotPath: receipt.SnapshotPath, SnapshotDevice: 10, SnapshotInode: 20,
		ModePerm: 0o600, Regular: true, RuntimeOwned: true, Source: append([]byte(nil), source...),
	}
	if err := ProbeAuthorized(item, receipt, check); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*Monitor, *ProbeApprovalReceipt, *ProbeAuthorizationCheck)
	}{
		{"monitor", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.MonitorID = "other"
		}},
		{"revision", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) { receipt.TriggerRevision++ }},
		{"mutation", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) { check.MutationID = "other" }},
		{"project", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.ProjectID = "other"
		}},
		{"agent", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) { receipt.AgentID = "other" }},
		{"language", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.Language = "javascript"
		}},
		{"workdir", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) {
			check.CanonicalWorkdir = "/other"
		}},
		{"interval", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) { receipt.IntervalMS++ }},
		{"timeout", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) { receipt.TimeoutMS++ }},
		{"source", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) {
			check.Source = []byte("other")
		}},
		{"hash", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.SourceSHA256 = sha256Hex([]byte("other"))
		}},
		{"symlink", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) { check.Symlink = true }},
		{"mode", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) { check.ModePerm = 0o644 }},
		{"runtime-owned", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) { check.RuntimeOwned = false }},
		{"identity", func(_ *Monitor, _ *ProbeApprovalReceipt, check *ProbeAuthorizationCheck) { check.SnapshotInode++ }},
		{"empty-subjects", func(item *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			item.ID, item.ProjectID, item.AgentID = "", "", ""
			receipt.MonitorID, receipt.ProjectID, receipt.AgentID = "", "", ""
		}},
		{"zero-expiry", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.ExpiresAt = time.Time{}
		}},
		{"claim-at-expiry", func(_ *Monitor, receipt *ProbeApprovalReceipt, _ *ProbeAuthorizationCheck) {
			receipt.ClaimedAt = timePointer(receipt.ExpiresAt)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidateItem := item
			candidateReceipt := receipt
			candidateReceipt.NormalizedSource = append([]byte(nil), receipt.NormalizedSource...)
			candidateCheck := check
			candidateCheck.Source = append([]byte(nil), check.Source...)
			test.mutate(&candidateItem, &candidateReceipt, &candidateCheck)
			if ProbeAuthorized(candidateItem, candidateReceipt, candidateCheck) == nil {
				t.Fatal("authorization mismatch accepted")
			}
		})
	}
}

func TestValidateMonitorRejectsDuplicatePendingFireIdentity(t *testing.T) {
	item := validMonitorFixture()
	item.Sequence = 1
	event := Event{
		ID: "task/task-1/1", ProjectID: item.ProjectID,
		AuthorityID: "task-store", AuthorityGeneration: 1,
		Kind: "task_changed", EntityID: "task-1", Origin: Origin{Kind: "runtime"},
	}
	pending, err := FreezePendingFire(
		item, event, "task changed",
		[]byte(`{"event":1}`), []byte(`{"action":1}`), time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	item.PendingFires = []PendingFire{pending, pending}
	if ValidateMonitor(item) == nil {
		t.Fatal("persisted duplicate pending fire set validated")
	}
}
