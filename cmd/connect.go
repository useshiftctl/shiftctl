package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/shiftctl/shiftctl/internal/credentials"
	"github.com/shiftctl/shiftctl/internal/destination"
	"github.com/shiftctl/shiftctl/internal/migration"
	"github.com/shiftctl/shiftctl/internal/platform"
	"github.com/shiftctl/shiftctl/internal/server"
)

var connectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Set up a new migration",
	Long: `Connect shiftctl to a source platform and configure a new migration.

Walks you through selecting a source, generating and storing a credential,
naming a destination, and optionally providing context the scan can't infer.
The destination credential is not collected here — it is requested only when
the first execution stage actually needs to write to the destination.`,
	RunE: runConnect,
}

// bold and dim are the only two styles used in the terminal connect flow.
// Keeping styling minimal so it reads clearly on any terminal theme.
var (
	bold   = lipgloss.NewStyle().Bold(true)
	dim    = lipgloss.NewStyle().Faint(true)
	check  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true).Render("✓")
	cross  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true).Render("✗")
	rule   = strings.Repeat("─", 52)
)

func runConnect(_ *cobra.Command, _ []string) error {
	printConnectHeader()

	// ── Step 1: select source platform ──────────────────────────────────────
	var platformID string
	if err := selectPlatform(&platformID); err != nil {
		return err
	}

	p := platform.ByID(platformID)
	if p == nil {
		return fmt.Errorf("unknown platform: %s", platformID)
	}

	if !p.IsImplemented() {
		implemented := implementedPlatformNames()
		fmt.Fprintf(os.Stderr, "\n%s scanner isn't built yet.\n", p.DisplayName())
		fmt.Fprintf(os.Stderr, "Currently available: %s\n\n", implemented)
		fmt.Fprintf(os.Stderr, "Run 'shiftctl connect' again to start with an available platform.\n\n")
		return nil
	}

	// ── Step 2: show credential instructions and collect credential ──────────
	credential, username, err := collectAndValidateCredential(p)
	if err != nil {
		return err
	}

	// ── Step 3: destination name (no credential yet) ─────────────────────────
	var destName string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("What are you migrating to?").
				Description("Supported destinations: GitHub, GitLab, Bitbucket, Azure DevOps").
				Placeholder("e.g. GitLab").
				Validate(func(s string) error {
					s = strings.TrimSpace(s)
					if s == "" {
						return fmt.Errorf("destination name cannot be empty")
					}
					if destination.ForName(s) == nil {
						return fmt.Errorf("%q is not a supported destination — choose GitHub, GitLab, Bitbucket, or Azure DevOps", s)
					}
					return nil
				}).
				Value(&destName),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	// ── Step 4: optional plain-English context ────────────────────────────────
	var userContext string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Anything the scan can't infer? (optional)").
				Description("Free-form notes: skipped repos, deadlines, constraints.\nPress Enter to skip.").
				Placeholder("e.g. skip the marketing repos, done by end of Q3").
				Value(&userContext),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	// ── Step 5: migration name ────────────────────────────────────────────────
	var rawName string
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title("Name this migration").
				Description("Used as an identifier in commands and local file paths.\nSpaces become hyphens; only letters, digits, hyphens, and underscores allowed.").
				Placeholder("e.g. github-to-gitlab-2026").
				Validate(func(s string) error {
					return migration.ValidateName(s)
				}).
				Value(&rawName),
		),
	).Run(); err != nil {
		return cancelledOrError(err)
	}

	name := migration.SanitizeName(rawName)

	// Guard against name collision.
	exists, err := migration.Exists(name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf(
			"a migration named %q already exists\n"+
				"Choose a different name, or run 'shiftctl disconnect --purge' to start fresh.",
			name,
		)
	}

	// ── Store credential ──────────────────────────────────────────────────────
	if err := credentials.Set(name, "source", credential); err != nil {
		return fmt.Errorf("could not store credential: %w", err)
	}

	// ── Save migration record ─────────────────────────────────────────────────
	rec := &migration.Record{
		Name:            name,
		SourcePlatform:  p.ID(),
		DestinationName: strings.TrimSpace(destName),
		Context:         strings.TrimSpace(userContext),
		CreatedAt:       time.Now().UTC(),
		Status:          migration.StatusSetup,
	}
	if err := migration.Save(rec); err != nil {
		// Best-effort cleanup: remove the stored credential if the record write fails.
		_ = credentials.Delete(name, "source")
		return fmt.Errorf("could not save migration record: %w", err)
	}

	// ── Success ───────────────────────────────────────────────────────────────
	printConnectSuccess(rec, username)

	// ── Open browser companion ─────────────────────────────────────────────────
	l, err := net.Listen("tcp", "127.0.0.1:4242")
	if err != nil {
		// Port in use — just tell them to run serve manually.
		fmt.Printf("  Run %s to open the browser companion.\n\n", bold.Render("shiftctl serve "+name))
		return nil
	}
	url := "http://127.0.0.1:4242"
	fmt.Printf("  Opening companion at %s\n", url)
	fmt.Println(dim.Render("  Press Ctrl+C to stop."))
	fmt.Println()
	openBrowser(url)
	return http.Serve(l, server.New(name))
}

