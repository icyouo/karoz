package main

import (
	"bufio"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var residentRepoSkippedDirs = map[string]bool{
	".git": true, ".karoz": true, "node_modules": true,
}

func safeResidentRepoPath(root, relative string) (string, error) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolvedRoot
	}
	relative = strings.TrimSpace(relative)
	if relative == "" {
		relative = "."
	}
	if filepath.IsAbs(relative) {
		return "", errors.New("repository path must be relative")
	}
	if residentRepoPathDenied(relative) {
		return "", errors.New("repository path is not available to resident agents")
	}
	full := filepath.Clean(filepath.Join(root, relative))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("repository path escapes the project workspace")
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err == nil {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		resolvedRel, relErr := filepath.Rel(root, resolved)
		if relErr != nil || resolvedRel == ".." || strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator)) {
			return "", errors.New("repository symlink escapes the project workspace")
		}
		if residentRepoPathDenied(resolvedRel) {
			return "", errors.New("repository path is not available to resident agents")
		}
		full = resolved
	}
	return full, nil
}

func repoListTool(ctx context.Context, workdir string, args map[string]any) string {
	repoRoot, _ := filepath.Abs(filepath.Clean(workdir))
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(repoRoot); resolveErr == nil {
		repoRoot = resolvedRoot
	}
	relative := toolStringArg(args, "path", 2000)
	full, err := safeResidentRepoPath(repoRoot, relative)
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_path", "message": err.Error()})
	}
	depth := clampToolInt(args, "depth", 2, 0, 6)
	limit := clampToolInt(args, "max_entries", 200, 1, 1000)
	type entry struct {
		Path string `json:"path"`
		Type string `json:"type"`
		Size int64  `json:"size_bytes,omitempty"`
	}
	var entries []entry
	truncated := false
	baseRel, _ := filepath.Rel(repoRoot, full)
	baseDepth := residentRepoPathDepth(baseRel)
	err = filepath.WalkDir(full, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		pathDepth := residentRepoPathDepth(rel)
		if item.IsDir() && residentRepoSkippedDirs[item.Name()] {
			return filepath.SkipDir
		}
		if residentRepoPathDenied(rel) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if pathDepth-baseDepth > depth {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(entries) >= limit {
			truncated = true
			return fs.SkipAll
		}
		kind := "file"
		if item.IsDir() {
			kind = "directory"
		} else if item.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		listed := entry{Path: filepath.ToSlash(rel), Type: kind}
		if info, infoErr := item.Info(); infoErr == nil && info.Mode().IsRegular() {
			listed.Size = info.Size()
		}
		entries = append(entries, listed)
		return nil
	})
	if err != nil {
		return toolJSON(map[string]any{"error": "repo_list_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"entries": entries, "truncated": truncated})
}

func repoReadTool(ctx context.Context, workdir string, args map[string]any) string {
	relative := toolStringArg(args, "path", 2000)
	if relative == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "path is required"})
	}
	full, err := safeResidentRepoPath(workdir, relative)
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_path", "message": err.Error()})
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return toolJSON(map[string]any{"error": "repo_read_failed", "message": firstNonEmpty(errorText(err), "path is not a regular file")})
	}
	if info.Size() > 4<<20 {
		return toolJSON(map[string]any{"error": "file_too_large", "message": "file exceeds the 4 MiB read limit"})
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return toolJSON(map[string]any{"error": "repo_read_failed", "message": err.Error()})
	}
	select {
	case <-ctx.Done():
		return toolJSON(map[string]any{"error": "cancelled", "message": ctx.Err().Error()})
	default:
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return toolJSON(map[string]any{"error": "binary_file", "message": "binary files are not readable through repo_read"})
	}
	start := clampToolInt(args, "start_line", 1, 1, 1_000_000)
	end := clampToolInt(args, "end_line", start+399, start, 1_000_000)
	maxChars := clampToolInt(args, "max_chars", 50000, 1000, 100000)
	lines := strings.Split(string(data), "\n")
	if start > len(lines) {
		return toolJSON(map[string]any{"path": filepath.ToSlash(relative), "start_line": start, "end_line": start - 1, "content": "", "truncated": false})
	}
	if end > len(lines) {
		end = len(lines)
	}
	content, truncated := truncateString(strings.Join(lines[start-1:end], "\n"), maxChars)
	return toolJSON(map[string]any{
		"path": filepath.ToSlash(relative), "start_line": start, "end_line": end,
		"content": content, "truncated": truncated,
	})
}

func repoSearchTool(ctx context.Context, workdir string, args map[string]any) string {
	repoRoot, _ := filepath.Abs(filepath.Clean(workdir))
	if resolvedRoot, resolveErr := filepath.EvalSymlinks(repoRoot); resolveErr == nil {
		repoRoot = resolvedRoot
	}
	query := toolStringArg(args, "query", 2000)
	if query == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "query is required"})
	}
	relative := toolStringArg(args, "path", 2000)
	full, err := safeResidentRepoPath(repoRoot, relative)
	if err != nil {
		return toolJSON(map[string]any{"error": "invalid_path", "message": err.Error()})
	}
	caseSensitive, _ := args["case_sensitive"].(bool)
	needle := query
	if !caseSensitive {
		needle = strings.ToLower(needle)
	}
	limit := clampToolInt(args, "max_results", 100, 1, 500)
	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	var matches []match
	truncated := false
	err = filepath.WalkDir(full, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if item.IsDir() {
			if path != full && residentRepoSkippedDirs[item.Name()] {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(repoRoot, path)
			if residentRepoPathDenied(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if item.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		if residentRepoPathDenied(rel) {
			return nil
		}
		info, infoErr := item.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 2<<20)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			haystack := line
			if !caseSensitive {
				haystack = strings.ToLower(haystack)
			}
			if !strings.Contains(haystack, needle) {
				continue
			}
			matches = append(matches, match{Path: filepath.ToSlash(rel), Line: lineNumber, Text: limitString(strings.TrimSpace(line), 500)})
			if len(matches) >= limit {
				truncated = true
				break
			}
		}
		_ = file.Close()
		if truncated {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return toolJSON(map[string]any{"error": "repo_search_failed", "message": err.Error()})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Path == matches[j].Path {
			return matches[i].Line < matches[j].Line
		}
		return matches[i].Path < matches[j].Path
	})
	return toolJSON(map[string]any{"query": query, "matches": matches, "truncated": truncated})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func residentRepoPathDepth(relative string) int {
	relative = filepath.ToSlash(filepath.Clean(relative))
	if relative == "." || relative == "" {
		return 0
	}
	return len(strings.Split(relative, "/"))
}

func residentRepoPathDenied(relative string) bool {
	relative = filepath.ToSlash(filepath.Clean(relative))
	for _, segment := range strings.Split(relative, "/") {
		lower := strings.ToLower(strings.TrimSpace(segment))
		if lower == "" || lower == "." {
			continue
		}
		if residentRepoSkippedDirs[lower] || residentRepoSensitiveName(lower) {
			return true
		}
	}
	return false
}

func residentRepoSensitiveName(name string) bool {
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true
	}
	switch name {
	case ".mcp.json", ".netrc", ".npmrc", ".pypirc", "id_rsa", "id_ed25519":
		return true
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
