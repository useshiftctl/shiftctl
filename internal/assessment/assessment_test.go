package assessment_test

import (
	"testing"

	"github.com/useshiftctl/shiftctl/internal/ai"
	"github.com/useshiftctl/shiftctl/internal/assessment"
)

func TestRiskSummary(t *testing.T) {
	t.Run("nil analysis returns zeros", func(t *testing.T) {
		s := &assessment.State{}
		low, medium, high, unknowns := assessment.RiskSummary(s)
		if low != 0 || medium != 0 || high != 0 || unknowns != 0 {
			t.Errorf("expected all zeros, got low=%d medium=%d high=%d unknowns=%d", low, medium, high, unknowns)
		}
	})

	t.Run("counts risk levels correctly", func(t *testing.T) {
		s := &assessment.State{
			Analysis: &ai.AnalysisResult{
				Repos: []ai.RepoAnalysis{
					{FullName: "org/a", RiskLevel: "low"},
					{FullName: "org/b", RiskLevel: "low"},
					{FullName: "org/c", RiskLevel: "medium"},
					{FullName: "org/d", RiskLevel: "high"},
					{FullName: "org/e", RiskLevel: "high"},
					{FullName: "org/f", RiskLevel: "high"},
				},
				KnownUnknowns: []ai.KnownUnknown{
					{Description: "monorepo detected", Scope: "org/mono"},
					{Description: "custom CI scripts", Scope: "org/ci"},
				},
			},
		}

		low, medium, high, unknowns := assessment.RiskSummary(s)

		if low != 2 {
			t.Errorf("low: got %d, want 2", low)
		}
		if medium != 1 {
			t.Errorf("medium: got %d, want 1", medium)
		}
		if high != 3 {
			t.Errorf("high: got %d, want 3", high)
		}
		if unknowns != 2 {
			t.Errorf("unknowns: got %d, want 2", unknowns)
		}
	})

	t.Run("empty repos returns zeros", func(t *testing.T) {
		s := &assessment.State{
			Analysis: &ai.AnalysisResult{},
		}
		low, medium, high, unknowns := assessment.RiskSummary(s)
		if low != 0 || medium != 0 || high != 0 || unknowns != 0 {
			t.Errorf("expected all zeros, got low=%d medium=%d high=%d unknowns=%d", low, medium, high, unknowns)
		}
	})
}
