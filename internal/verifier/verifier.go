// Package verifier checks that a completed migration stage landed correctly on
// the destination. It re-confirms every repository that was executed, checks
// that the git content is reachable, and reports any discrepancies.
package verifier

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/useshiftctl/shiftctl/internal/destination"
	"github.com/useshiftctl/shiftctl/internal/executor"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

// ItemVerification is the outcome of verifying a single repository.
type ItemVerification struct {
	FullName   string
	OK         bool
	DestWebURL string
	// Issue describes what was wrong (empty when OK is true).
	Issue string
}

// Report is returned by Run.
type Report struct {
	StageNumber int
	Items       []ItemVerification
}

// Passed returns true when every item verified successfully.
func (r *Report) Passed() bool {
	for _, it := range r.Items {
		if !it.OK {
			return false
		}
	}
	return true
}

// Summary returns (passed, failed) counts.
func (r *Report) Summary() (passed, failed int) {
	for _, it := range r.Items {
		if it.OK {
			passed++
		} else {
			failed++
		}
	}
	return
}

// Options controls verification behaviour.
type Options struct {
	// DestCredential is the write-access token for the destination.
	DestCredential string
	// Dest is the resolved destination implementation.
	Dest destination.Destination
	// DestNamespace is the authenticated username / group on the destination.
	DestNamespace string
	// ProgressFn is called after each item is verified.
	ProgressFn func(item ItemVerification)
}

// Run verifies every repository in the given stage. It checks:
//  1. The repository exists on the destination (API call).
//  2. The git remote is reachable and has at least one ref (git ls-remote).
//
// Returns a Report regardless of individual failures so the caller can show a
// complete picture before deciding whether to transition the stage.
func Run(ctx context.Context, stage plan.Stage, prog *executor.StageProgress, opts Options) (*Report, error) {
	report := &Report{StageNumber: stage.Number}

	for _, item := range prog.Items {
		iv := verifyOne(ctx, item, stage, opts)
		report.Items = append(report.Items, iv)
		if opts.ProgressFn != nil {
			opts.ProgressFn(iv)
		}
		if ctx.Err() != nil {
			break
		}
	}

	return report, nil
}

func verifyOne(ctx context.Context, item executor.ItemProgress, stage plan.Stage, opts Options) ItemVerification {
	iv := ItemVerification{FullName: item.FullName}

	// Items that failed during execution are carried through as failed.
	if item.Status == executor.ItemFailed {
		iv.Issue = fmt.Sprintf("execution failed: %s", item.Error)
		return iv
	}

	// Parse owner/repo from full name.
	parts := strings.SplitN(item.FullName, "/", 2)
	if len(parts) != 2 {
		iv.Issue = "malformed full name"
		return iv
	}
	repoName := parts[1]

	// Step 1: confirm it exists via the destination API.
	exists, err := opts.Dest.RepoExists(ctx, opts.DestCredential, opts.DestNamespace, repoName)
	if err != nil {
		iv.Issue = fmt.Sprintf("destination API error: %v", err)
		return iv
	}
	if !exists {
		iv.Issue = "repository not found on destination"
		return iv
	}

	// Reconstruct the destination web URL for display. We need the push URL to
	// run ls-remote; derive it from the destination type and namespace.
	pushURL := destPushURL(opts.Dest, opts.DestCredential, opts.DestNamespace, repoName)
	webURL := destWebURL(opts.Dest, opts.DestNamespace, repoName)
	iv.DestWebURL = webURL

	// Step 2: confirm git content is reachable.
	if err := checkGitReachable(ctx, pushURL); err != nil {
		iv.Issue = fmt.Sprintf("git content unreachable: %v", err)
		return iv
	}

	iv.OK = true
	return iv
}

// checkGitReachable runs `git ls-remote <url>` and checks that at least one
// ref is returned. An empty repo (no commits pushed) is treated as a failure.
func checkGitReachable(ctx context.Context, remoteURL string) error {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--tags", remoteURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("repository exists but has no refs. content may not have pushed correctly")
	}
	return nil
}

// destPushURL builds an authenticated git URL for the destination.
func destPushURL(dest destination.Destination, credential, namespace, repoName string) string {
	switch dest.ID() {
	case "gitlab":
		return fmt.Sprintf("https://oauth2:%s@gitlab.com/%s/%s.git", credential, namespace, repoName)
	case "github":
		return fmt.Sprintf("https://%s@github.com/%s/%s.git", credential, namespace, repoName)
	case "bitbucket":
		// credential is "username:app_password". embed as-is.
		return fmt.Sprintf("https://%s@bitbucket.org/%s/%s.git", credential, namespace, repoName)
	case "azuredevops":
		// credential is "org:pat"; use empty username + PAT for basic auth.
		org, pat, _ := strings.Cut(credential, ":")
		return fmt.Sprintf("https://%s:%s@dev.azure.com/%s/%s/_git/%s", org, pat, org, namespace, repoName)
	default:
		return fmt.Sprintf("https://%s@%s/%s/%s.git", credential, dest.ID(), namespace, repoName)
	}
}

func destWebURL(dest destination.Destination, namespace, repoName string) string {
	switch dest.ID() {
	case "gitlab":
		return fmt.Sprintf("https://gitlab.com/%s/%s", namespace, repoName)
	case "github":
		return fmt.Sprintf("https://github.com/%s/%s", namespace, repoName)
	case "bitbucket":
		return fmt.Sprintf("https://bitbucket.org/%s/%s", namespace, repoName)
	case "azuredevops":
		// namespace is the project name; org is not stored here, use a generic URL.
		return fmt.Sprintf("https://dev.azure.com/%s/_git/%s", namespace, repoName)
	default:
		return fmt.Sprintf("https://%s/%s/%s", dest.ID(), namespace, repoName)
	}
}
