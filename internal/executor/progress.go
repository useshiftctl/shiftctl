// Package executor handles the execution of an approved migration stage: it
// iterates the approved repositories in order, runs git mirror push for each
// one, saves progress after every item, and handles crash recovery.
package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/useshiftctl/shiftctl/internal/migration"
)

// ItemStatus is the execution state of a single repository in a stage.
type ItemStatus string

const (
	// ItemPending means the item has not been attempted yet.
	ItemPending ItemStatus = "pending"
	// ItemExecuting means execution started but no outcome has been recorded.
	// If progress is in this state when the process resumes, it is treated as
	// uncertain and re-verified before continuing.
	ItemExecuting ItemStatus = "executing"
	// ItemDone means the item was migrated successfully and verified.
	ItemDone ItemStatus = "done"
	// ItemUncertain means the item was executing when a crash occurred and the
	// outcome could not be confirmed. Requires live re-verification.
	ItemUncertain ItemStatus = "uncertain"
	// ItemFailed means the item failed with a known error. Execution continues
	// with the remaining items; the failure is reported at the end.
	ItemFailed ItemStatus = "failed"
)

// ItemProgress tracks execution state for one repository in a stage.
type ItemProgress struct {
	FullName  string     `json:"full_name"`
	Status    ItemStatus `json:"status"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

// StageProgress is the on-disk progress file for a single stage. One file is
// written per stage: progress-stage-<N>.json.
type StageProgress struct {
	MigrationName string         `json:"migration_name"`
	StageNumber   int            `json:"stage_number"`
	ApprovalID    string         `json:"approval_id"`
	Items         []ItemProgress `json:"items"`
}

// LoadOrInitProgress returns the progress file for the given stage. If no
// progress file exists (first execution of this stage), it is created from the
// approved item list with all items set to pending.
func LoadOrInitProgress(migrationName string, stageNumber int, approvalID string, fullNames []string) (*StageProgress, error) {
	p, err := LoadProgress(migrationName, stageNumber)
	if err == nil {
		return p, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	items := make([]ItemProgress, len(fullNames))
	for i, fn := range fullNames {
		items[i] = ItemProgress{FullName: fn, Status: ItemPending}
	}
	p = &StageProgress{
		MigrationName: migrationName,
		StageNumber:   stageNumber,
		ApprovalID:    approvalID,
		Items:         items,
	}
	return p, SaveProgress(p)
}

// LoadProgress reads the progress file for a stage from disk.
func LoadProgress(migrationName string, stageNumber int) (*StageProgress, error) {
	path, err := progressPath(migrationName, stageNumber)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p StageProgress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing progress file: %w", err)
	}
	return &p, nil
}

// SaveProgress writes the progress file atomically.
func SaveProgress(p *StageProgress) error {
	path, err := progressPath(p.MigrationName, p.StageNumber)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing progress: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving progress: %w", err)
	}
	return nil
}

// MarkExecuting sets the item to executing and saves. Called just before
// starting work on a repo so a crash leaves it in "uncertain" territory.
func MarkExecuting(p *StageProgress, fullName string) error {
	idx := itemIndex(p, fullName)
	if idx < 0 {
		return fmt.Errorf("item %q not found in progress", fullName)
	}
	now := time.Now().UTC()
	p.Items[idx].Status = ItemExecuting
	p.Items[idx].StartedAt = &now
	return SaveProgress(p)
}

// MarkDone sets the item to done and saves.
func MarkDone(p *StageProgress, fullName string) error {
	idx := itemIndex(p, fullName)
	if idx < 0 {
		return fmt.Errorf("item %q not found in progress", fullName)
	}
	now := time.Now().UTC()
	p.Items[idx].Status = ItemDone
	p.Items[idx].DoneAt = &now
	return SaveProgress(p)
}

// MarkFailed sets the item to failed with an error message and saves.
func MarkFailed(p *StageProgress, fullName, errMsg string) error {
	idx := itemIndex(p, fullName)
	if idx < 0 {
		return fmt.Errorf("item %q not found in progress", fullName)
	}
	now := time.Now().UTC()
	p.Items[idx].Status = ItemFailed
	p.Items[idx].Error = errMsg
	p.Items[idx].DoneAt = &now
	return SaveProgress(p)
}

// MarkUncertain transitions any ItemExecuting items to ItemUncertain. Called
// on startup when a previous progress file exists with executing items ,
// those items had an unknown outcome when the process was interrupted.
func MarkUncertain(p *StageProgress) error {
	changed := false
	for i := range p.Items {
		if p.Items[i].Status == ItemExecuting {
			p.Items[i].Status = ItemUncertain
			changed = true
		}
	}
	if changed {
		return SaveProgress(p)
	}
	return nil
}

// ResetFailedToPending resets all ItemFailed items back to ItemPending so they
// will be retried on the next execution run. Used by the --retry-failed flag.
func ResetFailedToPending(p *StageProgress) error {
	changed := false
	for i := range p.Items {
		if p.Items[i].Status == ItemFailed {
			p.Items[i].Status = ItemPending
			p.Items[i].Error = ""
			p.Items[i].StartedAt = nil
			p.Items[i].DoneAt = nil
			changed = true
		}
	}
	if changed {
		return SaveProgress(p)
	}
	return nil
}

// UncertainItems returns all items currently in the uncertain state.
func UncertainItems(p *StageProgress) []ItemProgress {
	var out []ItemProgress
	for _, it := range p.Items {
		if it.Status == ItemUncertain {
			out = append(out, it)
		}
	}
	return out
}

// PendingItems returns all items that have not yet been completed (pending or
// uncertain after recovery). Does not include done, failed.
func PendingItems(p *StageProgress) []ItemProgress {
	var out []ItemProgress
	for _, it := range p.Items {
		if it.Status == ItemPending || it.Status == ItemUncertain {
			out = append(out, it)
		}
	}
	return out
}

// Summary returns counts of each status.
func Summary(p *StageProgress) (done, failed, uncertain, pending int) {
	for _, it := range p.Items {
		switch it.Status {
		case ItemDone:
			done++
		case ItemFailed:
			failed++
		case ItemUncertain:
			uncertain++
		case ItemPending:
			pending++
		}
	}
	return
}

// Helpers ───────────────────────────────────────────────────────────────────

func itemIndex(p *StageProgress, fullName string) int {
	for i, it := range p.Items {
		if it.FullName == fullName {
			return i
		}
	}
	return -1
}

func progressPath(migrationName string, stageNumber int) (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	filename := fmt.Sprintf("progress-stage-%d.json", stageNumber)
	return filepath.Join(base, "migrations", migration.SanitizeName(migrationName), filename), nil
}
