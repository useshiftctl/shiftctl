package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/credentials"
	"github.com/useshiftctl/shiftctl/internal/destination"
	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/executor"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

var retryFailed bool

var executeCmd = &cobra.Command{
	Use:   "execute <migration-name>",
	Short: "Execute the approved stage",
	Long: `Execute the currently approved migration stage.

Execution requires an explicit prior approval (shiftctl approve <name>).
Nothing is executed without it.

For each repository in the approved stage, shiftctl:
  1. Creates the repository on the destination platform
  2. Mirrors the full git history (all branches, tags, refs)
  3. Copies webhooks and branch protection rules (best-effort)

Progress is saved after every completed item. If execution is interrupted,
running this command again resumes from where it stopped. Items that were
mid-flight during a crash are re-verified against the live destination before
being counted as done or re-run.

Use --retry-failed to re-attempt any items that failed in a previous run.

The destination credential is requested here if it has not been stored yet ,
it is never required before execution actually begins.`,
	Args: cobra.ExactArgs(1),
	RunE: runExecute,
}

func init() {
	rootCmd.AddCommand(executeCmd)
	executeCmd.Flags().BoolVar(&retryFailed, "retry-failed", false, "Re-attempt items that failed in a previous run")
}

func runExecute(_ *cobra.Command, args []string) error {
	ctx := context.Background()
	name := migration.SanitizeName(args[0])

	// ── Prerequisites ─────────────────────────────────────────────────────────
	rec, err := migration.Load(name)
	if err != nil {
		return fmt.Errorf("migration %q not found. run 'shiftctl connect' first", name)
	}

	p, err := plan.Load(name)
	if err != nil {
		return fmt.Errorf("no plan found. run 'shiftctl plan %s' first", name)
	}

	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		return fmt.Errorf("loading execution state: %w", err)
	}

	// ── Find approved stage ───────────────────────────────────────────────────
	stage, stageState, err := findApprovedOrExecutingStage(exec, p)
	if err != nil {
		return err
	}
	if stage == nil {
		return noApprovedStageMessage(exec, name)
	}

	// ── Check git is available ────────────────────────────────────────────────
	if err := executor.CheckGitAvailable(); err != nil {
		return err
	}

	// ── Source credential ─────────────────────────────────────────────────────
	srcCred, err := credentials.Get(name, "source")
	if err != nil {
		return fmt.Errorf("source credential not found. run 'shiftctl connect' to set it up")
	}

	// ── Resolve destination implementation ────────────────────────────────────
	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		return fmt.Errorf("unknown destination %q", rec.DestinationName)
	}
	if !dest.IsImplemented() {
		return fmt.Errorf("%s is not yet supported as a migration destination in this version of shiftctl", dest.DisplayName())
	}

	// ── Destination credential (deferred until first execution) ───────────────
	dstCred, err := credentials.Get(name, "destination")
	if err != nil {
		// Not stored yet. collect it now.
		dstCred, err = collectDestinationCredential(ctx, name, dest)
		if err != nil {
			return err
		}
	}

	// Validate the destination credential and get the namespace.
	fmt.Printf("\n  Verifying %s credential…\n", dest.DisplayName())
	destUsername, err := dest.ValidateCredential(ctx, dstCred)
	if err != nil {
		return fmt.Errorf("destination credential invalid: %w", err)
	}
	fmt.Printf("  %s Authenticated as %s on %s\n\n", check, bold.Render(destUsername), dest.DisplayName())

	// ── Transition stage to executing ─────────────────────────────────────────
	if stageState.Status == execstate.StatusApproved {
		if err := execstate.Transition(exec, stage.Number, execstate.StatusExecuting); err != nil {
			return fmt.Errorf("transitioning stage to executing: %w", err)
		}
	}

	// ── Print execution header ────────────────────────────────────────────────
	printExecutionHeader(rec.Name, *stage, len(p.Stages))

	// ── Run ───────────────────────────────────────────────────────────────────
	opts := executor.Options{
		SourcePlatform:   rec.SourcePlatform,
		SourceCredential: srcCred,
		DestCredential:   dstCred,
		Dest:             dest,
		DestNamespace:    destUsername,
		RetryFailed:      retryFailed,
		ProgressFn:       makeProgressFn(stage),
		RecoveryFn:       printRecoveryReport,
	}

	prog, err := executor.Run(ctx, exec, *stage, opts)
	if err != nil {
		// Transition to interrupted so the user knows to resume.
		_ = execstate.Transition(exec, stage.Number, execstate.StatusInterrupted)
		return fmt.Errorf("execution interrupted: %w", err)
	}

	// ── Results ───────────────────────────────────────────────────────────────
	done, failed, uncertain, _ := executor.Summary(prog)
	printExecutionSummary(stage.Number, done, failed, uncertain, name)

	if failed > 0 {
		printFailedItems(prog)
	}

	// ── Transition stage status ───────────────────────────────────────────────
	if failed == 0 && uncertain == 0 {
		if err := execstate.Transition(exec, stage.Number, execstate.StatusVerifying); err != nil {
			return fmt.Errorf("transitioning to verifying: %w", err)
		}
	} else {
		// Partial success. stay as executing so recovery works.
		_ = execstate.Transition(exec, stage.Number, execstate.StatusInterrupted)
	}

	return nil
}

