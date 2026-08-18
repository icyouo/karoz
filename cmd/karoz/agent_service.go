package main

import agentdomain "github.com/karoz/karoz/internal/agent"

// appAgentRepository is the JSON-backed repository for the agent service.
type appAgentRepository struct{ app *app }

func (a *app) ensureAgentService() *agentdomain.Service {
	if a.agentService == nil {
		a.agentService = agentdomain.NewService(appAgentRepository{app: a})
	}
	return a.agentService
}

func (repository appAgentRepository) List(projectID string) []agentdomain.Agent {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	return append([]agentdomain.Agent(nil), repository.app.agentDirectoryLocked().agents[projectID]...)
}

func (repository appAgentRepository) Find(projectID, agentID string) (agentdomain.Agent, bool) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	for _, agent := range repository.app.agentDirectoryLocked().agents[projectID] {
		if agent.ID == agentID {
			return agent, true
		}
	}
	return agentdomain.Agent{}, false
}

func (repository appAgentRepository) Insert(agent agentdomain.Agent) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	directory := repository.app.agentDirectoryLocked()
	directory.agents[agent.ProjectID] = append(directory.agents[agent.ProjectID], agent)
}

func (repository appAgentRepository) Update(agent agentdomain.Agent) bool {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	directory := repository.app.agentDirectoryLocked()
	for index := range directory.agents[agent.ProjectID] {
		if directory.agents[agent.ProjectID][index].ID == agent.ID {
			directory.agents[agent.ProjectID][index] = agent
			return true
		}
	}
	return false
}

func (repository appAgentRepository) Delete(projectID, agentID string) bool {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	directory := repository.app.agentDirectoryLocked()
	agents := directory.agents[projectID]
	for index := range agents {
		if agents[index].ID != agentID {
			continue
		}
		directory.agents[projectID] = append(agents[:index], agents[index+1:]...)
		return true
	}
	return false
}

func (repository appAgentRepository) Mutate(projectID, agentID string, mutate func(*agentdomain.Agent) bool) (agentdomain.Agent, bool) {
	repository.app.mu.Lock()
	defer repository.app.mu.Unlock()
	directory := repository.app.agentDirectoryLocked()
	for index := range directory.agents[projectID] {
		if directory.agents[projectID][index].ID != agentID {
			continue
		}
		current := &directory.agents[projectID][index]
		if !mutate(current) {
			return *current, false
		}
		return *current, true
	}
	return agentdomain.Agent{}, false
}
