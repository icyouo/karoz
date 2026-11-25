package persistence

import (
	"crypto/rand"
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

// LoadStrict preserves an authoritative file on decode failure. Unlike
// optional configuration, a corrupt event log must not become an empty store.
func (store JSONStore) LoadStrict(name string, target any) (bool, error) {
	data, err := os.ReadFile(store.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return true, fmt.Errorf("decode authoritative state %s: %w", store.path(name), err)
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

func (store JSONStore) SaveCompact(name string, value any, perm os.FileMode) error {
	return SaveJSONCompactAtomic(store.path(name), value, perm)
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
	return writeBytesAtomic(path, data, perm)
}

// SaveJSONCompactAtomic writes value as compact JSON to path using the same
// temp-file + fsync + atomic rename protocol as SaveJSONAtomic, avoiding
// whitespace inflation on high-frequency machine ledgers.
func SaveJSONCompactAtomic(path string, value any, perm os.FileMode) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeBytesAtomic(path, data, perm)
}

func writeBytesAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// Wall clocks can return the same nanosecond for concurrent writers. Use
	// random names with exclusive creation, preserving the caller's umask just
	// as a normal OpenFile does (CreateTemp followed by Chmod would bypass it).
	var tmp string
	var file *os.File
	var err error
	for {
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return err
		}
		tmp = fmt.Sprintf("%s.tmp-%x", path, nonce)
		file, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		break
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
