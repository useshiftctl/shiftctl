package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/credentials"
	"github.com/useshiftctl/shiftctl/internal/migration"
)

var disconnectCmd = &cobra.Command{
	Use:   "disconnect",
	Short: "Remove shiftctl credentials and local data",
	Long: `Remove shiftctl credentials and local migration data.

Use --purge to wipe everything: all credentials stored in the OS credential
manager and all local migration data under ~/.shiftctl/.

Your source and destination platform accounts are not affected. only the
local data shiftctl stored on this machine.`,
	RunE: runDisconnect,
}

var purgeFlag bool

func init() {
	disconnectCmd.Flags().BoolVar(&purgeFlag, "purge", false,
		"Remove all credentials and local migration data")
}

func runDisconnect(_ *cobra.Command, _ []string) error {
	if !purgeFlag {
		fmt.Fprintln(os.Stderr, "Nothing to do. Did you mean 'shiftctl disconnect --purge'?")
		fmt.Fprintln(os.Stderr, "Run 'shiftctl disconnect --help' for details.")
		return nil
	}

	names, err := migration.ListAll()
	if err != nil {
		return fmt.Errorf("could not read migration list: %w", err)
	}

	printPurgeWarning(names)

	var confirmed bool
	if err := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Remove all local shiftctl data?").
				Affirmative("Yes, remove everything").
				Negative("No, cancel").
				Value(&confirmed),
		),
	).Run(); err != nil {
		if err == huh.ErrUserAborted {
			fmt.Println("\nCancelled. nothing was removed.")
			return nil
		}
		return err
	}

	if !confirmed {
		fmt.Println("\nCancelled. nothing was removed.")
		return nil
	}

	return runPurge(names)
}

func runPurge(names []string) error {
	fmt.Println()
	anyError := false

	// Remove credentials for every known migration first.
	if len(names) > 0 {
		fmt.Printf("  Removing credentials from %s...\n", credentials.CredManagerName())
		for _, name := range names {
			if err := credentials.Delete(name, "source"); err != nil {
				fmt.Fprintf(os.Stderr, "    %s  could not remove source credential for %q: %v\n", cross, name, err)
				anyError = true
			}
			// Destination credential may or may not exist yet. Delete is a no-op if absent.
			_ = credentials.Delete(name, "destination")
		}
	}

	// Stop the companion server if it's running.
	stopCompanionServer()

	// Remove the ~/.shiftctl/ directory.
	fmt.Print("  Removing local migration data...")
	dir, err := migration.ShiftctlDir()
	if err != nil {
		return fmt.Errorf("could not determine shiftctl directory: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Printf(" %s\n", cross)
		return fmt.Errorf("could not remove %s: %w", dir, err)
	}
	fmt.Printf(" %s\n", check)

	fmt.Println()
	if anyError {
		fmt.Println("  Local data removed with some credential errors (see above).")
		fmt.Println("  You may need to remove remaining entries from your credential manager manually.")
	} else {
		fmt.Println("  All local data removed. shiftctl is now fully disconnected.")
	}
	fmt.Println()
	return nil
}

// stopCompanionServer kills any process listening on port 4242 so the user
// can immediately run shiftctl connect after disconnecting without hitting
// "address already in use".
func stopCompanionServer() {
	const port = "4242"

	// Quick check. if nothing is listening, do nothing.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 0)
	if err != nil {
		return // nothing running
	}
	conn.Close()

	fmt.Print("  Stopping companion server...")

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin", "linux":
		// lsof prints the PIDs, xargs kill -9 terminates them.
		out, err := exec.Command("lsof", "-ti", ":"+port).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			fmt.Println(" (not found)")
			return
		}
		pids := strings.TrimSpace(string(out))
		cmd = exec.Command("kill", append([]string{"-9"}, strings.Fields(pids)...)...)
	case "windows":
		cmd = exec.Command("cmd", "/C",
			"for /f \"tokens=5\" %a in ('netstat -aon ^| findstr :"+port+"') do taskkill /F /PID %a")
	default:
		fmt.Println(" (unsupported OS)")
		return
	}

	if err := cmd.Run(); err != nil {
		fmt.Printf(" %s\n", cross)
	} else {
		fmt.Printf(" %s\n", check)
	}
}

func printPurgeWarning(names []string) {
	fmt.Println()
	fmt.Println(rule)
	fmt.Println(bold.Render("  shiftctl disconnect --purge"))
	fmt.Println(rule)
	fmt.Println()
	fmt.Println("  This will permanently delete:")
	fmt.Printf("    - All shiftctl credentials from your %s\n", credentials.CredManagerName())
	fmt.Println("    - All local migration data under ~/.shiftctl/")
	fmt.Println()
	fmt.Println("  Your GitHub, GitLab, or other platform accounts are not affected.")
	fmt.Println("  Only the data shiftctl stored on this machine will be removed.")
	fmt.Println()

	if len(names) == 0 {
		fmt.Println("  No migrations found. only clearing any remaining local data.")
	} else {
		noun := "migration"
		if len(names) > 1 {
			noun = "migrations"
		}
		fmt.Printf("  Found %d %s to remove:\n", len(names), noun)
		for _, n := range names {
			fmt.Printf("    - %s\n", n)
		}
	}
	fmt.Println()
}
