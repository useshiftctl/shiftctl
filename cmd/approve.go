package cmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

var approveCmd = &cobra.Command{
	Use:   "approve <migration-name>",
	Short: "Approve the next stage for execution",
	Long: `Review the repositories in the next pending stage and explicitly approve them
for execution. Nothing is executed until you approve.

The approval loop repeats for each stage:
  Approve Stage N → execute → verify → Approve Stage N+1 → ...

Each approval generates a unique ID that is recorded in the migration log and
referenced by every action executed in that stage. every action is traceable
back to the specific approval that authorised it.

Certain stages trigger a mandatory extra-scrutiny notice before the approval
prompt. when the stage involves a high-risk system, or when the agent's
confidence on any item falls below the expected threshold.`,
	Args: cobra.ExactArgs(1),
	RunE: runApprove,
}

func init() {
	rootCmd.AddCommand(approveCmd)
}

func runApprove(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	// ── Load plan ─────────────────────────────────────────────────────────────
	p, err := plan.Load(name)
	if err != nil {
		return err
	}

	// ── Load or initialise execution state ───────────────────────────────────
	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		return fmt.Errorf("loading execution state: %w", err)
	}

	// ── Find next stage to approve ────────────────────────────────────────────
	pending, err := execstate.NextPendingStage(exec)
	if err != nil {
		return err
	}
	if pending == nil {
		return allStagesApprovedMessage(exec)
	}

	// Retrieve the full stage detail from the plan.
	stage, ok := findPlanStage(p, pending.Number)
	if !ok {
		return fmt.Errorf("stage %d not found in plan. try regenerating with 'shiftctl plan %s'", pending.Number, name)
	}

	// ── Print stage header ────────────────────────────────────────────────────
	printApprovalHeader(p.MigrationName, stage, len(p.Stages))

	// ── Detect and print escalation triggers ─────────────────────────────────
	triggers := detectEscalation(stage)
	if len(triggers) > 0 {
		printEscalationNotice(triggers)
	}

	// ── Print repo list with reasoning ───────────────────────────────────────
	printApprovalRepoList(stage)

	// ── Print what approving does + rollback notice ───────────────────────────
	printApprovalContext(stage)

	// ── Confirmation prompt ───────────────────────────────────────────────────
	label := fmt.Sprintf("Approve Stage %d? (%d %s. execution begins immediately after)",
		stage.Number,
		len(stage.Repos),
		repoNoun(len(stage.Repos)),
	)

	var confirmed bool
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(label).
				Affirmative(fmt.Sprintf("Yes, approve Stage %d", stage.Number)).
				Negative("No, not yet").
				Value(&confirmed),
		),
	).Run(); err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\nApproval cancelled. nothing has changed.")
			return nil
		}
		return err
	}
	if !confirmed {
		fmt.Println("\nApproval cancelled. nothing has changed.")
		return nil
	}

	// ── Record approval ───────────────────────────────────────────────────────
	items := makeApprovedItems(stage)
	rec, err := execstate.RecordApproval(exec, stage.Number, items)
	if err != nil {
		return fmt.Errorf("recording approval: %w", err)
	}

	// ── Success ───────────────────────────────────────────────────────────────
	printApprovalSuccess(rec, name)
	return nil
}

// Display helpers ───────────────────────────────────────────────────────────

func printApprovalHeader(migrationName string, stage plan.Stage, totalStages int) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Approve Stage %d of %d. %s\n",
		bold.Render("shiftctl approve"),
		stage.Number,
		totalStages,
		bold.Render(stage.Label),
	)
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  %s\n", stage.Reason)
	fmt.Println()
}

func printEscalationNotice(triggers []execstate.EscalationTrigger) {
	fmt.Println("  ┌─────────────────────────────────────────────────────────┐")
	fmt.Println("  │  Heads Up. this stage needs a closer look              │")
	fmt.Println("  └─────────────────────────────────────────────────────────┘")
	fmt.Println()
	for _, t := range triggers {
		if t.Repo != "" {
			fmt.Printf("  [%s]\n", bold.Render(t.Repo))
		}
		fmt.Printf("  %s\n\n", t.Reason)
	}
}

func printApprovalRepoList(stage plan.Stage) {
	const nameWidth = 42
	const riskWidth = 8
	const confWidth = 12

	noun := repoNoun(len(stage.Repos))
	fmt.Printf("  %d %s in this stage:\n\n", len(stage.Repos), noun)

	fmt.Printf("  %-*s  %-*s  %s\n",
		nameWidth, "NAME",
		riskWidth, "RISK",
		"CONFIDENCE",
	)
	fmt.Printf("  %s\n", strings.Repeat("─", nameWidth+riskWidth+confWidth+4))

	for _, repo := range stage.Repos {
		fmt.Printf("  %-*s  %-*s  %s\n",
			nameWidth, truncateName(repo.FullName, nameWidth),
			riskWidth, riskLabel(repo.RiskLevel),
			confidenceLabel(repo.Confidence),
		)

		// Always show evidence on the approval screen. this is the highest-
		// trust moment and the user should see exactly what the assessment found.
		if repo.ScanError != "" {
			fmt.Printf("  %s Heads Up: %s\n",
				dim.Render("  └"),
				dim.Render(repo.ScanError),
			)
		} else if repo.Evidence != "" {
			fmt.Printf("  %s %s\n",
				dim.Render("  └"),
				dim.Render(truncateName(repo.Evidence, 90)),
			)
		}
		if repo.Reasoning != "" {
			fmt.Printf("  %s %s\n",
				dim.Render("    "),
				dim.Render(truncateName(repo.Reasoning, 90)),
			)
		}
		if len(repo.DependsOn) > 0 {
			shorts := make([]string, len(repo.DependsOn))
			for i, d := range repo.DependsOn {
				parts := strings.SplitN(d, "/", 2)
				if len(parts) == 2 {
					shorts[i] = parts[1]
				} else {
					shorts[i] = d
				}
			}
			fmt.Printf("  %s Depends on: %s\n",
				dim.Render("    "),
				dim.Render(strings.Join(shorts, ", ")),
			)
		}
		if repo.UserNote != "" {
			fmt.Printf("  %s Your note: %s\n",
				dim.Render("    "),
				repo.UserNote,
			)
		}
		fmt.Println()
	}
}

