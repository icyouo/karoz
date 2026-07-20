package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

func (a *app) plansForProject(projectID string) []WorkPlan {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := append([]WorkPlan{}, a.plans[projectID]...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return items
}

func (a *app) planByID(projectID, planID string) (WorkPlan, bool) {
	for _, plan := range a.plansForProject(projectID) {
		if plan.ID == planID {
			return plan, true
		}
	}
	return WorkPlan{}, false
}

func validatePlanSteps(steps []PlanStep) error {
	if len(steps) == 0 {
		return errors.New("at least one todo step is required")
	}
	seen := map[string]bool{}
	for i := range steps {
		steps[i].ID = strings.TrimSpace(steps[i].ID)
		if steps[i].ID == "" {
			steps[i].ID = fmt.Sprintf("step-%d", i+1)
		}
		if seen[steps[i].ID] {
			return fmt.Errorf("duplicate step id %s", steps[i].ID)
		}
		seen[steps[i].ID] = true
		if strings.TrimSpace(steps[i].Title) == "" {
			return fmt.Errorf("step %s title is required", steps[i].ID)
		}
	}
	for _, step := range steps {
		for _, dependency := range step.Dependencies {
			if dependency == step.ID || !seen[dependency] {
				return fmt.Errorf("step %s has invalid dependency %s", step.ID, dependency)
			}
		}
	}
	return nil
}

func normalizeDraftSteps(steps []PlanStep) []PlanStep {
	out := append([]PlanStep{}, steps...)
	for i := range out {
		if strings.TrimSpace(out[i].ID) == "" {
			out[i].ID = fmt.Sprintf("step-%d", i+1)
		}
		out[i].Status = PlanStepPending
		out[i].TaskAttempts = nil
		out[i].Reviews = nil
		out[i].Version = 1
	}
	return out
}

func (a *app) createPlanDraft(project Project, author Agent, requestedVia string, req WorkPlanDraftRequest) (WorkPlan, error) {
	if author.ID == "karoz" {
		return WorkPlan{}, errors.New("Karoz routes business plans to a group coordinator or standalone agent; it cannot own an ordinary WorkPlan")
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Goal = strings.TrimSpace(req.Goal)
	if req.Title == "" || req.Goal == "" {
		return WorkPlan{}, errors.New("title and goal are required")
	}
	req.Steps = normalizeDraftSteps(req.Steps)
	if err := validatePlanSteps(req.Steps); err != nil {
		return WorkPlan{}, err
	}
	ownerType := "agent"
	ownerAgentID := author.ID
	ownerGroupID := ""
	if group, grouped := a.groupForAgent(project.ID, author.ID); grouped {
		ownerType = "group"
		ownerGroupID = group.ID
		ownerAgentID = group.CoordinatorAgentID
	}
	now := time.Now().UTC()
	maxConcurrency := req.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	if maxConcurrency > 8 {
		maxConcurrency = 8
	}
	plan := WorkPlan{
		ID: randomID(), ProjectID: project.ID, Title: req.Title, Goal: req.Goal, Status: PlanDraft, Revision: 1,
		RequestedViaAgentID: strings.TrimSpace(requestedVia), AuthoredByAgentID: author.ID,
		OwnerType: ownerType, OwnerGroupID: ownerGroupID, OwnerAgentID: ownerAgentID,
		MaxConcurrency: maxConcurrency, Version: 1, Steps: req.Steps, CreatedAt: now, UpdatedAt: now,
	}
	a.mu.Lock()
	if a.plans == nil {
		a.plans = map[string][]WorkPlan{}
	}
	a.plans[project.ID] = append([]WorkPlan{plan}, a.plans[project.ID]...)
	a.mu.Unlock()
	if err := a.savePlansForProject(project.ID); err != nil {
		return WorkPlan{}, err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ProjectID: project.ID, Kind: "plan_changed", EntityID: plan.ID, To: plan.Status, Reason: "plan_drafted", CreatedAt: now})
	return plan, nil
}

func (a *app) replacePlan(plan WorkPlan, expectedVersion int64) (WorkPlan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := a.plans[plan.ProjectID]
	for i := range items {
		if items[i].ID != plan.ID {
			continue
		}
		if expectedVersion > 0 && items[i].Version != expectedVersion {
			return WorkPlan{}, errors.New("plan version changed; reload before updating")
		}
		plan.Version = items[i].Version + 1
		plan.UpdatedAt = time.Now().UTC()
		items[i] = plan
		a.plans[plan.ProjectID] = items
		return plan, nil
	}
	return WorkPlan{}, errors.New("plan not found")
}

func (a *app) submitPlan(projectID, planID, actorID string, expectedVersion int64) (WorkPlan, error) {
	plan, ok := a.planByID(projectID, planID)
	if !ok {
		return WorkPlan{}, errors.New("plan not found")
	}
	if actorID != plan.OwnerAgentID && actorID != plan.AuthoredByAgentID && actorID != "user" {
		return WorkPlan{}, errors.New("only the author, owner, or user can submit this plan")
	}
	if plan.Status != PlanDraft {
		return WorkPlan{}, errors.New("only a draft plan can be submitted")
	}
	plan.Status = PlanAwaitingApproval
	updated, err := a.replacePlan(plan, expectedVersion)
	if err == nil {
		err = a.savePlansForProject(projectID)
	}
	return updated, err
}

func historicalTaskSuccessful(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "success", "succeeded":
		return true
	default:
		return false
	}
}