// Destination credential collection ─────────────────────────────────────────

func collectDestinationCredential(ctx context.Context, migrationName string, dest destination.Destination) (string, error) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Destination credential required\n", bold.Render("shiftctl execute"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  You haven't stored a %s credential yet.\n", dest.DisplayName())
	fmt.Printf("  This is requested now. only when execution actually needs it.\n\n")
	fmt.Printf("  %s\n\n", bold.Render("How to generate one:"))

	for _, line := range strings.Split(dest.CredentialInstructions(), "\n") {
		fmt.Printf("  %s\n", line)
	}
	fmt.Println()

	var cred string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(dest.CredentialPrompt()).
				EchoMode(huh.EchoModePassword).
				Value(&cred),
		),
	).Run(); err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\n  Credential entry cancelled. execution aborted.")
			return "", fmt.Errorf("destination credential not provided")
		}
		return "", err
	}

	cred = strings.TrimSpace(cred)
	if cred == "" {
		return "", fmt.Errorf("destination credential cannot be empty")
	}

	// Validate immediately.
	fmt.Printf("\n  Verifying %s credential…\n", dest.DisplayName())
	username, err := dest.ValidateCredential(ctx, cred)
	if err != nil {
		return "", fmt.Errorf("credential validation failed: %w\nPlease check the token and try again.", err)
	}
	fmt.Printf("  %s Authenticated as %s\n", check, bold.Render(username))

	// Store it for future runs.
	if err := credentials.Set(migrationName, "destination", cred); err != nil {
		fmt.Printf("  %s Could not store credential in keychain: %v\n", cross, err)
		fmt.Printf("    You'll be prompted again next time.\n")
	} else {
		fmt.Printf("  %s Credential stored in %s\n", check, credentials.CredManagerName())
	}

	fmt.Println()
	return cred, nil
}

// Display helpers ────────────────────────────────────────────────────────────

func printExecutionHeader(migrationName string, stage plan.Stage, totalStages int) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Executing Stage %d of %d. %s\n",
		bold.Render("shiftctl execute"),
		stage.Number,
		totalStages,
		bold.Render(stage.Label),
	)
	fmt.Println(rule)
	fmt.Printf("\n  %d %s to migrate\n\n", len(stage.Repos), repoNoun(len(stage.Repos)))
}

func makeProgressFn(stage *plan.Stage) func(executor.RepoResult, *executor.StageProgress) {
	total := len(stage.Repos)
	current := 0
	return func(result executor.RepoResult, _ *executor.StageProgress) {
		current++
		icon := check
		detail := ""
		if result.Status == executor.ItemFailed {
			icon = cross
			detail = fmt.Sprintf(". %s", result.Error)
		} else if result.DestWebURL != "" {
			detail = fmt.Sprintf(" → %s", result.DestWebURL)
		}
		fmt.Printf("  [%*d/%d] %s %s%s\n",
			len(fmt.Sprintf("%d", total)), current, total,
			icon,
			result.FullName,
			dim.Render(detail),
		)
	}
}

