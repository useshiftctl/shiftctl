// Package execstate tracks the execution lifecycle of each migration stage.
// It is the source of truth for what has been approved, what is executing,
// what is verifying, and what is complete — used by the approve, execute, and
// verify commands to know where the migration currently is and what is allowed
// next.
package execstate

import "time"

// StageStatus is the lifecycle state of one stage.
type StageStatus string

const (
	// StatusPending means this stage has not yet been approved.
	StatusPending StageStatus = "pending"
	// StatusApproved means the user has explicitly approved this stage.
	// Execution may now begin.
	StatusApproved StageStatus = "approved"
	// StatusExecuting means execution is in progress.
	StatusExecuting StageStatus = "executing"
	// StatusInterrupted means execution started but was interrupted (crash,
	// external failure, or user pause). The interrupted-stage recovery flow
	// applies before resuming.
	StatusInterrupted StageStatus = "interrupted"
	// StatusVerifying means execution completed and the verification step is
	// running or awaiting user review.
	StatusVerifying StageStatus = "verifying"
	// StatusVerified means the stage is fully complete and verified. The next
	// stage (if any) may now be approved.
	StatusVerified StageStatus = "verified"
)

// State is the on-disk execution state for a migration. It is initialised
// when the first approval is recorded and updated at each lifecycle transition.
type State struct {
	MigrationName string        `json:"migration_name"`
	Stages        []StageState  `json:"stages"`
	// Approvals is the append-only audit log of every approval recorded for
	// this migration. Every executed action will reference an entry here by
	// ApprovalID, providing full traceability from action → approval → user.
	Approvals     []ApprovalRecord `json:"approvals"`
}

// StageState tracks the current lifecycle status of one stage.
type StageState struct {
	Number     int         `json:"number"`
	Status     StageStatus `json:"status"`
	// ApprovalID is the ID of the ApprovalRecord that authorised execution of
	// this stage. Set when the stage transitions to StatusApproved.
	ApprovalID string      `json:"approval_id,omitempty"`
}

// ApprovalRecord is an immutable record of a single approval event. Every
// action executed in a stage is tagged with the ID of the record that
// authorised it.
type ApprovalRecord struct {
	// ID is a stable, human-readable reference: "apv-<stage>-<unix-ts>".
	ID          string        `json:"id"`
	StageNumber int           `json:"stage_number"`
	ApprovedAt  time.Time     `json:"approved_at"`
	ItemCount   int           `json:"item_count"`
	Items       []ApprovedItem `json:"items"`
}

// ApprovedItem records exactly what was in scope for a given approval, so the
// migration record can show "this action was authorised by approval apv-1-..."
// which in turn shows the exact item list that was approved.
type ApprovedItem struct {
	FullName   string `json:"full_name"`
	RiskLevel  string `json:"risk_level"`
	Confidence string `json:"confidence"`
}

// EscalationTrigger describes a condition that requires extra scrutiny before
// the user approves a stage, beyond the routine approval prompt.
type EscalationTrigger struct {
	// Repo is the full_name of the repository that triggered this, or empty
	// if the trigger is at the stage level.
	Repo   string
	Reason string
}
