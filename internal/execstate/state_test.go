package execstate_test

import (
	"testing"
	"time"

	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

func makeTestPlan(name string, stageCount int) *plan.Plan {
	stages := make([]plan.Stage, stageCount)
	for i := range stages {
		stages[i] = plan.Stage{
			Number: i + 1,
			Label:  "Stage label",
			Repos:  []plan.PlannedRepo{{FullName: "org/repo"}},
		}
	}
	return &plan.Plan{
		MigrationName: name,
		CreatedAt:     time.Now().UTC(),
		Stages:        stages,
	}
}

func TestLoadOrInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := makeTestPlan("test-migration", 2)

	s, err := execstate.LoadOrInit("test-migration", p)
	if err != nil {
		t.Fatalf("LoadOrInit() error: %v", err)
	}
	if len(s.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(s.Stages))
	}
	for _, stage := range s.Stages {
		if stage.Status != execstate.StatusPending {
			t.Errorf("stage %d: expected pending, got %q", stage.Number, stage.Status)
		}
	}

	// Second call should load from disk. same result.
	s2, err := execstate.LoadOrInit("test-migration", p)
	if err != nil {
		t.Fatalf("LoadOrInit() second call error: %v", err)
	}
	if len(s2.Stages) != 2 {
		t.Fatalf("second call: expected 2 stages, got %d", len(s2.Stages))
	}
}

func TestRecordApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := makeTestPlan("approval-test", 2)
	s, err := execstate.LoadOrInit("approval-test", p)
	if err != nil {
		t.Fatalf("LoadOrInit() error: %v", err)
	}

	items := []execstate.ApprovedItem{
		{FullName: "org/repo", RiskLevel: "low", Confidence: "confirmed"},
	}

	rec, err := execstate.RecordApproval(s, 1, items)
	if err != nil {
		t.Fatalf("RecordApproval() error: %v", err)
	}
	if rec.StageNumber != 1 {
		t.Errorf("ApprovalRecord.StageNumber = %d, want 1", rec.StageNumber)
	}
	if rec.ItemCount != 1 {
		t.Errorf("ApprovalRecord.ItemCount = %d, want 1", rec.ItemCount)
	}
	if s.Stages[0].Status != execstate.StatusApproved {
		t.Errorf("stage 1 status = %q, want %q", s.Stages[0].Status, execstate.StatusApproved)
	}

	// Approving an already-approved stage should error.
	_, err = execstate.RecordApproval(s, 1, items)
	if err == nil {
		t.Fatal("expected error approving already-approved stage, got nil")
	}
}

func TestNextPendingStage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	t.Run("stage 1 pending. returned immediately", func(t *testing.T) {
		p := makeTestPlan("next-pending-1", 2)
		s, _ := execstate.LoadOrInit("next-pending-1", p)

		stage, err := execstate.NextPendingStage(s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if stage == nil {
			t.Fatal("expected stage, got nil")
		}
		if stage.Number != 1 {
			t.Errorf("expected stage 1, got %d", stage.Number)
		}
	})

	t.Run("stage 2 not available until stage 1 is verified", func(t *testing.T) {
		p := makeTestPlan("next-pending-2", 2)
		s, _ := execstate.LoadOrInit("next-pending-2", p)

		// Approve stage 1 but don't verify it.
		execstate.RecordApproval(s, 1, nil)

		stage, err := execstate.NextPendingStage(s)
		if err == nil {
			t.Fatal("expected error: stage 2 blocked by unverified stage 1")
		}
		if stage != nil {
			t.Errorf("expected nil stage, got %d", stage.Number)
		}
	})

	t.Run("stage 2 available after stage 1 verified", func(t *testing.T) {
		p := makeTestPlan("next-pending-3", 2)
		s, _ := execstate.LoadOrInit("next-pending-3", p)

		execstate.RecordApproval(s, 1, nil)
		execstate.Transition(s, 1, execstate.StatusVerified)

		stage, err := execstate.NextPendingStage(s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if stage == nil {
			t.Fatal("expected stage 2, got nil")
		}
		if stage.Number != 2 {
			t.Errorf("expected stage 2, got %d", stage.Number)
		}
	})

	t.Run("all stages past pending. returns nil, nil", func(t *testing.T) {
		p := makeTestPlan("next-pending-4", 1)
		s, _ := execstate.LoadOrInit("next-pending-4", p)

		execstate.RecordApproval(s, 1, nil)
		execstate.Transition(s, 1, execstate.StatusVerified)

		stage, err := execstate.NextPendingStage(s)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if stage != nil {
			t.Errorf("expected nil, got stage %d", stage.Number)
		}
	})
}

func TestTransition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	p := makeTestPlan("transition-test", 1)
	s, _ := execstate.LoadOrInit("transition-test", p)

	transitions := []execstate.StageStatus{
		execstate.StatusApproved,
		execstate.StatusExecuting,
		execstate.StatusVerifying,
		execstate.StatusVerified,
	}
	for _, to := range transitions {
		if err := execstate.Transition(s, 1, to); err != nil {
			t.Errorf("Transition(1, %q) error: %v", to, err)
		}
		if s.Stages[0].Status != to {
			t.Errorf("after Transition(1, %q): status = %q", to, s.Stages[0].Status)
		}
	}

	// Transitioning a non-existent stage should error.
	if err := execstate.Transition(s, 99, execstate.StatusApproved); err == nil {
		t.Fatal("expected error for non-existent stage, got nil")
	}
}
