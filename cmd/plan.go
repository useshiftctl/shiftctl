package cmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/shiftctl/shiftctl/internal/assessment"
	"github.com/shiftctl/shiftctl/internal/execstate"
	"github.com/shiftctl/shiftctl/internal/migration"
	"github.com/shiftctl/shiftctl/internal/plan"
)

var planCmd = &cobra.Command{
	Use:   "plan <migration-name>",
	Short: "Generate a staged migration plan from the assessment",
	Long: `Read the completed assessment and group repositories into three stages:

  Stage 1 — Confirmed, low risk. Moved first.
  Stage 2 — Best guess or uncertain. Held until Stage 1 succeeds.
  Stage 3 — Confirmed high risk. Moved last, when confidence is highest.

Within each stage, repositories that depend on other repositories in the same
stage are ordered so dependencies come first.

Running this command always regenerates the plan from the latest assessment.
The plan is saved to ~/.shiftctl/migrations/<name>/plan.json.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlan,
}

func init() {
	rootCmd.AddCommand(planCmd)
}

func runPlan(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	// ── Guard: migration must exist ───────────────────────────────────────────
	if _, err := migration.Load(name); err != nil {
		return err
	}

	// ── Guard: assessment with analysis must exist ────────────────────────────
	state, err := assessment.Load(name)
	if err != nil {
		return err
	}
	if state.Analysis == nil {
		return fmt.Errorf(
			"assessment exists but has no analysis for %q\n"+
				"This can happen if 'shiftctl assess' was interrupted after scanning but before the AI step.\n"+
				"Run 'shiftctl assess %s' again to complete it.", name, name)
	}

	// ── Warn if plan already exists and execution has started ────────────────
	if existing, err := plan.Load(name); err == nil {
		exec, execErr := execstate.LoadOrInit(name, existing)
		if execErr == nil {
			for _, s := range exec.Stages {
				if s.Status != execstate.StatusPending {
					fmt.Printf("\n  %s  Warning: execution for this migration has already started.\n", cross)
					fmt.Println("  Regenerating the plan will reset stage statuses if the stage count changes.")
					fmt.Println("  Completed work is not lost, but you may need to re-approve.")
					fmt.Println()

					var proceed bool
					if err := huh.NewForm(
						huh.NewGroup(
							huh.NewConfirm().
								Title("Regenerate the plan anyway?").
								Affirmative("Yes, regenerate").
								Negative("No, keep existing plan").
								Value(&proceed),
						),
					).Run(); err != nil || !proceed {
						fmt.Println("\n  Plan regeneration cancelled.")
						return nil
					}
					break
				}
			}
		}
	}

	// ── Build plan ────────────────────────────────────────────────────────────
	p, err := plan.Build(state)
	if err != nil {
		return err
	}

	// ── Save plan ─────────────────────────────────────────────────────────────
	if err := plan.Save(p); err != nil {
		return fmt.Errorf("saving plan: %w", err)
	}

	// ── Display ───────────────────────────────────────────────────────────────
	printPlan(p)
	return nil
}

func printPlan(p *plan.Plan) {
	totalRepos := 0
	for _, s := range p.Stages {
		totalRepos += len(s.Repos)
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Migration plan — %s\n", bold.Render("shiftctl"), p.MigrationName)
	fmt.Printf("  %d repositories across %d stage(s)\n",
		totalRepos, len(p.Stages))
	fmt.Println(rule)

	for _, stage := range p.Stages {
		printStage(stage)
	}

	if len(p.KnownUnknowns) > 0 {
		printKnownUnknowns(p.KnownUnknowns)
	}

	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  Plan saved. Next: %s\n",
		bold.Render("shiftctl approve "+p.MigrationName))
	fmt.Println()
}

func printStage(s plan.Stage) {
	noun := "repo"
	if len(s.Repos) != 1 {
		noun = "repos"
	}

	fmt.Println()
	fmt.Printf("  %s  Stage %d — %s\n",
		stageIcon(s.Number, len(s.Repos)),
		s.Number,
		bold.Render(s.Label),
	)
	fmt.Printf("  %d %s\n", len(s.Repos), noun)
	fmt.Printf("  %s\n", dim.Render(s.Reason))
	fmt.Println()

	// Column widths.
	const nameWidth = 42
	const riskWidth = 8
	const confWidth = 12

	// Header.
	fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
		nameWidth, "NAME",
		riskWidth, "RISK",
		confWidth, "CONFIDENCE",
		"DEPENDS ON (within migration)",
	)
	fmt.Printf("  %s\n", strings.Repeat("─", nameWidth+riskWidth+confWidth+36))

	for _, repo := range s.Repos {
		// Main row.
		deps := "—"
		if len(repo.DependsOn) > 0 {
			// Show short names (after the slash) to keep the line readable.
			shorts := make([]string, len(repo.DependsOn))
			for i, d := range repo.DependsOn {
				parts := strings.SplitN(d, "/", 2)
				if len(parts) == 2 {
					shorts[i] = parts[1]
				} else {
					shorts[i] = d
				}
			}
			deps = strings.Join(shorts, ", ")
		}

		confLabel := confidenceLabel(repo.Confidence)

		fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
			nameWidth, truncateName(repo.FullName, nameWidth),
			riskWidth, riskLabel(repo.RiskLevel),
			confWidth, confLabel,
			dim.Render(truncateName(deps, 40)),
		)

		// Evidence / scan error on a second line, indented.
		if repo.ScanError != "" {
			fmt.Printf("  %s Heads Up: %s\n",
				dim.Render("  └"),
				dim.Render(repo.ScanError),
			)
		} else if repo.Evidence != "" && repo.Confidence == "best_guess" {
			// Only show evidence inline for best_guess items — for confirmed
			// items the user can inspect the full assessment file. Showing
			// evidence for every confirmed item makes Stage 1 lists noisy.
			fmt.Printf("  %s %s\n",
				dim.Render("  └"),
				dim.Render(truncateName(repo.Evidence, 80)),
			)
		}
	}

	if s.Note != "" {
		fmt.Println()
		fmt.Printf("  Note: %s\n", s.Note)
	}
}

func printKnownUnknowns(kus []plan.KnownUnknown) {
	fmt.Println()
	fmt.Printf("  %s  Heads Up — %d item(s) to review before approving\n",
		bold.Render("ℹ"), len(kus))
	fmt.Printf("  %s\n",
		dim.Render("These were detected but couldn't be fully assessed from facts alone."))
	fmt.Printf("  %s\n",
		dim.Render("Review them manually — they don't block the plan but may affect your approach."))
	fmt.Println()
	for _, ku := range kus {
		scope := ku.Scope
		if scope == "" || scope == "all" {
			scope = "all repos"
		}
		fmt.Printf("  [%s]  %s\n", dim.Render(scope), ku.Description)
	}
}

// stageIcon returns a visual marker for the stage heading based on which
// stage number it is in the final sequence (not the bucket number).
func stageIcon(number, count int) string {
	_ = count
	switch number {
	case 1:
		return check
	case 2:
		return "⚠ "
	default:
		return cross
	}
}

func riskLabel(level string) string {
	switch level {
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	default:
		return level
	}
}

func confidenceLabel(c string) string {
	switch c {
	case "confirmed":
		return "Confirmed"
	case "best_guess":
		return "Best guess"
	default:
		return c
	}
}

