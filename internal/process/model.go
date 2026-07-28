package process

import "time"

type State string

const (
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateSucceeded   State = "succeeded"
	StateFailed      State = "failed"
	StateKilled      State = "killed"
	StateInterrupted State = "interrupted"
)

func (state State) Valid() bool {
	switch state {
	case StateStarting, StateRunning, StateSucceeded, StateFailed, StateKilled, StateInterrupted:
		return true
	default:
		return false
	}
}

func (state State) Terminal() bool {
	switch state {
	case StateSucceeded, StateFailed, StateKilled, StateInterrupted:
		return true
	default:
		return false
	}
}

func (state State) Succeeded() bool {
	return state == StateSucceeded
}

func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case StateStarting:
		return to == StateRunning || to == StateFailed || to == StateInterrupted
	case StateRunning:
		return to == StateSucceeded || to == StateFailed || to == StateKilled || to == StateInterrupted
	default:
		return false
	}
}

type SeqRange struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

type Process struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	AgentID     string `json:"agent_id"`
	RunID       string `json:"run_id,omitempty"`
	Command     string `json:"command"`
	Workdir     string `json:"workdir"`
	Description string `json:"description,omitempty"`

	PID      int    `json:"pid,omitempty"`
	GuardPID int    `json:"guard_pid,omitempty"`
	PGID     int    `json:"pgid,omitempty"`
	State    State  `json:"state"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`

	LogPath      string `json:"log_path"`
	LogBytes     int64  `json:"log_bytes"`
	LogLines     int64  `json:"log_lines"`
	LogTruncated bool   `json:"log_truncated"`

	OutputSeq          uint64     `json:"output_seq"`
	OutputGaps         []SeqRange `json:"output_gaps,omitempty"`
	OutputGapCount     uint64     `json:"output_gap_count"`
	OutputLostLines    uint64     `json:"output_lost_lines"`
	OutputGapOldestSeq uint64     `json:"output_gap_oldest_seq"`
	OutputGapNewestSeq uint64     `json:"output_gap_newest_seq"`
	LifetimeMS         int64      `json:"lifetime_ms"`
	StartedAt          time.Time  `json:"started_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
}

// Normalize makes a recovered record safe for a new server instance. Process
// identifiers from an earlier instance are diagnostic only and must never be
// trusted for signaling.
func Normalize(item Process, now time.Time) Process {
	if item.State != StateStarting && item.State != StateRunning {
		return item
	}
	item.State = StateInterrupted
	item.PID = 0
	item.GuardPID = 0
	item.PGID = 0
	if item.Error == "" {
		item.Error = "server restarted"
	}
	item.UpdatedAt = now
	endedAt := now
	item.EndedAt = &endedAt
	return item
}
