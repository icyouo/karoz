package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

type JSONStore struct {
	Root string
}

func NewJSONStore(root string) JSONStore {
	return JSONStore{Root: root}
}

// Load reads the named JSON state file into target. A missing file reports
// found=false with no error. A file that exists but fails to decode is
// renamed aside to <name>.corrupt-<unixnano> and reported as found=false, so
// callers bootstrap from defaults instead of failing hard.
func (store JSONStore) Load(name string, target any) (bool, error) {
	data, err := os.ReadFile(store.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		quarantined, renameErr := store.quarantineCorrupt(name)
		if renameErr != nil {
			return true, err
		}
		log.Printf("persistence: cannot decode %s (%v); quarantined to %s", store.path(name), err, quarantined)
		return false, nil
	}
	return true, nil
}

func (store JSONStore) quarantineCorrupt(name string) (string, error) {
	path := store.path(name)
	quarantined := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	if err := os.Rename(path, quarantined); err != nil {
		return "", err
	}
	return quarantined, nil
}

func (store JSONStore) Save(name string, value any, perm os.FileMode) error {
	return SaveJSONAtomic(store.path(name), value, perm)
}

// SaveJSONAtomic writes value as indented JSON to path via a temp file +
// rename, fsyncing the temp file before the rename and the directory after
// it, so a crash cannot tear the file or lose the rename.
func SaveJSONAtomic(path string, value any, perm os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory so a rename inside it survives a crash.
// Best-effort: not every filesystem supports directory syncs.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func (store JSONStore) path(name string) string {
	return filepath.Join(store.Root, filepath.Clean(name))
}
