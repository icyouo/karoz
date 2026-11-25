package persistence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestJSONStoreConcurrentAtomicWritesHaveUniqueTempsAndCompleteRecords(t *testing.T) {
	store := NewJSONStore(t.TempDir())
	type record struct {
		Writer  int    `json:"writer"`
		Round   int    `json:"round"`
		Payload string `json:"payload"`
	}
	makeRecord := func(writer, round int) record {
		return record{writer, round, strings.Repeat(fmt.Sprintf("writer=%d round=%d;", writer, round), 128)}
	}
	if err := store.Save("state.json", makeRecord(0, 0), 0600); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 16, 8
	start := make(chan struct{})
	stopReader := make(chan struct{})
	errors := make(chan error, writers*rounds+1)
	var workers, reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stopReader:
				return
			default:
			}
			var got record
			found, err := store.LoadStrict("state.json", &got)
			if err != nil || !found || got != makeRecord(got.Writer, got.Round) {
				errors <- fmt.Errorf("observed partial record %+v: found=%t err=%v", got, found, err)
				return
			}
		}
	}()
	for writer := 0; writer < writers; writer++ {
		workers.Add(1)
		go func(writer int) {
			defer workers.Done()
			<-start
			for round := 0; round < rounds; round++ {
				if err := store.Save("state.json", makeRecord(writer, round), 0600); err != nil {
					errors <- err
				}
			}
		}(writer)
	}
	close(start)
	workers.Wait()
	close(stopReader)
	reader.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	matches, err := filepath.Glob(filepath.Join(store.Root, "state.json.tmp-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("atomic writes leaked temporary files: %v err=%v", matches, err)
	}
}

func TestJSONStoreSaveCompactRoundTripsWithoutIndentation(t *testing.T) {
	store := NewJSONStore(t.TempDir())
	want := map[string]any{
		"project": "karoz",
		"events":  []string{"started", "completed"},
	}
	if err := store.SaveCompact("ledger.json", want, 0640); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(store.Root, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\n  ") {
		t.Fatalf("expected compact JSON without indentation, got %q", string(raw))
	}
	var got struct {
		Project string   `json:"project"`
		Events  []string `json:"events"`
	}
	found, err := store.LoadStrict("ledger.json", &got)
	if err != nil || !found || got.Project != "karoz" || len(got.Events) != 2 {
		t.Fatalf("load strict = found=%v err=%v got=%+v", found, err, got)
	}
}
