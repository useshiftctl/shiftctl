package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/useshiftctl/shiftctl/internal/migration"
)

// Save writes the plan atomically to disk.
func Save(p *Plan) error {
	path, err := planPath(p.MigrationName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating plan directory: %w", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling plan: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing plan: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving plan: %w", err)
	}
	return nil
}

// Load reads the plan for the named migration from disk.
func Load(migrationName string) (*Plan, error) {
	path, err := planPath(migrationName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no plan found for migration %q. run 'shiftctl plan %s' first", migrationName, migrationName)
		}
		return nil, fmt.Errorf("reading plan: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing plan: %w", err)
	}
	return &p, nil
}

// Exists reports whether a plan has been generated for the named migration.
func Exists(migrationName string) (bool, error) {
	path, err := planPath(migrationName)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func planPath(migrationName string) (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations", migration.SanitizeName(migrationName), "plan.json"), nil
}
