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
