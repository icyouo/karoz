package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (a *app) scanProjects() ([]Project, error) {
	projects := make([]Project, 0)
	seen := map[string]bool{}
	for index, root := range a.settings.workspaceRoots() {
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
			projects = append(projects, a.applyProjectAlias(project))
		}
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

func (a *app) applyProjectAlias(project Project) Project {
	a.mu.Lock()
	alias := strings.TrimSpace(a.projectAliases[project.ID])
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
	branch := gitOutput(path, "rev-parse", "--abbrev-ref", "HEAD")
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
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Project{}, errors.New("project name is required")
	}
	if !isSafeProjectName(name) {
		return Project{}, errors.New("project name may only contain letters, numbers, dot, dash, and underscore")
	}
	path := filepath.Join(a.settings.ProjectsRoot, name)
	cleanRoot := filepath.Clean(a.settings.ProjectsRoot)
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
	if out, err := run(cleanPath, "git", "init"); err != nil {
		return Project{}, fmt.Errorf("git init failed: %w: %s", err, strings.TrimSpace(out))
	}
	branch := gitOutput(cleanPath, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		branch = "main"
	}
	return Project{
		ID:            projectID(cleanPath),
		Name:          name,
		Path:          cleanPath,
		WorkspaceRoot: cleanRoot,
		WorkspaceType: "main",
		DefaultBranch: branch,
		AgentName:     "karoz",
	}, nil
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
	a.mu.Lock()
	if a.projectAliases == nil {
		a.projectAliases = map[string]string{}
	}
	a.projectAliases[project.ID] = name
	a.settings.ExtraProjectsRoots = normalizeWorkspaceRoots(append(a.settings.ExtraProjectsRoots, projectPath), a.settings.ProjectsRoot)
	a.mu.Unlock()
	if err := a.saveProjectAliases(); err != nil {
		return Project{}, err
	}
	if err := a.saveSettings(); err != nil {
		return Project{}, err
	}
	return project, nil
}
