package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

type secureRuntimeStore struct {
	root string
}

type runtimeReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

func newSecureRuntimeStore(dataDir string) (*secureRuntimeStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("runtime data directory is empty")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	if info, statErr := filepath.EvalSymlinks(root); statErr == nil {
		if final, lstatErr := filepath.EvalSymlinks(filepath.Dir(root)); lstatErr == nil {
			resolvedFinal := filepath.Join(final, filepath.Base(root))
			if resolvedFinal != info {
				return nil, errors.New("runtime data directory is a symlink")
			}
		}
		root = info
	} else {
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(root))
		if parentErr != nil {
			return nil, parentErr
		}
		root = filepath.Join(parent, filepath.Base(root))
	}
	if err := secureEnsureRoot(root); err != nil {
		return nil, err
	}
	return &secureRuntimeStore{root: root}, nil
}

func (store *secureRuntimeStore) ensureDir(relative string) error {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return err
	}
	return secureEnsureDir(store.root, parts)
}

func (store *secureRuntimeStore) loadJSON(relative string, target any) (bool, error) {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return false, err
	}
	data, found, err := secureReadFile(store.root, parts)
	if err != nil || !found {
		return found, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return true, fmt.Errorf("strictly decode %s: %w", relative, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return true, fmt.Errorf("strictly decode %s: %w", relative, err)
	}
	return true, nil
}

func (store *secureRuntimeStore) saveJSON(relative string, value any) error {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return secureWriteFile(store.root, parts, data)
}

func (store *secureRuntimeStore) saveBytes(relative string, value []byte) error {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return err
	}
	return secureWriteFile(store.root, parts, value)
}

func (store *secureRuntimeStore) openLog(relative string) (io.WriteCloser, error) {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return nil, err
	}
	return secureOpenAppendFile(store.root, parts)
}

func (store *secureRuntimeStore) openRead(
	relative string,
) (runtimeReadSeekCloser, error) {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return nil, err
	}
	return secureOpenReadFile(store.root, parts)
}

func (store *secureRuntimeStore) remove(relative string) error {
	parts, err := secureRelativeParts(relative)
	if err != nil {
		return err
	}
	return secureRemoveFile(store.root, parts)
}

func secureRelativeParts(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return nil, errors.New("runtime store path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("runtime store path escapes root")
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("runtime store path contains unsafe component")
		}
	}
	return parts, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}
