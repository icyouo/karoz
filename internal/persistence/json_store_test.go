package persistence

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJSONStoreMissingSaveAndLoad(t *testing.T) {
	store := NewJSONStore(filepath.Join(t.TempDir(), "nested"))
	var target map[string]string
	if found, err := store.Load("state.json", &target); err != nil || found {
		t.Fatalf("missing load = found %v err %v", found, err)
	}
	want := map[string]string{"status": "ready"}
	if err := store.Save("state.json", want, 0640); err != nil {
		t.Fatal(err)
	}
	if found, err := store.Load("state.json", &target); err != nil || !found || target["status"] != "ready" {
		t.Fatalf("load = found %v target %+v err %v", found, target, err)
	}
	info, err := os.Stat(filepath.Join(store.Root, "state.json"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %v err %v", info.Mode().Perm(), err)
	}
}

func TestJSONStoreLoadQuarantinesCorruptFile(t *testing.T) {
	root := t.TempDir()
	store := NewJSONStore(root)
	garbage := []byte("{not json")
	if err := os.WriteFile(filepath.Join(root, "state.json"), garbage, 0644); err != nil {
		t.Fatal(err)
	}
	var target map[string]string
	if found, err := store.Load("state.json", &target); err != nil || found {
		t.Fatalf("corrupt load = found %v err %v", found, err)
	}
	if _, err := os.Stat(filepath.Join(root, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("corrupt file still in place: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(root, "state.json.corrupt-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined files = %v err=%v", matches, err)
	}
	quarantined, err := os.ReadFile(matches[0])
	if err != nil || string(quarantined) != string(garbage) {
		t.Fatalf("quarantined content = %q err=%v", quarantined, err)
	}
	if found, err := store.Load("state.json", &target); err != nil || found {
		t.Fatalf("post-quarantine load = found %v err %v", found, err)
	}
}
