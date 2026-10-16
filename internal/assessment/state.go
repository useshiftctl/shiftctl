package assessment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/useshiftctl/shiftctl/internal/ai"
	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
	"github.com/useshiftctl/shiftctl/internal/migration"
)

// State is the complete on-disk record of an assessment. It is written in two
// phases: once after scanning (Analysis is nil) and again after AI analysis
// (Analysis is populated). Both writes are atomic.
type State struct {
	MigrationName string                    `json:"migration_name"`
	ScannedAt     time.Time                 `json:"scanned_at"`
	RepoCount     int                       `json:"repo_count"`
	Facts         []githubscanner.RepoFact  `json:"facts"`
	AnalyzedAt    *time.Time                `json:"analyzed_at,omitempty"`
	Analysis      *ai.AnalysisResult        `json:"analysis,omitempty"`
}

// SaveFacts writes the scanning results to disk. Called after scanning
// completes, before the AI analysis begins — so a crash between the two phases
// can resume from the saved facts rather than re-scanning.
func SaveFacts(migrationName string, facts []githubscanner.RepoFact) error {
	s := &State{
		MigrationName: migrationName,
		ScannedAt:     time.Now().UTC(),
		RepoCount:     len(facts),
		Facts:         facts,
	}
	return writeState(migrationName, s)
}

// SaveAnalysis merges the AI analysis into the existing state file. Returns an
// error if no facts file exists (scanning must happen before analysis).
func SaveAnalysis(migrationName string, result *ai.AnalysisResult) error {
	s, err := Load(migrationName)
	if err != nil {
		return fmt.Errorf("loading existing assessment state: %w", err)
	}
	now := time.Now().UTC()
	s.AnalyzedAt = &now
	s.Analysis = result
	return writeState(migrationName, s)
}

// Load reads the assessment state for the named migration from disk.
func Load(migrationName string) (*State, error) {
	path, err := statePath(migrationName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no assessment found for migration %q — run 'shiftctl assess %s' first", migrationName, migrationName)
		}
		return nil, fmt.Errorf("reading assessment state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing assessment state: %w", err)
	}
	return &s, nil
}

// Exists reports whether an assessment (with analysis) exists for the named
// migration.
func Exists(migrationName string) (bool, error) {
	path, err := statePath(migrationName)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func writeState(migrationName string, s *State) error {
	path, err := statePath(migrationName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating assessment directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling assessment state: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing assessment state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving assessment state: %w", err)
	}
	return nil
}

func statePath(migrationName string) (string, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "migrations", migration.SanitizeName(migrationName), "assessment.json"), nil
}
