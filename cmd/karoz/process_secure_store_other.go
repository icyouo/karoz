//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package main

import (
	"errors"
	"io"
)

var errSecureRuntimeUnsupported = errors.New("secure runtime persistence is unsupported on this platform")

func secureEnsureRoot(string) error          { return errSecureRuntimeUnsupported }
func secureEnsureDir(string, []string) error { return errSecureRuntimeUnsupported }
func secureReadFile(string, []string) ([]byte, bool, error) {
	return nil, false, errSecureRuntimeUnsupported
}
func secureWriteFile(string, []string, []byte) error { return errSecureRuntimeUnsupported }
func secureOpenAppendFile(string, []string) (io.WriteCloser, error) {
	return nil, errSecureRuntimeUnsupported
}
func secureRemoveFile(string, []string) error { return errSecureRuntimeUnsupported }
