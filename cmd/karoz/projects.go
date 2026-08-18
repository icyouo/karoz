package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	executiondomain "github.com/karoz/karoz/internal/execution"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (a *app) scanProjects() ([]Project, error) {
	projects, err := scanProjectsForSettings(a.settings)
	if err != nil {
		return nil, err
	}
	for index := range projects {
		projects[index] = a.applyProjectAlias(projects[index])
	}
	sort.Slice(projects, func(i, j int) bool {
		left := strings.ToLower(projects[i].Name)
		right := strings.ToLower(projects[j].Name)
		if left == right {
			return projects[i].Path < projects[j].Path
		}
		return left < right
	})
	return projects, nil
}

func scanProjectsForSettings(settings Settings) ([]Project, error) {
	projects := make([]Project, 0)
	seen := map[string]bool{}
	for index, root := range settings.WorkspaceRoots() {
		scanned, err := scanWorkspaceProjects(root, index == 0)
		if err != nil {
			if index == 0 {
				return nil, err
			}
			log.Printf("scan extra workspace %s: %v", root, err)
			continue
		}
		for _, project := range scanned {
			if seen[project.ID] {
				continue
			}
			seen[project.ID] = true
			projects = append(projects, project)
		}
	}
	return projects, nil
}

func (a *app) applyProjectAlias(project Project) Project {
	a.mu.Lock()
	alias := strings.TrimSpace(a.projectRegistryLocked().aliases[project.ID])
	a.mu.Unlock()
	if alias != "" {
		project.Name = alias
	}
	return project
}

func scanWorkspaceProjects(root string, main bool) ([]Project, error) {
	root = filepath.Clean(expandHome(root))
	workspaceType := "extra"
	if main {
		workspaceType = "main"
	}
	projects := make([]Project, 0)
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		project := projectFromPath(root, root, workspaceType)
		projects = append(projects, project)
		return projects, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return projects, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			continue
		}
		projects = append(projects, projectFromPath(path, root, workspaceType))
	}
	return projects, nil
}

func projectFromPath(path, workspaceRoot, workspaceType string) Project {
	path = filepath.Clean(path)
	branch := projectGitOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		branch = "main"
	}
	return Project{
		ID:            projectID(path),
		Name:          filepath.Base(path),
		Path:          path,
		WorkspaceRoot: filepath.Clean(workspaceRoot),
		WorkspaceType: workspaceType,
		DefaultBranch: branch,
		AgentName:     "karoz",
	}
}

func projectGitOutput(dir string, args ...string) string {
	result, err := executiondomain.NewHostRunner().Run(context.Background(), commandRequest(dir, "git", args...))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Output())
}

func (a *app) projectByID(id string) (Project, error) {
	projects, err := a.scanProjects()
	if err != nil {
		return Project{}, err
	}
	for _, project := range projects {
		if project.ID == id {
			return project, nil
		}
	}
	return Project{}, errors.New("project not found")
}

func (a *app) createProject(req ProjectCreateRequest) (Project, error) {
	if strings.EqualFold(strings.TrimSpace(req.Mode), "import") || strings.TrimSpace(req.Path) != "" {
		return a.importProject(req)
	}
	a.projectRegistryLocked().registrationMu.Lock()
	defer a.projectRegistryLocked().registrationMu.Unlock()

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Project{}, errors.New("project name is required")
	}
	if !isSafeProjectName(name) {
		return Project{}, errors.New("project name may only contain letters, numbers, dot, dash, and underscore")
	}
	a.mu.Lock()
	projectsRoot := a.settings.ProjectsRoot
	a.mu.Unlock()
	path := filepath.Join(projectsRoot, name)
	cleanRoot := filepath.Clean(projectsRoot)
	cleanPath := filepath.Clean(path)
	if cleanPath == cleanRoot || !strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator)) {
		return Project{}, errors.New("project path escapes projects root")
	}
	if _, err := os.Stat(cleanPath); err == nil {
		return Project{}, errors.New("project already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Project{}, err
	}
	if err := os.MkdirAll(cleanPath, 0755); err != nil {
		return Project{}, err
	}
	if out, err := a.runTaskCommand(context.Background(), cleanPath, "git", "init"); err != nil {
		return Project{}, fmt.Errorf("git init failed: %w: %s", err, strings.TrimSpace(out))
	}
	branch := a.gitOutput(cleanPath, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		branch = "main"
	}
	project := Project{
		ID:            projectID(cleanPath),
		Name:          name,
		Path:          cleanPath,
		WorkspaceRoot: cleanRoot,
		WorkspaceType: "main",
		DefaultBranch: branch,
		AgentName:     "karoz",
	}
	if err := initializeProjectKaroz(project.Path); err != nil {
		return Project{}, err
	}
	if err := a.registerProcessRuntimeProject(project); err != nil {
		return Project{}, fmt.Errorf("register project runtime: %w", err)
	}
	if hook := a.projectRegistryLocked().createAfterRegistrationHook; hook != nil {
		hook()
	}
	return project, nil
}

