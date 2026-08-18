package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	processdomain "github.com/karoz/karoz/internal/process"
)

var (
	errProcessNotFound           = errors.New("process not found")
	errProcessRuntimeUnavailable = errors.New("process runtime is unavailable")
)

func (a *app) requireProcessProject(projectID string) error {
	if a.processRuntime == nil {
		return errProcessRuntimeUnavailable
	}
	if a.processRuntime.ProjectError(projectID) != nil ||
		a.processRuntime.projectRuntime(projectID) == nil {
		return errProcessRuntimeUnavailable
	}
	return nil
}

type processView struct {
	ID                 string                   `json:"id"`
	ProjectID          string                   `json:"project_id"`
	AgentID            string                   `json:"agent_id"`
	State              processdomain.State      `json:"state"`
	Terminal           bool                     `json:"terminal"`
	Succeeded          bool                     `json:"succeeded"`
	ExitCode           int                      `json:"exit_code"`
	Error              string                   `json:"error,omitempty"`
	Description        string                   `json:"description,omitempty"`
	CommandSummary     string                   `json:"command_summary"`
	CommandSHA256      string                   `json:"command_sha256"`
	RuntimeMS          int64                    `json:"runtime_ms"`
	LifetimeMS         int64                    `json:"lifetime_ms"`
	LogBytes           int64                    `json:"log_bytes"`
	LogLines           int64                    `json:"log_lines"`
	LogTruncated       bool                     `json:"log_truncated"`
	LastLine           string                   `json:"last_line,omitempty"`
	OutputGaps         []processdomain.SeqRange `json:"output_gaps,omitempty"`
	OutputGapCount     uint64                   `json:"output_gap_count"`
	OutputLostLines    uint64                   `json:"output_lost_lines"`
	OutputGapOldestSeq uint64                   `json:"output_gap_oldest_seq"`
	OutputGapNewestSeq uint64                   `json:"output_gap_newest_seq"`
	StartedAt          time.Time                `json:"started_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	EndedAt            *time.Time               `json:"ended_at,omitempty"`
}

func (a *app) processViews(
	projectID, agentID string,
	limit int,
) ([]processView, error) {
	if err := a.requireProcessProject(projectID); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	records := a.processRuntime.List(projectID)
	for index := range records {
		if a.processSupervisor == nil {
			break
		}
		if live, ok := a.processSupervisor.LiveSnapshot(records[index].ID); ok &&
			live.ProjectID == projectID {
			records[index] = live
		}
	}
	filtered := records[:0]
	for _, record := range records {
		if agentID == "" || record.AgentID == agentID {
			filtered = append(filtered, record)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].StartedAt.Equal(filtered[j].StartedAt) {
			return filtered[i].ID > filtered[j].ID
		}
		return filtered[i].StartedAt.After(filtered[j].StartedAt)
	})
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	views := make([]processView, 0, len(filtered))
	for _, record := range filtered {
		lastLine := a.processLastLine(record)
		views = append(views, newProcessView(record, lastLine, time.Now().UTC()))
	}
	return views, nil
}

func (a *app) processView(
	projectID, agentID, processID string,
) (processView, error) {
	record, err := a.processRecord(projectID, processID)
	if err != nil {
		return processView{}, err
	}
	if agentID != "" && record.AgentID != agentID {
		return processView{}, errProcessNotFound
	}
	return newProcessView(
		record,
		a.processLastLine(record),
		time.Now().UTC(),
	), nil
}

func (a *app) processLastLine(record processdomain.Process) string {
	if a.processSupervisor != nil {
		if tail, ok := a.processSupervisor.LiveTail(
			record.ProjectID,
			record.ID,
			1,
		); ok &&
			len(tail) == 1 {
			return limitString(
				redactSensitiveProcessText(tail[0].Text),
				maxProcessLogLineBytes,
			)
		}
	}
	window, err := a.readProcessLog(record.ProjectID, record.ID, 0, 1, true)
	if err != nil || len(window.Lines) == 0 {
		return ""
	}
	return window.Lines[len(window.Lines)-1]
}

func (a *app) readProcessLog(
	projectID, processID string,
	offset, limit int,
	tail bool,
) (processLogWindow, error) {
	if err := a.requireProcessProject(projectID); err != nil {
		return processLogWindow{}, err
	}
	reader, record, err := a.processRuntime.OpenLogReader(projectID, processID)
	if err != nil {
		return processLogWindow{}, err
	}
	defer reader.Close()
	if a.processSupervisor != nil {
		if live, ok := a.processSupervisor.LiveSnapshot(processID); ok &&
			live.ProjectID == projectID {
			record = live
		}
	}
	return readProcessLogWindow(
		reader,
		processID,
		record.LogLines,
		offset,
		limit,
		tail,
	)
}

func (a *app) stopProcess(
	projectID, agentID, processID string,
) (processView, error) {
	record, err := a.processRecord(projectID, processID)
	if err != nil {
		return processView{}, err
	}
	if agentID != "" && record.AgentID != agentID {
		return processView{}, errProcessNotFound
	}
	if record.State.Terminal() {
		return newProcessView(
			record,
			a.processLastLine(record),
			time.Now().UTC(),
		), nil
	}
	if a.processSupervisor == nil {
		return processView{}, errors.New("process supervisor is unavailable")
	}
	if err := a.processSupervisor.Stop(processID); err != nil {
		current, lookupErr := a.processRecord(projectID, processID)
		if lookupErr != nil || !current.State.Terminal() {
			return processView{}, err
		}
	}
	return a.processView(projectID, agentID, processID)
}

func (a *app) stopOwnedProcesses(projectID, agentID string) error {
	if a.processRuntime == nil || a.processSupervisor == nil {
		return nil
	}
	if err := a.requireProcessProject(projectID); err != nil {
		return err
	}
	for _, record := range a.processRuntime.List(projectID) {
		if record.AgentID != agentID || record.State.Terminal() {
			continue
		}
		if err := a.processSupervisor.StopWithCause(
			record.ID,
			processdomain.StateKilled,
			"owner deleted",
		); err != nil {
			return err
		}
	}
	return nil
}

func newProcessView(
	record processdomain.Process,
	lastLine string,
	now time.Time,
) processView {
	sum := sha256.Sum256([]byte(record.Command))
	end := now
	if record.EndedAt != nil {
		end = *record.EndedAt
	}
	runtime := end.Sub(record.StartedAt)
	if runtime < 0 {
		runtime = 0
	}
	return processView{
		ID: record.ID, ProjectID: record.ProjectID, AgentID: record.AgentID,
		State: record.State, Terminal: record.State.Terminal(),
		Succeeded: record.State.Succeeded(), ExitCode: record.ExitCode,
		Error:       redactSensitiveProcessText(limitString(record.Error, 1000)),
		Description: redactSensitiveProcessText(limitString(record.Description, 500)),
		CommandSummary: redactSensitiveProcessText(
			limitString(strings.TrimSpace(record.Command), 300),
		),
		CommandSHA256: hex.EncodeToString(sum[:]),
		RuntimeMS:     runtime.Milliseconds(), LifetimeMS: record.LifetimeMS,
		LogBytes: record.LogBytes, LogLines: record.LogLines,
		LogTruncated:       record.LogTruncated,
		LastLine:           redactSensitiveProcessText(limitString(lastLine, 1000)),
		OutputGaps:         append([]processdomain.SeqRange(nil), record.OutputGaps...),
		OutputGapCount:     record.OutputGapCount,
		OutputLostLines:    record.OutputLostLines,
		OutputGapOldestSeq: record.OutputGapOldestSeq,
		OutputGapNewestSeq: record.OutputGapNewestSeq,
		StartedAt:          record.StartedAt, UpdatedAt: record.UpdatedAt,
		EndedAt: record.EndedAt,
	}
}
