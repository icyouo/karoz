package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (a *app) groupsForProject(projectID string) []AgentGroup {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AgentGroup{}, a.groups[projectID]...)
}

func (a *app) groupByID(projectID, groupID string) (AgentGroup, bool) {
	for _, group := range a.groupsForProject(projectID) {
		if group.ID == groupID {
			return group, true
		}
	}
	return AgentGroup{}, false
}

func (a *app) groupForAgent(projectID, agentID string) (AgentGroup, bool) {
	for _, group := range a.groupsForProject(projectID) {
		for _, memberID := range group.MemberAgentIDs {
			if memberID == agentID {
				return group, true
			}
		}
	}
	return AgentGroup{}, false
}

func (a *app) isGroupCoordinator(projectID, agentID string) bool {
	group, ok := a.groupForAgent(projectID, agentID)
	return ok && group.CoordinatorAgentID == agentID
}

func (a *app) upsertAgentGroup(projectID, groupID, name, templateID, coordinatorAgentID string, members []Agent) (AgentGroup, error) {
	if strings.TrimSpace(groupID) == "" || strings.TrimSpace(coordinatorAgentID) == "" {
		return AgentGroup{}, errors.New("group id and coordinator are required")
	}
	now := time.Now().UTC()
	memberIDs := make([]string, 0, len(members))
	coordinatorFound := false
	for _, member := range members {
		if member.GroupID != groupID {
			continue
		}
		memberIDs = append(memberIDs, member.ID)
		coordinatorFound = coordinatorFound || member.ID == coordinatorAgentID
	}
	if !coordinatorFound {
		return AgentGroup{}, errors.New("coordinator must be a group member")
	}
	sort.Strings(memberIDs)
	a.mu.Lock()
	if a.groups == nil {
		a.groups = map[string][]AgentGroup{}
	}
	items := append([]AgentGroup{}, a.groups[projectID]...)
	group := AgentGroup{ID: groupID, ProjectID: projectID, Name: firstNonEmpty(name, groupID), TemplateID: templateID, CoordinatorAgentID: coordinatorAgentID, MemberAgentIDs: memberIDs, Version: 1, CreatedAt: now, UpdatedAt: now}
	found := false
	for i := range items {
		if items[i].ID != groupID {
			continue
		}
		group.CreatedAt = items[i].CreatedAt
		group.Version = items[i].Version + 1
		items[i] = group
		found = true
		break
	}
	if !found {
		items = append(items, group)
	}
	a.groups[projectID] = items
	a.mu.Unlock()
	if err := a.saveGroupsForProject(projectID); err != nil {
		// Some unit-level and embedded callers construct an in-memory project
		// without registering it in the project scanner. Keep the group usable in
		// memory; real projects are persisted through their project-local store.
		if err.Error() == "project not found" {
			return group, nil
		}
		return AgentGroup{}, err
	}
	return group, nil
}

func (a *app) transferGroupCoordinator(project Project, groupID, coordinatorAgentID string) (AgentGroup, error) {
	group, ok := a.groupByID(project.ID, groupID)
	if !ok {
		return AgentGroup{}, errors.New("group not found")
	}
	member := false
	for _, id := range group.MemberAgentIDs {
		member = member || id == coordinatorAgentID
	}
	if !member {
		return AgentGroup{}, errors.New("coordinator must be a group member")
	}
	oldCoordinator := group.CoordinatorAgentID
	group.CoordinatorAgentID = coordinatorAgentID
	group.Version++
	group.UpdatedAt = time.Now().UTC()
	a.mu.Lock()
	for i := range a.groups[project.ID] {
		if a.groups[project.ID][i].ID == groupID {
			a.groups[project.ID][i] = group
		}
	}
	for i := range a.plans[project.ID] {
		if a.plans[project.ID][i].OwnerType == "group" && a.plans[project.ID][i].OwnerGroupID == groupID && a.plans[project.ID][i].Status != PlanCompleted && a.plans[project.ID][i].Status != PlanCancelled {
			a.plans[project.ID][i].OwnerAgentID = coordinatorAgentID
			a.plans[project.ID][i].Version++
			a.plans[project.ID][i].UpdatedAt = group.UpdatedAt
		}
	}
	a.mu.Unlock()
	if err := a.saveGroupsForProject(project.ID); err != nil {
		return AgentGroup{}, err
	}
	if err := a.savePlansForProject(project.ID); err != nil {
		return AgentGroup{}, err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ProjectID: project.ID, Kind: "group_coordinator_changed", EntityID: groupID, From: oldCoordinator, To: coordinatorAgentID, Reason: "coordinator_transferred", CreatedAt: group.UpdatedAt})
	return group, nil
}