// selectPlatform renders an interactive platform selector and writes the
// chosen platform ID into target.
func selectPlatform(target *string) error {
	platforms := platform.All()
	opts := make([]huh.Option[string], len(platforms))
	for i, p := range platforms {
		label := p.DisplayName()
		if !p.IsImplemented() {
			label += " (coming soon)"
		}
		opts[i] = huh.NewOption(label, p.ID())
	}

	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("What are you migrating from?").
				Options(opts...).
				Value(target),
		),
	).Run()
}

// collectAndValidateCredential shows platform-specific instructions, prompts
// for the credential, and validates it against the platform's API. Retries up
// to 3 times on validation failure before giving up.
func collectAndValidateCredential(p platform.Platform) (credential, username string, err error) {
	fmt.Println()
	fmt.Println(bold.Render(p.CredentialInstructions()))
	fmt.Println()

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Prompt for credential.
		var raw string
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
					Value(&raw),
			),
		).Run(); err != nil {
			return "", "", cancelledOrError(err)
		}

		raw = strings.TrimSpace(raw)

		// Validate.
		fmt.Printf("  Validating credential... ")
		user, valErr := p.ValidateCredential(context.Background(), raw)
		if valErr != nil {
			fmt.Println(cross)
			fmt.Printf("  %s\n\n", valErr)
			if attempt < maxAttempts {
				fmt.Printf("  Try again (%d of %d attempts remaining).\n\n", maxAttempts-attempt, maxAttempts)
				continue
			}
			return "", "", fmt.Errorf("credential validation failed after %d attempts: %w", maxAttempts, valErr)
		}

		fmt.Printf("%s  Authenticated as %s\n\n", check, bold.Render(user))
		return raw, user, nil
	}

	// Unreachable, but satisfies the compiler.
	return "", "", fmt.Errorf("credential validation failed")
}

// implementedPlatformNames returns a comma-separated list of platform display
// names that have a built scanner.
func implementedPlatformNames() string {
	var names []string
	for _, p := range platform.All() {
		if p.IsImplemented() {
			names = append(names, p.DisplayName())
		}
	}
	if len(names) == 0 {
		return "none yet"
	}
	return strings.Join(names, ", ")
}

// cancelledOrError translates a huh ErrUserAborted into a clean exit rather
// than an error message, since cancelling setup is expected behaviour.
func cancelledOrError(err error) error {
	if err == huh.ErrUserAborted {
		fmt.Fprintln(os.Stderr, "\nSetup cancelled.")
		os.Exit(0)
	}
	return err
}

func printConnectHeader() {
	fmt.Println()
	fmt.Println(rule)
	fmt.Println(bold.Render("  shiftctl — Migration Setup"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Println("  Everything runs on your machine.")
	fmt.Println("  Nothing is shared without your explicit approval.")
	fmt.Println()
}

func printConnectSuccess(rec *migration.Record, username string) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Printf("  %s  Migration %q set up.\n\n", check, rec.Name)
	fmt.Printf("  Source:       %s (%s)\n", platformDisplayName(rec.SourcePlatform), username)
	fmt.Printf("  Destination:  %s\n", rec.DestinationName)
	if rec.Context != "" {
		fmt.Printf("  Context:      %s\n", rec.Context)
	}
	fmt.Println()
	fmt.Printf("  Credential stored securely in your %s.\n", credentials.CredManagerName())
	fmt.Println()
	fmt.Println(dim.Render("  To remove all local data at any time:"))
	fmt.Println(dim.Render("    shiftctl disconnect --purge"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Printf("  Run %s to start the assessment.\n", bold.Render("shiftctl assess "+rec.Name))
	fmt.Println()
}

func platformDisplayName(id string) string {
	if p := platform.ByID(id); p != nil {
		return p.DisplayName()
	}
	return id
}