func printRecoveryReport(report executor.RecoveryReport) {
	if len(report.Confirmed) == 0 && len(report.Requeued) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │  Resuming interrupted execution                         │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()
	if len(report.Confirmed) > 0 {
		fmt.Printf("  %s Already on destination (%d). skipped:\n", check, len(report.Confirmed))
		for _, n := range report.Confirmed {
			fmt.Printf("    %s\n", dim.Render(n))
		}
		fmt.Println()
	}
	if len(report.Requeued) > 0 {
		fmt.Printf("  %s Not found on destination (%d). will re-run:\n", cross, len(report.Requeued))
		for _, n := range report.Requeued {
			fmt.Printf("    %s\n", dim.Render(n))
		}
		fmt.Println()
	}
}

func printExecutionSummary(stageNumber, done, failed, uncertain int, migrationName string) {
	fmt.Println()
	fmt.Println(rule)
	if failed == 0 && uncertain == 0 {
		fmt.Printf("  %s  Stage %d complete. %d %s migrated successfully\n",
			check, stageNumber, done, repoNoun(done))
		fmt.Println(rule)
		fmt.Printf("\n  Run %s to verify the migration.\n\n",
			bold.Render(fmt.Sprintf("shiftctl verify %s", migrationName)))
	} else {
		fmt.Printf("  Stage %d finished with issues\n", stageNumber)
		fmt.Println(rule)
		fmt.Printf("\n  Done: %d  Failed: %d  Uncertain: %d\n\n", done, failed, uncertain)
		if failed > 0 || uncertain > 0 {
			fmt.Printf("  Run %s to resume. completed items will not be repeated.\n\n",
				bold.Render(fmt.Sprintf("shiftctl execute %s", migrationName)))
		}
	}
}

func printFailedItems(prog *executor.StageProgress) {
	fmt.Println("  Failed items:")
	for _, item := range prog.Items {
		if item.Status == executor.ItemFailed {
			fmt.Printf("    %s %s\n", cross, item.FullName)
			if item.Error != "" {
				fmt.Printf("      %s\n", dim.Render(item.Error))
			}
		}
	}
	fmt.Println()
}

// Logic helpers ─────────────────────────────────────────────────────────────

// findApprovedOrExecutingStage finds a stage that is ready to execute (approved
// or already executing/interrupted = resumable).
func findApprovedOrExecutingStage(exec *execstate.State, p *plan.Plan) (*plan.Stage, *execstate.StageState, error) {
	for i := range exec.Stages {
		ss := &exec.Stages[i]
		switch ss.Status {
		case execstate.StatusApproved, execstate.StatusExecuting, execstate.StatusInterrupted:
			stage, ok := findPlanStage(p, ss.Number)
			if !ok {
				return nil, nil, fmt.Errorf("stage %d not found in plan", ss.Number)
			}
			return &stage, ss, nil
		}
	}
	return nil, nil, nil
}

func noApprovedStageMessage(exec *execstate.State, name string) error {
	// Check if all stages are done.
	allVerified := true
	for _, s := range exec.Stages {
		if s.Status != execstate.StatusVerified {
			allVerified = false
			break
		}
	}
	if allVerified {
		fmt.Printf("\n  %s  All stages have been executed and verified.\n", check)
		fmt.Printf("  Run 'shiftctl decommission %s' to complete the migration.\n\n", name)
		return nil
	}

	// Check for stages waiting on approval or verification.
	for _, s := range exec.Stages {
		if s.Status == execstate.StatusPending {
			fmt.Printf("\n  Stage %d is pending approval.\n", s.Number)
			fmt.Printf("  Run 'shiftctl approve %s' first.\n\n", name)
			return nil
		}
		if s.Status == execstate.StatusVerifying {
			fmt.Printf("\n  Stage %d is awaiting verification.\n", s.Number)
			fmt.Printf("  Run 'shiftctl verify %s' to complete it before executing the next stage.\n\n", name)
			return nil
		}
	}

	fmt.Printf("\n  Nothing to execute for migration %q.\n\n", name)
	return nil
}
