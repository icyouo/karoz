package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

const (
	monitorProbeApprovePrefix = "monitor_probe_approve:"
	monitorProbeDenyPrefix    = "monitor_probe_deny:"
)

func (a *app) prepareMonitorProbeFromTool(
	toolCtx ResidentToolContext,
	args map[string]any,
) string {
	source, ok := args["source"].(string)
	if !ok {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": "source is required",
		})
	}
	request := monitorProbeApprovalRequest{
		AgentID:    toolCtx.Agent.ID,
		MonitorID:  toolStringArg(args, "monitor_id", 200),
		Language:   toolStringArg(args, "language", 32),
		Workdir:    toolStringArg(args, "workdir", 4096),
		Source:     source,
		IntervalMS: int64(clampToolInt(args, "interval_ms", int(defaultMonitorProbeInterval), int(minimumMonitorProbeInterval), 86_400_000)),
		TimeoutMS:  int64(clampToolInt(args, "timeout_ms", int(defaultMonitorProbeTimeout), 1, int(maximumMonitorProbeTimeout))),
	}
	request, normalized, workdir, digest, err := normalizeMonitorProbeRequest(
		toolCtx.Project,
		request,
	)
	if err != nil {
		return toolJSON(map[string]any{
			"error": "validation_error", "message": err.Error(),
		})
	}
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneMonitorProbeApprovalsLocked(now)
	ownerCreatedAt, ownerAvailable := a.residentApprovalOwnerLocked(
		toolCtx.Project.ID,
		toolCtx.Agent,
	)
	if !ownerAvailable {
		return toolJSON(map[string]any{
			"error":   "owner_not_found",
			"message": "probe owner is no longer registered",
		})
	}
	monitorID := request.MonitorID
	triggerRevision := 1
	if monitorID == "" {
		monitorID = randomID()
	} else {
		for _, item := range a.monitors[toolCtx.Project.ID] {
			if item.ID != monitorID {
				continue
			}
			if item.AgentID != toolCtx.Agent.ID {
				return toolJSON(map[string]any{
					"error":   "ownership_error",
					"message": "probe monitor owner mismatch",
				})
			}
			triggerRevision = item.Trigger.Revision + 1
			break
		}
	}
	for _, candidate := range a.monitorProbeChallenges {
		if candidate.ProjectID == toolCtx.Project.ID &&
			candidate.AgentID == toolCtx.Agent.ID &&
			candidate.MonitorID == monitorID &&
			candidate.TriggerRevision == triggerRevision &&
			candidate.Language == request.Language &&
			candidate.CanonicalWorkdir == workdir &&
			candidate.SourceSHA256 == digest &&
			candidate.IntervalMS == request.IntervalMS &&
			candidate.TimeoutMS == request.TimeoutMS &&
			candidate.State == "pending_agent" &&
			now.Before(candidate.ExpiresAt) {
			return monitorProbeChoiceRequest(toolCtx, candidate)
		}
	}
	for _, reservation := range a.monitorProbeReservations {
		if reservation.ProjectID == toolCtx.Project.ID &&
			reservation.MonitorID == monitorID &&
			now.Before(reservation.ExpiresAt) {
			return toolJSON(map[string]any{
				"error":   "approval_reserved",
				"message": "probe monitor already has a live reservation",
			})
		}
	}
	if countProjectProbeApprovals(
		toolCtx.Project.ID,
		a.monitorProbeReservations,
	) >= maximumProbeApprovals {
		return toolJSON(map[string]any{
			"error":   "approval_capacity",
			"message": "probe approval capacity reached",
		})
	}
	choiceID := randomID()
	reservation := monitorProbeReservation{
		ID: randomID(), ProjectID: toolCtx.Project.ID,
		AgentID: toolCtx.Agent.ID, OwnerCreatedAt: ownerCreatedAt,
		MonitorID: monitorID, TriggerRevision: triggerRevision,
		ExpiresAt: now.Add(monitorProbeApprovalTTL),
	}
	challenge := monitorProbeChallenge{
		ID: choiceID, ReservationID: reservation.ID,
		ChoiceRequestID: choiceID,
		ProjectID:       toolCtx.Project.ID, AgentID: toolCtx.Agent.ID,
		MonitorID: monitorID, TriggerRevision: triggerRevision,
		CanonicalWorkdir: workdir, Language: request.Language,
		NormalizedSource: append([]byte(nil), normalized...),
		SourceSHA256:     digest, IntervalMS: request.IntervalMS,
		TimeoutMS: request.TimeoutMS,
		ExpiresAt: now.Add(monitorProbeApprovalTTL), State: "pending_agent",
	}
	a.monitorProbeReservations[reservation.ID] = reservation
	a.monitorProbeChallenges[challenge.ID] = challenge
	if err := a.saveMonitorsLocked(); err != nil {
		delete(a.monitorProbeReservations, reservation.ID)
		delete(a.monitorProbeChallenges, challenge.ID)
		return toolJSON(map[string]any{
			"error": "save_failed", "message": err.Error(),
		})
	}
	a.appendAgentMonitorProbeApprovalEventLocked(challenge, "requested")
	if err := a.saveAgentSessionEventsLocked(); err != nil {
		log.Printf("save monitor probe approval event: %v", err)
	}
	return monitorProbeChoiceRequest(toolCtx, challenge)
}