func historicalTaskTerminal(status string) bool {
	if historicalTaskSuccessful(status) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "blocked", "cancelled", "canceled", "superseded", "deploy_failed":
		return true
	default:
		return false
	}
}

func (a *app) reconcilePlanHistory(project Project, actor Agent, req PlanHistoryReconciliationRequest) (WorkPlan, error) {
	plan, ok := a.planByID(project.ID, strings.TrimSpace(req.PlanID))
	if !ok {
		return WorkPlan{}, errors.New("plan not found")
	}
	if actor.ID != plan.OwnerAgentID && actor.ID != plan.AuthoredByAgentID {
		return WorkPlan{}, errors.New("only the plan owner or author can reconcile historical tasks")
	}
	if plan.Status != PlanDraft && plan.Status != PlanAwaitingApproval {
		return WorkPlan{}, errors.New("historical reconciliation is only allowed before plan activation")
	}
	if len(req.Steps) == 0 {
		return WorkPlan{}, errors.New("at least one step reconciliation is required")
	}
	seenSteps := map[string]bool{}
	seenTasks := map[string]bool{}
	now := time.Now().UTC()
	for _, item := range req.Steps {
		item.StepID = strings.TrimSpace(item.StepID)
		item.Decision = strings.ToLower(strings.TrimSpace(item.Decision))
		item.Evidence = strings.TrimSpace(item.Evidence)
		idx := planStepIndex(plan, item.StepID)
		if idx < 0 || seenSteps[item.StepID] {
			return WorkPlan{}, fmt.Errorf("invalid or duplicate plan step %s", item.StepID)
		}
		seenSteps[item.StepID] = true
		if item.Decision != "accepted" && item.Decision != "needs_review" {
			return WorkPlan{}, fmt.Errorf("step %s has invalid reconciliation decision", item.StepID)
		}
		if item.Evidence == "" || len(item.TaskIDs) == 0 {
			return WorkPlan{}, fmt.Errorf("step %s requires task ids and evidence", item.StepID)
		}
		step := &plan.Steps[idx]
		attemptsByTask := map[string]bool{}
		for _, attempt := range step.TaskAttempts {
			attemptsByTask[attempt.TaskID] = true
		}
		for _, taskID := range item.TaskIDs {
			taskID = strings.TrimSpace(taskID)
			if taskID == "" || seenTasks[taskID] {
				return WorkPlan{}, fmt.Errorf("invalid or duplicate historical task id for step %s", item.StepID)
			}
			seenTasks[taskID] = true
			task, exists := a.findTask(project.ID, taskID)
			if !exists {
				return WorkPlan{}, fmt.Errorf("historical task %s was not found", taskID)
			}
			if !historicalTaskTerminal(task.Status) {
				return WorkPlan{}, fmt.Errorf("historical task %s is not terminal", taskID)
			}
			if item.Decision == "accepted" && !historicalTaskSuccessful(task.Status) {
				return WorkPlan{}, fmt.Errorf("historical task %s is not successful and cannot be accepted", taskID)
			}
			if !attemptsByTask[task.ID] {
				step.TaskAttempts = append(step.TaskAttempts, PlanTaskAttempt{
					Attempt: len(step.TaskAttempts) + 1, TaskID: task.ID, Status: task.Status,
					CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
				})
				attemptsByTask[task.ID] = true
			}
			if step.AssignedAgentID == "" {
				step.AssignedAgentID = task.OwnerAgentID
			}
		}
		step.Result = item.Evidence
		step.Blocker = ""
		if item.Decision == "accepted" {
			step.Status = PlanStepCompleted
		} else {
			step.Status = PlanStepAwaitingDecision
		}
		step.Version++
	}
	plan.Revision++
	updated, err := a.replacePlan(plan, req.ExpectedVersion)
	if err != nil {
		return WorkPlan{}, err
	}
	if err := a.savePlansForProject(project.ID); err != nil {
		return WorkPlan{}, err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ProjectID: project.ID, Kind: "plan_changed", EntityID: plan.ID, To: updated.Status, Reason: "historical_tasks_reconciled", CreatedAt: now})
	return updated, nil
}