func (a *app) importProject(req ProjectCreateRequest) (Project, error) {
	projectPath := filepath.Clean(expandHome(strings.TrimSpace(req.Path)))
	if projectPath == "." || projectPath == "" {
		return Project{}, errors.New("project path is required")
	}
	info, err := os.Stat(projectPath)
	if err != nil {
		return Project{}, err
	}
	if !info.IsDir() {
		return Project{}, errors.New("project path must be a directory")
	}
	if _, err := os.Stat(filepath.Join(projectPath, ".git")); err != nil {
		return Project{}, errors.New("imported project must be a Git repository")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(projectPath)
	}
	project := projectFromPath(projectPath, projectPath, "extra")
	project.Name = name
	a.projectRegistryLocked().registrationMu.Lock()
	defer a.projectRegistryLocked().registrationMu.Unlock()
	a.mu.Lock()
	previousSettings := a.settings
	previousSettings.ExtraProjectsRoots = append(
		[]string(nil), a.settings.ExtraProjectsRoots...,
	)
	previousAliases := cloneProjectAliases(a.projectRegistryLocked().aliases)
	desiredSettings := previousSettings
	desiredSettings.ExtraProjectsRoots = normalizeWorkspaceRoots(
		append(append([]string(nil), previousSettings.ExtraProjectsRoots...), projectPath),
		previousSettings.ProjectsRoot,
	)
	desiredAliases := cloneProjectAliases(previousAliases)
	desiredAliases[project.ID] = name
	a.mu.Unlock()
	intent, err := a.buildProjectImportIntent(
		projectPath, name, desiredSettings, desiredAliases,
	)
	if err != nil {
		return Project{}, err
	}
	if err := a.registerProcessRuntimeProjectPrepared(project, intent, func() (bool, error) {
		if err := initializeProjectKaroz(project.Path); err != nil {
			return false, err
		}
		if err := a.processRuntimePersistenceFail(processPersistAfterImportWorkspace); err != nil {
			return false, err
		}
		a.mu.Lock()
		a.settings = desiredSettings
		a.projectRegistryLocked().aliases = cloneProjectAliases(desiredAliases)
		a.mu.Unlock()
		var settingsErr error
		if save := a.projectRegistryLocked().importSettingsSave; save != nil {
			settingsErr = save()
		} else {
			settingsErr = a.saveSettings()
		}
		if settingsErr != nil {
			actualDigest, digestErr := configFileSHA256(
				filepath.Join(a.settings.DataDir, "settings.json"),
			)
			if digestErr != nil {
				return true, errors.Join(settingsErr, digestErr)
			}
			if actualDigest == intent.SettingsAfterSHA256 {
				return true, settingsErr
			}
			if actualDigest != intent.SettingsBeforeSHA256 {
				return true, errors.Join(
					settingsErr, errors.New("project import settings commit is ambiguous"),
				)
			}
			a.mu.Lock()
			a.settings = previousSettings
			a.projectRegistryLocked().aliases = cloneProjectAliases(previousAliases)
			a.mu.Unlock()
			return false, settingsErr
		}
		settingsDigest, err := configFileSHA256(
			filepath.Join(a.settings.DataDir, "settings.json"),
		)
		if err != nil {
			return true, err
		}
		if settingsDigest != intent.SettingsAfterSHA256 {
			return true, errors.New("project import settings commit digest mismatch")
		}
		if err := a.processRuntimePersistenceFail(processPersistAfterImportSettings); err != nil {
			return true, err
		}
		if err := a.advanceProcessRuntimeProjectImport(
			project.ID, projectImportClaimed, projectImportSettingsCommitted,
		); err != nil {
			return true, err
		}
		if err := a.processRuntimePersistenceFail(processPersistAfterImportSettingsState); err != nil {
			return true, err
		}
		if err := a.saveProjectAliases(); err != nil {
			return true, err
		}
		aliasesDigest, err := configFileSHA256(
			filepath.Join(a.settings.DataDir, "project-aliases.json"),
		)
		if err != nil {
			return true, err
		}
		if aliasesDigest != intent.AliasesAfterSHA256 {
			return true, errors.New("project import aliases commit digest mismatch")
		}
		if err := a.processRuntimePersistenceFail(processPersistAfterImportAliases); err != nil {
			return true, err
		}
		if err := a.advanceProcessRuntimeProjectImport(
			project.ID, projectImportSettingsCommitted, projectImportAliasesCommitted,
		); err != nil {
			return true, err
		}
		if err := a.processRuntimePersistenceFail(processPersistAfterImportAliasesState); err != nil {
			return true, err
		}
		return true, nil
	}); err != nil {
		return Project{}, fmt.Errorf("register imported project runtime: %w", err)
	}
	return project, nil
}

