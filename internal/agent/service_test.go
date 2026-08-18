package agent

import "testing"

type memoryRepository struct {
	agents map[string][]Agent
}

func (repository *memoryRepository) List(projectID string) []Agent {
	return append([]Agent(nil), repository.agents[projectID]...)
}

func (repository *memoryRepository) Find(projectID, agentID string) (Agent, bool) {
	for _, agent := range repository.agents[projectID] {
		if agent.ID == agentID {
			return agent, true
		}
	}
	return Agent{}, false
}

func (repository *memoryRepository) Insert(agent Agent) {
	repository.agents[agent.ProjectID] = append(repository.agents[agent.ProjectID], agent)
}

func (repository *memoryRepository) Update(agent Agent) bool {
	for index := range repository.agents[agent.ProjectID] {
		if repository.agents[agent.ProjectID][index].ID == agent.ID {
			repository.agents[agent.ProjectID][index] = agent
			return true
		}
	}
	return false
}

func (repository *memoryRepository) Delete(projectID, agentID string) bool {
	agents := repository.agents[projectID]
	for index := range agents {
		if agents[index].ID != agentID {
			continue
		}
		repository.agents[projectID] = append(agents[:index], agents[index+1:]...)
		return true
	}
	return false
}

func (repository *memoryRepository) Mutate(projectID, agentID string, mutate func(*Agent) bool) (Agent, bool) {
	for index := range repository.agents[projectID] {
		if repository.agents[projectID][index].ID != agentID {
			continue
		}
		current := &repository.agents[projectID][index]
		if !mutate(current) {
			return *current, false
		}
		return *current, true
	}
	return Agent{}, false
}

func TestServiceDelegatesAgentCRUDToRepository(t *testing.T) {
	repository := &memoryRepository{agents: map[string][]Agent{}}
	service := NewService(repository)
	service.Insert(Agent{ID: "karoz", ProjectID: "project-1"})
	if agents := service.List("project-1"); len(agents) != 1 || agents[0].ID != "karoz" {
		t.Fatalf("list = %+v", agents)
	}
	agent, ok := service.Find("project-1", "karoz")
	if !ok || agent.ID != "karoz" {
		t.Fatalf("find = %+v, ok=%v", agent, ok)
	}
	agent.Nickname = "Karoz"
	if !service.Update(agent) {
		t.Fatal("update reported missing agent")
	}
	if updated, _ := service.Find("project-1", "karoz"); updated.Nickname != "Karoz" {
		t.Fatalf("updated agent = %+v", updated)
	}
	if !service.Delete("project-1", "karoz") || len(service.List("project-1")) != 0 {
		t.Fatal("delete did not remove agent")
	}
}

func TestServiceEnsureCreatesOnlyOneDefaultAgent(t *testing.T) {
	repository := &memoryRepository{agents: map[string][]Agent{}}
	service := NewService(repository)
	first, created := service.Ensure("project-1", func() Agent {
		return Agent{ID: "karoz", ProjectID: "project-1"}
	})
	second, createdAgain := service.Ensure("project-1", func() Agent {
		return Agent{ID: "unexpected", ProjectID: "project-1"}
	})
	if len(first) != 1 || len(second) != 1 || second[0].ID != "karoz" || !created || createdAgain {
		t.Fatalf("ensure results = first=%+v created=%v second=%+v createdAgain=%v", first, created, second, createdAgain)
	}
}

func TestServiceMutateDelegatesRepositoryMutation(t *testing.T) {
	repository := &memoryRepository{agents: map[string][]Agent{
		"project-1": {{ID: "worker", ProjectID: "project-1", Nickname: "before"}},
	}}
	service := NewService(repository)
	mutated, ok := service.Mutate("project-1", "worker", func(agent *Agent) bool {
		agent.Nickname = "after"
		return true
	})
	if !ok || mutated.Nickname != "after" {
		t.Fatalf("mutated agent = %+v, ok=%v", mutated, ok)
	}
}
