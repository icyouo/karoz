package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	monitordomain "github.com/karoz/karoz/internal/monitor"
)

const (
	monitorProbeApprovalTTL     = 10 * time.Minute
	monitorProbeSessionIdle     = 30 * time.Minute
	monitorProbeSessionLifetime = 8 * time.Hour
	defaultMonitorProbeInterval = int64(60_000)
	minimumMonitorProbeInterval = int64(10_000)
	defaultMonitorProbeTimeout  = int64(5_000)
	maximumMonitorProbeTimeout  = int64(30_000)
	maximumEnabledProbes        = 16
	maximumProbeConcurrency     = 4
	maximumProbeApprovals       = 64
	maximumProbeSessions        = 16
	maximumProbeSnapshotFiles   = 228
)

var errScriptProbeUnsupported = errors.New("unsupported_platform: script probes are unsupported on this platform")

type monitorProbeSnapshot struct {
	Source   []byte
	Device   uint64
	Inode    uint64
	OwnerUID uint32
	Mode     os.FileMode
}

type monitorProbeApprovalRequest struct {
	AgentID    string `json:"agent_id"`
	MonitorID  string `json:"monitor_id,omitempty"`
	Language   string `json:"language"`
	Workdir    string `json:"workdir"`
	Source     string `json:"source"`
	IntervalMS int64  `json:"interval_ms,omitempty"`
	TimeoutMS  int64  `json:"timeout_ms,omitempty"`
}

type probeBoundedBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (buffer *probeBoundedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - len(buffer.data)
	if remaining > 0 {
		copied := remaining
		if len(value) < copied {
			copied = len(value)
		}
		buffer.data = append(buffer.data, value[:copied]...)
	}
	if len(value) > remaining {
		buffer.overflow = true
	}
	return len(value), nil
}

func normalizeMonitorProbeRequest(
	project Project,
	request monitorProbeApprovalRequest,
) (monitorProbeApprovalRequest, []byte, string, string, error) {
	if !scriptProbeSupported {
		return request, nil, "", "", errScriptProbeUnsupported
	}
	request.AgentID = strings.TrimSpace(request.AgentID)
	request.MonitorID = strings.TrimSpace(request.MonitorID)
	request.Language = strings.TrimSpace(request.Language)
	if request.Language != "shell" && request.Language != "javascript" {
		return request, nil, "", "", errors.New("probe language must be shell or javascript")
	}
	if len(request.Source) == 0 ||
		len([]byte(request.Source)) > monitordomain.MaxProbeSourceBytes {
		return request, nil, "", "", errors.New("probe source must be between 1 byte and 64 KiB")
	}
	source := []byte(strings.ReplaceAll(request.Source, "\r\n", "\n"))
	if len(source) == 0 || len(source) > monitordomain.MaxProbeSourceBytes {
		return request, nil, "", "", errors.New("normalized probe source exceeds 64 KiB")
	}
	if request.IntervalMS == 0 {
		request.IntervalMS = defaultMonitorProbeInterval
	}
	if request.IntervalMS < minimumMonitorProbeInterval {
		return request, nil, "", "", errors.New("probe interval must be at least 10 seconds")
	}
	if request.TimeoutMS == 0 {
		request.TimeoutMS = defaultMonitorProbeTimeout
	}
	if request.TimeoutMS <= 0 || request.TimeoutMS > maximumMonitorProbeTimeout {
		return request, nil, "", "", errors.New("probe timeout must be between 1ms and 30 seconds")
	}
	workdir := firstNonEmpty(request.Workdir, project.Path)
	canonical, err := canonicalResidentWorkdir(workdir)
	if err != nil {
		return request, nil, "", "", err
	}
	projectRoot, err := canonicalResidentWorkdir(project.Path)
	if err != nil {
		return request, nil, "", "", err
	}
	if !pathInside(canonical, projectRoot) {
		return request, nil, "", "", errors.New("probe workdir escapes the project")
	}
	interpreter := "bash"
	if request.Language == "javascript" {
		interpreter = "node"
	}
	if _, err := exec.LookPath(interpreter); err != nil {
		return request, nil, "", "", fmt.Errorf("probe interpreter %s is unavailable", interpreter)
	}
	sum := sha256.Sum256(source)
	return request, source, canonical, hex.EncodeToString(sum[:]), nil
}

