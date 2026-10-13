package sandbox

import (
	"context"
	"errors"
	"io"

	publictool "github.com/afterlune/luneCore/tool"
)

// Run executes a serialized command request in a platform sandbox. The Linux
// implementation replaces the helper process with the requested shell after
// installing kernel restrictions; Windows creates an AppContainer child.
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