func (a *app) activatePlan(project Project, planID, approvedBy string, expectedVersion int64) (WorkPlan, error) {
	now := time.Now().UTC()
	a.mu.Lock()
	items := a.plans[project.ID]
	planIndex := -1
	for i := range items {
		if items[i].ID == planID {
			planIndex = i
			break
		}
	}
	if planIndex < 0 {
		a.mu.Unlock()
		return WorkPlan{}, errors.New("plan not found")
	}
	plan := items[planIndex]
	if expectedVersion > 0 && plan.Version != expectedVersion {
		a.mu.Unlock()
		return WorkPlan{}, errors.New("plan version changed; reload before updating")
	}
	if plan.Status != PlanAwaitingApproval && plan.Status != PlanDraft && plan.Status != PlanPaused {
		a.mu.Unlock()
		return WorkPlan{}, errors.New("plan cannot be activated from status " + plan.Status)
	}
	if plan.OwnerType == "group" && strings.TrimSpace(plan.OwnerGroupID) != "" {
		for _, candidate := range items {
			if candidate.ID == plan.ID || candidate.OwnerType != "group" || candidate.OwnerGroupID != plan.OwnerGroupID || !planReservesGroupExecutionSlot(candidate.Status) {
				continue
			}
			a.mu.Unlock()
			return WorkPlan{}, fmt.Errorf("group %s already has execution plan %q (%s); complete or cancel it before activating another", plan.OwnerGroupID, candidate.Title, candidate.ID)
		}
	}
	previousStatus := plan.Status
	plan.Status = PlanActive
	plan.ApprovedBy = firstNonEmpty(strings.TrimSpace(approvedBy), "user")
	plan.ApprovedAt = &now
	refreshReadyPlanSteps(&plan)
	plan.Version++
	plan.UpdatedAt = now
	items[planIndex] = plan
	a.plans[project.ID] = items
	a.mu.Unlock()
	if err := a.savePlansForProject(project.ID); err != nil {
		return WorkPlan{}, err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ProjectID: project.ID, Kind: "plan_changed", EntityID: plan.ID, From: previousStatus, To: PlanActive, Reason: "plan_activated", CreatedAt: now})
	a.schedulePlanEvent(project.ID, plan.OwnerAgentID, plan.ID, "", "plan_activated", "")
	return plan, nil
}

func planReservesGroupExecutionSlot(status string) bool {
	return status == PlanActive || status == PlanPaused
}

func refreshReadyPlanSteps(plan *WorkPlan) {
	completed := map[string]bool{}
	for _, step := range plan.Steps {
		completed[step.ID] = step.Status == PlanStepCompleted || step.Status == PlanStepSkipped
	}
	for i := range plan.Steps {
		if plan.Steps[i].Status != PlanStepPending && plan.Steps[i].Status != PlanStepChangesRequested {
			continue
		}
		ready := true
		for _, dependency := range plan.Steps[i].Dependencies {
			ready = ready && completed[dependency]
		}
		if ready {
			plan.Steps[i].Status = PlanStepReady
			plan.Steps[i].Version++
		}
	}
}