func printApprovalContext(stage plan.Stage) {
	fmt.Printf("  %s\n", bold.Render("What approving does:"))
	fmt.Printf("    - Authorises shiftctl to migrate exactly these %d %s, nothing else\n",
		len(stage.Repos), repoNoun(len(stage.Repos)))
	fmt.Println("    - Does not touch any repository not listed above")
	fmt.Println("    - Execution can be paused at any point; completed items cannot be undone")
	fmt.Println("    - A verification step runs after execution before the next stage begins")
	fmt.Println()
	fmt.Printf("  %s\n", bold.Render("If execution is interrupted:"))
	fmt.Println("    - State is saved after every completed item")
	fmt.Println("    - Recovery shows exactly what completed, what's uncertain, and what didn't start")
	fmt.Println("    - Uncertain items are re-verified against the live source before resuming")
	fmt.Println()
}

func printApprovalSuccess(rec execstate.ApprovalRecord, migrationName string) {
	fmt.Println()
	fmt.Printf("  %s  Stage %d approved.\n", check, rec.StageNumber)
	fmt.Printf("      Approval ID:  %s\n", bold.Render(rec.ID))
	fmt.Printf("      Approved at:  %s\n", rec.ApprovedAt.Format("2006-01-02 15:04:05 UTC"))
	fmt.Printf("      In scope:     %d %s\n\n", rec.ItemCount, repoNoun(rec.ItemCount))
	fmt.Printf("  Run %s to begin execution.\n",
		bold.Render(fmt.Sprintf("shiftctl execute %s", migrationName)))
	fmt.Println()
}

func allStagesApprovedMessage(exec *execstate.State) error {
	// Check if any stage is in a non-pending, non-verified state.
	for _, s := range exec.Stages {
		switch s.Status {
		case execstate.StatusApproved:
			fmt.Printf("\n  Stage %d is approved and waiting for execution.\n", s.Number)
			fmt.Printf("  Run 'shiftctl execute %s' to begin.\n\n", exec.MigrationName)
			return nil
		case execstate.StatusExecuting:
			fmt.Printf("\n  Stage %d is currently executing.\n\n", s.Number)
			return nil
		case execstate.StatusInterrupted:
			fmt.Printf("\n  Stage %d was interrupted.\n", s.Number)
			fmt.Printf("  Run 'shiftctl execute %s' to resume from where it stopped.\n\n", exec.MigrationName)
			return nil
		case execstate.StatusVerifying:
			fmt.Printf("\n  Stage %d is awaiting verification.\n", s.Number)
			fmt.Printf("  Run 'shiftctl verify %s' to complete it.\n\n", exec.MigrationName)
			return nil
		}
	}

	// All stages are verified.
	fmt.Printf("\n  %s  All stages have been approved, executed, and verified.\n", check)
	fmt.Printf("  Run 'shiftctl decommission %s' to complete the migration.\n\n", exec.MigrationName)
	return nil
}

// Logic helpers ─────────────────────────────────────────────────────────────

// detectEscalation returns the list of escalation triggers for a stage. Three
// conditions trigger proactive escalation (beyond routine per-stage approval):
//
//  1. The stage contains a high-risk item. "a stage involving a
//     high-criticality system" trigger.
//  2. The stage contains a best_guess item with non-low risk. "agent's own
//     confidence falling below a threshold" trigger.
//  3. A repo has a scan error. partial information, uncertain scope trigger.
func detectEscalation(stage plan.Stage) []execstate.EscalationTrigger {
	var triggers []execstate.EscalationTrigger

	for _, repo := range stage.Repos {
		if repo.RiskLevel == "high" {
			triggers = append(triggers, execstate.EscalationTrigger{
				Repo: repo.FullName,
				Reason: fmt.Sprintf(
					"Marked High risk: %s\n  Verify this is the intended system before approving.",
					repo.Evidence,
				),
			})
		} else if repo.Confidence == "best_guess" && repo.RiskLevel == "medium" {
			triggers = append(triggers, execstate.EscalationTrigger{
				Repo: repo.FullName,
				Reason: fmt.Sprintf(
					"Medium risk with Best guess confidence. shiftctl inferred this rather than finding direct evidence.\n  Evidence: %s",
					repo.Evidence,
				),
			})
		} else if repo.ScanError != "" {
			triggers = append(triggers, execstate.EscalationTrigger{
				Repo:   repo.FullName,
				Reason: fmt.Sprintf("Scan was incomplete for this repo: %s", repo.ScanError),
			})
		}
	}

	return triggers
}

func makeApprovedItems(stage plan.Stage) []execstate.ApprovedItem {
	items := make([]execstate.ApprovedItem, len(stage.Repos))
	for i, r := range stage.Repos {
		items[i] = execstate.ApprovedItem{
			FullName:   r.FullName,
			RiskLevel:  r.RiskLevel,
			Confidence: r.Confidence,
		}
	}
	return items
}

func findPlanStage(p *plan.Plan, number int) (plan.Stage, bool) {
	for _, s := range p.Stages {
		if s.Number == number {
			return s, true
		}
	}
	return plan.Stage{}, false
}

func repoNoun(n int) string {
	if n == 1 {
		return "repository"
	}
	return "repositories"
}
