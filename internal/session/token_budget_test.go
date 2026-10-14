package session

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestExecutionTokenBudgetReconcilesAndPreservesUnknownReservations(t *testing.T) {
	store, path := openTestStore(t)
	ctx := context.Background()
	if err := store.SetExecutionTokenBudget(ctx, "session", "execution", 100); err != nil {
		t.Fatal(err)
	}
	first, allowed, err := store.ReserveExecutionTokens(ctx, "execution", 20, 60)
	if err != nil || !allowed || first.OutputLimit != 60 || first.OutputWasLimited {
		t.Fatalf("first reservation = %#v, allowed=%v, error=%v", first, allowed, err)
	}
	unknown, allowed, err := store.ReserveExecutionTokens(ctx, "execution", 10, 80)
	if err != nil || !allowed || unknown.OutputLimit != 10 || !unknown.OutputWasLimited {
		t.Fatalf("parallel reservation = %#v, allowed=%v, error=%v", unknown, allowed, err)
	}
	if _, allowed, err := store.ReserveExecutionTokens(ctx, "execution", 1, 1); err != nil || allowed {
		t.Fatalf("reservation beyond shared cap allowed=%v, error=%v", allowed, err)
	}
	if err := store.SettleExecutionTokens(ctx, "execution", first.ID, 70); err != nil {
		t.Fatalf("settle measured usage: %v", err)
	}
	if err := store.SettleExecutionTokens(ctx, "execution", first.ID, 70); err != nil {
		t.Fatalf("repeat settlement: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen token ledger: %v", err)
	}
	defer store.Close()
	if _, allowed, err := store.ReserveExecutionTokens(ctx, "execution", 10, 1); err != nil || allowed {
		t.Fatalf("unsettled reservation was not retained after restart: allowed=%v, error=%v", allowed, err)
	}
	if err := store.SetExecutionTokenBudget(ctx, "session", "execution", 150); err != nil {
		t.Fatalf("raise absolute cap: %v", err)
	}
	resumed, allowed, err := store.ReserveExecutionTokens(ctx, "execution", 10, 100)
	if err != nil || !allowed || resumed.OutputLimit != 50 {
		t.Fatalf("reservation after raising cap = %#v, allowed=%v, error=%v", resumed, allowed, err)
	}
}

func TestExecutionTokenBudgetReservationsAreAtomicAcrossConcurrentBranches(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	if err := store.SetExecutionTokenBudget(ctx, "session", "parallel", 100); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowedCount := 0
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, allowed, err := store.ReserveExecutionTokens(ctx, "parallel", 10, 100)
			if err != nil {
				t.Errorf("reserve tokens: %v", err)
				return
			}
			if allowed {
				mu.Lock()
				allowedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowedCount != 1 {
		t.Fatalf("successful concurrent reservations = %d, want 1", allowedCount)
	}
}

func TestDeletingSessionClearsItsExecutionTokenLedgers(t *testing.T) {
	store, _ := openTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Snapshot{ID: "session-delete", Workspace: "workspace", Data: []byte(`{"history":null}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExecutionTokenBudget(ctx, "session-delete", "execution-delete", 100); err != nil {
		t.Fatal(err)
	}
	if _, allowed, err := store.ReserveExecutionTokens(ctx, "execution-delete", 10, 20); err != nil || !allowed {
		t.Fatalf("reserve before delete: allowed=%v, error=%v", allowed, err)
	}
	if deleted, err := store.Delete(ctx, "session-delete", "workspace"); err != nil || !deleted {
		t.Fatalf("Delete session: deleted=%v, error=%v", deleted, err)
	}
	var budgets, reservations int
	if err := store.db.QueryRow(`SELECT count(*) FROM execution_token_budgets WHERE session_id = ?`, "session-delete").Scan(&budgets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM execution_token_reservations WHERE execution_id = ?`, "execution-delete").Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if budgets != 0 || reservations != 0 {
		t.Fatalf("deleted session retained %d budget rows and %d reservations", budgets, reservations)
	}
}

func TestOpenV11DatabaseAddsTokenBudgetTablesAndPreservesSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions-v11.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), Snapshot{ID: "existing", Workspace: "workspace", Data: []byte(`{"history":null}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE execution_token_reservations`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE execution_token_budgets`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA user_version = 11`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatalf("migrate v11 database: %v", err)
	}
	defer store.Close()
	if _, err := store.Get(context.Background(), "existing", "workspace"); err != nil {
		t.Fatalf("existing session lost during migration: %v", err)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
}
