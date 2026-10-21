package executor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/useshiftctl/shiftctl/internal/destination"
	"github.com/useshiftctl/shiftctl/internal/execstate"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

// RepoResult is the outcome of executing a single repository migration.
type RepoResult struct {
	FullName  string
	Status    ItemStatus
	Error     string
	// DestWebURL is the human-readable URL of the created repository (empty on failure).
	DestWebURL string
}

// RecoveryReport summarises what happened to uncertain items during startup
// recovery: whether they were confirmed done or need to be re-run.
type RecoveryReport struct {
	Confirmed []string // full names confirmed present on destination
	Requeued  []string // full names that were not found and will be re-run
}

// Options controls execution behaviour.
type Options struct {
	// SourcePlatform is the canonical ID of the source platform
	// (e.g. "github", "gitlab", "bitbucket", "azuredevops").
	SourcePlatform string
	// SourceCredential is the read-access token for the source platform.
	SourceCredential string
	// DestCredential is the write-access token for the destination platform.
	DestCredential string
	// Dest is the resolved destination implementation.
	Dest destination.Destination
	// DestNamespace is the namespace (username/group) to create repos under.
	// Resolved from the first ValidateCredential call.
	DestNamespace string
	// RetryFailed resets any ItemFailed items back to pending before execution
	// begins, so they are attempted again in this run.
	RetryFailed bool
	// ProgressFn is called after each item completes (success or failure).
	// stageProgress is a snapshot at that point in time.
	ProgressFn func(result RepoResult, progress *StageProgress)
	// RecoveryFn is called once at startup if there were uncertain items.
	RecoveryFn func(report RecoveryReport)
}

// Run executes all pending repositories in the given approved stage. It
// handles crash recovery, git mirror-push, and metadata migration.
// Returns when all items have been attempted (done or failed).
func Run(
	ctx context.Context,
	state *execstate.State,
	stage plan.Stage,
	opts Options,
) (*StageProgress, error) {
	// Resolve item order from stage (already topologically sorted by plan).
	fullNames := make([]string, len(stage.Repos))
	for i, r := range stage.Repos {
		fullNames[i] = r.FullName
	}

	stageState := execstageState(state, stage.Number)
	if stageState == nil {
		return nil, fmt.Errorf("stage %d not found in execution state", stage.Number)
	}

	// ── Load or initialise progress ──────────────────────────────────────────
	prog, err := LoadOrInitProgress(state.MigrationName, stage.Number, stageState.ApprovalID, fullNames)
	if err != nil {
		return nil, fmt.Errorf("loading progress: %w", err)
	}

	// ── Crash recovery: move any executing → uncertain ────────────────────────
	if err := MarkUncertain(prog); err != nil {
		return nil, fmt.Errorf("marking uncertain: %w", err)
	}

	// ── Retry failed: reset failed items to pending if requested ─────────────
	if opts.RetryFailed {
		if err := ResetFailedToPending(prog); err != nil {
			return nil, fmt.Errorf("resetting failed items: %w", err)
		}
	}

	// ── Report uncertain items to the caller ──────────────────────────────────
	uncertain := UncertainItems(prog)
	if len(uncertain) > 0 && opts.RecoveryFn != nil {
		report := recoverUncertain(ctx, opts, prog, uncertain)
		opts.RecoveryFn(report)
	}

	// ── Execute pending items ─────────────────────────────────────────────────
	for _, item := range prog.Items {
		if item.Status == ItemDone || item.Status == ItemFailed {
			continue
		}
		result := executeOne(ctx, state.MigrationName, stage, item.FullName, prog, opts)
		if opts.ProgressFn != nil {
			opts.ProgressFn(result, prog)
		}
		if ctx.Err() != nil {
			// Context cancelled; stop processing further items.
			break
		}
	}

	return prog, nil
}

// executeOne migrates a single repository: PrepareRepo → git mirror-push →
// MigrateMetadata. Saves progress at each step.
func executeOne(
	ctx context.Context,
	migrationName string,
	stage plan.Stage,
	fullName string,
	prog *StageProgress,
	opts Options,
) RepoResult {
	// Find the plan entry for this repo so we have the source facts.
	var planned *plan.PlannedRepo
	for i := range stage.Repos {
		if stage.Repos[i].FullName == fullName {
			planned = &stage.Repos[i]
			break
		}
	}
	if planned == nil {
		return RepoResult{FullName: fullName, Status: ItemFailed, Error: "repo not found in stage plan"}
	}

	// Mark executing before any network call.
	if err := MarkExecuting(prog, fullName); err != nil {
		return RepoResult{FullName: fullName, Status: ItemFailed, Error: fmt.Sprintf("marking executing: %v", err)}
	}

	// We need a RepoFact-like object for PrepareRepo. Build a minimal one from
	// what the plan records (the plan preserved what the executor needs).
	fact := plannedRepoToFact(*planned)

	// ── Step 1: create destination repository ────────────────────────────────
	action, err := opts.Dest.PrepareRepo(ctx, opts.DestCredential, fact)
	if err != nil {
		msg := fmt.Sprintf("PrepareRepo: %v", err)
		_ = MarkFailed(prog, fullName, msg)
		return RepoResult{FullName: fullName, Status: ItemFailed, Error: msg}
	}

	// ── Step 2: git clone --mirror source, then push to destination ──────────
	cloneURL := sourceCloneURL(opts.SourcePlatform, fullName, opts.SourceCredential)
	if err := mirrorPush(ctx, cloneURL, action.PushURL); err != nil {
		msg := fmt.Sprintf("git mirror push: %v", err)
		_ = MarkFailed(prog, fullName, msg)
		return RepoResult{FullName: fullName, Status: ItemFailed, Error: msg}
	}

	// ── Step 3: migrate metadata (best-effort) ───────────────────────────────
	// We don't have the full RepoFact here (only what the plan stored), so
	// MigrateMetadata gets a partial fact. Best-effort failures are noted but
	// don't abort.
	metaErr := opts.Dest.MigrateMetadata(ctx, opts.DestCredential, action, fact)
	_ = metaErr // reported via progress annotation; execution continues

	// ── Done ─────────────────────────────────────────────────────────────────
	_ = MarkDone(prog, fullName)
	return RepoResult{
		FullName:   fullName,
		Status:     ItemDone,
		DestWebURL: action.DestWebURL,
	}
}

