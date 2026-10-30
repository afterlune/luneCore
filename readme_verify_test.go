package core_test

import (
	"context"
	"encoding/json"
	"testing"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/model"
	"github.com/afterlune/luneCore/tool"
)

// TestReadmeAPISignatures verifies that all public APIs and types shown in README.md
// compile and match public signatures.
func TestReadmeAPISignatures(t *testing.T) {
	ctx := context.Background()
	client, err := core.New(core.Config{
		Workspace: t.TempDir(),
		StateDir:  t.TempDir(),
		Memory:    core.MemoryConfig{Disabled: true},
		ModelConfig: &model.Config{
			Providers:      map[string]model.ProviderConfig{"external": {Protocol: model.ProtocolResponses}},
			Profiles:       map[string]model.ModelProfile{"main": {Provider: "external", Model: "dummy-model"}},
			DefaultProfile: "main",
		},
		ModelProviders: map[string]model.Provider{"external": externalProvider{}},
	})
	if err != nil {
		t.Fatalf("core.New: %v", err)
	}
	defer client.Close(ctx)

	session, found, err := client.LatestSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		session, err = client.CreateSession(ctx, json.RawMessage("null"))
		if err != nil {
			t.Fatal(err)
		}
	}

	runtime, err := client.OpenSessionRuntime(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(ctx)

	// Goal API signatures
	budget := int64(1000)
	_, _ = runtime.CreateGoal(ctx, core.GoalCreateOptions{
		Objective:              "test",
		TokenBudget:            &budget,
		NoInteractiveDecisions: false,
	})
	_, _ = runtime.GetGoal(ctx)
	_, _ = runtime.PauseGoal(ctx)
	_, _ = runtime.ResumeGoal(ctx)
	_, _ = runtime.EditGoal(ctx, "new objective")
	_, _ = runtime.SetGoalBudget(ctx, &budget)
	_ = runtime.ClearGoal(ctx)

	// Queue management signatures
	input, _ := core.NewTaskInput("test")
	_ = runtime.CancelQueuedTurn(ctx, "fake-id")
	_, _ = runtime.CancelPendingTurns(ctx)
	_, _, _ = runtime.ReplaceQueuedTurnInput(ctx, "fake-id", input)

	// Event replay complete signature
	events, _ := runtime.SubscribeEventsWithReplayComplete(ctx, 0)
	_ = events

	// Question draft payload signature
	wait := core.UserQuestionAnswer{ExecutionID: "e", RunID: "r", InvocationID: "i", CallID: "c"}
	_ = runtime.SaveUserQuestionDraftPayload(ctx, wait, []byte("{}"))

	// Plan implementation signature
	_, _ = runtime.ImplementPlan(ctx, "fake-id", core.PlanImplementationOptions{
		Mode:           core.PlanImplementSameContext,
		PermissionMode: core.PermissionWorkspace,
		Authorization: core.PlanAuthorization{
			Paths:                    []tool.PathGrant{{Path: "/tmp", Access: tool.PathReadWrite}},
			AllowNetwork:             true,
			AllowedTools:             []string{"read_file"},
			ApprovedHighRiskCommands: []string{"test"},
		},
	})

	// Approval decision signature
	_ = runtime.ResolveApprovalDecision(ctx, "approval-id", tool.ApprovalAllowOnce)
	_ = runtime.ResolveApprovalDecision(ctx, "approval-id", tool.ApprovalAllowSession)
	_ = runtime.ResolveApprovalDecision(ctx, "approval-id", tool.ApprovalDeny)

	// Toolchain paths signature
	_ = client.CommandToolchainPaths()
	_ = client.SetCommandToolchainPaths([]string{"/bin"})
}