func planStepIndex(plan WorkPlan, stepID string) int {
	for i := range plan.Steps {
		if plan.Steps[i].ID == stepID {
			return i
		}
	}
	return -1
}

func (a *app) advancePlan(project Project, actor Agent, planID string, req PlanActionRequest) (WorkPlan, error) {
	plan, ok := a.planByID(project.ID, planID)
	if !ok {
		return WorkPlan{}, errors.New("plan not found")
	}
	if plan.OwnerAgentID != actor.ID {
		return WorkPlan{}, errors.New("only the current WorkPlan owner can advance it")
	}
	if plan.Status != PlanActive && req.Action != "pause" && req.Action != "cancel" {
		return WorkPlan{}, errors.New("plan is not active")
	}
	idx := planStepIndex(plan, req.StepID)
	if idx < 0 && req.Action != "complete_plan" && req.Action != "pause" && req.Action != "cancel" && req.Action != "add_step" {
		return WorkPlan{}, errors.New("plan step not found")
	}
	now := time.Now().UTC()
	switch req.Action {
	case "delegate_group":
		step := &plan.Steps[idx]
		if step.Status != PlanStepReady && step.Status != PlanStepChangesRequested {
			return WorkPlan{}, errors.New("step is not ready for group delegation")
		}
		if strings.TrimSpace(req.GroupID) == "" {
			return WorkPlan{}, errors.New("group_id is required")
		}
		result := a.sendToGroup(project.ID, actor.ID, "", map[string]any{
			"group_id": req.GroupID, "intent": "handoff", "subject": "WorkPlan step: " + step.Title,
			"body": step.Description, "objective": firstNonEmpty(req.Goal, step.Description),
			"expected_output": strings.Join(step.AcceptanceCriteria, "; "), "parent_plan_id": plan.ID, "parent_step_id": step.ID,
		})
		var decoded map[string]any
		if err := jsonUnmarshalToolResult(result, &decoded); err != nil || decoded["error"] != nil {
			return WorkPlan{}, fmt.Errorf("delegate group: %s", result)
		}
		step.AssignedGroupID = req.GroupID
		step.Status = PlanStepRunning
		step.Version++
	case "dispatch_task":
		step := &plan.Steps[idx]
		if step.Status != PlanStepReady && step.Status != PlanStepChangesRequested {
			return WorkPlan{}, errors.New("step is not ready for dispatch")
		}
		active := 0
		for _, candidate := range plan.Steps {
			if candidate.Status == PlanStepRunning || candidate.Status == PlanStepDispatching || candidate.Status == PlanStepReviewing {
				active++
			}
		}
		if active >= plan.MaxConcurrency {
			return WorkPlan{}, errors.New("plan concurrency limit reached")
		}
		attempt := len(step.TaskAttempts) + 1
		task := a.createTask(project, TaskCreateRequest{
			Type: firstNonEmpty(req.TaskType, "feature"), Title: firstNonEmpty(req.Title, step.Title),
			Description: firstNonEmpty(req.Description, step.Description), Goal: firstNonEmpty(req.Goal, step.Description, plan.Goal),
			OwnerAgentID: actor.ID, PlanID: plan.ID, PlanStepID: step.ID, Attempt: attempt,
		})
		a.registerTaskRuntimeHook(project.ID, actor.ID, task.ID, map[string]any{"plan_id": plan.ID, "step_id": step.ID, "attempt": attempt})
		step.AssignedAgentID = firstNonEmpty(req.AgentID, actor.ID)
		step.Status = PlanStepRunning
		step.TaskAttempts = append(step.TaskAttempts, PlanTaskAttempt{Attempt: attempt, TaskID: task.ID, Status: task.Status, CreatedAt: now, UpdatedAt: now})
		step.Version++
		a.startTaskAsync(project, task, "work_plan:"+plan.ID+"/"+step.ID)
	case "accept_step":
		step := &plan.Steps[idx]
		if step.Status != PlanStepAwaitingDecision && step.Status != PlanStepReviewing && step.Status != PlanStepRunning {
			return WorkPlan{}, errors.New("step is not awaiting acceptance")
		}
		step.Status = PlanStepCompleted
		step.Result = strings.TrimSpace(req.Result)
		step.Blocker = ""
		step.Version++
		refreshReadyPlanSteps(&plan)
	case "request_rework":
		step := &plan.Steps[idx]
		step.Status = PlanStepChangesRequested
		step.Blocker = strings.TrimSpace(req.Reason)
		step.Version++
		refreshReadyPlanSteps(&plan)
	case "delegate_review":
		step := &plan.Steps[idx]
		reviewer, exists := a.projectAgent(project, req.ReviewerAgentID)
		if !exists || reviewer.ID == actor.ID {
			return WorkPlan{}, errors.New("reviewer must be another project agent")
		}
		args := map[string]any{
			"target_agent_id": reviewer.ID, "intent": "request", "subject": "Review WorkPlan step: " + step.Title,
			"body":            fmt.Sprintf("Review plan %s step %s. Acceptance criteria: %s", plan.ID, step.ID, strings.Join(step.AcceptanceCriteria, "; ")),
			"objective":       "Independently verify the completed step and return approved or changes_requested with findings.",
			"expected_output": "A review decision and concrete findings.", "correlation_id": "plan-review/" + plan.ID + "/" + step.ID + "/" + randomID(),
		}
		result := a.sendToAgent(project.ID, actor.ID, "", args)
		var decoded map[string]any
		if err := jsonUnmarshalToolResult(result, &decoded); err != nil || decoded["error"] != nil {
			return WorkPlan{}, fmt.Errorf("delegate review: %s", result)
		}
		handoffID, _ := decoded["message_id"].(string)
		step.Status = PlanStepReviewing
		step.Reviews = append(step.Reviews, PlanReview{ID: randomID(), Mode: "handoff", ReviewerAgentID: reviewer.ID, HandoffID: handoffID, Status: "pending", CreatedAt: now, UpdatedAt: now})
		step.Version++
	case "block_step":
		plan.Steps[idx].Status = PlanStepBlocked
		plan.Steps[idx].Blocker = strings.TrimSpace(req.Reason)
		plan.Steps[idx].Version++
		plan.Status = PlanBlocked
	case "skip_step":
		plan.Steps[idx].Status = PlanStepSkipped
		plan.Steps[idx].Result = strings.TrimSpace(req.Result)
		plan.Steps[idx].Version++
		refreshReadyPlanSteps(&plan)
	case "add_step":
		step := PlanStep{ID: firstNonEmpty(req.StepID, "step-"+randomID()[:8]), Title: strings.TrimSpace(req.Title), Description: strings.TrimSpace(req.Description), Dependencies: append([]string{}, req.Dependencies...), Status: PlanStepPending, Version: 1}
		if step.Title == "" {
			return WorkPlan{}, errors.New("new step title is required")
		}
		candidate := append(append([]PlanStep{}, plan.Steps...), step)
		if err := validatePlanSteps(candidate); err != nil {
			return WorkPlan{}, err
		}
		plan.Steps = candidate
		plan.Revision++
		refreshReadyPlanSteps(&plan)
	case "pause":
		plan.Status = PlanPaused
	case "cancel":
		plan.Status = PlanCancelled
	case "complete_plan":
		for _, step := range plan.Steps {
			if step.Status != PlanStepCompleted && step.Status != PlanStepSkipped && step.Status != PlanStepCancelled {
				return WorkPlan{}, errors.New("all required steps must be completed before the plan")
			}
		}
		plan.Status = PlanCompleted
	default:
		return WorkPlan{}, errors.New("unsupported plan action")
	}
	updated, err := a.replacePlan(plan, req.ExpectedVersion)
	if err != nil {
		return WorkPlan{}, err
	}
	if err := a.savePlansForProject(project.ID); err != nil {
		return WorkPlan{}, err
	}
	a.emitRuntimeStateChanged(RuntimeEvent{ProjectID: project.ID, Kind: "plan_changed", EntityID: plan.ID, To: updated.Status, Reason: req.Action, CreatedAt: now})
	if stepID, shouldContinue := planOwnerContinuation(updated); shouldContinue {
		a.schedulePlanEvent(project.ID, updated.OwnerAgentID, updated.ID, stepID, "plan_advanced", "")
	}
	return updated, nil
}

