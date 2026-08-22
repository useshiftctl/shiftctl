package bitbucket

import (
	"context"
	"fmt"
)

type platform struct{}

// New returns the Bitbucket platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "bitbucket" }
func (p *platform) DisplayName() string { return "Bitbucket" }
func (p *platform) IsImplemented() bool { return false }

func (p *platform) CredentialInstructions() string {
	return `To generate a Bitbucket app password:

  1. Open: https://bitbucket.org/account/settings/app-passwords/
  2. Click "Create app password"
  3. Give it a label and select these permissions:
       Repositories: Read
       Projects: Read
       Webhooks: Read and write
  4. Click "Create" and copy the result

  You will also need your Bitbucket username (not your email address).`
}

func (p *platform) CredentialPrompt() string {
	return "Paste your Bitbucket app password"
}

func (p *platform) ValidateCredential(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("Bitbucket scanner not yet implemented")
}
