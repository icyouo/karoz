package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapQuarantinesCorruptStateFile(t *testing.T) {
	dataDir := t.TempDir()
	a := newApp(Settings{DataDir: dataDir, ProjectsRoot: t.TempDir()})
	garbage := []byte("{not json")
	if err := os.WriteFile(filepath.Join(dataDir, "tasks.json"), garbage, 0644); err != nil {
		t.Fatal(err)
	}
	if err := a.bootstrap(); err != nil {
		t.Fatalf("bootstrap with corrupt state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "tasks.json")); !os.IsNotExist(err) {
		t.Fatalf("corrupt file still in place: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dataDir, "tasks.json.corrupt-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined files = %v err=%v", matches, err)
	}
	quarantined, err := os.ReadFile(matches[0])
	if err != nil || string(quarantined) != string(garbage) {
		t.Fatalf("quarantined content = %q err=%v", quarantined, err)
	}
	if len(a.tasks) != 0 {
		t.Fatalf("state did not fall back to defaults: %+v", a.tasks)
	}
}
