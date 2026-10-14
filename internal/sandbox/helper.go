package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"

	publictool "github.com/afterlune/luneCore/tool"
)

// ExitError preserves the shell's exit code across the helper boundary.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("sandbox command exited with code %d", e.Code) }

const HelperProtocolVersion = 1

// HelperRequest fails closed with old helpers that reject unknown JSON fields.
type HelperRequest struct {
	ProtocolVersion int `json:"helper_protocol_version"`
	publictool.CommandRequest
}

// Run executes a serialized command request in a platform sandbox. The Linux
// implementation supervises a shell that inherits kernel restrictions; Windows
// creates an AppContainer or restricted-token child.
func Run(request publictool.CommandRequest, output io.Writer) error {
	return RunContext(context.Background(), request, output)
}

// RunContext runs a command and releases any temporary sandbox grants if the
// caller cancels the operation.
func RunContext(ctx context.Context, request publictool.CommandRequest, output io.Writer) error {
	if ctx == nil {
		return errors.New("sandbox context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Command == "" || request.Workspace == "" {
		return errors.New("command and workspace are required")
	}
	if output == nil {
		return errors.New("command output writer is required")
	}
	return runPlatform(ctx, request, output)
}
