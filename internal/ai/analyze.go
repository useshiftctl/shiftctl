package ai

import (
	"context"
	"encoding/json"
	"fmt"

	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
)

// buildSystemPrompt returns a system prompt tailored to the source platform so
// the model reasons correctly about platform-specific conventions (e.g. ADO
// project/repo naming, GitLab namespace paths, Bitbucket workspace slugs).
func buildSystemPrompt(sourcePlatform string) string {
	platformName := platformDisplayName(sourcePlatform)
	return fmt.Sprintf(`You are analyzing a %s repository migration. You have been given structured facts about each repository , no raw file contents are included. Based solely on these structured facts, assess the migration risk for each repository.

For each repository assess:
- risk_level: "low" (isolated, few dependencies, no production-critical signals), "medium" (has CI/CD complexity, webhooks, or depends on other repos in this set), or "high" (production-critical systems such as auth, payments, or primary databases; complex webhook integrations; or strict branch protection indicating high-stakes systems)
- confidence: "confirmed" when you can point to a specific named fact (e.g. a specific CI file, a named webhook domain, a branch protection rule), "best_guess" when you are inferring from indirect signals (e.g. naming patterns, topic tags, or the absence of information)
- evidence: cite the specific field or value that supports your assessment. Be precise , "branch protection requires 2 approvals and enforces admins, found on main" not "has branch protection". Name the actual file, domain, or field. "config" is not specific enough.
- depends_on: list full_names of other repos in this migration set that this repo appears to depend on. Base this on naming patterns (e.g. shared prefix suggesting a monorepo split), shared topics, CI files that reference other repos, or webhook domains that suggest shared infrastructure. If uncertain, omit rather than guess.
- reasoning: one to two plain-language sentences explaining the risk assessment in terms a non-specialist reviewer could understand.

known_unknowns: items you detected the presence of but cannot fully assess from the facts alone. Examples: a webhook pointing to a bare IP address (can't verify what service), a repo with no dependency files despite having CI (unclear what it deploys), or an archived repo with active deploy keys (unclear if still in use). Surface these as items worth checking manually, not as risks.

IMPORTANT: Reason only from the facts provided. Do not speculate beyond what the structured data shows. When confident, be specific. When uncertain, say so via best_guess and explain what would resolve the uncertainty in your evidence field.`, platformName)
}

// platformDisplayName maps a canonical platform ID to its human-readable name.
func platformDisplayName(id string) string {
	switch id {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "bitbucket":
		return "Bitbucket Cloud"
	case "azuredevops":
		return "Azure DevOps"
	default:
		return id
	}
}

// toolInputSchema is the JSON Schema for the record_assessment tool. Using
// forced tool use guarantees a structured JSON response from the model.
var toolInputSchema = map[string]interface{}{
	"type": "object",
	"required": []string{"repos", "known_unknowns"},
	"properties": map[string]interface{}{
		"repos": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "object",
				"required": []string{"full_name", "risk_level", "confidence", "evidence", "depends_on", "reasoning"},
				"properties": map[string]interface{}{
					"full_name":  map[string]string{"type": "string"},
					"risk_level": map[string]interface{}{"type": "string", "enum": []string{"low", "medium", "high"}},
					"confidence": map[string]interface{}{"type": "string", "enum": []string{"confirmed", "best_guess"}},
					"evidence":   map[string]string{"type": "string"},
					"depends_on": map[string]interface{}{
						"type":  "array",
						"items": map[string]string{"type": "string"},
					},
					"reasoning": map[string]string{"type": "string"},
				},
			},
		},
		"known_unknowns": map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type":     "object",
				"required": []string{"description", "scope"},
				"properties": map[string]interface{}{
					"description": map[string]string{"type": "string"},
					"scope":       map[string]string{"type": "string"},
				},
			},
		},
	},
}

// AnalyzeRepos sends structured repository facts to the model and returns a
// risk assessment for each repository, plus a list of known unknowns.
//
// Facts are marshalled to JSON as the user message. The model is forced to
// respond via the record_assessment tool, guaranteeing a structured response
// that can be parsed directly into AnalysisResult.
func AnalyzeRepos(ctx context.Context, client *Client, sourcePlatform string, facts []githubscanner.RepoFact) (*AnalysisResult, error) {
	factsJSON, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling repo facts: %w", err)
	}

	userMessage := fmt.Sprintf(
		"Assess the migration risk for these %d repositories:\n\n```json\n%s\n```",
		len(facts), string(factsJSON),
	)

	req := anthropicRequest{
		Model:     model,
		MaxTokens: 8096,
		System:    buildSystemPrompt(sourcePlatform),
		Tools: []anthropicTool{
			{
				Name:        "record_assessment",
				Description: "Record the completed risk assessment for all repositories in this migration",
				InputSchema: toolInputSchema,
			},
		},
		ToolChoice: anthropicToolChoice{Type: "tool", Name: "record_assessment"},
		Messages: []anthropicMessage{
			{Role: "user", Content: userMessage},
		},
	}

	resp, err := client.complete(ctx, req)
	if err != nil {
		return nil, err
	}

	return extractAnalysisResult(resp)
}

// extractAnalysisResult parses the tool_use content block from the API response
// into an AnalysisResult.
func extractAnalysisResult(resp *anthropicResponse) (*AnalysisResult, error) {
	for _, block := range resp.Content {
		if block.Type != "tool_use" || block.Name != "record_assessment" {
			continue
		}
		// block.Input was decoded as interface{} , re-marshal and unmarshal
		// into the typed struct.
		raw, err := json.Marshal(block.Input)
		if err != nil {
			return nil, fmt.Errorf("re-marshalling tool input: %w", err)
		}
		var result AnalysisResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("parsing assessment result: %w", err)
		}
		return &result, nil
	}
	return nil, fmt.Errorf("Anthropic response did not include a record_assessment tool call , the model may have refused the request")
}
