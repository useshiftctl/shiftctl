package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/useshiftctl/shiftctl/internal/migration"
	"github.com/useshiftctl/shiftctl/internal/server"
)

var servePort int

var serveCmd = &cobra.Command{
	Use:   "serve <migration-name>",
	Short: "Start the browser companion for a migration",
	Long: `Start a local HTTP server and open the browser companion for the
named migration. The companion reads the same state files as the CLI and
provides a richer review interface for assessment, planning, approval, and
decommission.

The server binds to 127.0.0.1 only and is not accessible from other machines.`,
	Args: cobra.ExactArgs(1),
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVar(&servePort, "port", 4242, "Port to listen on")
}

func runServe(_ *cobra.Command, args []string) error {
	name := migration.SanitizeName(args[0])

	s := server.New(name)
	addr := fmt.Sprintf("127.0.0.1:%d", servePort)

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("could not start server on %s: %w", addr, err)
	}

	url := fmt.Sprintf("http://%s", addr)
	fmt.Printf("\n  shiftctl companion\n\n  Migration: %s\n  URL:       %s\n\n  Press Ctrl+C to stop.\n\n", name, url)
	openBrowser(url)

	return http.Serve(l, s)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