func monitorProbeChoiceRequest(
	toolCtx ResidentToolContext,
	challenge monitorProbeChallenge,
) string {
	agentName := firstNonEmpty(
		toolCtx.Agent.Nickname,
		toolCtx.Agent.DisplayName,
		toolCtx.Agent.Name,
		toolCtx.Agent.ID,
		"resident agent",
	)
	return toolJSON(map[string]any{
		"kind":          "choice_request",
		"status":        "pending_user_choice",
		"approval_type": "monitor_probe",
		"approval_id":   challenge.ID,
		"monitor_id":    challenge.MonitorID,
		"question": fmt.Sprintf(
			"Allow %s to register this exact %s script probe in %s?\n\nProject: %s\nAgent: %s\nMonitor: %s\nTrigger revision: %d\nSHA-256: %s\nWorkdir: %s\nInterval: %dms\nTimeout: %dms\n\nSource:\n%s",
			agentName,
			challenge.Language,
			firstNonEmpty(toolCtx.Project.Name, toolCtx.Project.ID, "the project"),
			challenge.ProjectID,
			challenge.AgentID,
			challenge.MonitorID,
			challenge.TriggerRevision,
			challenge.SourceSHA256,
			challenge.CanonicalWorkdir,
			challenge.IntervalMS,
			challenge.TimeoutMS,
			string(challenge.NormalizedSource),
		),
		"mode": "yes_no",
		"choices": []map[string]string{
			{
				"id":          monitorProbeApprovePrefix + challenge.ID,
				"label":       "Approve probe",
				"description": "Approve this exact probe snapshot once.",
			},
			{
				"id":          monitorProbeDenyPrefix + challenge.ID,
				"label":       "Cancel",
				"description": "Do not approve this probe.",
			},
		},
	})
}

func isMonitorProbeChoice(choiceID string) bool {
	choiceID = strings.TrimSpace(choiceID)
	return strings.HasPrefix(choiceID, monitorProbeApprovePrefix) ||
		strings.HasPrefix(choiceID, monitorProbeDenyPrefix)
}

