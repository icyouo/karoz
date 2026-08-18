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
	a.collaborationServiceLocked().ResetGroups()
	a.collaborationServiceLocked().ResetGroupInbox()
	a.collaborationServiceLocked().ResetPlans()
	for _, project := range projects {
		store := persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz"))
		var groups []AgentGroup
		if found, err := store.Load("groups.json", &groups); err != nil {
			return err
		} else if found {
			a.collaborationServiceLocked().ReplaceGroups(project.ID, groups)
		}
		var inbox []GroupInboxMessage
		if found, err := store.Load("group-inbox.json", &inbox); err != nil {
			return err
		} else if found {
			a.collaborationServiceLocked().ReplaceGroupInbox(project.ID, inbox)
		}
		var plans []WorkPlan
		if found, err := store.Load("plans.json", &plans); err != nil {
			return err
		} else if found {
			a.collaborationServiceLocked().ReplacePlans(project.ID, plans)
		}
	}
	return nil
}

func (a *app) saveGroupsForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	items := a.collaborationServiceLocked().GroupsFor(projectID)
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("groups.json", items, 0644)
}

func (a *app) saveGroupInboxForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	items := a.collaborationServiceLocked().GroupInboxFor(projectID)
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("group-inbox.json", items, 0644)
}

func (a *app) savePlansForProject(projectID string) error {
	project, err := a.projectByID(projectID)
	if err != nil {
		return err
	}
	items := a.collaborationServiceLocked().PlansFor(projectID)
	return persistenceadapter.NewJSONStore(filepath.Join(project.Path, ".karoz")).Save("plans.json", items, 0644)
}
