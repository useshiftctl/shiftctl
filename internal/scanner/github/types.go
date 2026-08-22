package github

import "time"

// RepoFact is the structured, redacted set of facts extracted from a single
// GitHub repository. This is what gets written to disk and sent to the AI for
// analysis — no raw file contents are included.
type RepoFact struct {
	Owner           string    `json:"owner"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Description     string    `json:"description,omitempty"`
	PrimaryLanguage string    `json:"primary_language,omitempty"`
	Topics          []string  `json:"topics,omitempty"`
	SizeKB          int       `json:"size_kb"`
	Visibility      string    `json:"visibility"` // "public" or "private"
	IsArchived      bool      `json:"is_archived,omitempty"`
	IsFork          bool      `json:"is_fork,omitempty"`
	IsEmpty         bool      `json:"is_empty,omitempty"`
	DefaultBranch   string    `json:"default_branch,omitempty"`
	BranchCount     int       `json:"branch_count"`
	LastPushedAt    time.Time `json:"last_pushed_at"`
	OpenIssueCount  int       `json:"open_issue_count"`

	HasCI   bool     `json:"has_ci,omitempty"`
	CIFiles []string `json:"ci_files,omitempty"` // file names only, not contents

	Webhooks         []WebhookFact         `json:"webhooks,omitempty"`
	DeployKeys       []DeployKeyFact       `json:"deploy_keys,omitempty"`
	BranchProtection *BranchProtectionFact `json:"branch_protection,omitempty"`
	DependencyFiles  []DependencyFileFact  `json:"dependency_files,omitempty"`

	// ScanError is set when a non-fatal error occurred while scanning this
	// repo. The repo is included in the assessment with whatever facts were
	// successfully gathered.
	ScanError string `json:"scan_error,omitempty"`
}

// WebhookFact records what a webhook does, without including any credentials
// from the webhook URL.
type WebhookFact struct {
	Events []string `json:"events"`
	Active bool     `json:"active"`
	// Domain is the host extracted from the webhook URL after redaction —
	// e.g. "hooks.slack.com" or "api.pagerduty.com".
	Domain string `json:"domain,omitempty"`
}

// DeployKeyFact records the presence and read/write nature of a deploy key.
type DeployKeyFact struct {
	Title    string `json:"title"`
	ReadOnly bool   `json:"read_only"`
}

// BranchProtectionFact records the protection rules on the default branch.
type BranchProtectionFact struct {
	Branch                string   `json:"branch"`
	RequiresPRReview      bool     `json:"requires_pr_review"`
	RequiredApprovalCount int      `json:"required_approval_count,omitempty"`
	RequiredStatusChecks  []string `json:"required_status_checks,omitempty"`
	EnforcesAdmins        bool     `json:"enforces_admins,omitempty"`
}

// DependencyFileFact records a dependency manifest that was found, with parsed
// dependency names (not versions or the raw file content).
type DependencyFileFact struct {
	File         string   `json:"file"`          // e.g. "go.mod"
	Dependencies []string `json:"dependencies"`  // names only, no versions
}

// — GitHub REST API response shapes (unexported, used only during scanning) —

type githubAPIRepo struct {
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Description   string    `json:"description"`
	Private       bool      `json:"private"`
	Language      string    `json:"language"`
	Topics        []string  `json:"topics"`
	Size          int       `json:"size"`
	DefaultBranch string    `json:"default_branch"`
	Archived      bool      `json:"archived"`
	Fork          bool      `json:"fork"`
	PushedAt      time.Time `json:"pushed_at"`
	OpenIssues    int       `json:"open_issues_count"`
}

type githubAPIHook struct {
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
	Events []string `json:"events"`
	Active bool     `json:"active"`
}

type githubAPIKey struct {
	Title    string `json:"title"`
	ReadOnly bool   `json:"read_only"`
}

type githubAPIBranch struct {
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
}

type githubAPIProtection struct {
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
	RequiredStatusChecks *struct {
		Contexts []string `json:"contexts"`
	} `json:"required_status_checks"`
	EnforceAdmins *struct {
		Enabled bool `json:"enabled"`
	} `json:"enforce_admins"`
}

type githubAPIContent struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`   // "file" or "dir"
	Content  string `json:"content"` // base64-encoded, only for files
	Encoding string `json:"encoding"`
}