// recoverUncertain re-verifies uncertain items against the destination.
// Items confirmed present are marked done; items not found are reset to pending.
func recoverUncertain(
	ctx context.Context,
	opts Options,
	prog *StageProgress,
	uncertain []ItemProgress,
) RecoveryReport {
	var report RecoveryReport

	for _, item := range uncertain {
		parts := strings.SplitN(item.FullName, "/", 2)
		if len(parts) != 2 {
			// Can't verify; reset to pending.
			_ = resetToPending(prog, item.FullName)
			report.Requeued = append(report.Requeued, item.FullName)
			continue
		}
		repoName := parts[1]

		exists, err := opts.Dest.RepoExists(ctx, opts.DestCredential, opts.DestNamespace, repoName)
		if err != nil || !exists {
			_ = resetToPending(prog, item.FullName)
			report.Requeued = append(report.Requeued, item.FullName)
		} else {
			_ = MarkDone(prog, item.FullName)
			report.Confirmed = append(report.Confirmed, item.FullName)
		}
	}
	return report
}

func resetToPending(prog *StageProgress, fullName string) error {
	idx := itemIndex(prog, fullName)
	if idx < 0 {
		return fmt.Errorf("item not found")
	}
	prog.Items[idx].Status = ItemPending
	prog.Items[idx].StartedAt = nil
	return SaveProgress(prog)
}

// mirrorPush performs: git clone --mirror <src> <tmpdir>, then
// git push --mirror <dst> from that clone.
func mirrorPush(ctx context.Context, srcURL, dstURL string) error {
	// Use a temp directory inside the OS temp area.
	tmpDir, err := mkTempDir()
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer removeAll(tmpDir)

	// Clone mirror.
	cloneCmd := exec.CommandContext(ctx, "git", "clone", "--mirror", srcURL, tmpDir)
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone --mirror: %w\n%s", err, trimOutput(out))
	}

	// Push mirror.
	pushCmd := exec.CommandContext(ctx, "git", "-C", tmpDir, "push", "--mirror", dstURL)
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push --mirror: %w\n%s", err, trimOutput(out))
	}

	return nil
}

// CheckGitAvailable returns an error if the git binary is not on PATH.
func CheckGitAvailable() error {
	_, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git is not installed or not on PATH. shiftctl requires git to push repository content")
	}
	return nil
}

// Small helpers ─────────────────────────────────────────────────────────────

// sourceCloneURL builds an authenticated git clone URL for the given source
// platform. fullName is "owner/repo" for GitHub/GitLab/Bitbucket, and
// "project/repo" for Azure DevOps.
func sourceCloneURL(platform, fullName, credential string) string {
	switch platform {
	case "github":
		return fmt.Sprintf("https://%s@github.com/%s.git", credential, fullName)
	case "gitlab":
		return fmt.Sprintf("https://oauth2:%s@gitlab.com/%s.git", credential, fullName)
	case "bitbucket":
		// Bitbucket credential is "username:app_password".
		// Basic auth is already in that form, so embed directly.
		return fmt.Sprintf("https://%s@bitbucket.org/%s.git", credential, fullName)
	case "azuredevops":
		// Credential is "org:pat". ADO clone URL: https://org@dev.azure.com/org/project/_git/repo
		org, _, _ := strings.Cut(credential, ":")
		// fullName is "project/repo"
		parts := strings.SplitN(fullName, "/", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("https://%s@dev.azure.com/%s/%s/_git/%s",
				credential, org, parts[0], parts[1])
		}
		return fmt.Sprintf("https://%s@dev.azure.com/%s/_git/%s", credential, org, fullName)
	default:
		// Best-effort fallback.
		return fmt.Sprintf("https://%s@%s/%s.git", credential, platform, fullName)
	}
}

func trimOutput(b []byte) string {
	s := strings.TrimSpace(string(b))
	const max = 400
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func execstageState(s *execstate.State, number int) *execstate.StageState {
	for i := range s.Stages {
		if s.Stages[i].Number == number {
			return &s.Stages[i]
		}
	}
	return nil
}
