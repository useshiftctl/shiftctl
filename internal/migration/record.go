package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Status represents the current phase of a migration.
type Status string

const (
	StatusSetup     Status = "setup"
	StatusAssessing Status = "assessing"
	StatusPlanning  Status = "planning"
	StatusExecuting Status = "executing"
	StatusComplete  Status = "complete"
)

// Record is the on-disk representation of a migration. It is written to
// ~/.shiftctl/migrations/<name>/record.json.
type Record struct {
	Name           string    `json:"name"`
	SourcePlatform string    `json:"source_platform"`
	DestinationName string   `json:"destination_name"`
	Context        string    `json:"context,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	Status         Status    `json:"status"`
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9\-_]*$`)

// SanitizeName lowercases the input, replaces spaces with hyphens, and strips
// characters that are not alphanumeric, hyphens, or underscores.
func SanitizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidateName returns an error if the sanitized name is not suitable for use
// as a migration identifier or directory name.
func ValidateName(name string) error {
	sanitized := SanitizeName(name)
	if sanitized == "" {
		return fmt.Errorf("migration name cannot be empty")
	}
	if !validName.MatchString(sanitized) {
		return fmt.Errorf("migration name must start with a letter or digit")
	}
	return nil
}

// Save writes the record atomically to disk, creating directories as needed.
func Save(r *Record) error {
	dir, err := recordDir(r.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("could not create migration directory: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal migration record: %w", err)
	}
	// Atomic write: write to temp file, then rename.
	path := filepath.Join(dir, "record.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("could not write migration record: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("could not save migration record: %w", err)
	}
	return nil
}

// Load reads the record for the named migration from disk.
func Load(name string) (*Record, error) {
	dir, err := recordDir(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "record.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no migration named %q found", name)
		}
		return nil, fmt.Errorf("could not read migration record: %w", err)
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("could not parse migration record: %w", err)
	}
	return &r, nil
}

// ListAll returns the names of all migrations stored on disk, sorted
// alphabetically.
func ListAll() ([]string, error) {
	dir, err := migrationsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read migrations directory: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Verify a record.json exists inside , guards against leftover temp dirs.
		recPath := filepath.Join(dir, e.Name(), "record.json")
		if _, err := os.Stat(recPath); err == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Exists reports whether a migration with the given name already exists on disk.
func Exists(name string) (bool, error) {
	dir, err := recordDir(name)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(dir, "record.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// RemoveAll deletes the on-disk directory for the named migration.
func RemoveAll(name string) error {
	dir, err := recordDir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ShiftctlDir returns the path to ~/.shiftctl, creating it if it does not exist.
func ShiftctlDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".shiftctl"), nil
}

func migrationsDir() (string, error) {
	base, err := ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations"), nil
}

func recordDir(name string) (string, error) {
	dir, err := migrationsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, SanitizeName(name)), nil
}
