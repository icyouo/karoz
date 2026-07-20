package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	persistenceadapter "github.com/karoz/karoz/internal/persistence"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func getenv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func expandHome(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func defaultProjectsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "karoz-projects"
	}
	return filepath.Join(home, "karoz-projects")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func normalizeWorkspaceRoots(roots []string, mainRoot string) []string {
	mainRoot = filepath.Clean(expandHome(strings.TrimSpace(mainRoot)))
	seen := map[string]bool{mainRoot: true}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		clean := filepath.Clean(expandHome(root))
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func (s Settings) workspaceRoots() []string {
	roots := []string{filepath.Clean(expandHome(s.ProjectsRoot))}
	roots = append(roots, normalizeWorkspaceRoots(s.ExtraProjectsRoots, s.ProjectsRoot)...)
	return roots
}

func projectID(path string) string {
	sum := sha1.Sum([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])[:12]
}

func taskID() string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())))
	return hex.EncodeToString(sum[:])[:16]
}

func messageID() string {
	sum := sha1.Sum([]byte(fmt.Sprintf("msg-%d-%d", time.Now().UnixNano(), os.Getpid())))
	return hex.EncodeToString(sum[:])[:16]
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func residentSessionID(projectID, agentID string) string {
	sum := sha1.Sum([]byte("resident-session:" + projectID + ":" + agentID))
	return hex.EncodeToString(sum[:])[:16]
}

func projectAgentKey(projectID, agentID string) string {
	return projectID + "/" + agentID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func limitString(value string, maxChars int) string {
	value = strings.TrimSpace(value)
	if maxChars <= 0 || len(value) <= maxChars {
		return value
	}
	if maxChars <= 3 {
		return value[:maxChars]
	}
	return strings.TrimSpace(value[:maxChars-3]) + "..."
}

func truncateString(value string, maxChars int) (string, bool) {
	if maxChars <= 0 || len(value) <= maxChars {
		return value, false
	}
	return value[:maxChars] + "\n...[truncated]", true
}

func slugify(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func splitPath(path string) []string {
	var out []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isSafeProjectName(name string) bool {
	if name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func normalizeTaskType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "deploy", "deployment", "release":
		return "deploy"
	case "bug", "bugfix", "fix":
		return "bug"
	case "feature", "development", "dev":
		return "feature"
	default:
		return "feature"
	}
}

func inferTaskType(message string) string {
	lower := strings.ToLower(message)
	if strings.Contains(lower, "deploy") || strings.Contains(message, "部署") || strings.Contains(message, "发布") {
		return "deploy"
	}
	if strings.Contains(lower, "bug") || strings.Contains(lower, "fix") || strings.Contains(message, "修复") || strings.Contains(message, "缺陷") {
		return "bug"
	}
	return "feature"
}

func defaultTaskTitle(taskType string) string {
	switch taskType {
	case "deploy":
		return "Deployment task"
	case "bug":
		return "Bugfix task"
	default:
		return "Feature task"
	}
}

func shortTitle(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(message, "\n", " "))
	if len([]rune(message)) <= 48 {
		return message
	}
	runes := []rune(message)
	return string(runes[:48])
}

func validAgentIntent(intent string) bool {
	switch intent {
	case "note", "request", "handoff", "status", "question", "result", "reply":
		return true
	default:
		return false
	}
}

func validActivityKind(kind string) bool {
	switch kind {
	case "focus", "start", "progress", "blocker", "handoff", "done", "error", "next_step", "decision_needed":
		return true
	default:
		return false
	}
}

func mimeTypeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".md", ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "text/plain; charset=utf-8"
	}
}

