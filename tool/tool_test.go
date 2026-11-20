package tool

import (
	"context"
	"errors"
	"testing"
)

func TestMetadataValid(t *testing.T) {
	for _, cls := range []PermissionClass{
		PermissionSessionControl,
		PermissionWorkspaceRead,
		PermissionWorkspaceWrite,
		PermissionCommand,
		PermissionNetwork,
	} {
		if !(Metadata{PermissionClass: cls}).Valid() {
			t.Fatalf("expected %q to be valid", cls)
		}
	}
	if (Metadata{PermissionClass: "invalid"}).Valid() {
		t.Fatal("expected invalid permission class to be rejected")
	}
}

func TestResultText(t *testing.T) {
	if got := (Result{Output: "ok"}).Text(); got != "ok" {
		t.Fatalf("got %q", got)
	}
	if got := (Result{Err: errors.New("fail")}).Text(); got != "Error: fail" {
		t.Fatalf("got %q", got)
	}
	if got := (Result{Output: "partial", Err: errors.New("fail")}).Text(); got != "partial\nError: fail" {
		t.Fatalf("got %q", got)
	}
}

func TestContextHelpers(t *testing.T) {
	if WithCallIdentity(nil, CallIdentity{SessionID: "s"}) != nil {
		t.Fatal("expected nil context")
	}
	if CallIdentityFromContext(nil) != (CallIdentity{}) {
		t.Fatal("expected empty identity for nil context")
	}
	if InvocationID(nil) != "" {
		t.Fatal("expected empty invocation ID for nil context")
	}
	ctx := WithCallIdentity(context.Background(), CallIdentity{SessionID: "s1", AgentID: "lead"})
	if WithInvocationID(ctx, "") != ctx {
		t.Fatal("empty invocation ID should return unchanged context")
	}
	ctx = WithInvocationID(ctx, "inv-1")
	if got := CallIdentityFromContext(ctx); got.SessionID != "s1" || got.InvocationID != "inv-1" {
		t.Fatalf("unexpected identity: %#v", got)
	}
	if InvocationID(ctx) != "inv-1" {
		t.Fatalf("got %q", InvocationID(ctx))
	}

	if WithPlanAuthorization(nil, PlanAuthorization{}) != nil {
		t.Fatal("expected nil context")
	}
	if len(PlanAuthorizationFromContext(nil).Paths) != 0 {
		t.Fatal("expected empty plan auth")
	}
	auth := PlanAuthorization{Paths: []PathGrant{{Path: "/tmp", Access: PathRead}}, AllowNetwork: true, AllowedTools: []string{"read_file"}, ApprovedHighRiskCommands: []string{"rm"}}
	ctx = WithPlanAuthorization(ctx, auth)
	gotAuth := PlanAuthorizationFromContext(ctx)
	if !gotAuth.AllowNetwork || len(gotAuth.Paths) != 1 || gotAuth.AllowedTools[0] != "read_file" {
		t.Fatalf("unexpected plan auth: %#v", gotAuth)
	}

	if WithNoInteractiveDecisions(nil, true) != nil || NoInteractiveDecisions(nil) {
		t.Fatal("unexpected nil behavior")
	}
	ctx = WithNoInteractiveDecisions(ctx, true)
	if !NoInteractiveDecisions(ctx) {
		t.Fatal("expected NoInteractiveDecisions=true")
	}
}

func TestCommandStatusAndBlockerError(t *testing.T) {
	if !CommandRunning.Active() || !CommandStopping.Active() || CommandSucceeded.Active() || CommandFailed.Active() {
		t.Fatal("unexpected CommandStatus.Active result")
	}
	var nilErr *ExecutionBlockerError
	if nilErr.Error() == "" {
		t.Fatal("nil ExecutionBlockerError should have message")
	}
	if got := (&ExecutionBlockerError{Tool: "command"}).Error(); got == "" {
		t.Fatal("missing tool message")
	}
	if got := (&ExecutionBlockerError{Tool: "command", Reason: "custom"}).Error(); got != "custom" {
		t.Fatalf("got %q", got)
	}
}
