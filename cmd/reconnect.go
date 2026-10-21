package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/credentials"
	"github.com/useshiftctl/shiftctl/internal/destination"
	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/platform"
)

var reconnectCmd = &cobra.Command{
	Use:   "reconnect <migration-name>",
	Short: "Update a stored credential for an existing migration",
	Long: `Update the source or destination credential for an existing migration.

Use this when a token has expired, been rotated, or needs to be replaced
without restarting the migration from scratch.

The new credential is validated before being stored , if validation fails,
the existing credential is left untouched.`,
	Args: cobra.ExactArgs(1),
	RunE: runReconnect,
}

func init() {
	rootCmd.AddCommand(reconnectCmd)
}

func runReconnect(_ *cobra.Command, args []string) error {
	ctx := context.Background()
	name := migration.SanitizeName(args[0])

	rec, err := migration.Load(name)
	if err != nil {
		return fmt.Errorf("migration %q not found", name)
	}

	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Update credential , %s\n", bold.Render("shiftctl reconnect"), bold.Render(name))
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  Source:       %s\n", platformDisplayName(rec.SourcePlatform))
	fmt.Printf("  Destination:  %s\n", rec.DestinationName)
	fmt.Println()

	// ── Choose which credential to update ────────────────────────────────────
	var role string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Which credential do you want to update?").
				Options(
					huh.NewOption("Source , "+platformDisplayName(rec.SourcePlatform), "source"),
					huh.NewOption("Destination , "+rec.DestinationName, "destination"),
				).
				Value(&role),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	switch role {
	case "source":
		return updateSourceCredential(ctx, rec)
	case "destination":
		return updateDestinationCredential(ctx, rec)
	}
	return nil
}

func updateSourceCredential(ctx context.Context, rec *migration.Record) error {
	p := platform.ByID(rec.SourcePlatform)
	if p == nil {
		return fmt.Errorf("unknown source platform %q", rec.SourcePlatform)
	}

	fmt.Println()
	fmt.Printf("  %s\n\n", bold.Render("How to generate a new "+p.DisplayName()+" credential:"))
	for _, line := range strings.Split(p.CredentialInstructions(), "\n") {
		fmt.Printf("  %s\n", line)
	}
	fmt.Println()

	var cred string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(p.CredentialPrompt()).
				EchoMode(huh.EchoModePassword).
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("credential cannot be empty")
					}
					return nil
				}).
				Value(&cred),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	cred = strings.TrimSpace(cred)

	fmt.Printf("\n  Validating %s credential… ", p.DisplayName())
	username, err := p.ValidateCredential(ctx, cred)
	if err != nil {
		fmt.Println(cross)
		return fmt.Errorf("validation failed: %w\nExisting credential was not changed.", err)
	}
	fmt.Printf("%s  Authenticated as %s\n", check, bold.Render(username))

	if err := credentials.Set(rec.Name, "source", cred); err != nil {
		return fmt.Errorf("storing credential: %w", err)
	}

	fmt.Printf("  %s Source credential updated in %s\n\n", check, credentials.CredManagerName())
	return nil
}

func updateDestinationCredential(ctx context.Context, rec *migration.Record) error {
	dest := destination.ForName(rec.DestinationName)
	if dest == nil {
		return fmt.Errorf("destination %q is not a supported platform , cannot validate a credential for it", rec.DestinationName)
	}

	fmt.Println()
	fmt.Printf("  %s\n\n", bold.Render("How to generate a new "+dest.DisplayName()+" credential:"))
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
				Validate(func(s string) error {
					if strings.TrimSpace(s) == "" {
						return fmt.Errorf("credential cannot be empty")
					}
					return nil
				}).
				Value(&cred),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	cred = strings.TrimSpace(cred)

	fmt.Printf("\n  Validating %s credential… ", dest.DisplayName())
	username, err := dest.ValidateCredential(ctx, cred)
	if err != nil {
		fmt.Println(cross)
		return fmt.Errorf("validation failed: %w\nExisting credential was not changed.", err)
	}
	fmt.Printf("%s  Authenticated as %s\n", check, bold.Render(username))

	if err := credentials.Set(rec.Name, "destination", cred); err != nil {
		return fmt.Errorf("storing credential: %w", err)
	}

	fmt.Printf("  %s Destination credential updated in %s\n\n", check, credentials.CredManagerName())
	return nil
}
