package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"

	publictool "github.com/afterlune/luneCore/tool"
)

func TestExitErrorAndRunValidation(t *testing.T) {
	if got := (&ExitError{Code: 42}).Error(); !strings.Contains(got, "42") {
		t.Fatalf("unexpected ExitError: %q", got)
	}
	if err := RunContext(nil, publictool.CommandRequest{}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for nil context")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunContext(canceled, publictool.CommandRequest{Command: "echo", Workspace: t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for canceled context")
	}
	if err := Run(publictool.CommandRequest{Workspace: t.TempDir()}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for empty command")
	}
	if err := Run(publictool.CommandRequest{Command: "echo"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error for empty workspace")
	}
	if err := Run(publictool.CommandRequest{Command: "echo", Workspace: t.TempDir()}, nil); err == nil {
		t.Fatal("expected error for nil writer")
	}
	if err := RunChild(publictool.CommandRequest{}); err == nil {
		t.Fatal("expected error for empty RunChild request")
	}
}
