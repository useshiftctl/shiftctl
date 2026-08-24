package bitbucket

import (
	"context"

	bbscanner "github.com/shiftctl/shiftctl/internal/scanner/bitbucket"
)

type platform struct{}

// New returns the Bitbucket platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "bitbucket" }
func (p *platform) DisplayName() string { return "Bitbucket" }
func (p *platform) IsImplemented() bool { return true }

func (p *platform) CredentialInstructions() string {
	return `To generate a Bitbucket app password:

  1. Open: https://bitbucket.org/account/settings/app-passwords/
  2. Click "Create app password"
  3. Give it a label (e.g. "shiftctl-migration") and select:
       Repositories: Read
       Projects: Read
       Webhooks: Read and write
       SSH keys: Read
  4. Click "Create" and copy the result

  When prompted, enter your credential as:  username:app_password
  (your Bitbucket username, NOT your email address)`
}

func (p *platform) CredentialPrompt() string {
	return "Bitbucket credential (username:app_password)"
}

// ValidateCredential accepts "username:app_password", calls the Bitbucket API
// to confirm the credential is valid, and returns the authenticated username.
func (p *platform) ValidateCredential(ctx context.Context, credential string) (string, error) {
	scanner, err := bbscanner.NewScanner(credential)
	if err != nil {
		return "", err
	}
	return scanner.ValidateAndGetUsername(ctx)
}
