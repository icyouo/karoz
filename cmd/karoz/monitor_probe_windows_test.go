//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

func TestWindowsMonitorProbeFailsClosedBeforeMutation(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := Project{
		ID: projectID(projectPath), Name: "project", Path: projectPath,
	}
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	owner := Agent{
		ID: "owner", ProjectID: project.ID,
		CreatedAt: time.Now().UTC(),
	}
	a.agentDirectoryLocked().agents[project.ID] = []Agent{owner}
	if _, _, err := a.prepareMonitorProbeApproval(
		project,
		monitorProbeApprovalRequest{
			AgentID: owner.ID, Language: "shell",
			Workdir: project.Path,
			Source:  "printf '{\"matched\":false}'\n",
		},
		"",
	); !errors.Is(err, errScriptProbeUnsupported) {
		t.Fatalf("prepare error=%v", err)
	}
	if len(a.monitorProbeReservations) != 0 ||
		len(a.monitorProbeChallenges) != 0 ||
		len(a.monitorProbeReceipts) != 0 {
		t.Fatal("unsupported prepare mutated approval state")
	}
	item := Monitor{
		ID: "probe", ProjectID: project.ID, AgentID: owner.ID,
		Name: "probe", Revision: 1,
		Trigger: monitordomain.Trigger{
			Kind:          monitordomain.TriggerScriptProbe,
			Revision:      1,
			ProbeLanguage: "shell",
			ProbePath:     "monitor-probes/project/receipt/probe.sh",
			ProbeSHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			IntervalMS:    60_000, TimeoutMS: 5_000,
			ApprovalReceiptID: "receipt",
		},
		Action: monitordomain.Action{
			Kind:     monitordomain.ActionBlackboard,
			Revision: 1,
			Topic:    "probe",
		},
		State:     monitordomain.StateActive,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if _, err := a.createMonitor(project, item); !errors.Is(
		err,
		errScriptProbeUnsupported,
	) {
		t.Fatalf("HTTP create boundary error=%v", err)
	}
	if _, err := a.claimScriptProbeMonitor(
		project,
		owner,
		item,
		"receipt",
		"mutation",
	); !errors.Is(err, errScriptProbeUnsupported) {
		t.Fatalf("claim error=%v", err)
	}
	if len(a.monitors[project.ID]) != 0 {
		t.Fatal("unsupported mutation created a monitor")
	}

	a.monitors[project.ID] = []Monitor{item}
	if err := a.saveMonitors(); err != nil {
		t.Fatal(err)
	}
	reloaded := newApp(Settings{DataDir: dataDir, ProjectsRoot: root})
	if err := reloaded.loadMonitors(); err != nil {
		t.Fatal(err)
	}
	loaded := reloaded.monitorsForProject(project.ID)
	if len(loaded) != 1 ||
		loaded[0].State != monitordomain.StateError ||
		loaded[0].ErrorCode != "unsupported_platform" {
		t.Fatalf("persisted probe did not load fail-closed: %+v", loaded)
	}
	before := loaded[0]
	if _, err := reloaded.setMonitorState(
		project,
		item.ID,
		monitordomain.StateActive,
	); !errors.Is(err, errScriptProbeUnsupported) {
		t.Fatalf("resume error=%v", err)
	}
	after := reloaded.monitorsForProject(project.ID)[0]
	if after.State != before.State ||
		after.ErrorCode != before.ErrorCode ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("unsupported resume mutated monitor: before=%+v after=%+v", before, after)
	}
}
