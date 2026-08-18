package main

import (
	"errors"
	"fmt"
	persistenceadapter "github.com/karoz/karoz/internal/persistence"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *app) bootstrap() error {
	if err := os.MkdirAll(a.settings.DataDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(a.settings.ProjectsRoot, 0755); err != nil {
		return err
	}
	if err := a.loadTasks(); err != nil {
		return err
	}
	if err := a.loadAgents(); err != nil {
		return err
	}
	if err := a.loadProjectCoordinationState(); err != nil {
		return err
	}
	if err := a.reconcileAgentGroups(); err != nil {
		return err
	}
	if err := a.loadArtifacts(); err != nil {
		return err
	}
	if err := a.loadMemories(); err != nil {
		return err
	}
	if err := a.loadBlackboard(); err != nil {
		return err
	}
	if err := a.loadInbox(); err != nil {
		return err
	}
	if err := a.loadTaskHooks(); err != nil {
		return err
	}
	if err := a.loadAgentRoutes(); err != nil {
		return err
	}
	if err := a.loadAgentSessionEvents(); err != nil {
		return err
	}
	if err := a.loadMonitors(); err != nil {
		return err
	}
	if err := a.loadProjectAliases(); err != nil {
		return err
	}
	if err := a.loadScheduledRuns(); err != nil {
		return err
	}
	// Terminal recovery delivers into the already-loaded durable agent message
	// stream. Load the scheduler first: a recovery event can immediately freeze
	// and admit a monitor fire without a later queue recovery overwriting it.
	if err := a.bootstrapProcessRuntime(); err != nil {
		return err
	}
	if err := a.reconcileProjectImportIntents(); err != nil {
		return err
	}
	a.resumeMonitorPending()
	a.armMonitorProbes()
	if err := a.reconcileWorkspaceArtifacts(); err != nil {
		return err
	}
	return a.rebuildBlackboardProjections()
}

func (a *app) loadArtifacts() error {
	projects, err := a.scanProjects()
	if err != nil {
		return err
	}
	loaded := map[string][]Artifact{}
	for _, project := range projects {
		var items []Artifact
		found, loadErr := persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Load("artifacts.json", &items)
		if loadErr != nil {
			return loadErr
		}
		if found {
			loaded[project.ID] = items
		}
	}
	changed := false
	for projectID, artifacts := range loaded {
		for i := range artifacts {
			if artifacts[i].Revision <= 0 {
				artifacts[i].Revision = 1
				changed = true
			}
			if strings.TrimSpace(artifacts[i].Status) == "" {
				artifacts[i].Status = ArtifactDraft
				changed = true
			}
			if strings.TrimSpace(artifacts[i].Kind) == "" {
				artifacts[i].Kind = inferArtifactKind(artifacts[i].Path)
				changed = true
			}
			if artifacts[i].UpdatedAt.IsZero() {
				artifacts[i].UpdatedAt = artifacts[i].CreatedAt
				changed = true
			}
			artifacts[i].Previewable = artifactPreviewable(artifacts[i].MimeType)
		}
		loaded[projectID] = artifacts
	}
	a.mu.Lock()
	a.artifactCatalogLocked().artifacts = loaded
	a.mu.Unlock()
	if changed {
		return a.saveArtifacts()
	}
	return nil
}

// saveOrLog keeps best-effort persistence visible: saves whose errors are not
// actionable at the call site are logged instead of silently discarded.
func (a *app) saveOrLog(what string, err error) {
	if err != nil {
		log.Printf("save %s: %v", what, err)
	}
}

func (a *app) saveArtifacts() error {
	// Resolve project paths before taking a.mu: projectByID locks a.mu
	// internally (applyProjectAlias), so it cannot run while a.mu is held.
	a.mu.Lock()
	catalog := a.artifactCatalogLocked()
	projectIDs := make([]string, 0, len(catalog.artifacts))
	for projectID := range catalog.artifacts {
		projectIDs = append(projectIDs, projectID)
	}
	a.mu.Unlock()
	projectPaths := make(map[string]string, len(projectIDs))
	for _, projectID := range projectIDs {
		project, err := a.projectByID(projectID)
		if err != nil {
			return err
		}
		projectPaths[projectID] = project.Path
	}
	// Hold a.mu through the writes (same pattern as the other save* funcs) so
	// concurrent saves cannot snapshot and write artifacts.json out of order.
	a.mu.Lock()
	defer a.mu.Unlock()
	catalog = a.artifactCatalogLocked()
	for projectID, path := range projectPaths {
		if err := persistenceadapter.NewJSONStore(filepath.Join(path, ".karoz")).Save("artifacts.json", catalog.artifacts[projectID], 0644); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) loadSettings() error {
	var persisted Settings
	found, err := a.loadJSON("settings.json", &persisted)
	if err != nil || !found {
		return err
	}
	if strings.TrimSpace(persisted.ProjectsRoot) != "" {
		a.settings.ProjectsRoot = expandHome(persisted.ProjectsRoot)
	}
	a.settings.ExtraProjectsRoots = normalizeWorkspaceRoots(persisted.ExtraProjectsRoots, a.settings.ProjectsRoot)
	a.settings.MCPServers = normalizeMCPServers(persisted.MCPServers)
	return nil
}

func (a *app) saveSettings() error {
	return a.saveJSON("settings.json", a.settings, 0644)
}

func (a *app) loadProjectAliases() error {
	loaded := map[string]string{}
	found, err := a.loadJSON("project-aliases.json", &loaded)
	if err != nil {
		return err
	}
	if !found {
		loaded = map[string]string{}
	}
	a.mu.Lock()
	a.projectRegistryLocked().aliases = loaded
	a.mu.Unlock()
	return nil
}

func (a *app) saveProjectAliases() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("project-aliases.json", a.projectRegistryLocked().aliases, 0644)
}

func (a *app) loadTasks() error {
	loaded := map[string][]Task{}
	_, err := a.loadJSON("tasks.json", &loaded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.projectTasksLocked().tasks = loaded
	a.mu.Unlock()
	return err
}

func (a *app) saveTasks() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("tasks.json", a.projectTasksLocked().tasks, 0644)
}

func (a *app) loadAgents() error {
	loaded := map[string][]Agent{}
	_, err := a.loadJSON("agents.json", &loaded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.agentDirectoryLocked().agents = loaded
	a.mu.Unlock()
	return nil
}

func (a *app) saveAgents() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agents.json", a.agentDirectoryLocked().agents, 0644)
}

func (a *app) loadMemories() error {
	loaded := map[string][]AgentMemoryEntry{}
	_, err := a.loadJSON("agent-memory.json", &loaded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.memoryStoreLocked().entries = loaded
	a.mu.Unlock()
	return nil
}

func (a *app) saveMemories() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-memory.json", a.memoryStoreLocked().entries, 0644)
}

func (a *app) loadBlackboard() error {
	loaded := map[string][]AgentBlackboardEntry{}
	found, err := a.loadJSON("agent-blackboard.json", &loaded)
	if err != nil || !found {
		return err
	}
	changed := false
	for projectID, entries := range loaded {
		for i := range entries {
			if strings.TrimSpace(entries[i].SourceType) == "" {
				entries[i].SourceType = blackboardSourceAgentReport
				changed = true
			}
			if strings.TrimSpace(entries[i].SourceID) == "" {
				entries[i].SourceID = entries[i].ID
				changed = true
			}
			if entries[i].UpdatedAt.IsZero() {
				entries[i].UpdatedAt = entries[i].CreatedAt
				changed = true
			}
		}
		loaded[projectID] = entries
	}
	a.collaborationServiceLocked().ReplaceBlackboard(loaded)
	if changed {
		return a.saveBlackboard()
	}
	return nil
}

func (a *app) saveBlackboard() error {
	if strings.TrimSpace(a.settings.DataDir) == "" {
		return nil
	}
	return a.saveJSON("agent-blackboard.json", a.collaborationServiceLocked().BlackboardSnapshot(), 0644)
}

func (a *app) loadInbox() error {
	loaded := map[string][]AgentInboxMessage{}
	found, err := a.loadJSON("agent-inbox.json", &loaded)
	if err != nil || !found {
		return err
	}
	changed := false
	for key, messages := range loaded {
		for i := range messages {
			var itemChanged bool
			messages[i], itemChanged = normalizeHandoffMessage(messages[i])
			changed = changed || itemChanged
		}
		loaded[key] = messages
	}
	a.collaborationServiceLocked().ReplaceInbox(loaded)
	if changed {
		return a.saveInbox()
	}
	return nil
}

func (a *app) saveInbox() error {
	return a.saveJSON("agent-inbox.json", a.collaborationServiceLocked().InboxSnapshot(), 0644)
}

func (a *app) loadTaskHooks() error {
	loaded := map[string][]TaskRuntimeHook{}
	_, err := a.loadJSON("task-hooks.json", &loaded)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.projectTasksLocked().hooks = loaded
	a.mu.Unlock()
	return nil
}

func (a *app) saveTaskHooks() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("task-hooks.json", a.projectTasksLocked().hooks, 0644)
}

func (a *app) loadAgentRoutes() error {
	loaded := map[string][]AgentRoute{}
	_, err := a.loadJSON("agent-routes.json", &loaded)
	a.collaborationServiceLocked().ReplaceAllRoutes(loaded)
	return err
}

func (a *app) saveAgentRoutes() error {
	return a.saveJSON("agent-routes.json", a.collaborationServiceLocked().RoutesSnapshot(), 0644)
}

func (a *app) appendTaskLog(projectID, taskID, line string) {
	path := a.taskLogPath(projectID, taskID)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("task log open: %v", err)
		return
	}
	defer f.Close()
	for _, part := range strings.Split(strings.TrimRight(line, "\n"), "\n") {
		_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), part)
	}
}

func (a *app) readTaskLog(projectID, taskID string) ([]byte, error) {
	logs, err := os.ReadFile(a.taskLogPath(projectID, taskID))
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, nil
	}
	return logs, err
}

func (a *app) taskLogPath(projectID, taskID string) string {
	return filepath.Join(a.settings.DataDir, "task-logs", projectID, taskID+".log")
}
