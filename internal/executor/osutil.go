package executor

import (
	"fmt"
	"os"

	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
	"github.com/useshiftctl/shiftctl/internal/plan"
)

func mkTempDir() (string, error) {
	dir, err := os.MkdirTemp("", "shiftctl-mirror-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	return dir, nil
}

func removeAll(path string) {
	_ = os.RemoveAll(path)
}

// plannedRepoToFact builds a minimal RepoFact from the information stored in
// the plan. The plan does not store full scan facts (webhooks, branch
// protection, etc.) — those are only available from the live assessment state.
// For the execute command, PrepareRepo only needs Name, FullName, Description,
// and Visibility, so this is sufficient.
func plannedRepoToFact(r plan.PlannedRepo) githubscanner.RepoFact {
	parts := splitOwnerRepo(r.FullName)
	return githubscanner.RepoFact{
		Owner:    parts[0],
		Name:     parts[1],
		FullName: r.FullName,
		// Visibility is not stored in the plan; default to private (safe).
		Visibility: "private",
	}
}

func splitOwnerRepo(fullName string) [2]string {
	for i, c := range fullName {
		if c == '/' {
			return [2]string{fullName[:i], fullName[i+1:]}
		}
	}
	return [2]string{"", fullName}
}