func (a *app) resolveMonitorProbeChoice(
	projectID, agentID, runID, choiceID string,
) (bool, error) {
	choiceID = strings.TrimSpace(choiceID)
	approved := strings.HasPrefix(choiceID, monitorProbeApprovePrefix)
	denied := strings.HasPrefix(choiceID, monitorProbeDenyPrefix)
	if !approved && !denied {
		return false, nil
	}
	a.agentRuntimeLocked().backgroundOwnerMu.Lock()
	defer a.agentRuntimeLocked().backgroundOwnerMu.Unlock()
	id := strings.TrimPrefix(choiceID, monitorProbeApprovePrefix)
	if denied {
		id = strings.TrimPrefix(choiceID, monitorProbeDenyPrefix)
	}
	now := time.Now().UTC()
	a.mu.Lock()
	challenge, ok := a.monitorProbeChallenges[id]
	if !ok || !now.Before(challenge.ExpiresAt) ||
		(challenge.State != "pending_agent" &&
			challenge.State != "staging_agent" &&
			challenge.State != "consumed") {
		a.mu.Unlock()
		return true, errors.New("probe approval is missing, expired, or resolved")
	}
	if challenge.ProjectID != projectID || challenge.AgentID != agentID {
		a.mu.Unlock()
		return true, errors.New("probe approval belongs to a different project or agent")
	}
	if challenge.ConsumedReceiptID != "" {
		receipt, receiptOK := a.monitorProbeReceipts[challenge.ConsumedReceiptID]
		if denied || !receiptOK ||
			receipt.ProjectID != projectID ||
			receipt.AgentID != agentID ||
			receipt.MonitorID != challenge.MonitorID ||
			receipt.TriggerRevision != challenge.TriggerRevision ||
			receipt.ApprovalFlow != "agent_choice" ||
			receipt.ChoiceRequestID != challenge.ChoiceRequestID {
			a.mu.Unlock()
			return true, errors.New("consumed probe approval receipt is unavailable")
		}
		a.mu.Unlock()
		return true, nil
	}
	reservation, reservationOK := a.monitorProbeReservations[challenge.ReservationID]
	if !reservationOK || reservation.ProjectID != projectID ||
		reservation.AgentID != agentID {
		a.mu.Unlock()
		return true, errors.New("probe approval is missing, expired, or resolved")
	}
	var owner Agent
	ownerOK := false
	for _, candidate := range a.agentDirectoryLocked().agents[projectID] {
		if candidate.ID == agentID {
			owner = candidate
			ownerOK = true
			break
		}
	}
	if !ownerOK || !owner.CreatedAt.Equal(reservation.OwnerCreatedAt) {
		a.mu.Unlock()
		return true, errors.New("probe approval owner is no longer registered")
	}
	if denied {
		if challenge.State != "pending_agent" {
			a.mu.Unlock()
			return true, errors.New("probe approval is already staging")
		}
		delete(a.monitorProbeChallenges, id)
		delete(a.monitorProbeReservations, reservation.ID)
		err := a.saveMonitorsLocked()
		if err == nil {
			a.appendAgentMonitorProbeApprovalEventLocked(challenge, "denied")
			if eventErr := a.saveAgentSessionEventsLocked(); eventErr != nil {
				log.Printf("save monitor probe approval event: %v", eventErr)
			}
		}
		a.mu.Unlock()
		return true, err
	}
	if strings.TrimSpace(runID) == "" {
		a.mu.Unlock()
		return true, errors.New("probe approval requires an active run")
	}
	receiptID := challenge.StagingReceiptID
	relative := challenge.StagingPath
	if challenge.State == "pending_agent" {
		if countProjectProbeSnapshotFilesLocked(
			projectID,
			a.monitorProbeChallenges,
			a.monitorProbeReceipts,
		) >= maximumProbeSnapshotFiles {
			a.mu.Unlock()
			return true, errors.New("probe snapshot capacity reached")
		}
		receiptID = randomID()
		extension := "sh"
		if challenge.Language == "javascript" {
			extension = "js"
		}
		relative = filepath.Join(
			"monitor-probes",
			monitordomain.SafeProjectKey(projectID),
			"receipts",
			receiptID,
			challenge.SourceSHA256+"."+extension,
		)
		original := challenge
		challenge.State = "staging_agent"
		challenge.ApprovalRunID = runID
		challenge.StagingReceiptID = receiptID
		challenge.StagingPath = relative
		a.monitorProbeChallenges[id] = challenge
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitorProbeChallenges[id] = original
			a.mu.Unlock()
			return true, err
		}
	}
	a.mu.Unlock()

	store, err := newSecureRuntimeStore(a.settings.DataDir)
	if err != nil {
		return true, err
	}
	if err := store.saveBytes(relative, challenge.NormalizedSource); err != nil {
		return true, err
	}
	snapshot, err := readMonitorProbeSnapshot(store, relative)
	if err != nil {
		_ = store.remove(relative)
		return true, err
	}
	receipt := monitordomain.ProbeApprovalReceipt{
		ID: receiptID, ProjectID: projectID, AgentID: agentID,
		MonitorID:       challenge.MonitorID,
		TriggerRevision: challenge.TriggerRevision,
		ApprovalFlow:    "agent_choice", ApprovalRunID: challenge.ApprovalRunID,
		ChoiceRequestID:  challenge.ChoiceRequestID,
		CanonicalWorkdir: challenge.CanonicalWorkdir,
		Language:         challenge.Language, SnapshotPath: relative,
		SnapshotDevice: snapshot.Device, SnapshotInode: snapshot.Inode,
		NormalizedSource: append([]byte(nil), challenge.NormalizedSource...),
		SourceSHA256:     challenge.SourceSHA256,
		IntervalMS:       challenge.IntervalMS, TimeoutMS: challenge.TimeoutMS,
		ApprovedBy: localOperatorApprovalID(a.settings.DataDir),
		ApprovedAt: now, ExpiresAt: challenge.ExpiresAt,
	}
	a.mu.Lock()
	current := a.monitorProbeChallenges[id]
	if current.State != "staging_agent" ||
		current.StagingReceiptID != receiptID ||
		current.StagingPath != relative {
		a.mu.Unlock()
		_ = store.remove(relative)
		return true, errors.New("probe approval staging identity changed")
	}
	current.State = "consumed"
	current.ConsumedReceiptID = receiptID
	a.monitorProbeChallenges[id] = current
	a.monitorProbeReceipts[receiptID] = receipt
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitorProbeChallenges[id] = challenge
		delete(a.monitorProbeReceipts, receiptID)
		a.mu.Unlock()
		_ = store.remove(relative)
		return true, err
	}
	a.appendAgentMonitorProbeApprovalEventLocked(current, "approved")
	if err := a.saveAgentSessionEventsLocked(); err != nil {
		log.Printf("save monitor probe approval event: %v", err)
	}
	a.mu.Unlock()
	return true, nil
}
