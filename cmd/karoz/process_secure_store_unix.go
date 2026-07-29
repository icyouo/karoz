//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func secureEnsureRoot(root string) error {
	if err := rejectSymlinkComponents(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return validateSecureUnixFD(fd, true)
}

func secureEnsureDir(root string, parts []string) error {
	rootFD, err := openSecureUnixRoot(root)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	fd := rootFD
	for _, part := range parts {
		if err := unix.Mkdirat(fd, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			if fd != rootFD {
				unix.Close(fd)
			}
			return err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if fd != rootFD {
			unix.Close(fd)
		}
		if err != nil {
			return err
		}
		if err := validateSecureUnixFD(next, true); err != nil {
			unix.Close(next)
			return err
		}
		fd = next
	}
	if fd != rootFD {
		defer unix.Close(fd)
	}
	return unix.Fsync(fd)
}

func secureReadFile(root string, parts []string) ([]byte, bool, error) {
	parent, name, err := openSecureUnixParent(root, parts, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := validateSecureUnixFD(fd, false); err != nil {
		unix.Close(fd)
		return nil, true, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, true, errors.New("create runtime file handle")
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	return data, true, err
}

func secureWriteFile(root string, parts []string, data []byte) error {
	parent, name, err := openSecureUnixParent(root, parts, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	tmp := fmt.Sprintf(".%s.tmp-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	fd, err := unix.Openat(parent, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if fd >= 0 {
			unix.Close(fd)
		}
		if cleanup {
			_ = unix.Unlinkat(parent, tmp, 0)
		}
	}()
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tmp)
	if file == nil {
		return errors.New("create runtime temp handle")
	}
	if err := writeAll(file, data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	fd = -1
	if err := unix.Renameat(parent, tmp, parent, name); err != nil {
		return err
	}
	cleanup = false
	return unix.Fsync(parent)
}

func secureOpenAppendFile(root string, parts []string) (io.WriteCloser, error) {
	parent, name, err := openSecureUnixParent(root, parts, true)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	unix.Close(parent)
	if err != nil {
		return nil, err
	}
	if err := validateSecureUnixFD(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func secureOpenReadFile(
	root string,
	parts []string,
) (runtimeReadSeekCloser, error) {
	parent, name, err := openSecureUnixParent(root, parts, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(
		parent,
		name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if err := validateSecureUnixFD(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		unix.Close(fd)
		return nil, errors.New("create runtime read handle")
	}
	return file, nil
}

func secureRemoveFile(root string, parts []string) error {
	parent, name, err := openSecureUnixParent(root, parts, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateSecureUnixFD(fd, false); err != nil {
		unix.Close(fd)
		return err
	}
	unix.Close(fd)
	if err := unix.Unlinkat(parent, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(parent)
}

func openSecureUnixRoot(root string) (int, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if err := validateSecureUnixFD(fd, true); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func openSecureUnixParent(root string, parts []string, create bool) (int, string, error) {
	if len(parts) == 0 {
		return -1, "", errors.New("runtime file path is empty")
	}
	rootFD, err := openSecureUnixRoot(root)
	if err != nil {
		return -1, "", err
	}
	fd := rootFD
	for _, part := range parts[:len(parts)-1] {
		if create {
			if err := unix.Mkdirat(fd, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				if fd != rootFD {
					unix.Close(fd)
				}
				unix.Close(rootFD)
				return -1, "", err
			}
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if fd != rootFD {
			unix.Close(fd)
		}
		if err != nil {
			unix.Close(rootFD)
			if errors.Is(err, unix.ENOENT) {
				return -1, "", os.ErrNotExist
			}
			return -1, "", err
		}
		if err := validateSecureUnixFD(next, true); err != nil {
			unix.Close(next)
			unix.Close(rootFD)
			return -1, "", err
		}
		fd = next
	}
	if fd != rootFD {
		unix.Close(rootFD)
	}
	return fd, parts[len(parts)-1], nil
}

func validateSecureUnixFD(fd int, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	mode := stat.Mode & unix.S_IFMT
	if directory && mode != unix.S_IFDIR {
		return errors.New("runtime path component is not a directory")
	}
	if !directory && mode != unix.S_IFREG {
		return errors.New("runtime path component is not a regular file")
	}
	if int(stat.Uid) != os.Geteuid() {
		return errors.New("runtime path component owner mismatch")
	}
	if stat.Mode&0o077 != 0 {
		return errors.New("runtime path component permissions are not owner-only")
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime path contains symlink component %s", current)
		}
	}
	return nil
}
