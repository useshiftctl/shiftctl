package platform

import "context"

// Platform describes a code-hosting source that shiftctl can migrate from.
// Each platform is responsible for providing credential instructions and
// validating that a supplied credential has the access shiftctl needs.
type Platform interface {
	// ID returns the canonical lowercase identifier (e.g. "github").
	ID() string
	// DisplayName returns the human-readable name shown to users (e.g. "GitHub").
	DisplayName() string
	// CredentialInstructions returns step-by-step terminal instructions for
	// how to generate the required credential on this platform.
	CredentialInstructions() string
	// CredentialPrompt is the label shown above the masked input field.
	CredentialPrompt() string
	// ValidateCredential verifies that the credential is valid and has the
	// access shiftctl requires. Returns the authenticated username on success.
	ValidateCredential(ctx context.Context, credential string) (username string, err error)
	// IsImplemented reports whether the scanner for this platform has been
	// built. Unimplemented platforms can still be registered so they appear
	// in listings, but shiftctl will decline to proceed with them.
	IsImplemented() bool
}

// All returns every registered platform in display order.
func All() []Platform {
	return []Platform{
		GitHub(),
		GitLab(),
		Bitbucket(),
		AzureDevOps(),
	}
}

// ByID returns the platform with the given ID, or nil if not found.
func ByID(id string) Platform {
	for _, p := range All() {
		if p.ID() == id {
			return p
		}
	}
	return nil
}
