package executor_test

import (
	"testing"

	"github.com/useshiftctl/shiftctl/internal/executor"
)

func TestLoadOrInitProgress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/repo-a", "org/repo-b", "org/repo-c"}
	p, err := executor.LoadOrInitProgress("test-migration", 1, "apv-1-123", names)
	if err != nil {
		t.Fatalf("LoadOrInitProgress() error: %v", err)
	}

	if p.MigrationName != "test-migration" {
		t.Errorf("MigrationName = %q, want %q", p.MigrationName, "test-migration")
	}
	if p.StageNumber != 1 {
		t.Errorf("StageNumber = %d, want 1", p.StageNumber)
	}
	if len(p.Items) != 3 {
		t.Fatalf("Items len = %d, want 3", len(p.Items))
	}
	for _, item := range p.Items {
		if item.Status != executor.ItemPending {
			t.Errorf("item %q: status = %q, want pending", item.FullName, item.Status)
		}
	}

	// Second call returns existing progress (idempotent).
	p2, err := executor.LoadOrInitProgress("test-migration", 1, "apv-1-123", names)
	if err != nil {
		t.Fatalf("LoadOrInitProgress() second call error: %v", err)
	}
	if len(p2.Items) != 3 {
		t.Fatalf("second call: Items len = %d, want 3", len(p2.Items))
	}
}

func TestMarkExecutingAndDone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p, _ := executor.LoadOrInitProgress("exec-done", 1, "apv-1", []string{"org/repo"})

	if err := executor.MarkExecuting(p, "org/repo"); err != nil {
		t.Fatalf("MarkExecuting() error: %v", err)
	}
	if p.Items[0].Status != executor.ItemExecuting {
		t.Errorf("after MarkExecuting: status = %q, want executing", p.Items[0].Status)
	}
	if p.Items[0].StartedAt == nil {
		t.Error("after MarkExecuting: StartedAt is nil")
	}

	if err := executor.MarkDone(p, "org/repo"); err != nil {
		t.Fatalf("MarkDone() error: %v", err)
	}
	if p.Items[0].Status != executor.ItemDone {
		t.Errorf("after MarkDone: status = %q, want done", p.Items[0].Status)
	}
	if p.Items[0].DoneAt == nil {
		t.Error("after MarkDone: DoneAt is nil")
	}
}

func TestMarkFailed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p, _ := executor.LoadOrInitProgress("exec-failed", 1, "apv-1", []string{"org/repo"})

	executor.MarkExecuting(p, "org/repo")
	if err := executor.MarkFailed(p, "org/repo", "push rejected: 403 Forbidden"); err != nil {
		t.Fatalf("MarkFailed() error: %v", err)
	}
	if p.Items[0].Status != executor.ItemFailed {
		t.Errorf("status = %q, want failed", p.Items[0].Status)
	}
	if p.Items[0].Error != "push rejected: 403 Forbidden" {
		t.Errorf("Error = %q, want %q", p.Items[0].Error, "push rejected: 403 Forbidden")
	}
}

func TestMarkUncertain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/a", "org/b", "org/c"}
	p, _ := executor.LoadOrInitProgress("exec-uncertain", 1, "apv-1", names)

	executor.MarkExecuting(p, "org/a")
	executor.MarkExecuting(p, "org/b")
	// org/c stays pending

	if err := executor.MarkUncertain(p); err != nil {
		t.Fatalf("MarkUncertain() error: %v", err)
	}

	statuses := map[string]executor.ItemStatus{}
	for _, item := range p.Items {
		statuses[item.FullName] = item.Status
	}
	if statuses["org/a"] != executor.ItemUncertain {
		t.Errorf("org/a: status = %q, want uncertain", statuses["org/a"])
	}
	if statuses["org/b"] != executor.ItemUncertain {
		t.Errorf("org/b: status = %q, want uncertain", statuses["org/b"])
	}
	if statuses["org/c"] != executor.ItemPending {
		t.Errorf("org/c: status = %q, want pending (unchanged)", statuses["org/c"])
	}
}

func TestUncertainItems(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/a", "org/b", "org/c"}
	p, _ := executor.LoadOrInitProgress("uncertain-items", 1, "apv-1", names)

	executor.MarkExecuting(p, "org/a")
	executor.MarkUncertain(p)

	uncertain := executor.UncertainItems(p)
	if len(uncertain) != 1 {
		t.Fatalf("UncertainItems() len = %d, want 1", len(uncertain))
	}
	if uncertain[0].FullName != "org/a" {
		t.Errorf("uncertain item = %q, want org/a", uncertain[0].FullName)
	}
}

func TestPendingItems(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/a", "org/b", "org/c", "org/d"}
	p, _ := executor.LoadOrInitProgress("pending-items", 1, "apv-1", names)

	executor.MarkExecuting(p, "org/a")
	executor.MarkDone(p, "org/a")
	executor.MarkExecuting(p, "org/b")
	executor.MarkFailed(p, "org/b", "error")
	// org/c and org/d are still pending

	pending := executor.PendingItems(p)
	if len(pending) != 2 {
		t.Fatalf("PendingItems() len = %d, want 2", len(pending))
	}
}

func TestSummary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/a", "org/b", "org/c", "org/d", "org/e"}
	p, _ := executor.LoadOrInitProgress("summary-test", 1, "apv-1", names)

	executor.MarkExecuting(p, "org/a")
	executor.MarkDone(p, "org/a")

	executor.MarkExecuting(p, "org/b")
	executor.MarkFailed(p, "org/b", "err")

	executor.MarkExecuting(p, "org/c")
	executor.MarkUncertain(p) // org/c → uncertain

	// org/d and org/e remain pending

	done, failed, uncertain, pending := executor.Summary(p)
	if done != 1 {
		t.Errorf("done = %d, want 1", done)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if uncertain != 1 {
		t.Errorf("uncertain = %d, want 1", uncertain)
	}
	if pending != 2 {
		t.Errorf("pending = %d, want 2", pending)
	}
}

func TestResetFailedToPending(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	names := []string{"org/a", "org/b"}
	p, _ := executor.LoadOrInitProgress("reset-failed", 1, "apv-1", names)

	executor.MarkExecuting(p, "org/a")
	executor.MarkFailed(p, "org/a", "some error")

	if err := executor.ResetFailedToPending(p); err != nil {
		t.Fatalf("ResetFailedToPending() error: %v", err)
	}

	for _, item := range p.Items {
		if item.FullName == "org/a" {
			if item.Status != executor.ItemPending {
				t.Errorf("org/a: status = %q, want pending after reset", item.Status)
			}
			if item.Error != "" {
				t.Errorf("org/a: Error = %q, want empty after reset", item.Error)
			}
		}
	}
}