func (a *app) prepareMonitorProbeApproval(
	project Project,
	request monitorProbeApprovalRequest,
	sessionToken string,
) (map[string]any, string, error) {
	request, source, workdir, digest, err := normalizeMonitorProbeRequest(
		project,
		request,
	)
	if err != nil {
		return nil, "", err
	}
	owner, ok := a.projectAgent(project, request.AgentID)
	if !ok {
		return nil, "", errors.New("probe owner agent not found")
	}
	now := time.Now().UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneMonitorProbeApprovalsLocked(now)
	monitorID := request.MonitorID
	triggerRevision := 1
	if monitorID == "" {
		monitorID = randomID()
	} else {
		for _, item := range a.monitors[project.ID] {
			if item.ID == monitorID {
				if item.AgentID != request.AgentID {
					return nil, "", errors.New("probe monitor owner mismatch")
				}
				triggerRevision = item.Trigger.Revision + 1
				break
			}
		}
	}
	for _, reservation := range a.monitorProbeReservations {
		if reservation.ProjectID == project.ID &&
			reservation.MonitorID == monitorID &&
			now.Before(reservation.ExpiresAt) {
			return nil, "", errors.New("probe monitor already has a live reservation")
		}
	}
	if countProjectProbeApprovals(
		project.ID,
		a.monitorProbeReservations,
	) >= maximumProbeApprovals {
		return nil, "", errors.New("approval_capacity")
	}
	beforeSessions := make(
		map[string]monitorProbeOperatorSession,
		len(a.monitorProbeSessions),
	)
	for id, session := range a.monitorProbeSessions {
		beforeSessions[id] = session
	}
	rawSession, sessionID, err := a.ensureMonitorProbeSessionLocked(
		project.ID,
		sessionToken,
		now,
	)
	if err != nil {
		return nil, "", err
	}
	reservation := monitorProbeReservation{
		ID: randomID(), ProjectID: project.ID, AgentID: request.AgentID,
		OwnerCreatedAt: owner.CreatedAt,
		MonitorID:      monitorID, TriggerRevision: triggerRevision,
		ExpiresAt: now.Add(monitorProbeApprovalTTL),
	}
	challenge := monitorProbeChallenge{
		ID: randomID(), ReservationID: reservation.ID,
		OperatorSessionID: sessionID,
		ProjectID:         project.ID, AgentID: request.AgentID,
		MonitorID: monitorID, TriggerRevision: triggerRevision,
		CanonicalWorkdir: workdir, Language: request.Language,
		NormalizedSource: append([]byte(nil), source...),
		SourceSHA256:     digest, IntervalMS: request.IntervalMS,
		TimeoutMS: request.TimeoutMS,
		ExpiresAt: now.Add(monitorProbeApprovalTTL), State: "pending",
	}
	a.monitorProbeReservations[reservation.ID] = reservation
	a.monitorProbeChallenges[challenge.ID] = challenge
	if err := a.saveMonitorsLocked(); err != nil {
		delete(a.monitorProbeReservations, reservation.ID)
		delete(a.monitorProbeChallenges, challenge.ID)
		a.monitorProbeSessions = beforeSessions
		return nil, "", err
	}
	return map[string]any{
		"challenge_id":      challenge.ID,
		"monitor_id":        monitorID,
		"trigger_revision":  triggerRevision,
		"project_id":        project.ID,
		"agent_id":          request.AgentID,
		"language":          request.Language,
		"canonical_workdir": workdir,
		"source_sha256":     digest,
		"source":            string(source),
		"interval_ms":       request.IntervalMS,
		"timeout_ms":        request.TimeoutMS,
		"expires_at":        challenge.ExpiresAt,
	}, rawSession, nil
}

