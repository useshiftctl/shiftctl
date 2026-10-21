package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/credentials"
	"github.com/useshiftctl/shiftctl/internal/destination"
	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/executor"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
	"github.com/useshiftctl/shiftctl/internal/verifier"
)

var verifyCmd = &cobra.Command{
	Use:   "verify <migration-name>",
	Short: "Verify a completed stage against the destination",
	Long: `Verify that every repository in the completed stage landed correctly
on the destination platform.

For each repository, shiftctl:
  1. Confirms the repository exists on the destination (API check)
  2. Confirms git content is reachable (git ls-remote)

If all items pass, the stage is marked verified and the next stage may be
approved. If any item fails, the stage stays in the verifying state and you
can re-run execute to retry, or investigate and re-run verify.`,
	Args: cobra.ExactArgs(1),
	RunE: runVerify,
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(_ *cobra.Command, args []string) error {
	ctx := context.Background()
	name := migration.SanitizeName(args[0])

	// ── Prerequisites ─────────────────────────────────────────────────────────
	rec, err := migration.Load(name)
	if err != nil {
		return fmt.Errorf("migration %q not found", name)
	}

	p, err := plan.Load(name)
	if err != nil {
		return fmt.Errorf("no plan found. run 'shiftctl plan %s' first", name)
	}

	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		return fmt.Errorf("loading execution state: %w", err)
	}

	// ── Find stage in verifying (or interrupted, as a fallback) state ─────────
	stage, stageState, err := findVerifyableStage(exec, p)
	if err != nil {
		return err
	}
	if stage == nil {
		return noVerifyableStageMessage(exec, name)
	}

	// ── Check git ─────────────────────────────────────────────────────────────
	if err := executor.CheckGitAvailable(); err != nil {
		return err
	}

	// ── Destination credential ────────────────────────────────────────────────
	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		return fmt.Errorf("unknown destination %q", rec.DestinationName)
	}

	dstCred, err := credentials.Get(name, "destination")
	if err != nil {
		return fmt.Errorf("destination credential not found. run 'shiftctl execute %s' to set it up", name)
	}

	fmt.Printf("\n  Verifying %s credential…\n", dest.DisplayName())
	destUsername, err := dest.ValidateCredential(ctx, dstCred)
	if err != nil {
		return fmt.Errorf("destination credential invalid: %w", err)
	}
	fmt.Printf("  %s Authenticated as %s on %s\n\n", check, bold.Render(destUsername), dest.DisplayName())

	// ── Load progress for this stage ──────────────────────────────────────────
	prog, err := executor.LoadProgress(name, stage.Number)
	if err != nil {
		return fmt.Errorf("no execution progress found for stage %d. was 'shiftctl execute %s' run?", stage.Number, name)
	}

	// ── Print header ──────────────────────────────────────────────────────────
	printVerifyHeader(name, *stage, len(p.Stages))

	// ── Run verification ──────────────────────────────────────────────────────
	opts := verifier.Options{
		DestCredential: dstCred,
		Dest:           dest,
		DestNamespace:  destUsername,
		ProgressFn:     makeVerifyProgressFn(stage),
	}

	report, err := verifier.Run(ctx, *stage, prog, opts)
	if err != nil {
		return fmt.Errorf("verification error: %w", err)
	}

	// ── Persist results for browser companion ─────────────────────────────────
	_ = verifier.SaveReport(name, report)

	// ── Results ───────────────────────────────────────────────────────────────
	passed, failed := report.Summary()
	printVerifySummary(report, name)

	if failed > 0 {
		printVerifyFailures(report)
	}

	// ── Transition stage ──────────────────────────────────────────────────────
	if report.Passed() {
		if err := execstate.Transition(exec, stage.Number, execstate.StatusVerified); err != nil {
			return fmt.Errorf("transitioning to verified: %w", err)
		}
		printVerifyNextStep(exec, stage.Number, name, len(p.Stages))
	} else {
		_ = passed // used in summary above
		fmt.Printf("  Stage %d remains in verifying state.\n", stageState.Number)
		fmt.Printf("  Fix the issues above and re-run %s.\n\n",
			bold.Render(fmt.Sprintf("shiftctl verify %s", name)))
	}

	return nil
}

// Display helpers ────────────────────────────────────────────────────────────

func printVerifyHeader(migrationName string, stage plan.Stage, totalStages int) {
	fmt.Println(rule)
	fmt.Printf("  %s  Verifying Stage %d of %d. %s\n",
		bold.Render("shiftctl verify"),
		stage.Number,
		totalStages,
		bold.Render(stage.Label),
	)
	fmt.Println(rule)
	fmt.Printf("\n  Checking %d %s on destination…\n\n", len(stage.Repos), repoNoun(len(stage.Repos)))
}

