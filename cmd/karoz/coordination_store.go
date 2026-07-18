package main

import (
	persistenceadapter "github.com/karoz/karoz/internal/persistence"
	"path/filepath"
)

func (a *app) loadProjectCoordinationState() error {
	projects, err := a.scanProjects()
	if err != nil {
		return err
	}
	a.groups = map[string][]AgentGroup{}
	a.groupInbox = map[string][]GroupInboxMessage{}
	a.plans = map[string][]WorkPlan{}
	for _, project := range projects {
		store := persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz"))
		var groups []AgentGroup
		if found, err := store.Load("groups.json", &groups); err != nil {
			return err
		} else if found {
			a.groups[project.ID] = groups
		}
		var inbox []GroupInboxMessage
		if found, err := store.Load("group-inbox.json", &inbox); err != nil {
			return err
		} else if found {
			a.groupInbox[project.ID] = inbox
		}
		var plans []WorkPlan
		if found, err := store.Load("plans.json", &plans); err != nil {
			return err
		} else if found {
			a.plans[project.ID] = plans
		}
	}
	return nil
}

func (a *app) saveGroupsForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	items := append([]AgentGroup{}, a.groups[projectID]...)
	a.mu.Unlock()
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("groups.json", items, 0644)
}

func (a *app) saveGroupInboxForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	items := append([]GroupInboxMessage{}, a.groupInbox[projectID]...)
	a.mu.Unlock()
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("group-inbox.json", items, 0644)
}

func (a *app) savePlansForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	a.mu.Lock()
	items := append([]WorkPlan{}, a.plans[projectID]...)
	a.mu.Unlock()
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("plans.json", items, 0644)
}