func planOwnerContinuation(plan WorkPlan) (string, bool) {
	if plan.Status != PlanActive {
		return "", false
	}
	allResolved := len(plan.Steps) > 0
	for _, step := range plan.Steps {
		if step.Status == PlanStepAwaitingDecision {
			return step.ID, true
		}
		if step.Status == PlanStepReviewing {
			for _, review := range step.Reviews {
				if review.Status == "delivered" {
					return step.ID, true
				}
			}
		}
		if step.Status != PlanStepCompleted && step.Status != PlanStepSkipped && step.Status != PlanStepCancelled {
			allResolved = false
		}
	}
	if allResolved {
		return "", true
	}
	active := 0
	for _, step := range plan.Steps {
		if step.Status == PlanStepRunning || step.Status == PlanStepDispatching || step.Status == PlanStepReviewing {
			active++
		}
	}
	limit := plan.MaxConcurrency
	if limit <= 0 {
		limit = 1
	}
	if active >= limit {
		return "", false
	}
	for _, step := range plan.Steps {
		if step.Status == PlanStepReady || step.Status == PlanStepChangesRequested {
			return step.ID, true
		}
	}
	return "", false
}

func (a *app) resumeActionablePlans() {
	pending := map[string]bool{}
	for _, job := range a.ensureSchedulerQueue().Jobs() {
		if job.Kind == ScheduledRunPlanEvent && (job.Status == ScheduledRunQueued || job.Status == ScheduledRunRunning) {
			pending[job.ProjectID+"/"+job.SourceID] = true
		}
	}
	a.mu.Lock()
	var plans []WorkPlan
	for _, projectPlans := range a.plans {
		plans = append(plans, projectPlans...)
	}
	a.mu.Unlock()
	for _, plan := range plans {
		stepID, actionable := planOwnerContinuation(plan)
		if !actionable || pending[plan.ProjectID+"/"+plan.ID] {
			continue
		}
		a.schedulePlanEvent(plan.ProjectID, plan.OwnerAgentID, plan.ID, stepID, "plan_recovered", "")
	}
}

