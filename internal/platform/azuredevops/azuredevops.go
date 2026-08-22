package azuredevops

import (
	"context"
	"fmt"
)

type platform struct{}

// New returns the Azure DevOps platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "azuredevops" }
func (p *platform) DisplayName() string { return "Azure DevOps" }
func (p *platform) IsImplemented() bool { return false }

func (p *platform) CredentialInstructions() string {
	return `To generate an Azure DevOps personal access token:

  1. Open: https://dev.azure.com/<your-org>/_usersSettings/tokens
     (replace <your-org> with your organisation name)
  2. Click "New Token"
  3. Give it a name and set an expiration date
  4. Under Scopes, select:
       Code: Read
       Build: Read
       Release: Read
  5. Click "Create" and copy the result`
}

func (p *platform) CredentialPrompt() string {
	return "Paste your Azure DevOps personal access token"
}

func (p *platform) ValidateCredential(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("Azure DevOps scanner not yet implemented")
}
