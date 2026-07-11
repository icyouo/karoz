package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
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

func (store JSONStore) Load(name string, target any) (bool, error) {
	data, err := os.ReadFile(store.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return true, err
	}
	return true, nil
}

func (store JSONStore) Save(name string, value any, perm os.FileMode) error {
	return SaveJSONAtomic(store.path(name), value, perm)
}

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
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (store JSONStore) path(name string) string {
	return filepath.Join(store.Root, filepath.Clean(name))
}