func (a *app) markPlanTaskTerminal(projectID string, task Task) (WorkPlan, bool) {
	if task.PlanID == "" || task.PlanStepID == "" {
		return WorkPlan{}, false
	}
	plan, ok := a.planByID(projectID, task.PlanID)
	if !ok {
		return WorkPlan{}, false
	}
	idx := planStepIndex(plan, task.PlanStepID)
	if idx < 0 {
		return WorkPlan{}, false
	}
	for i := range plan.Steps[idx].TaskAttempts {
		if plan.Steps[idx].TaskAttempts[i].TaskID == task.ID {
			plan.Steps[idx].TaskAttempts[i].Status = task.Status
			plan.Steps[idx].TaskAttempts[i].UpdatedAt = time.Now().UTC()
		}
	}
	if task.Status == "done" {
		plan.Steps[idx].Status = PlanStepAwaitingDecision
	} else {
		plan.Steps[idx].Status = PlanStepChangesRequested
		plan.Steps[idx].Blocker = firstNonEmpty(task.FailureSummary, "task ended with status "+task.Status)
	}
	plan.Steps[idx].Version++
	updated, err := a.replacePlan(plan, 0)
	if err != nil || a.savePlansForProject(projectID) != nil {
		return WorkPlan{}, false
	}
	return updated, true
}

func (a *app) recordPlanReviewDelivery(projectID, handoffID, reviewerAgentID, findings string) {
	for _, candidate := range a.plansForProject(projectID) {
		changed := false
		stepID := ""
		for stepIndex := range candidate.Steps {
			for reviewIndex := range candidate.Steps[stepIndex].Reviews {
				review := &candidate.Steps[stepIndex].Reviews[reviewIndex]
				if review.HandoffID != handoffID || review.ReviewerAgentID != reviewerAgentID || review.Status != "pending" {
					continue
				}
				review.Status = "delivered"
				review.Findings = strings.TrimSpace(findings)
				review.UpdatedAt = time.Now().UTC()
				candidate.Steps[stepIndex].Version++
				stepID = candidate.Steps[stepIndex].ID
				changed = true
			}
		}
		if !changed {
			continue
		}
		updated, err := a.replacePlan(candidate, 0)
		if err == nil && a.savePlansForProject(projectID) == nil {
			a.schedulePlanEvent(projectID, updated.OwnerAgentID, updated.ID, stepID, "review_delivered", "")
		}
		return
	}
}

