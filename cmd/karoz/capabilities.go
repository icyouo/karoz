package main

import agentdomain "github.com/karoz/karoz/internal/agent"

// AgentCapabilities is the single policy surface for product-level Agent
// permissions. Every resident Agent remains a first-class direct conversation
// target; capabilities only control privileged project operations and
// role-specific artifact behavior.
type AgentCapabilities = agentdomain.Capabilities

func capabilitiesForAgent(agent Agent) AgentCapabilities {
	return agentdomain.CapabilitiesFor(agent)
}
