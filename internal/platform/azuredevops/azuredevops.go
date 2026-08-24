package azuredevops

import (
	"context"

	adoscanner "github.com/shiftctl/shiftctl/internal/scanner/azuredevops"
)

type platform struct{}

// New returns the Azure DevOps platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "azuredevops" }
func (p *platform) DisplayName() string { return "Azure DevOps" }
func (p *platform) IsImplemented() bool { return true }

func (p *platform) CredentialInstructions() string {
	return `To generate an Azure DevOps personal access token:

  1. Open: https://dev.azure.com/<your-org>/_usersSettings/tokens
     (replace <your-org> with your organisation name)
  2. Click "New Token"
  3. Give it a name and set an expiration date
  4. Under Scopes, select:
       Code: Read
       Build: Read
       Project and Team: Read
  5. Click "Create" and copy the result

  When prompted, enter your credential as:  organization:personal_access_token
  (the organisation name from your Azure DevOps URL: dev.azure.com/{organisation})`
}

func (p *platform) CredentialPrompt() string {
	return "Azure DevOps credential (organization:personal_access_token)"
}

// ValidateCredential accepts "org:pat", calls the Azure DevOps API to confirm
// the credential is valid, and returns the authenticated display name.
func (p *platform) ValidateCredential(ctx context.Context, credential string) (string, error) {
	scanner, err := adoscanner.NewScanner(credential)
	if err != nil {
		return "", err
	}
	return scanner.ValidateAndGetUsername(ctx)
}
