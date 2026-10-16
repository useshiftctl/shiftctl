package gitlab

import (
	"context"

	glscanner "github.com/useshiftctl/shiftctl/internal/scanner/gitlab"
)

type platform struct{}

// New returns the GitLab platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "gitlab" }
func (p *platform) DisplayName() string { return "GitLab" }
func (p *platform) IsImplemented() bool { return true }

func (p *platform) CredentialInstructions() string {
	return `To generate a GitLab personal access token:

  1. Open: https://gitlab.com/-/user_settings/personal_access_tokens
  2. Click "Add new token"
  3. Give it a name and expiration date
  4. Select these scopes:
       read_api          (read access to the API)
       read_repository   (read access to repositories)
  5. Click "Create personal access token" and copy the result

  The token will start with "glpat-".`
}

func (p *platform) CredentialPrompt() string {
	return "GitLab personal access token (glpat-...)"
}

// ValidateCredential calls the GitLab API to confirm the token is valid and
// returns the authenticated username.
func (p *platform) ValidateCredential(ctx context.Context, credential string) (string, error) {
	scanner := glscanner.NewScanner(credential)
	return scanner.ValidateAndGetUsername(ctx)
}
