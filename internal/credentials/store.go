package credentials

import (
	"fmt"
	"runtime"

	"github.com/zalando/go-keyring"
)

const service = "shiftctl"

// KeyFor returns the keyring account key for the given migration and role.
// role is "source" or "destination".
func KeyFor(migrationName, role string) string {
	return migrationName + ":" + role
}

// Set stores a credential in the OS credential manager.
func Set(migrationName, role, value string) error {
	if err := keyring.Set(service, KeyFor(migrationName, role), value); err != nil {
		return fmt.Errorf("could not store credential in %s: %w", credManagerName(), err)
	}
	return nil
}

// Get retrieves a credential from the OS credential manager.
func Get(migrationName, role string) (string, error) {
	val, err := keyring.Get(service, KeyFor(migrationName, role))
	if err != nil {
		if err == keyring.ErrNotFound {
			return "", fmt.Errorf("no %s credential found for migration %q", role, migrationName)
		}
		return "", fmt.Errorf("could not read credential from %s: %w", credManagerName(), err)
	}
	return val, nil
}

// Delete removes a credential from the OS credential manager. It does not
// return an error if the credential does not exist.
func Delete(migrationName, role string) error {
	err := keyring.Delete(service, KeyFor(migrationName, role))
	if err != nil && err != keyring.ErrNotFound {
		return fmt.Errorf("could not remove credential from %s: %w", credManagerName(), err)
	}
	return nil
}

// CredManagerName returns a human-readable name for the current OS credential
// manager, for use in user-facing messages.
func CredManagerName() string {
	return credManagerName()
}

func credManagerName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS Keychain"
	case "linux":
		return "system keychain (Secret Service)"
	case "windows":
		return "Windows Credential Manager"
	default:
		return "system credential manager"
	}
}
