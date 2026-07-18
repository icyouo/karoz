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
	if err := a.loadArchives(); err != nil {
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
	if err := a.loadAgentMessages(); err != nil {
		return err
	}
	if err := a.loadAgentSessions(); err != nil {
		return err
	}
	if err := a.loadProjectAliases(); err != nil {
		return err
	}
	if err := a.loadScheduledRuns(); err != nil {
		return err
	}
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
	a.artifacts = map[string][]Artifact{}
	for _, project := range projects {
		var items []Artifact
		found, loadErr := persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Load("artifacts.json", &items)
		if loadErr != nil {
			return loadErr
		}
		if found {
			a.artifacts[project.ID] = items
		}
	}
	changed := false
	for projectID, artifacts := range a.artifacts {
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
		a.artifacts[projectID] = artifacts
	}
	if changed {
		return a.saveArtifacts()
	}
	return nil
}

func (a *app) saveArtifacts() error {
	a.mu.Lock()
	snapshot := make(map[string][]Artifact, len(a.artifacts))
	for projectID, items := range a.artifacts {
		snapshot[projectID] = append([]Artifact{}, items...)
	}
	a.mu.Unlock()
	for projectID, items := range snapshot {
		project, err := a.projectByID(projectID)
		if err != nil {
			return err
		}
		if err := persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("artifacts.json", items, 0644); err != nil {
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
	found, err := a.loadJSON("project-aliases.json", &a.projectAliases)
	if err != nil {
		return err
	}
	if !found {
		if a.projectAliases == nil {
			a.projectAliases = map[string]string{}
		}
		return nil
	}
	if a.projectAliases == nil {
		a.projectAliases = map[string]string{}
	}
	return nil
}

func (a *app) saveProjectAliases() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.projectAliases == nil {
		a.projectAliases = map[string]string{}
	}
	return a.saveJSON("project-aliases.json", a.projectAliases, 0644)
}

func (a *app) loadTasks() error {
	_, err := a.loadJSON("tasks.json", &a.tasks)
	return err
}

func (a *app) saveTasks() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("tasks.json", a.tasks, 0644)
}

func (a *app) loadAgents() error {
	_, err := a.loadJSON("agents.json", &a.agents)
	if err != nil {
		return err
	}
	if a.agents == nil {
		a.agents = map[string][]Agent{}
	}
	return nil
}

func (a *app) saveAgents() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agents.json", a.agents, 0644)
}

func (a *app) loadArchives() error {
	_, err := a.loadJSON("agent-archive-messages.json", &a.archives)
	return err
}

func (a *app) saveArchives() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-archive-messages.json", a.archives, 0644)
}

func (a *app) loadMemories() error {
	_, err := a.loadJSON("agent-memory.json", &a.memories)
	return err
}

func (a *app) saveMemories() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-memory.json", a.memories, 0644)
}

func (a *app) loadBlackboard() error {
	found, err := a.loadJSON("agent-blackboard.json", &a.blackboard)
	if err != nil || !found {
		return err
	}
	changed := false
	for projectID, entries := range a.blackboard {
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
		a.blackboard[projectID] = entries
	}
	if changed {
		return a.saveBlackboard()
	}
	return nil
}

func (a *app) saveBlackboard() error {
	if strings.TrimSpace(a.settings.DataDir) == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-blackboard.json", a.blackboard, 0644)
}

func (a *app) loadInbox() error {
	found, err := a.loadJSON("agent-inbox.json", &a.inbox)
	if err != nil || !found {
		return err
	}
	changed := false
	for key, messages := range a.inbox {
		for i := range messages {
			var itemChanged bool
			messages[i], itemChanged = normalizeHandoffMessage(messages[i])
			changed = changed || itemChanged
		}
		a.inbox[key] = messages
	}
	if changed {
		return a.saveInbox()
	}
	return nil
}

func (a *app) saveInbox() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-inbox.json", a.inbox, 0644)
}

func (a *app) loadTaskHooks() error {
	_, err := a.loadJSON("task-hooks.json", &a.taskHooks)
	return err
}

func (a *app) saveTaskHooks() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("task-hooks.json", a.taskHooks, 0644)
}

func (a *app) loadAgentRoutes() error {
	_, err := a.loadJSON("agent-routes.json", &a.agentRoutes)
	return err
}

func (a *app) saveAgentRoutes() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-routes.json", a.agentRoutes, 0644)
}

func (a *app) loadAgentMessages() error {
	found, err := a.loadJSON("agent-messages.json", &a.agentMessages)
	if err != nil || !found {
		return err
	}
	changed := false
	for key, messages := range a.agentMessages {
		for i := range messages {
			if messages[i].Seq <= 0 {
				messages[i].Seq = int64(i + 1)
				changed = true
			}
			if strings.TrimSpace(messages[i].SessionID) == "" {
				parts := strings.SplitN(key, "/", 2)
				if len(parts) == 2 {
					messages[i].SessionID = residentSessionID(parts[0], parts[1])
					changed = true
				}
			}
		}
		a.agentMessages[key] = messages
	}
	if changed {
		return a.saveAgentMessages()
	}
	return nil
}

func (a *app) saveAgentMessages() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-messages.json", a.agentMessages, 0644)
}

func (a *app) loadAgentSessions() error {
	_, err := a.loadJSON("agent-session-state.json", &a.agentSessions)
	if err != nil {
		return err
	}
	changed := false
	for key, state := range a.agentSessions {
		normalized := normalizeResidentSummary(state.ResidentSummary, 6000)
		if normalized != state.ResidentSummary {
			state.ResidentSummary = normalized
			a.agentSessions[key] = state
			changed = true
		}
	}
	if changed {
		return a.saveAgentSessions()
	}
	return nil
}

func (a *app) saveAgentSessions() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveJSON("agent-session-state.json", a.agentSessions, 0644)
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