func (a *app) buildProjectImportIntent(
	root, alias string,
	settings Settings,
	aliases map[string]string,
) (runtimeProjectImportIntent, error) {
	settingsBefore, err := configFileSHA256(filepath.Join(a.settings.DataDir, "settings.json"))
	if err != nil {
		return runtimeProjectImportIntent{}, err
	}
	aliasesBefore, err := configFileSHA256(filepath.Join(a.settings.DataDir, "project-aliases.json"))
	if err != nil {
		return runtimeProjectImportIntent{}, err
	}
	settingsAfter, err := configValueSHA256(settings)
	if err != nil {
		return runtimeProjectImportIntent{}, err
	}
	aliasesAfter, err := configValueSHA256(aliases)
	if err != nil {
		return runtimeProjectImportIntent{}, err
	}
	intent := runtimeProjectImportIntent{
		DesiredRoot: filepath.Clean(root), DesiredAlias: alias,
		SettingsBeforeSHA256: settingsBefore, SettingsAfterSHA256: settingsAfter,
		AliasesBeforeSHA256: aliasesBefore, AliasesAfterSHA256: aliasesAfter,
		Progress: projectImportClaimed,
	}
	return intent, validateRuntimeProjectImportIntent(intent)
}

func configValueSHA256(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func configFileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return missingConfigDigest, nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func cloneProjectAliases(aliases map[string]string) map[string]string {
	cloned := make(map[string]string, len(aliases))
	for id, name := range aliases {
		cloned[id] = name
	}
	return cloned
}

func initializeProjectKaroz(projectPath string) error {
	karozDir := filepath.Join(projectPath, ".karoz")
	marker := filepath.Join(karozDir, "ignore-initialized")
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(karozDir, 0755); err != nil {
		return err
	}
	ignorePath := filepath.Join(projectPath, ".gitignore")
	content, err := os.ReadFile(ignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	found := false
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "/.karoz/" {
			found = true
			break
		}
	}
	if !found {
		file, err := os.OpenFile(ignorePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		prefix := ""
		if len(content) > 0 && content[len(content)-1] != '\n' {
			prefix = "\n"
		}
		_, writeErr := file.WriteString(prefix + "/.karoz/\n")
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return os.WriteFile(marker, []byte("initialized\n"), 0644)
}
