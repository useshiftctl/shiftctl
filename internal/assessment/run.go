// Package assessment orchestrates the full assessment phase: scanning
// repositories, extracting structured facts, calling the AI for risk analysis,
// and persisting the results to disk.
package assessment

import (
	"context"
	"fmt"

	"github.com/shiftctl/shiftctl/internal/ai"
	"github.com/shiftctl/shiftctl/internal/migration"
	adoscanner "github.com/shiftctl/shiftctl/internal/scanner/azuredevops"
	bbscanner "github.com/shiftctl/shiftctl/internal/scanner/bitbucket"
	githubscanner "github.com/shiftctl/shiftctl/internal/scanner/github"
)

// ProgressFunc is called after each repository is scanned, with the 1-based
// current index, total count, and the repository's full name.
type ProgressFunc func(current, total int, fullName string)

// Run executes the full assessment for the named migration:
//  1. Enumerate and scan all source repositories.
//  2. Persist the raw facts to disk (crash-safe checkpoint).
//  3. Send structured facts to the AI for risk analysis.
//  4. Persist the analysis results to disk.
//  5. Advance the migration status to "planning".
//
// The token is the source platform credential; apiKey is ANTHROPIC_API_KEY.
func Run(ctx context.Context, rec *migration.Record, token, apiKey string, progress ProgressFunc) (*State, error) {
	// ── Phase 1: scan ────────────────────────────────────────────────────────
	facts, err := scanSource(ctx, rec.SourcePlatform, token, progress)
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return nil, fmt.Errorf("no repositories found — check that your credential has the required read scopes")
	}

	// ── Phase 2: persist facts (checkpoint before AI call) ───────────────────
	if err := SaveFacts(rec.Name, facts); err != nil {
		return nil, fmt.Errorf("saving scan results: %w", err)
	}

	// ── Phase 3: AI analysis ─────────────────────────────────────────────────
	client := ai.NewClient(apiKey)
	result, err := ai.AnalyzeRepos(ctx, client, facts)
	if err != nil {
		return nil, fmt.Errorf("AI analysis: %w", err)
	}

	// ── Phase 4: persist analysis ─────────────────────────────────────────────
	if err := SaveAnalysis(rec.Name, result); err != nil {
		return nil, fmt.Errorf("saving analysis results: %w", err)
	}

	// ── Phase 5: advance migration status ────────────────────────────────────
	rec.Status = migration.StatusPlanning
	if err := migration.Save(rec); err != nil {
		// Non-fatal: the assessment data is saved; the status update is cosmetic
		// at this point. Log but do not fail.
		fmt.Printf("  warning: could not update migration status: %v\n", err)
	}

	return Load(rec.Name)
}

// scanSource dispatches to the correct scanner based on the source platform ID.
func scanSource(ctx context.Context, platform, credential string, progress ProgressFunc) ([]githubscanner.RepoFact, error) {
	switch platform {
	case "github":
		scanner := githubscanner.NewScanner(credential)
		facts, err := scanner.Scan(ctx, progress)
		if err != nil {
			return nil, fmt.Errorf("scanning repositories: %w", err)
		}
		return facts, nil

	case "bitbucket":
		scanner, err := bbscanner.NewScanner(credential)
		if err != nil {
			return nil, fmt.Errorf("initialising Bitbucket scanner: %w", err)
		}
		facts, err := scanner.Scan(ctx, progress)
		if err != nil {
			return nil, fmt.Errorf("scanning repositories: %w", err)
		}
		return facts, nil

	case "azuredevops":
		scanner, err := adoscanner.NewScanner(credential)
		if err != nil {
			return nil, fmt.Errorf("initialising Azure DevOps scanner: %w", err)
		}
		facts, err := scanner.Scan(ctx, progress)
		if err != nil {
			return nil, fmt.Errorf("scanning repositories: %w", err)
		}
		return facts, nil

	default:
		return nil, fmt.Errorf("assessment is not yet supported for %q sources", platform)
	}
}

// RiskSummary returns counts of low/medium/high risk and known unknowns from
// an analysis result.
func RiskSummary(s *State) (low, medium, high, unknowns int) {
	if s.Analysis == nil {
		return
	}
	for _, r := range s.Analysis.Repos {
		switch r.RiskLevel {
		case "low":
			low++
		case "medium":
			medium++
		case "high":
			high++
		}
	}
	unknowns = len(s.Analysis.KnownUnknowns)
	return
}