func (a *app) reconcileAgentGroups() error {
	projects, err := a.scanProjects()
	if err != nil {
		return err
	}
	for _, project := range projects {
		agents := a.projectAgents(project)
		byGroup := map[string][]Agent{}
		for _, agent := range agents {
			if strings.TrimSpace(agent.GroupID) != "" {
				byGroup[agent.GroupID] = append(byGroup[agent.GroupID], agent)
			}
		}
		for groupID, members := range byGroup {
			if existing, ok := a.groupByID(project.ID, groupID); ok {
				coordinator := existing.CoordinatorAgentID
				valid := false
				for _, member := range members {
					valid = valid || member.ID == coordinator
				}
				if valid {
					_, err = a.upsertAgentGroup(project.ID, groupID, existing.Name, existing.TemplateID, coordinator, members)
					if err != nil {
						return err
					}
					continue
				}
			}
			sort.SliceStable(members, func(i, j int) bool {
				if members[i].GroupOrder == members[j].GroupOrder {
					return members[i].ID < members[j].ID
				}
				return members[i].GroupOrder < members[j].GroupOrder
			})
			coordinatorID := members[0].ID
			templateID := ""
			for _, team := range residentAgentTeams() {
				if groupID == team.ID || strings.HasPrefix(groupID, team.ID+"-") {
					templateID = team.ID
					for _, member := range members {
						if member.GroupRole == team.CoordinatorMemberID {
							coordinatorID = member.ID
						}
					}
					break
				}
			}
			if _, err := a.upsertAgentGroup(project.ID, groupID, firstNonEmpty(members[0].GroupName, groupID), templateID, coordinatorID, members); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) sendToGroup(projectID, sourceAgentID, parentRunID string, args map[string]any) string {
	groupID := toolStringArg(args, "group_id", 128)
	group, ok := a.groupByID(projectID, groupID)
	if !ok {
		return toolJSON(map[string]any{"error": "group_not_found", "message": "target group was not found"})
	}
	if sourceAgentID != "karoz" {
		if sourceGroup, grouped := a.groupForAgent(projectID, sourceAgentID); grouped && sourceGroup.CoordinatorAgentID != sourceAgentID {
			return toolJSON(map[string]any{"error": "group_coordinator_required", "message": "cross-group work must be routed by the source group coordinator"})
		}
	}
	body := toolStringArg(args, "body", 20000)
	if body == "" {
		return toolJSON(map[string]any{"error": "validation_error", "message": "body is required"})
	}
	now := time.Now().UTC()
	message := GroupInboxMessage{
		ID: randomID(), ProjectID: projectID, GroupID: groupID, SourceAgentID: sourceAgentID,
		CoordinatorAgentID: group.CoordinatorAgentID, PreferredMemberID: toolStringArg(args, "preferred_member_id", 128),
		Intent: firstNonEmpty(toolStringArg(args, "intent", 64), "request"), Subject: firstNonEmpty(toolStringArg(args, "subject", 500), "Group handoff"), Body: body,
		Objective:      firstNonEmpty(toolStringArg(args, "objective", 4000), toolStringArg(args, "subject", 500), "Complete the requested group work."),
		ExpectedOutput: firstNonEmpty(toolStringArg(args, "expected_output", 4000), "A concrete group result or explicit blocker."), Status: "delegated",
		ParentPlanID: toolStringArg(args, "parent_plan_id", 128), ParentStepID: toolStringArg(args, "parent_step_id", 128), CreatedAt: now, UpdatedAt: now,
	}
	if sourceGroup, grouped := a.groupForAgent(projectID, sourceAgentID); grouped {
		message.SourceGroupID = sourceGroup.ID
	}
	forwardArgs := map[string]any{
		"target_agent_id": group.CoordinatorAgentID, "intent": message.Intent, "subject": message.Subject,
		"body":      fmt.Sprintf("[group:%s] %s\n\nPreferred member: %s", group.ID, message.Body, firstNonEmpty(message.PreferredMemberID, "coordinator decides")),
		"objective": message.Objective, "expected_output": message.ExpectedOutput,
	}
	result := a.sendToAgentWithRoute(projectID, sourceAgentID, parentRunID, forwardArgs, true)
	var decoded map[string]any
	if err := jsonUnmarshalToolResult(result, &decoded); err != nil || decoded["error"] != nil {
		return result
	}
	if id, _ := decoded["message_id"].(string); id != "" {
		message.AgentInboxMessageID = id
	}
	a.mu.Lock()
	if a.groupInbox == nil {
		a.groupInbox = map[string][]GroupInboxMessage{}
	}
	a.groupInbox[projectID] = append(a.groupInbox[projectID], message)
	a.mu.Unlock()
	if err := a.saveGroupInboxForProject(projectID); err != nil {
		return toolJSON(map[string]any{"error": "save_failed", "message": err.Error()})
	}
	return toolJSON(map[string]any{"group_message": message, "status": "delivered", "coordinator_agent_id": group.CoordinatorAgentID})
}

func jsonUnmarshalToolResult(result string, target any) error {
	if err := json.Unmarshal([]byte(result), target); err != nil {
		return fmt.Errorf("decode tool result: %w", err)
	}
	return nil
}
