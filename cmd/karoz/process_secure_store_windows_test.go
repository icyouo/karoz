//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsSecureRuntimeStoreOwnerACLAndReparseRejection(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "runtime")
	store, err := newSecureRuntimeStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensureDir(filepath.Join("project-runtime", "safe")); err != nil {
		t.Fatal(err)
	}
	if err := store.saveJSON(
		filepath.Join("project-runtime", "safe", "state.json"),
		map[string]int{"schema_version": 1},
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		dataDir,
		filepath.Join(dataDir, "project-runtime"),
		filepath.Join(dataDir, "project-runtime", "safe"),
		filepath.Join(dataDir, "project-runtime", "safe", "state.json"),
	} {
		if err := validateWindowsSecurePath(path); err != nil {
			t.Fatalf("owner-only ACL %s: %v", path, err)
		}
	}

	target := t.TempDir()
	reparse := filepath.Join(dataDir, "project-runtime", "reparse")
	if err := os.Symlink(target, reparse); err != nil {
		t.Skipf("Windows runner cannot create reparse fixture: %v", err)
	}
	if err := store.ensureDir(filepath.Join("project-runtime", "reparse", "nested")); err == nil {
		t.Fatal("runtime store followed a reparse-point directory")
	}
}