func (a *app) recordPlanGroupDelivery(projectID, handoffID, body string) {
	a.mu.Lock()
	var linked GroupInboxMessage
	found := false
	for i := range a.groupInbox[projectID] {
		item := &a.groupInbox[projectID][i]
		if item.AgentInboxMessageID != handoffID || item.ParentPlanID == "" || item.ParentStepID == "" {
			continue
		}
		item.Status = "completed"
		item.UpdatedAt = time.Now().UTC()
		linked = *item
		found = true
		break
	}
	a.mu.Unlock()
	if !found {
		return
	}
	a.saveOrLog("group inbox", a.saveGroupInboxForProject(projectID))
	plan, ok := a.planByID(projectID, linked.ParentPlanID)
	if !ok {
		return
	}
	idx := planStepIndex(plan, linked.ParentStepID)
	if idx < 0 || plan.Steps[idx].Status != PlanStepRunning {
		return
	}
	plan.Steps[idx].Status = PlanStepAwaitingDecision
	plan.Steps[idx].Result = strings.TrimSpace(body)
	plan.Steps[idx].Version++
	updated, err := a.replacePlan(plan, 0)
	if err == nil && a.savePlansForProject(projectID) == nil {
		a.schedulePlanEvent(projectID, updated.OwnerAgentID, updated.ID, linked.ParentStepID, "group_result_delivered", "")
	}
}

func (a *app) schedulePlanEvent(projectID, ownerAgentID, planID, stepID, event, taskID string) {
	if strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "0") || strings.EqualFold(os.Getenv("KAROZ_AGENT_AUTO_RESPOND"), "false") {
		return
	}
	var planVersion int64
	if plan, ok := a.planByID(projectID, planID); ok {
		planVersion = plan.Version
	}
	job, err := newScheduledRun(
		ScheduledRunPlanEvent,
		AgentRunInput{ProjectID: projectID, AgentID: ownerAgentID, Trigger: RunTriggerPlanEvent, TurnType: "plan", SourceID: planID},
		fmt.Sprintf("plan_event/%s/%s/%s/%s/v%d", projectID, planID, event, firstNonEmpty(taskID, stepID, "root"), planVersion),
		PlanEventRunPayload{PlanID: planID, PlanVersion: planVersion, StepID: stepID, Event: event, TaskID: taskID},
		3*time.Minute,
	)
	if err == nil {
		a.scheduleAgentRun(job)
	}
}

func (a *app) executePlanEventScheduledRun(ctx context.Context, job ScheduledRun) error {
	var payload PlanEventRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	project, err := a.projectByID(job.ProjectID)
	if err != nil {
		return err
	}
	plan, ok := a.planByID(project.ID, payload.PlanID)
	if !ok || plan.Status != PlanActive {
		return nil
	}
	if payload.PlanVersion > 0 && payload.PlanVersion < plan.Version {
		return nil
	}
	owner, ok := a.projectAgent(project, plan.OwnerAgentID)
	if !ok {
		return errors.New("plan owner not found")
	}
	planJSON, _ := json.Marshal(plan)
	prompt := fmt.Sprintf("[plan event] event=%s plan_id=%s plan_version=%d step_id=%s task_id=%s\n\nCurrent WorkPlan:\n%s\n\nYou own this active WorkPlan. Continue advancing its todo list. Inspect task/review/group-result evidence, then call advance_plan with one concrete action. Task completion alone never completes a step. You may accept it, delegate review, request rework, block it, dispatch a local task, delegate cross-group work through the group inbox, or complete the plan when every required step is accepted. Do not only summarize.", payload.Event, plan.ID, payload.PlanVersion, payload.StepID, payload.TaskID, string(planJSON))
	out, err := a.runResidentAgentTurn(ctx, project, owner, prompt, "plan", nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		a.appendAgentMessageForRun(project.ID, owner.ID, job.ID, "assistant", "plan_result", out)
	}
	return nil
}
