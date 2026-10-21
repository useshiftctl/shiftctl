package execstate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

// LoadOrInit returns the execution state for the named migration. If no state
// file exists yet (first call to 'shiftctl approve'), it is initialised from
// the plan with all stages set to pending.
func LoadOrInit(migrationName string, p *plan.Plan) (*State, error) {
	s, err := Load(migrationName)
	if err == nil {
		// Validate that the loaded state matches the current plan; if the
		// plan was regenerated with a different number of stages, reinitialise.
		if len(s.Stages) == len(p.Stages) {
			return s, nil
		}
		// Plan changed: reset stage statuses but preserve the approval log.
		approvals := s.Approvals
		s = initFromPlan(p)
		s.Approvals = approvals
		return s, Save(s)
	}
	if !os.IsNotExist(pathErr(err)) {
		return nil, err
	}
	s = initFromPlan(p)
	return s, Save(s)
}

// Load reads the execution state from disk.
func Load(migrationName string) (*State, error) {
	path, err := statePath(migrationName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing execution state: %w", err)
	}
	return &s, nil
}

// Save writes the execution state atomically to disk.
func Save(s *State) error {
	path, err := statePath(s.MigrationName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling execution state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing execution state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving execution state: %w", err)
	}
	return nil
}

// RecordApproval appends an approval record and transitions the given stage
// from pending → approved. Returns the new ApprovalRecord.
func RecordApproval(s *State, stageNumber int, items []ApprovedItem) (ApprovalRecord, error) {
	idx := stageIndex(s, stageNumber)
	if idx < 0 {
		return ApprovalRecord{}, fmt.Errorf("stage %d not found in execution state", stageNumber)
	}
	if s.Stages[idx].Status != StatusPending {
		return ApprovalRecord{}, fmt.Errorf(
			"stage %d cannot be approved: current status is %q",
			stageNumber, s.Stages[idx].Status,
		)
	}

	rec := ApprovalRecord{
		ID:          approvalID(stageNumber),
		StageNumber: stageNumber,
		ApprovedAt:  time.Now().UTC(),
		ItemCount:   len(items),
		Items:       items,
	}

	s.Approvals = append(s.Approvals, rec)
	s.Stages[idx].Status = StatusApproved
	s.Stages[idx].ApprovalID = rec.ID

	return rec, Save(s)
}

// NextPendingStage returns the stage that is next in line to be approved, or
// nil if all stages are past the pending state. It enforces the prerequisite
// that each stage must wait for the previous one to be verified.
func NextPendingStage(s *State) (*StageState, error) {
	for i := range s.Stages {
		stage := &s.Stages[i]
		if stage.Status != StatusPending {
			continue
		}
		// Stage 1 has no prerequisite. All others require the previous to be
		// verified before they can be approved.
		if i > 0 {
			prev := s.Stages[i-1]
			if prev.Status != StatusVerified {
				return nil, fmt.Errorf(
					"Stage %d cannot be approved yet. Stage %d must complete and be verified first\n"+
						"(Stage %d is currently: %s)",
					stage.Number, prev.Number, prev.Number, prev.Status,
				)
			}
		}
		return stage, nil
	}
	return nil, nil // all stages are past pending
}

// Transition moves a stage from its current status to the next one. Returns
// an error if the transition is not valid.
func Transition(s *State, stageNumber int, to StageStatus) error {
	idx := stageIndex(s, stageNumber)
	if idx < 0 {
		return fmt.Errorf("stage %d not found", stageNumber)
	}
	s.Stages[idx].Status = to
	return Save(s)
}

// Helpers ──────────────────────────────────────────────────────────────────

func initFromPlan(p *plan.Plan) *State {
	stages := make([]StageState, len(p.Stages))
	for i, s := range p.Stages {
		stages[i] = StageState{Number: s.Number, Status: StatusPending}
	}
	return &State{
		MigrationName: p.MigrationName,
		Stages:        stages,
	}
}

func stageIndex(s *State, number int) int {
	for i, stage := range s.Stages {
		if stage.Number == number {
			return i
		}
	}
	return -1
}

func approvalID(stageNumber int) string {
	return fmt.Sprintf("apv-%d-%d", stageNumber, time.Now().Unix())
}

func statePath(migrationName string) (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations", migration.SanitizeName(migrationName), "execstate.json"), nil
}

// pathErr unwraps an error to a *os.PathError so os.IsNotExist works on it.
func pathErr(err error) error {
	return err
}
