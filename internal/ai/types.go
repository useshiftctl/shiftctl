package ai

// AnalysisResult is what the AI returns after reasoning over the structured
// facts for a set of repositories.
type AnalysisResult struct {
	Repos         []RepoAnalysis `json:"repos"`
	KnownUnknowns []KnownUnknown `json:"known_unknowns"`
}

// RepoAnalysis is the AI's risk assessment for a single repository.
type RepoAnalysis struct {
	FullName   string   `json:"full_name"`
	RiskLevel  string   `json:"risk_level"`  // "low", "medium", or "high"
	Confidence string   `json:"confidence"`  // "confirmed" or "best_guess"
	Evidence   string   `json:"evidence"`    // specific reference to the supporting fact
	DependsOn  []string `json:"depends_on"`  // full_names of repos in this set
	Reasoning  string   `json:"reasoning"`
}

// KnownUnknown is something the AI found but could not fully assess from the
// facts alone. surfaced to the user for manual verification rather than
// silently omitted.
type KnownUnknown struct {
	Description string `json:"description"`
	// Scope is the full_name of the affected repo, or "all" if it applies
	// across the entire migration.
	Scope string `json:"scope"`
}

// Anthropic API wire types (unexported) ─────────────────────────────────────

type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     string             `json:"system"`
	Tools      []anthropicTool    `json:"tools"`
	ToolChoice anthropicToolChoice `json:"tool_choice"`
	Messages   []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type anthropicResponse struct {
	Content []anthropicContent `json:"content"`
	Error   *anthropicError    `json:"error,omitempty"`
}

type anthropicContent struct {
	Type  string      `json:"type"`
	Name  string      `json:"name,omitempty"`
	Input interface{} `json:"input,omitempty"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
