package main

// agentDirectory owns the durable project Agent projection. Agent CRUD and
// default-agent policy are exposed through internal/agent.Service; this type
// is the repository state it coordinates.
type agentDirectory struct {
	agents map[string][]Agent
}

func newAgentDirectory() *agentDirectory {
	return &agentDirectory{agents: map[string][]Agent{}}
}

// agentDirectoryLocked returns the only mutable Agent projection. Callers
// must hold app.mu; laziness keeps isolated application fixtures explicit
// without preserving an app-owned compatibility map.
func (a *app) agentDirectoryLocked() *agentDirectory {
	if a.agentDirectory == nil {
		a.agentDirectory = newAgentDirectory()
	}
	return a.agentDirectory
}
