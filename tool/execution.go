package tool

// ExecutionDetails carries transient execution facts for hosts. It is not part
// of model output and must not be written to the runtime event log.
type ExecutionDetails struct {
	Output  string
	Command *CommandOutput
	Changes []FileChange
}

type FileChange struct {
	Path      string
	Added     int
	Removed   int
	Lines     []DiffLine
	Truncated bool
}

type DiffLine struct {
	Kind    string
	OldLine int
	NewLine int
	Text    string
}
