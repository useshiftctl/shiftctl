package verifier

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/useshiftctl/shiftctl/internal/migration"
)

// SavedReport is the on-disk representation of a verification run.
type SavedReport struct {
	StageNumber int         `json:"stage_number"`
	SavedAt     time.Time   `json:"saved_at"`
	Items       []SavedItem `json:"items"`
}

// SavedItem is one entry in a saved verification report.
type SavedItem struct {
	FullName   string `json:"full_name"`
	OK         bool   `json:"ok"`
	DestWebURL string `json:"dest_web_url,omitempty"`
	Issue      string `json:"issue,omitempty"`
}

// SaveReport writes the verification report to verify-stage-N.json.
func SaveReport(migrationName string, report *Report) error {
	saved := &SavedReport{
		StageNumber: report.StageNumber,
		SavedAt:     time.Now().UTC(),
	}
	for _, it := range report.Items {
		saved.Items = append(saved.Items, SavedItem{
			FullName:   it.FullName,
			OK:         it.OK,
			DestWebURL: it.DestWebURL,
			Issue:      it.Issue,
		})
	}

	base, err := migration.ShiftctlDir()
	if err != nil {
		return err
	}
	path := filepath.Join(base, "migrations", migration.SanitizeName(migrationName),
		fmt.Sprintf("verify-stage-%d.json", report.StageNumber))

	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling verify report: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("writing verify report: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadReport reads the saved verification report for a stage. Returns nil, nil
// if no report exists yet.
func LoadReport(migrationName string, stageNumber int) (*SavedReport, error) {
	base, err := migration.ShiftctlDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(base, "migrations", migration.SanitizeName(migrationName),
		fmt.Sprintf("verify-stage-%d.json", stageNumber))

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var r SavedReport
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
