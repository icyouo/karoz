//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func secureEnsureRoot(root string) error {
	if err := rejectWindowsReparseComponents(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return applyAndValidateWindowsOwnerACL(root)
}

func secureEnsureDir(root string, parts []string) error {
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			if err := applyAndValidateWindowsOwnerACL(current); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("runtime path component is not a directory")
		}
		if err := validateWindowsSecurePath(current); err != nil {
			return err
		}
	}
	return nil
}

func secureReadFile(root string, parts []string) ([]byte, bool, error) {
	path, err := secureWindowsPath(root, parts, false)
	if err != nil {
		return nil, false, err
	}
	if err := validateWindowsSecurePath(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, true, err
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func secureWriteFile(root string, parts []string, data []byte) error {
	path, err := secureWindowsPath(root, parts, true)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := applyAndValidateWindowsOwnerACL(tmp); err != nil {
		_ = file.Close()
		return err
	}
	if err := writeAll(file, data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(
		from,
		to,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	); err != nil {
		return err
	}
	cleanup = false
	return validateWindowsSecurePath(path)
}

func secureOpenAppendFile(root string, parts []string) (io.WriteCloser, error) {
	path, err := secureWindowsPath(root, parts, true)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := applyAndValidateWindowsOwnerACL(path); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func secureRemoveFile(root string, parts []string) error {
	path, err := secureWindowsPath(root, parts, false)
	if err != nil {
		return err
	}
	if err := validateWindowsSecurePath(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Remove(path)
}

func secureWindowsPath(root string, parts []string, createParent bool) (string, error) {
	if createParent && len(parts) > 1 {
		if err := secureEnsureDir(root, parts[:len(parts)-1]); err != nil {
			return "", err
		}
	}
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		if err := rejectWindowsReparsePath(current); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return current, nil
}

func rejectWindowsReparseComponents(path string) error {
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	rest := strings.TrimPrefix(path, current)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if err := rejectWindowsReparsePath(current); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	return nil
}

func rejectWindowsReparsePath(path string) error {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(value)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return os.ErrNotExist
		}
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("runtime path contains a reparse point")
	}
	return nil
}

func validateWindowsSecurePath(path string) error {
	if err := rejectWindowsReparsePath(path); err != nil {
		return err
	}
	actual, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	desired, owner, err := windowsOwnerSecurityDescriptor()
	if err != nil {
		return err
	}
	actualOwner, _, err := actual.Owner()
	if err != nil {
		return err
	}
	if !actualOwner.Equals(owner) {
		return errors.New("runtime path owner mismatch")
	}
	control, _, err := actual.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("runtime path ACL inherits permissions")
	}
	actualDACL, _, err := actual.DACL()
	if err != nil {
		return err
	}
	desiredDACL, _, err := desired.DACL()
	if err != nil {
		return err
	}
	if !sameWindowsACL(actualDACL, desiredDACL) {
		return errors.New("runtime path ACL is not owner-only")
	}
	return nil
}

func sameWindowsACL(left, right *windows.ACL) bool {
	if left == nil || right == nil || left.AceCount != right.AceCount {
		return false
	}
	rightEntries := make([]*windows.ACCESS_ALLOWED_ACE, right.AceCount)
	for index := range rightEntries {
		if windows.GetAce(right, uint32(index), &rightEntries[index]) != nil {
			return false
		}
	}
	matched := make([]bool, len(rightEntries))
	for index := uint16(0); index < left.AceCount; index++ {
		var candidate *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(left, uint32(index), &candidate) != nil ||
			candidate.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		candidateSID := (*windows.SID)(unsafe.Pointer(&candidate.SidStart))
		found := false
		for desiredIndex, desired := range rightEntries {
			desiredSID := (*windows.SID)(unsafe.Pointer(&desired.SidStart))
			if !matched[desiredIndex] &&
				candidate.Header.AceType == desired.Header.AceType &&
				candidate.Header.AceFlags == desired.Header.AceFlags &&
				candidate.Mask == desired.Mask &&
				candidateSID.Equals(desiredSID) {
				matched[desiredIndex] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func applyAndValidateWindowsOwnerACL(path string) error {
	desired, owner, err := windowsOwnerSecurityDescriptor()
	if err != nil {
		return err
	}
	dacl, _, err := desired.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner,
		nil,
		dacl,
		nil,
	); err != nil {
		return err
	}
	return validateWindowsSecurePath(path)
}

func windowsOwnerSecurityDescriptor() (*windows.SECURITY_DESCRIPTOR, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	owner := user.User.Sid
	sddl := fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)", owner.String(), owner.String())
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	return descriptor, owner, err
}
