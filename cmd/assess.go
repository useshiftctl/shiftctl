package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/shiftctl/shiftctl/internal/assessment"
	"github.com/shiftctl/shiftctl/internal/credentials"
	"github.com/shiftctl/shiftctl/internal/migration"
)

var assessYes bool

var assessCmd = &cobra.Command{
	Use:   "assess <migration-name>",
	Short: "Scan source repositories and assess migration risk",
	Long: `Scan all repositories accessible to the source credential and produce a
risk-annotated inventory of what exists, how items relate to each other, and
what should be migrated in which order.

What this command does:
  1. Lists every repository your token can access.
  2. Reads structured metadata (CI files, webhooks, branch protection, dependencies).
  3. Strips secrets from any string field before anything leaves your machine.
  4. Sends structured facts — no raw file contents — to Anthropic for analysis.
  5. Saves the full assessment to ~/.shiftctl/migrations/<name>/assessment.json.

Nothing is sent to Anthropic without showing you exactly what first.`,
	Args:    cobra.ExactArgs(1),
	RunE:    runAssess,
}

func init() {
	rootCmd.AddCommand(assessCmd)
	assessCmd.Flags().BoolVarP(&assessYes, "yes", "y", false, "Skip the confirmation prompt and proceed immediately")
}

func runAssess(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	// ── Load migration record ─────────────────────────────────────────────────
	rec, err := migration.Load(name)
	if err != nil {
		return err
	}

	// ── Check ANTHROPIC_API_KEY ───────────────────────────────────────────────
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, `
shiftctl assess requires an Anthropic API key.

Set it in your current session:
  export ANTHROPIC_API_KEY=sk-ant-...

Or add it to your shell profile (~/.zshrc or ~/.bashrc) to persist it:
  echo 'export ANTHROPIC_API_KEY=sk-ant-...' >> ~/.zshrc

Get an API key at: https://console.anthropic.com/`)
		return fmt.Errorf("ANTHROPIC_API_KEY is not set")
	}

	// ── Load source credential ────────────────────────────────────────────────
	token, err := credentials.Get(name, "source")
	if err != nil {
		return fmt.Errorf("could not load source credential: %w\n\nRun 'shiftctl connect' to set up the migration first.", err)
	}

	// ── "Confirm What's Shared" ───────────────────────────────────────────────
	printConfirmWhatShared(rec, apiKey)

	if !assessYes {
		var proceed bool
		if err := huh.NewForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Proceed with assessment?").
					Affirmative("Yes, start scanning").
					Negative("No, cancel").
					Value(&proceed),
			),
		).Run(); err != nil {
			if err == huh.ErrUserAborted {
				fmt.Println("\nAssessment cancelled.")
				return nil
			}
			return err
		}
		if !proceed {
			fmt.Println("\nAssessment cancelled.")
			return nil
		}
	}

	fmt.Println()

	// ── Scan ──────────────────────────────────────────────────────────────────
	var totalRepos int
	progress := func(current, total int, fullName string) {
		totalRepos = total
		// Pad the counter so columns align for up to 999 repos.
		fmt.Printf("  [%3d/%d] %-45s %s\n",
			current, total,
			truncateName(fullName, 45),
			check,
		)
	}

	fmt.Printf("  Scanning repositories accessible to %s...\n\n",
		platformDisplayName(rec.SourcePlatform))

	ctx := context.Background()
	state, err := assessment.Run(ctx, rec, token, apiKey, progress)
	if err != nil {
		fmt.Printf("\n  %s  Assessment failed: %v\n", cross, err)
		return err
	}

	// ── Summary ───────────────────────────────────────────────────────────────
	printAssessmentSummary(state, totalRepos)
	return nil
}

func printConfirmWhatShared(rec *migration.Record, apiKey string) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Println(bold.Render("  Before scanning — what shiftctl sends and doesn't send"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Println("  Stays on your machine (never sent anywhere):")
	fmt.Println("    - Raw file contents from your repositories")
	fmt.Println("    - Credentials, tokens, and secrets (stripped before any analysis)")
	fmt.Println("    - Your shiftctl configuration and stored credentials")
	fmt.Println()
	fmt.Println("  Sent to Anthropic's API for analysis:")
	fmt.Println("    - Repository names, sizes, primary languages, and topics")
	fmt.Println("    - Names of CI/CD config files (not their contents)")
	fmt.Println("    - Parsed dependency names (not versions or full manifests)")
	fmt.Println("    - Webhook destination domains (any credentials in URLs are stripped)")
	fmt.Println("    - Branch protection settings")
	fmt.Println("    - Deploy key titles and read/write status")
	fmt.Println()
	fmt.Printf("  Migration:   %s  (%s → %s)\n",
		rec.Name, platformDisplayName(rec.SourcePlatform), rec.DestinationName)
	keyPreview := apiKey
	if len(keyPreview) > 12 {
		keyPreview = keyPreview[:8] + "..." + keyPreview[len(keyPreview)-4:]
	}
	fmt.Printf("  API key:     %s\n", dim.Render(keyPreview))
	fmt.Println()
}

func printAssessmentSummary(state *assessment.State, totalRepos int) {
	low, medium, high, unknowns := assessment.RiskSummary(state)

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Assessment complete — %d repositories scanned\n\n", check, totalRepos)

	fmt.Println("  Risk summary:")
	if low > 0 {
		fmt.Printf("    %s  Low risk (Confirmed):    %d\n", check, low)
	}
	if medium > 0 {
		fmt.Printf("    ⚠   Medium risk:            %d\n", medium)
	}
	if high > 0 {
		fmt.Printf("    ✗   High risk:              %d\n", high)
	}
	if unknowns > 0 {
		fmt.Printf("    ℹ   Heads Up (review):      %d\n", unknowns)
	}

	if state.Analysis != nil && len(state.Analysis.KnownUnknowns) > 0 {
		fmt.Println()
		fmt.Println("  Items to review before planning:")
		for _, ku := range state.Analysis.KnownUnknowns {
			scope := ku.Scope
			if scope == "" {
				scope = "all"
			}
			fmt.Printf("    - [%s] %s\n", dim.Render(scope), ku.Description)
		}
	}

	fmt.Println()
	fmt.Println(dim.Render("  Full results saved to:"))
	fmt.Println(dim.Render("    ~/.shiftctl/migrations/" + state.MigrationName + "/assessment.json"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  Run %s to review the plan.\n",
		bold.Render("shiftctl plan "+state.MigrationName))
	fmt.Println()
}

func truncateName(s string, max int) string {
	if len(s) <= max {
		return s + strings.Repeat(" ", max-len(s))
	}
	return s[:max-1] + "…"
}
