package tool

import "time"

// CommandOptions bounds Core-managed commands. Timeout is the default for
// task-scoped commands and the maximum for explicit command timeouts.
type CommandOptions struct {
	Timeout       time.Duration
	MaxConcurrent int
}

type CommandStatus string

const (
	CommandRunning     CommandStatus = "running"
	CommandStopping    CommandStatus = "stopping"
	CommandSucceeded   CommandStatus = "succeeded"
	CommandFailed      CommandStatus = "failed"
	CommandCancelled   CommandStatus = "cancelled"
	CommandTimedOut    CommandStatus = "timed_out"
	CommandInterrupted CommandStatus = "interrupted"
)

func (s CommandStatus) Active() bool { return s == CommandRunning || s == CommandStopping }

type CommandInfo struct {
	Command      string        `json:"command_text"`
	ID           string        `json:"id"`
	SessionID    string        `json:"session_id"`
	ExecutionID  string        `json:"execution_id"`
	AgentID      string        `json:"agent_id"`
	InvocationID string        `json:"invocation_id"`
	TaskID       string        `json:"task_id,omitempty"`
	Background   bool          `json:"background,omitempty"`
	Status       CommandStatus `json:"status"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at,omitempty"`
	ExitCode     *int          `json:"exit_code,omitempty"`
}

// Cursor and NextCursor are absolute byte offsets. Truncated means the
// requested cursor preceded the retained output. EndCursor may exceed NextCursor.
type CommandOutput struct {
	Command    CommandInfo `json:"command"`
	Output     string      `json:"output"`
	Cursor     int64       `json:"cursor"`
	NextCursor int64       `json:"next_cursor"`
	EndCursor  int64       `json:"end_cursor"`
	Truncated  bool        `json:"truncated,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type CommandReadOptions struct {
	Cursor int64
	// Wait waits for new output or completion, up to five seconds. Zero polls.
	Wait time.Duration
}