func makeVerifyProgressFn(stage *plan.Stage) func(verifier.ItemVerification) {
	total := len(stage.Repos)
	current := 0
	return func(iv verifier.ItemVerification) {
		current++
		icon := check
		detail := ""
		if !iv.OK {
			icon = cross
			detail = fmt.Sprintf(". %s", iv.Issue)
		} else if iv.DestWebURL != "" {
			detail = fmt.Sprintf(" %s", iv.DestWebURL)
		}
		fmt.Printf("  [%*d/%d] %s %s%s\n",
			len(fmt.Sprintf("%d", total)), current, total,
			icon,
			iv.FullName,
			dim.Render(detail),
		)
	}
}

func printVerifySummary(report *verifier.Report, migrationName string) {
	passed, failed := report.Summary()
	fmt.Println()
	fmt.Println(rule)
	if report.Passed() {
		fmt.Printf("  %s  Stage %d verified. %d %s confirmed on destination\n",
			check, report.StageNumber, passed, repoNoun(passed))
	} else {
		fmt.Printf("  Stage %d verification: %d passed, %d failed\n",
			report.StageNumber, passed, failed)
	}
	fmt.Println(rule)
	fmt.Println()
}

func printVerifyFailures(report *verifier.Report) {
	fmt.Println("  Failed items:")
	for _, iv := range report.Items {
		if !iv.OK {
			fmt.Printf("    %s %s\n", cross, iv.FullName)
			if iv.Issue != "" {
				fmt.Printf("      %s\n", dim.Render(iv.Issue))
			}
		}
	}
	fmt.Println()
}

func printVerifyNextStep(exec *execstate.State, verifiedStageNumber int, migrationName string, totalStages int) {
	// Find the next stage.
	var nextStage *execstate.StageState
	for i := range exec.Stages {
		if exec.Stages[i].Number == verifiedStageNumber+1 {
			nextStage = &exec.Stages[i]
			break
		}
	}

	if nextStage != nil && nextStage.Status == execstate.StatusPending {
		fmt.Printf("  Run %s to approve the next stage.\n\n",
			bold.Render(fmt.Sprintf("shiftctl approve %s", migrationName)))
		return
	}

	// All stages verified.
	allVerified := true
	for _, s := range exec.Stages {
		if s.Status != execstate.StatusVerified {
			allVerified = false
			break
		}
	}
	if allVerified {
		fmt.Printf("  %s  All %d stages verified.\n", check, totalStages)
		fmt.Printf("  Run %s to complete the migration.\n\n",
			bold.Render(fmt.Sprintf("shiftctl decommission %s", migrationName)))
	}
}

// Logic helpers ─────────────────────────────────────────────────────────────

func findVerifyableStage(exec *execstate.State, p *plan.Plan) (*plan.Stage, *execstate.StageState, error) {
	for i := range exec.Stages {
		ss := &exec.Stages[i]
		if ss.Status == execstate.StatusVerifying || ss.Status == execstate.StatusInterrupted {
			stage, ok := findPlanStage(p, ss.Number)
			if !ok {
				return nil, nil, fmt.Errorf("stage %d not found in plan", ss.Number)
			}
			return &stage, ss, nil
		}
	}
	return nil, nil, nil
}

func noVerifyableStageMessage(exec *execstate.State, name string) error {
	for _, s := range exec.Stages {
		switch s.Status {
		case execstate.StatusPending:
			fmt.Printf("\n  Stage %d has not been approved yet.\n", s.Number)
			fmt.Printf("  Run 'shiftctl approve %s' first.\n\n", name)
			return nil
		case execstate.StatusApproved:
			fmt.Printf("\n  Stage %d is approved but has not been executed yet.\n", s.Number)
			fmt.Printf("  Run 'shiftctl execute %s' first.\n\n", name)
			return nil
		case execstate.StatusExecuting:
			fmt.Printf("\n  Stage %d is currently executing.\n\n", s.Number)
			return nil
		}
	}

	// Check all verified.
	allVerified := true
	for _, s := range exec.Stages {
		if s.Status != execstate.StatusVerified {
			allVerified = false
			break
		}
	}
	if allVerified {
		fmt.Printf("\n  %s  All stages have been verified.\n", check)
		fmt.Printf("  Run 'shiftctl decommission %s' to complete the migration.\n\n", name)
		return nil
	}

	// Check for stage awaiting next approval.
	hasPending := false
	for _, s := range exec.Stages {
		if s.Status == execstate.StatusPending {
			hasPending = true
			fmt.Printf("\n  Stage %d is pending approval.\n", s.Number)
			fmt.Printf("  Run 'shiftctl approve %s'.\n\n", name)
			break
		}
	}
	if !hasPending {
		fmt.Printf("\n  Nothing to verify for migration %q.\n\n", name)
	}
	return nil
}

