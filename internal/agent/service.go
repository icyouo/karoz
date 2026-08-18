package agent

import "sync"

// Repository is the durable agent state port. The application can keep its
// current JSON-backed representation while agent lifecycle callers depend on
// this small contract instead of reaching into the aggregate map directly.
type Repository interface {
	List(projectID string) []Agent
	Find(projectID, agentID string) (Agent, bool)
	Insert(Agent)
	Update(Agent) bool
	Delete(projectID, agentID string) bool
	Mutate(projectID, agentID string, mutate func(*Agent) bool) (Agent, bool)
}

// Service owns the CRUD seam for project agents. Runtime state (active runs,
// transcripts, and collaboration topology) remains in the application until
// those lifecycles are moved behind their own ports.
type Service struct {
	repository Repository
	ensureMu   sync.Mutex
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

func (service *Service) List(projectID string) []Agent {
	if service == nil || service.repository == nil {
		return nil
	}
	return service.repository.List(projectID)
}

func (service *Service) Find(projectID, agentID string) (Agent, bool) {
	if service == nil || service.repository == nil {
		return Agent{}, false
	}
	return service.repository.Find(projectID, agentID)
}

func (service *Service) Insert(agent Agent) {
	if service == nil || service.repository == nil {
		return
	}
	service.repository.Insert(agent)
}

func (service *Service) Update(agent Agent) bool {
	if service == nil || service.repository == nil {
		return false
	}
	return service.repository.Update(agent)
}

func (service *Service) Delete(projectID, agentID string) bool {
	if service == nil || service.repository == nil {
		return false
	}
	return service.repository.Delete(projectID, agentID)
}

func (service *Service) Mutate(projectID, agentID string, mutate func(*Agent) bool) (Agent, bool) {
	if service == nil || service.repository == nil || mutate == nil {
		return Agent{}, false
	}
	return service.repository.Mutate(projectID, agentID, mutate)
}

// Ensure installs one default agent when a project has no persisted agents.
// The small lock matters during startup when multiple readers can ask for a
// project's agents at the same time.
func (service *Service) Ensure(projectID string, factory func() Agent) ([]Agent, bool) {
	if service == nil || service.repository == nil {
		return nil, false
	}
	service.ensureMu.Lock()
	defer service.ensureMu.Unlock()
	agents := service.repository.List(projectID)
	if len(agents) == 0 && factory != nil {
		service.repository.Insert(factory())
		agents = service.repository.List(projectID)
		return agents, true
	}
	return agents, false
}
