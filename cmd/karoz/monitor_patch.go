package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

type monitorHTTPPatch struct {
	Revision    int                   `json:"revision"`
	Name        *string               `json:"name,omitempty"`
	Action      *monitordomain.Action `json:"action,omitempty"`
	State       *monitordomain.State  `json:"state,omitempty"`
	CooldownMS  *int64                `json:"cooldown_ms,omitempty"`
	MaxTriggers *int                  `json:"max_triggers,omitempty"`
}

func (a *app) patchMonitorHTTP(
	project Project,
	monitorID string,
	request *http.Request,
) (Monitor, error) {
	var raw map[string]json.RawMessage
	if err := readJSON(request, &raw); err != nil {
		return Monitor{}, err
	}
	allowed := map[string]bool{
		"revision": true, "name": true, "action": true,
		"state": true, "cooldown_ms": true, "max_triggers": true,
	}
	for field := range raw {
		if !allowed[field] {
			return Monitor{}, errors.New(
				"dev_turn_required: monitor trigger fields cannot be patched over HTTP",
			)
		}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return Monitor{}, err
	}
	var patch monitorHTTPPatch
	if err := json.Unmarshal(encoded, &patch); err != nil {
		return Monitor{}, err
	}
	if patch.Revision <= 0 {
		return Monitor{}, errors.New("monitor revision is required")
	}
	a.backgroundOwnerMu.Lock()
	defer a.backgroundOwnerMu.Unlock()
	a.mu.Lock()
	items := a.monitors[project.ID]
	for index := range items {
		if items[index].ID != monitorID {
			continue
		}
		before := cloneMonitorList(items)
		candidate := items[index]
		if patch.Revision != candidate.Revision {
			a.mu.Unlock()
			return Monitor{}, errors.New("monitor revision conflict")
		}
		if patch.Name != nil {
			candidate.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.Action != nil {
			nextRevision := candidate.Action.Revision + 1
			candidate.Action = *patch.Action
			candidate.Action.Revision = nextRevision
		}
		if patch.State != nil {
			if *patch.State != monitordomain.StateActive &&
				*patch.State != monitordomain.StateDisabled {
				a.mu.Unlock()
				return Monitor{}, errors.New("invalid monitor state")
			}
			if *patch.State == monitordomain.StateActive &&
				candidate.ErrorCode == "owner_deleted" {
				a.mu.Unlock()
				return Monitor{}, errors.New(
					"owner-deleted monitor cannot be resumed",
				)
			}
			if *patch.State == monitordomain.StateActive &&
				candidate.ErrorCode == "probe_authorization" {
				a.mu.Unlock()
				return Monitor{}, errors.New(
					"probe authorization error requires a newly approved trigger revision",
				)
			}
			candidate.State = *patch.State
		}
		if patch.CooldownMS != nil {
			candidate.CooldownMS = *patch.CooldownMS
		}
		if patch.MaxTriggers != nil {
			candidate.MaxTriggers = *patch.MaxTriggers
		}
		candidate.Revision++
		candidate.UpdatedAt = time.Now().UTC()
		if candidate.Action.Kind == monitordomain.ActionNotifyAgent {
			if !a.projectAgentExistsLocked(
				project.ID,
				candidate.Action.AgentID,
			) {
				a.mu.Unlock()
				return Monitor{}, errors.New("monitor notify target agent not found")
			}
		}
		if candidate.Trigger.Kind == monitordomain.TriggerScriptProbe &&
			candidate.State == monitordomain.StateActive {
			if !scriptProbeSupported {
				a.mu.Unlock()
				return Monitor{}, errScriptProbeUnsupported
			}
			if a.enabledScriptProbeCountLocked(project.ID, candidate.ID) >=
				maximumEnabledProbes {
				a.mu.Unlock()
				return Monitor{}, errors.New("enabled script probe capacity exceeded")
			}
			receipt, ok := a.monitorProbeReceipts[candidate.Trigger.ApprovalReceiptID]
			if !ok {
				a.mu.Unlock()
				return Monitor{}, errors.New("probe approval receipt not found")
			}
			if _, err := a.authorizedMonitorProbeSnapshot(
				candidate,
				receipt,
			); err != nil {
				a.mu.Unlock()
				return Monitor{}, err
			}
		}
		if err := monitordomain.ValidateMonitor(candidate); err != nil {
			a.mu.Unlock()
			return Monitor{}, err
		}
		items[index] = candidate
		a.monitors[project.ID] = items
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitors[project.ID] = before
			a.mu.Unlock()
			return Monitor{}, err
		}
		a.mu.Unlock()
		if candidate.Trigger.Kind == monitordomain.TriggerScriptProbe {
			if candidate.State == monitordomain.StateActive {
				a.armMonitorProbe(candidate)
			} else {
				a.cancelMonitorProbe(project.ID, candidate.ID)
			}
		}
		return candidate, nil
	}
	a.mu.Unlock()
	return Monitor{}, errors.New("monitor not found")
}

func (a *app) projectAgentExistsLocked(projectID, agentID string) bool {
	for _, agent := range a.agents[projectID] {
		if agent.ID == agentID {
			return true
		}
	}
	return false
}
