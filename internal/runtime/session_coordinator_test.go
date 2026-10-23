package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestSessionCoordinatorTasks(t *testing.T) {
	sc := newSessionCoordinator()

	if sc.HasRunningTasks() {
		t.Fatal("expected no running tasks initially")
	}

	sc.SetTaskState("s1", TaskState{Status: TaskRunning, ExecutionID: "e1"})
	if !sc.HasRunningTasks() {
		t.Fatal("expected running task")
	}

	st, ok := sc.TaskState("s1")
	if !ok || st.Status != TaskRunning || st.ExecutionID != "e1" {
		t.Fatalf("unexpected task state: %+v, ok=%v", st, ok)
	}

	sc.SetTaskState("s1", TaskState{Status: TaskCompleted, ExecutionID: "e1"})
	if sc.HasRunningTasks() {
		t.Fatal("expected no running tasks after completion")
	}
}

func TestSessionCoordinatorCancels(t *testing.T) {
	sc := newSessionCoordinator()

	ctx, cancel := context.WithCancel(context.Background())
	sc.SetTaskCancel("s1", cancel)

	cancelled := sc.CancelTask("s1")
	if !cancelled {
		t.Fatal("expected task to be cancelled")
	}
	if ctx.Err() == nil {
		t.Fatal("expected context to be cancelled")
	}

	// Double cancel returns false
	if sc.CancelTask("s1") {
		t.Fatal("expected false on second cancel")
	}
}

func TestSessionCoordinatorApprovals(t *testing.T) {
	sc := newSessionCoordinator()

	resp := make(chan bool, 1)
	sc.AddApproval("app-1", pendingApproval{
		response:  resp,
		sessionID: "s1",
	})

	apps := sc.ApprovalsForSession("s1")
	if len(apps) != 1 {
		t.Fatalf("expected 1 approval, got %d", len(apps))
	}
	if len(sc.ApprovalsForSession("s2")) != 0 {
		t.Fatal("expected 0 approvals for s2")
	}

	app, ok := sc.GetApproval("app-1")
	if !ok || app.sessionID != "s1" {
		t.Fatalf("expected approval app-1, got %+v", app)
	}

	sc.RemoveApproval("app-1")
	if _, ok := sc.GetApproval("app-1"); ok {
		t.Fatal("expected approval to be removed")
	}
}

func TestSessionCoordinatorClose(t *testing.T) {
	sc := newSessionCoordinator()

	waiterCh := make(chan turnResult, 1)
	sc.SetTurnWaiter("exec-1", waiterCh)

	appResp := make(chan bool, 1)
	sc.AddApproval("app-1", pendingApproval{response: appResp})

	interResp := make(chan userInteractionResult, 1)
	sc.AddInteraction("int-1", pendingInteraction{response: interResp})

	customErr := errors.New("closed test")
	sc.Close(customErr)

	// Waiter receives error
	select {
	case res := <-waiterCh:
		if !errors.Is(res.err, customErr) {
			t.Fatalf("expected %v, got %v", customErr, res.err)
		}
	default:
		t.Fatal("waiter did not receive close error")
	}

	// Approval receives false
	select {
	case granted := <-appResp:
		if granted {
			t.Fatal("expected false for closed approval")
		}
	default:
		t.Fatal("approval did not receive close notification")
	}

	// Interaction receives error
	select {
	case interRes := <-interResp:
		if !errors.Is(interRes.Err, customErr) {
			t.Fatalf("expected %v, got %v", customErr, interRes.Err)
		}
	default:
		t.Fatal("interaction did not receive close error")
	}
}

func TestSessionCoordinatorConcurrency(t *testing.T) {
	sc := newSessionCoordinator()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sess := string(rune('a' + (id % 10)))
			sc.SetTaskState(sess, TaskState{Status: TaskRunning})
			_ = sc.HasRunningTasks()
			sc.SetInteractive(sess, true)
			_ = sc.IsInteractive(sess)
			sc.SetQueueDraining(sess, true)
			_ = sc.IsQueueDraining(sess)
			sc.SetTaskState(sess, TaskState{Status: TaskCompleted})
		}(i)
	}

	wg.Wait()
}