func (a *app) agentWorkspaceDir(projectID, agentID string) (string, error) {
	project, err := a.projectByID(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(project.Path, ".karoz", "artifacts", agentID), nil
}

func (a *app) listWorkspaceFiles(projectID, agentID string) ([]WorkspaceFile, error) {
	root, err := a.agentWorkspaceDir(projectID, agentID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	var files []WorkspaceFile
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, WorkspaceFile{
			Path:      filepath.ToSlash(rel),
			Filename:  entry.Name(),
			MimeType:  mimeTypeForPath(path),
			SizeBytes: info.Size(),
			UpdatedAt: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].UpdatedAt.Equal(files[j].UpdatedAt) {
			return strings.ToLower(files[i].Path) < strings.ToLower(files[j].Path)
		}
		return files[i].UpdatedAt.After(files[j].UpdatedAt)
	})
	return files, nil
}

func (a *app) getWorkspaceFilePreview(projectID, agentID, relPath string) (WorkspaceFilePreview, error) {
	full, err := a.safeWorkspacePath(projectID, agentID, relPath)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	root, err := a.agentWorkspaceDir(projectID, agentID)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return WorkspaceFilePreview{}, err
	}
	mimeType := mimeTypeForPath(full)
	content := string(data)
	encoding := "utf-8"
	if strings.HasPrefix(mimeType, "image/") && mimeType != "image/svg+xml" {
		content = base64.StdEncoding.EncodeToString(data)
		encoding = "base64"
	} else if len(content) > 200_000 {
		content = content[:200_000] + "\n...[truncated]"
	}
	return WorkspaceFilePreview{
		Path:     filepath.ToSlash(rel),
		Filename: filepath.Base(full),
		MimeType: mimeType,
		Encoding: encoding,
		Content:  content,
	}, nil
}

func (a *app) safeWorkspacePath(projectID, agentID, relPath string) (string, error) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(relPath) {
		return "", errors.New("absolute paths are not allowed")
	}
	workspace, err := a.agentWorkspaceDir(projectID, agentID)
	if err != nil {
		return "", err
	}
	root := filepath.Clean(workspace)
	full := filepath.Clean(filepath.Join(root, relPath))
	if full == root || !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("path escapes workspace")
	}
	return full, nil
}

func toolStatus(name string) ToolStatus {
	path, err := exec.LookPath(name)
	if err != nil {
		return ToolStatus{Available: false, Error: err.Error()}
	}
	version, _ := run("", name, "--version")
	return ToolStatus{Available: true, Path: path, Version: strings.TrimSpace(version)}
}

func gitOutput(dir string, args ...string) string {
	out, err := run(dir, "git", args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeJSONFileAtomic(path string, value any, perm os.FileMode) error {
	return persistenceadapter.SaveJSONAtomic(path, value, perm)
}

func toolStringArg(args map[string]any, key string, max int) string {
	value, _ := args[key].(string)
	value = strings.TrimSpace(value)
	if max > 0 {
		value = limitString(value, max)
	}
	return value
}

func toolBoolArg(args map[string]any, key string, defaultValue bool) bool {
	switch raw := args[key].(type) {
	case bool:
		return raw
	case string:
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1", "yes", "y":
			return true
		case "false", "0", "no", "n":
			return false
		}
	}
	return defaultValue
}

func clampToolInt(args map[string]any, key string, defaultValue, minValue, maxValue int) int {
	value := defaultValue
	switch raw := args[key].(type) {
	case float64:
		value = int(raw)
	case int:
		value = raw
	case json.Number:
		if n, err := raw.Int64(); err == nil {
			value = int(n)
		}
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func toolJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return `{"error":"marshal_failed"}`
	}
	return string(data)
}

func indentPrompt(value, prefix string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func memorySummary(entry AgentMemoryEntry) map[string]any {
	return map[string]any{
		"id":         entry.ID,
		"layer":      entry.Layer,
		"state":      entry.State,
		"priority":   entry.Priority,
		"summary":    entry.Summary,
		"detail":     entry.Detail,
		"metadata":   entry.Metadata,
		"created_at": entry.CreatedAt,
		"updated_at": entry.UpdatedAt,
	}
}

func (a *app) memorySummaries(entries []AgentMemoryEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, memorySummary(entry))
	}
	return out
}
