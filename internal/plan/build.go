package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/useshiftctl/shiftctl/internal/ai"
	"github.com/useshiftctl/shiftctl/internal/assessment"
)

// Build converts an assessment into a staged Plan. The staging rules follow
// the spec exactly:
//
//   Stage 1 , confidence=confirmed AND risk=low
//   Stage 2 , confidence=best_guess (any risk) OR risk=medium (any confidence)
//   Stage 3 , confidence=confirmed AND risk=high
//
// Repos with scan errors or missing from the AI analysis are placed in Stage 2
// (treated as uncertain). Within each stage, repos are ordered so that
// dependencies come before the repos that depend on them.
func Build(state *assessment.State) (*Plan, error) {
	if state.Analysis == nil {
		return nil, fmt.Errorf("assessment has no analysis , run 'shiftctl assess %s' first", state.MigrationName)
	}

	// Index the AI analysis by full_name for fast lookup.
	byName := make(map[string]ai.RepoAnalysis, len(state.Analysis.Repos))
	for _, r := range state.Analysis.Repos {
		byName[r.FullName] = r
	}

	// Bucket repos into stages.
	buckets := [4][]PlannedRepo{} // index 1,2,3 used; 0 unused

	for _, fact := range state.Facts {
		analysis, found := byName[fact.FullName]
		if !found {
			// AI did not return an assessment for this repo , place in Stage 2.
			reason := "not assessed by AI"
			if fact.ScanError != "" {
				reason = "scan error: " + fact.ScanError
			}
			buckets[2] = append(buckets[2], PlannedRepo{
				FullName:  fact.FullName,
				ScanError: reason,
			})
			continue
		}

		stage := assignStage(analysis)
		buckets[stage] = append(buckets[stage], PlannedRepo{
			FullName:   analysis.FullName,
			RiskLevel:  analysis.RiskLevel,
			Confidence: analysis.Confidence,
			Evidence:   analysis.Evidence,
			DependsOn:  analysis.DependsOn,
			Reasoning:  analysis.Reasoning,
		})
	}

	// Build the final stages, skipping any that are empty.
	var stages []Stage
	for i := 1; i <= 3; i++ {
		repos := buckets[i]
		if len(repos) == 0 {
			continue
		}
		sorted := topoSort(repos)
		stages = append(stages, Stage{
			Number: i,
			Label:  stageLabels[i],
			Reason: stageReasons[i],
			Repos:  sorted,
		})
	}

	if len(stages) == 0 {
		return nil, fmt.Errorf("no repositories to plan , the assessment may be empty")
	}

	// Re-number stages sequentially in case any bucket was empty.
	// (e.g. if there are no high-risk repos, stage 3 becomes stage 2.)
	// Per spec: stage numbers shown to the user reflect the actual sequence,
	// not the abstract bucket number.
	for i := range stages {
		stages[i].Number = i + 1
	}

	return &Plan{
		MigrationName: state.MigrationName,
		CreatedAt:     time.Now().UTC(),
		Stages:        stages,
		KnownUnknowns: state.Analysis.KnownUnknowns,
	}, nil
}

// assignStage maps a repo's risk level and confidence to a stage bucket.
func assignStage(r ai.RepoAnalysis) int {
	switch {
	case r.Confidence == "confirmed" && r.RiskLevel == "low":
		return 1
	case r.Confidence == "confirmed" && r.RiskLevel == "high":
		return 3
	default:
		// best_guess at any risk level, or confirmed medium risk.
		return 2
	}
}

// topoSort orders repos within a stage so that within-stage dependencies come
// before the repos that depend on them. Uses Kahn's algorithm; if a cycle is
// detected (which should not happen in practice), the cyclic repos are appended
// alphabetically rather than silently dropped.
func topoSort(repos []PlannedRepo) []PlannedRepo {
	// Build a set of full_names present in this stage for fast lookup.
	inStage := make(map[string]bool, len(repos))
	for _, r := range repos {
		inStage[r.FullName] = true
	}

	byName := make(map[string]PlannedRepo, len(repos))
	for _, r := range repos {
		byName[r.FullName] = r
	}

	// Compute in-degree: how many of this repo's depends_on are also in
	// this stage.
	inDegree := make(map[string]int, len(repos))
	for _, r := range repos {
		inDegree[r.FullName] = 0
	}
	for _, r := range repos {
		for _, dep := range r.DependsOn {
			if inStage[dep] {
				inDegree[r.FullName]++
			}
		}
	}

	// Seed the queue with repos that have no in-stage dependencies.
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	// Reverse adjacency: dep → repos that depend on it (within this stage).
	revAdj := make(map[string][]string)
	for _, r := range repos {
		for _, dep := range r.DependsOn {
			if inStage[dep] {
				revAdj[dep] = append(revAdj[dep], r.FullName)
			}
		}
	}

	result := make([]PlannedRepo, 0, len(repos))
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		result = append(result, byName[curr])

		dependents := revAdj[curr]
		sort.Strings(dependents)
		for _, dep := range dependents {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = insertSorted(queue, dep)
			}
		}
	}

	// Append any cycle members alphabetically.
	if len(result) < len(repos) {
		included := make(map[string]bool, len(result))
		for _, r := range result {
			included[r.FullName] = true
		}
		var remaining []PlannedRepo
		for _, r := range repos {
			if !included[r.FullName] {
				remaining = append(remaining, r)
			}
		}
		sort.Slice(remaining, func(i, j int) bool {
			return remaining[i].FullName < remaining[j].FullName
		})
		result = append(result, remaining...)
	}

	return result
}

// insertSorted inserts s into a sorted string slice, maintaining sort order.
func insertSorted(ss []string, s string) []string {
	i := sort.SearchStrings(ss, s)
	ss = append(ss, "")
	copy(ss[i+1:], ss[i:])
	ss[i] = s
	return ss
}
