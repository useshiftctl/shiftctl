package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "shiftctl",
	Short: "A local-first migration tool for DevOps and SRE teams",
	Long: `shiftctl helps you plan and execute code-hosting migrations with a staged,
reviewable process you control at every step.

Everything runs on your machine. Nothing is shared without your explicit
approval. Uses AI to analyze your repositories and propose a migration plan —
you review and approve every step before anything runs.

To remove all local data at any time: shiftctl disconnect --purge`,
	// No RunE — show help by default when called with no subcommand.
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(connectCmd)
	rootCmd.AddCommand(disconnectCmd)
}
