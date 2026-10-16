package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all migrations",
	Long:  `List every migration stored on this machine with its current status.`,
	Args:  cobra.NoArgs,
	RunE:  runList,
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func runList(_ *cobra.Command, _ []string) error {
	names, err := migration.ListAll()
	if err != nil {
		return fmt.Errorf("reading migrations: %w", err)
	}

	if len(names) == 0 {
		fmt.Println()
		fmt.Println("  No migrations found.")
		fmt.Printf("  Run %s to create one.\n\n", bold.Render("shiftctl connect"))
		return nil
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Migrations\n", bold.Render("shiftctl list"))
	fmt.Println(rule)
	fmt.Println()

	const nameW = 32
	const srcW = 10
	const dstW = 14
	const stageW = 22

	fmt.Printf("  %-*s  %-*s  %-*s  %s\n", nameW, "NAME", srcW, "SOURCE", dstW, "DESTINATION", "STATUS")
	fmt.Printf("  %s\n", fmt.Sprintf("%s  %s  %s  %s",
		repeatN("─", nameW),
		repeatN("─", srcW),
		repeatN("─", dstW),
		repeatN("─", stageW),
	))

	for _, name := range names {
		rec, err := migration.Load(name)
		if err != nil {
			fmt.Printf("  %-*s  %s\n", nameW, truncateName(name, nameW), dim.Render("(error reading record)"))
			continue
		}

		statusStr := migrationStatusLine(rec.Name, rec.Status)

		fmt.Printf("  %-*s  %-*s  %-*s  %s\n",
			nameW, truncateName(rec.Name, nameW),
			srcW, truncateName(platformDisplayName(rec.SourcePlatform), srcW),
			dstW, truncateName(rec.DestinationName, dstW),
			statusStr,
		)
	}

	fmt.Println()
	fmt.Printf("  %s  Run %s for details on any migration.\n\n",
		dim.Render("tip:"),
		bold.Render("shiftctl status <name>"),
	)

	return nil
}

// migrationStatusLine produces a concise status string for the list view.
// For migrations with an execstate, it reflects the active stage status.
func migrationStatusLine(name string, recStatus migration.Status) string {
	if recStatus == migration.StatusComplete {
		return check + " complete"
	}

	// Try to load plan + execstate for a richer status.
	p, err := plan.Load(name)
	if err != nil {
		// No plan yet — show the record status.
		return dim.Render(string(recStatus))
	}

	exec, err := execstate.LoadOrInit(name, p)
	if err != nil {
		return dim.Render(string(recStatus))
	}

	// Find the most progressed stage.
	for i := len(exec.Stages) - 1; i >= 0; i-- {
		s := exec.Stages[i]
		switch s.Status {
		case execstate.StatusVerified:
			// Keep going — maybe all are verified.
			continue
		case execstate.StatusVerifying:
			return fmt.Sprintf("stage %d — verifying", s.Number)
		case execstate.StatusInterrupted:
			return cross + fmt.Sprintf(" stage %d — interrupted", s.Number)
		case execstate.StatusExecuting:
			return fmt.Sprintf("stage %d — executing", s.Number)
		case execstate.StatusApproved:
			return fmt.Sprintf("stage %d — approved, ready to execute", s.Number)
		case execstate.StatusPending:
			// Pending means nothing is executing — fall through to show what's waiting.
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
	if allVerified && len(exec.Stages) > 0 {
		return check + " all stages verified"
	}

	// Nothing active — find first pending stage.
	for _, s := range exec.Stages {
		if s.Status == execstate.StatusPending {
			return fmt.Sprintf("stage %d — pending approval", s.Number)
		}
	}

	return dim.Render(string(recStatus))
}

func repeatN(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
