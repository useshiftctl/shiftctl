// Package destination defines the interface for platforms that shiftctl can
// migrate repositories TO, and maps user-supplied destination names to their
// implementations.
package destination

import (
	"context"
	"strings"

	githubscanner "github.com/shiftctl/shiftctl/internal/scanner/github"
)

// Destination is a code-hosting platform that shiftctl can write to during
// execution. The interface mirrors the source Platform interface so the two
// sides feel consistent.
type Destination interface {
	// ID is the canonical lowercase identifier (e.g. "gitlab").
	ID() string
	// DisplayName is the human-readable name (e.g. "GitLab").
	DisplayName() string
	// IsImplemented reports whether this destination has a working
	// implementation. Unimplemented destinations are rejected at execute time.
	IsImplemented() bool

	// CredentialInstructions returns step-by-step instructions for generating
	// a write-access credential on this platform.
	CredentialInstructions() string
	// CredentialPrompt is the label above the masked input field.
	CredentialPrompt() string
	// ValidateCredential verifies the credential is valid and has the access
	// needed to create and push to repositories. Returns the authenticated
	// username on success.
	ValidateCredential(ctx context.Context, credential string) (username string, err error)

	// PrepareRepo creates the repository on the destination and returns the
	// details needed to mirror content into it. Idempotent: if the repo
	// already exists, returns its existing details rather than failing.
	PrepareRepo(ctx context.Context, credential string, fact githubscanner.RepoFact) (*MigrationAction, error)

	// RepoExists checks whether a repository already exists on the
	// destination. Used during crash recovery to re-verify uncertain items.
	RepoExists(ctx context.Context, credential, namespace, repoName string) (bool, error)

	// MigrateMetadata copies webhooks and branch protection rules from the
	// source facts to the destination repository. Best-effort: failures here
	// are reported but do not abort execution.
	MigrateMetadata(ctx context.Context, credential string, action *MigrationAction, fact githubscanner.RepoFact) error
}

// MigrationAction is returned by PrepareRepo. It holds everything the executor
// needs to mirror git content into the new destination repository.
type MigrationAction struct {
	// RepoFullName is the source repository's full name (e.g. "owner/repo").
	RepoFullName string
	// DestRepoID is the destination platform's internal numeric or string ID,
	// used for subsequent metadata API calls (webhooks, branch protection).
	DestRepoID string
	// PushURL is the authenticated git remote URL to push content to.
	// Contains a credential and must never be logged or shown to the user.
	PushURL string
	// DestWebURL is the human-readable URL of the new repository (safe to show).
	DestWebURL string
	// DestNamespace is the namespace/group the repo was created under.
	DestNamespace string
}

// ForName resolves a free-text destination name (as the user typed it during
// 'shiftctl connect') to a Destination implementation. Returns nil when the
// name doesn't match any known platform.
func ForName(name string) Destination {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(lower, "gitlab"):
		return GitLab()
	case strings.Contains(lower, "github"):
		return GitHub()
	default:
		return nil
	}
}

// GitLab returns the GitLab Destination implementation.
func GitLab() Destination {
	return newGitLab()
}

// GitHub returns the GitHub Destination implementation.
func GitHub() Destination {
	return newGitHub()
}