func (a *app) confirmMonitorProbeApproval(
	project Project,
	challengeID, sessionToken string,
) (map[string]any, error) {
	if !scriptProbeSupported {
		return nil, errScriptProbeUnsupported
	}
	now := time.Now().UTC()
	sessionID := monitorProbeSessionID(sessionToken)
	a.mu.Lock()
	session, sessionOK := a.monitorProbeSessions[sessionID]
	challenge, challengeOK := a.monitorProbeChallenges[challengeID]
	reservation, reservationOK := a.monitorProbeReservations[challenge.ReservationID]
	ownerCurrent := false
	for _, owner := range a.agents[project.ID] {
		if owner.ID == challenge.AgentID &&
			owner.CreatedAt.Equal(reservation.OwnerCreatedAt) {
			ownerCurrent = true
			break
		}
	}
	if !sessionOK || session.ProjectID != project.ID ||
		now.Sub(session.LastActiveAt) > monitorProbeSessionIdle ||
		!now.Before(session.ExpiresAt) ||
		!challengeOK || challenge.ProjectID != project.ID ||
		!reservationOK || reservation.ProjectID != project.ID ||
		!ownerCurrent ||
		challenge.OperatorSessionID != sessionID ||
		!now.Before(challenge.ExpiresAt) {
		a.mu.Unlock()
		return nil, errors.New("probe approval challenge is unavailable")
	}
	if challenge.ConsumedReceiptID != "" {
		receipt := a.monitorProbeReceipts[challenge.ConsumedReceiptID]
		a.mu.Unlock()
		return redactedProbeReceipt(receipt), nil
	}
	if challenge.State != "pending" && challenge.State != "staging" {
		a.mu.Unlock()
		return nil, errors.New("probe approval challenge is unavailable")
	}
	receiptID := challenge.StagingReceiptID
	relative := challenge.StagingPath
	if challenge.State == "pending" {
		if countProjectProbeSnapshotFilesLocked(
			project.ID,
			a.monitorProbeChallenges,
			a.monitorProbeReceipts,
		) >= maximumProbeSnapshotFiles {
			a.mu.Unlock()
			return nil, errors.New("probe snapshot capacity reached")
		}
		receiptID = randomID()
		extension := "sh"
		if challenge.Language == "javascript" {
			extension = "js"
		}
		relative = filepath.Join(
			"monitor-probes",
			monitordomain.SafeProjectKey(project.ID),
			"receipts",
			receiptID,
			challenge.SourceSHA256+"."+extension,
		)
		original := challenge
		challenge.State = "staging"
		challenge.StagingReceiptID = receiptID
		challenge.StagingPath = relative
		a.monitorProbeChallenges[challengeID] = challenge
		if err := a.saveMonitorsLocked(); err != nil {
			a.monitorProbeChallenges[challengeID] = original
			a.mu.Unlock()
			return nil, err
		}
	}
	a.mu.Unlock()

	store, err := newSecureRuntimeStore(a.settings.DataDir)
	if err != nil {
		return nil, err
	}
	if err := store.saveBytes(relative, challenge.NormalizedSource); err != nil {
		return nil, err
	}
	snapshot, err := readMonitorProbeSnapshot(store, relative)
	if err != nil {
		_ = store.remove(relative)
		return nil, err
	}
	receipt := monitordomain.ProbeApprovalReceipt{
		ID: receiptID, ProjectID: project.ID, AgentID: challenge.AgentID,
		MonitorID:         challenge.MonitorID,
		TriggerRevision:   challenge.TriggerRevision,
		ApprovalFlow:      "ui_challenge",
		OperatorSessionID: sessionID, ChallengeID: challenge.ID,
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
	before := challenge
	challenge = a.monitorProbeChallenges[challengeID]
	if challenge.State != "staging" ||
		challenge.StagingReceiptID != receiptID ||
		challenge.StagingPath != relative {
		a.mu.Unlock()
		_ = store.remove(relative)
		return nil, errors.New("probe approval staging identity changed")
	}
	challenge.State = "consumed"
	challenge.ConsumedReceiptID = receiptID
	a.monitorProbeChallenges[challengeID] = challenge
	a.monitorProbeReceipts[receiptID] = receipt
	if err := a.saveMonitorsLocked(); err != nil {
		a.monitorProbeChallenges[challengeID] = before
		delete(a.monitorProbeReceipts, receiptID)
		a.mu.Unlock()
		_ = store.remove(relative)
		return nil, err
	}
	a.mu.Unlock()
	return redactedProbeReceipt(receipt), nil
}

func (a *app) ensureMonitorProbeSessionLocked(
	projectID, raw string,
	now time.Time,
) (string, string, error) {
	sessionID := monitorProbeSessionID(raw)
	if raw != "" {
		if session, ok := a.monitorProbeSessions[sessionID]; ok &&
			session.ProjectID == projectID &&
			now.Sub(session.LastActiveAt) <= monitorProbeSessionIdle &&
			now.Before(session.ExpiresAt) {
			session.LastActiveAt = now
			a.monitorProbeSessions[sessionID] = session
			return raw, sessionID, nil
		}
	}
	if countProjectProbeSessions(
		projectID,
		a.monitorProbeSessions,
	) >= maximumProbeSessions {
		return "", "", errors.New("operator_session_capacity")
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", "", err
	}
	raw = hex.EncodeToString(token[:])
	sessionID = monitorProbeSessionID(raw)
	a.monitorProbeSessions[sessionID] = monitorProbeOperatorSession{
		ID: sessionID, ProjectID: projectID,
		CreatedAt: now, LastActiveAt: now,
		ExpiresAt: now.Add(monitorProbeSessionLifetime),
	}
	return raw, sessionID, nil
}

func (a *app) pruneMonitorProbeApprovalsLocked(now time.Time) {
	referencedReceipts := make(map[string]bool)
	for _, monitors := range a.monitors {
		for _, item := range monitors {
			if item.Trigger.Kind == monitordomain.TriggerScriptProbe &&
				item.Trigger.ApprovalReceiptID != "" {
				referencedReceipts[item.Trigger.ApprovalReceiptID] = true
			}
		}
	}
	var store *secureRuntimeStore
	removeSnapshot := func(relative string) bool {
		if relative == "" {
			return true
		}
		if store == nil {
			var err error
			store, err = newSecureRuntimeStore(a.settings.DataDir)
			if err != nil {
				return false
			}
		}
		return store.remove(relative) == nil
	}
	for id, reservation := range a.monitorProbeReservations {
		ownerCurrent := false
		for _, owner := range a.agents[reservation.ProjectID] {
			if owner.ID == reservation.AgentID &&
				owner.CreatedAt.Equal(reservation.OwnerCreatedAt) {
				ownerCurrent = true
				break
			}
		}
		if !now.Before(reservation.ExpiresAt) || !ownerCurrent {
			delete(a.monitorProbeReservations, id)
		}
	}
	for id, challenge := range a.monitorProbeChallenges {
		_, reservationCurrent := a.monitorProbeReservations[challenge.ReservationID]
		if now.Before(challenge.ExpiresAt) &&
			(reservationCurrent || challenge.ConsumedReceiptID != "") {
			continue
		}
		if challenge.ConsumedReceiptID == "" &&
			(challenge.State == "staging" ||
				challenge.State == "staging_agent") &&
			!removeSnapshot(challenge.StagingPath) {
			continue
		}
		delete(a.monitorProbeChallenges, id)
	}
	for id, receipt := range a.monitorProbeReceipts {
		remove := receipt.RevokedAt != nil && !referencedReceipts[id]
		if receipt.ClaimedMutationID == "" &&
			!receipt.ExpiresAt.IsZero() &&
			!now.Before(receipt.ExpiresAt) {
			remove = true
		}
		if remove && removeSnapshot(receipt.SnapshotPath) {
			delete(a.monitorProbeReceipts, id)
		}
	}
	for id, session := range a.monitorProbeSessions {
		if !now.Before(session.ExpiresAt) ||
			now.Sub(session.LastActiveAt) > monitorProbeSessionIdle {
			delete(a.monitorProbeSessions, id)
		}
	}
}

func countProjectProbeSnapshotFilesLocked(
	projectID string,
	challenges map[string]monitorProbeChallenge,
	receipts map[string]monitordomain.ProbeApprovalReceipt,
) int {
	paths := make(map[string]bool)
	for _, challenge := range challenges {
		if challenge.ProjectID == projectID &&
			challenge.StagingPath != "" {
			paths[challenge.StagingPath] = true
		}
	}
	for _, receipt := range receipts {
		if receipt.ProjectID == projectID &&
			receipt.SnapshotPath != "" {
			paths[receipt.SnapshotPath] = true
		}
	}
	return len(paths)
}

func monitorProbeSessionID(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func countProjectProbeApprovals(
	projectID string,
	items map[string]monitorProbeReservation,
) int {
	count := 0
	for _, item := range items {
		if item.ProjectID == projectID {
			count++
		}
	}
	return count
}

func countProjectProbeSessions(
	projectID string,
	items map[string]monitorProbeOperatorSession,
) int {
	count := 0
	for _, item := range items {
		if item.ProjectID == projectID {
			count++
		}
	}
	return count
}

func redactedProbeReceipt(
	receipt monitordomain.ProbeApprovalReceipt,
) map[string]any {
	return map[string]any{
		"id":                receipt.ID,
		"project_id":        receipt.ProjectID,
		"agent_id":          receipt.AgentID,
		"monitor_id":        receipt.MonitorID,
		"trigger_revision":  receipt.TriggerRevision,
		"approval_flow":     receipt.ApprovalFlow,
		"language":          receipt.Language,
		"source_sha256":     receipt.SourceSHA256,
		"canonical_workdir": receipt.CanonicalWorkdir,
		"interval_ms":       receipt.IntervalMS,
		"timeout_ms":        receipt.TimeoutMS,
		"expires_at":        receipt.ExpiresAt,
		"claimed":           receipt.ClaimedMutationID != "",
	}
}

func localOperatorApprovalID(dataDir string) string {
	return "local_operator:" + projectID(dataDir)
}
