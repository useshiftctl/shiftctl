package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/credentials"
	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

var decommissionYes bool

var decommissionCmd = &cobra.Command{
	Use:   "decommission <migration-name>",
	Short: "Complete a fully verified migration",
	Long: `Complete a migration after all stages have been executed and verified.

Decommission:
  - Removes the source credential from the OS keychain
  - Removes the destination credential from the OS keychain
  - Removes all local migration files (plan, assessment, execution state,
    progress files) for this migration

The migration record itself is kept with status "complete" so the migration
name cannot be accidentally reused.

This command requires all stages to be in the "verified" state. It will not
run if any stage is still pending, executing, or verifying.`,
	Args: cobra.ExactArgs(1),
	RunE: runDecommission,
}

func init() {
	rootCmd.AddCommand(decommissionCmd)
	decommissionCmd.Flags().BoolVarP(&decommissionYes, "yes", "y", false, "Skip the confirmation prompt")
}

func runDecommission(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	// ── Load migration ────────────────────────────────────────────────────────
	rec, err := migration.Load(name)
	if err != nil {
		return fmt.Errorf("migration %q not found", name)
	}

	// ── Load execstate ────────────────────────────────────────────────────────
	p, err := plan.Load(name)
	if err != nil {
		return fmt.Errorf("no plan found for %q , cannot verify completion status", name)
	}

	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		return fmt.Errorf("loading execution state: %w", err)
	}

	// ── Guard: all stages must be verified ────────────────────────────────────
	if err := requireAllVerified(exec); err != nil {
		return err
	}

	// ── Print what will happen ────────────────────────────────────────────────
	printDecommissionPreview(rec, exec)

	// ── Confirmation prompt ───────────────────────────────────────────────────
	if !decommissionYes {
		var confirmed bool
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title(fmt.Sprintf("Complete migration %q and remove credentials?", name)).
					Affirmative("Yes, complete and clean up").
					Negative("No, not yet").
					Value(&confirmed),
			),
		).Run(); err != nil {
			if err == huh.ErrUserAborted {
				fmt.Println("\n  Decommission cancelled , nothing changed.")
				return nil
			}
			return err
		}
		if !confirmed {
			fmt.Println("\n  Decommission cancelled , nothing changed.")
			return nil
		}
	}

	// ── Remove credentials ────────────────────────────────────────────────────
	var credErrors []string
	if err := credentials.Delete(name, "source"); err != nil {
		credErrors = append(credErrors, fmt.Sprintf("source credential: %v", err))
	}
	if err := credentials.Delete(name, "destination"); err != nil {
		// Destination credential may never have been stored if execution never ran.
		// Don't treat this as an error.
		_ = err
	}

	// ── Remove migration data files (preserve record.json) ────────────────────
	if err := removeMigrationData(name); err != nil {
		credErrors = append(credErrors, fmt.Sprintf("removing data files: %v", err))
	}

	// ── Mark migration complete ───────────────────────────────────────────────
	rec.Status = migration.StatusComplete
	if err := migration.Save(rec); err != nil {
		credErrors = append(credErrors, fmt.Sprintf("marking complete: %v", err))
	}

	// ── Print result ──────────────────────────────────────────────────────────
	printDecommissionResult(name, exec, credErrors)
	return nil
}

// , Display helpers ────────────────────────────────────────────────────────────

func printDecommissionPreview(rec *migration.Record, exec *execstate.State) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Complete migration , %s\n",
		bold.Render("shiftctl decommission"),
		bold.Render(rec.Name),
	)
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  Source:       %s\n", rec.SourcePlatform)
	fmt.Printf("  Destination:  %s\n", rec.DestinationName)
	fmt.Printf("  Stages:       %d (all verified)\n", len(exec.Stages))
	total := 0
	for _, s := range exec.Stages {
		if len(exec.Approvals) > 0 {
			for _, a := range exec.Approvals {
				if a.StageNumber == s.Number {
					total += a.ItemCount
				}
			}
		}
	}
	if total > 0 {
		fmt.Printf("  Repositories: %d migrated\n", total)
	}
	fmt.Println()
	fmt.Println("  This will:")
	fmt.Println("    - Remove source and destination credentials from the OS keychain")
	fmt.Println("    - Delete all local migration files (plan, assessment, progress)")
	fmt.Println("    - Mark the migration as complete (name cannot be reused)")
	fmt.Println()
}

func printDecommissionResult(name string, exec *execstate.State, errs []string) {
	fmt.Println()
	if len(errs) == 0 {
		fmt.Printf("  %s  Migration %q is complete.\n", check, name)
		fmt.Println()
		fmt.Println("  What happened:")
		fmt.Println("    - Source credential removed from OS keychain")
		fmt.Println("    - Destination credential removed from OS keychain")
		fmt.Println("    - Local migration files removed")
		fmt.Printf("    - %d stage(s) of repositories are now on the destination\n", len(exec.Stages))
	} else {
		fmt.Printf("  %s  Migration %q marked complete with warnings:\n", check, name)
		for _, e := range errs {
			fmt.Printf("    %s %s\n", cross, e)
		}
	}
	fmt.Println()
}

// , Logic helpers ─────────────────────────────────────────────────────────────

func requireAllVerified(exec *execstate.State) error {
	for _, s := range exec.Stages {
		if s.Status != execstate.StatusVerified {
			return fmt.Errorf(
				"Stage %d is not yet verified (status: %s)\n"+
					"All stages must be verified before decommissioning.\n"+
					"Run 'shiftctl verify' to complete pending stages.",
				s.Number, s.Status,
			)
		}
	}
	return nil
}

// removeMigrationData deletes the data files in the migration directory but
// leaves the record.json so the name stays reserved.
func removeMigrationData(name string) error {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "migrations", migration.SanitizeName(name))

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Remove everything except record.json.
	for _, entry := range entries {
		if entry.Name() == "record.json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("removing %s: %w", entry.Name(), err)
		}
	}
	return nil
}
