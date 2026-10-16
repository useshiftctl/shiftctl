package plan

import (
	"time"

	"github.com/useshiftctl/shiftctl/internal/ai"
)

// Plan is the full staged migration plan, written to disk after the user runs
// 'shiftctl plan <name>'. It is what feeds the Approval and Execution phases.
type Plan struct {
	MigrationName string         `json:"migration_name"`
	CreatedAt     time.Time      `json:"created_at"`
	Stages        []Stage        `json:"stages"`
	KnownUnknowns []ai.KnownUnknown `json:"known_unknowns,omitempty"`
}

// Stage is one of the three grouped execution phases.
type Stage struct {
	Number int    `json:"number"`
	// Label is the brief heading shown on every stage-related screen.
	Label  string `json:"label"`
	// Reason is the plain-language explanation of why items were grouped here,
	// shown on the Planning and Approval screens.
	Reason string `json:"reason"`
	Repos  []PlannedRepo `json:"repos"`
	// Note is a user-added annotation for this stage (populated via the
	// browser companion's "Add a Note About This Stage" interaction).
	Note   string `json:"note,omitempty"`
}

// PlannedRepo is one item within a stage. It carries the AI's full analysis
// so the Approval command can display reasoning and evidence without
// re-reading the assessment file.
type PlannedRepo struct {
	FullName   string   `json:"full_name"`
	RiskLevel  string   `json:"risk_level"`  // "low", "medium", "high"
	Confidence string   `json:"confidence"`  // "confirmed", "best_guess"
	Evidence   string   `json:"evidence"`
	DependsOn  []string `json:"depends_on"`  // full_names within this migration
	Reasoning  string   `json:"reasoning"`
	// UserNote is set when the user corrects or annotates this item via the
	// browser companion's "Flag as incorrect" interaction. It carries forward
	// into the migration record as provenance.
	UserNote   string   `json:"user_note,omitempty"`
	// ScanError is propagated from assessment when scanning partially failed
	// for this repo.
	ScanError  string   `json:"scan_error,omitempty"`
}

// stageLabels and stageReasons are the canonical text shown on every
// stage-related screen — Spec mandates these be in plain language with the
// reasoning stated explicitly, not just "Stage 1 of 3".
var stageLabels = [4]string{
	"",
	"Confirmed, low risk",
	"Best guess or uncertain",
	"Confirmed high risk",
}

// KnownUnknown is re-exported from internal/ai so callers of the plan package
// do not need to import internal/ai directly.
type KnownUnknown = ai.KnownUnknown

var stageReasons = [4]string{
	"",
	"Moved first. If anything goes wrong here it will be on something small and unimportant — the safest way to build confidence before the critical systems.",
	"Held until Stage 1 has already completed successfully. These items have weaker evidence or were inferred from indirect signals — review the reasoning before approving.",
	"Moved last, once confidence is highest from the earlier stages completing. These are your most important or critical systems.",
}
