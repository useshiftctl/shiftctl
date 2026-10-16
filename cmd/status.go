package cmd

import (
	"fmt"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/assessment"
	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/executor"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

var statusCmd = &cobra.Command{
	Use:   "status <migration-name>",
	Short: "Show the current state of a migration",
	Long: `Show a full snapshot of where a migration currently stands:
what has been done, what is in progress, and what the next step is.`,
	Args: cobra.ExactArgs(1),
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	rec, err := migration.Load(name)
	if err != nil {
		return fmt.Errorf("migration %q not found — run 'shiftctl connect' to create it", name)
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  %s\n", bold.Render("shiftctl status"), bold.Render(rec.Name))
	fmt.Println(rule)
	fmt.Println()

	// ── Migration record ──────────────────────────────────────────────────────
	fmt.Printf("  %-16s %s\n", "Source:", platformDisplayName(rec.SourcePlatform))
	fmt.Printf("  %-16s %s\n", "Destination:", rec.DestinationName)
	fmt.Printf("  %-16s %s\n", "Created:", rec.CreatedAt.Format("2006-01-02 15:04 UTC"))
	if rec.Context != "" {
		fmt.Printf("  %-16s %s\n", "Context:", rec.Context)
	}
	fmt.Println()

	// ── Assessment ────────────────────────────────────────────────────────────
	fmt.Printf("  %s\n", bold.Render("Assessment"))
	assessExists, _ := assessment.Exists(name)
	if !assessExists {
		fmt.Printf("    %s Not started\n", dim.Render("○"))
		fmt.Printf("    %s\n\n", dim.Render("Run: shiftctl assess "+name))
		return nil
	}

	state, err := assessment.Load(name)
	if err != nil {
		fmt.Printf("    %s Error loading assessment: %v\n\n", cross, err)
		return nil
	}

	fmt.Printf("    %s %d repositories scanned", check, state.RepoCount)
	if state.ScannedAt.IsZero() {
		fmt.Println()
	} else {
		fmt.Printf("  %s\n", dim.Render("("+state.ScannedAt.Format("2006-01-02 15:04 UTC")+")"))
	}

	if state.Analysis == nil {
		fmt.Printf("    %s Analysis not yet run\n", dim.Render("○"))
		fmt.Printf("    %s\n\n", dim.Render("Run: shiftctl assess "+name))
		return nil
	}

	low, medium, high, unknowns := assessment.RiskSummary(state)
	fmt.Printf("    %s Analysis complete — %s %s %s",
		check,
		riskLabel("low")+" ×"+fmt.Sprint(low),
		riskLabel("medium")+" ×"+fmt.Sprint(medium),
		riskLabel("high")+" ×"+fmt.Sprint(high),
	)
	if unknowns > 0 {
		fmt.Printf("  %s", dim.Render(fmt.Sprintf("%d unknown(s)", unknowns)))
	}
	fmt.Println()
	fmt.Println()

	// ── Plan ──────────────────────────────────────────────────────────────────
	fmt.Printf("  %s\n", bold.Render("Plan"))
	planExists, _ := plan.Exists(name)
	if !planExists {
		fmt.Printf("    %s Not generated\n", dim.Render("○"))
		fmt.Printf("    %s\n\n", dim.Render("Run: shiftctl plan "+name))
		return nil
	}

	p, err := plan.Load(name)
	if err != nil {
		fmt.Printf("    %s Error loading plan: %v\n\n", cross, err)
		return nil
	}

	totalRepos := 0
	for _, s := range p.Stages {
		totalRepos += len(s.Repos)
	}
	fmt.Printf("    %s %d stages, %d repositories\n", check, len(p.Stages), totalRepos)
	fmt.Println()

	// ── Execution state ───────────────────────────────────────────────────────
	fmt.Printf("  %s\n", bold.Render("Stages"))

	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		fmt.Printf("    %s Error loading execution state: %v\n\n", cross, err)
		return nil
	}

	for _, s := range p.Stages {
		ss := findStageState(exec, s.Number)
		if ss == nil {
			continue
		}

		icon, statusText := stageStatusDisplay(ss.Status)
		fmt.Printf("    %s  Stage %d — %s  %s\n",
			icon, s.Number, s.Label, dim.Render(statusText))

		// For executing/verifying/interrupted stages, show per-item progress.
		switch ss.Status {
		case execstate.StatusExecuting, execstate.StatusVerifying,
			execstate.StatusInterrupted:
			prog, err := executor.LoadProgress(name, s.Number)
			if err == nil {
				done, failed, uncertain, pending := executor.Summary(prog)
				fmt.Printf("         %s\n",
					dim.Render(fmt.Sprintf(
						"done: %d  failed: %d  uncertain: %d  pending: %d",
						done, failed, uncertain, pending,
					)),
				)
			}
		case execstate.StatusApproved:
			// Show approval ID for traceability.
			if ss.ApprovalID != "" {
				fmt.Printf("         %s\n", dim.Render("approval: "+ss.ApprovalID))
			}
		}
	}
	fmt.Println()

	// ── Next step ─────────────────────────────────────────────────────────────
	printStatusNextStep(exec, name)

	return nil
}

func stageStatusDisplay(status execstate.StageStatus) (icon, text string) {
	switch status {
	case execstate.StatusPending:
		return dim.Render("○"), "pending approval"
	case execstate.StatusApproved:
		return "◎", "approved — ready to execute"
	case execstate.StatusExecuting:
		return "◉", "executing"
	case execstate.StatusInterrupted:
		return cross, "interrupted — run execute to resume"
	case execstate.StatusVerifying:
		return "◎", "awaiting verification"
	case execstate.StatusVerified:
		return check, "verified"
	default:
		return "?", string(status)
	}
}

func printStatusNextStep(exec *execstate.State, name string) {
	fmt.Printf("  %s\n", bold.Render("Next step"))

	for _, s := range exec.Stages {
		switch s.Status {
		case execstate.StatusPending:
			fmt.Printf("    shiftctl approve %s\n\n", name)
			return
		case execstate.StatusApproved:
			fmt.Printf("    shiftctl execute %s\n\n", name)
			return
		case execstate.StatusExecuting, execstate.StatusInterrupted:
			fmt.Printf("    shiftctl execute %s\n\n", name)
			return
		case execstate.StatusVerifying:
			fmt.Printf("    shiftctl verify %s\n\n", name)
			return
		}
	}

	// Check if all verified.
	allVerified := true
	for _, s := range exec.Stages {
		if s.Status != execstate.StatusVerified {
			allVerified = false
			break
		}
	}
	if allVerified {
		fmt.Printf("    %s  All stages verified\n", check)
		fmt.Printf("    shiftctl decommission %s\n\n", name)
		return
	}

	fmt.Printf("    %s\n\n", dim.Render("Nothing pending"))
}

func findStageState(exec *execstate.State, number int) *execstate.StageState {
	for i := range exec.Stages {
		if exec.Stages[i].Number == number {
			return &exec.Stages[i]
		}
	}
	return nil
}

