package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
)

// gitHubImpl is the GitHub implementation of Destination.
type gitHubImpl struct {
	client *http.Client
}

func newGitHub() *gitHubImpl {
	return &gitHubImpl{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *gitHubImpl) ID() string          { return "github" }
func (g *gitHubImpl) DisplayName() string { return "GitHub" }
func (g *gitHubImpl) IsImplemented() bool { return true }

func (g *gitHubImpl) CredentialInstructions() string {
	return `To generate a GitHub personal access token with write access:

  Classic token (simpler):
  1. Go to https://github.com/settings/tokens/new
  2. Give it a name (e.g. "shiftctl-migration")
  3. Set an expiry date
  4. Select the "repo" scope (full control of private repositories)
  5. Click "Generate token" and copy the result — it starts with "ghp_"

  Fine-grained token (more secure):
  1. Go to https://github.com/settings/personal-access-tokens/new
  2. Set repository access to "All repositories" (or select specific ones)
  3. Under Repository permissions, grant:
       Contents: Read and write
       Metadata: Read-only
       Webhooks: Read and write
       Administration: Read and write
  4. Click "Generate token" — it starts with "github_pat_"`
}

func (g *gitHubImpl) CredentialPrompt() string {
	return "GitHub personal access token (ghp_... or github_pat_...)"
}

// ValidateCredential checks the token against the GitHub API and returns the
// authenticated username.
func (g *gitHubImpl) ValidateCredential(ctx context.Context, credential string) (string, error) {
	type ghUser struct {
		Login string `json:"login"`
	}
	var user ghUser
	if err := g.getJSON(ctx, credential, "https://api.github.com/user", &user); err != nil {
		return "", fmt.Errorf("GitHub credential validation failed: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("GitHub returned an empty username — check token scopes (need: repo)")
	}
	return user.Login, nil
}

// PrepareRepo creates the GitHub repository and returns the details needed to
// push git content into it. Idempotent: if the repo already exists it is
// returned rather than failing.
func (g *gitHubImpl) PrepareRepo(ctx context.Context, credential string, fact githubscanner.RepoFact) (*MigrationAction, error) {
	username, err := g.ValidateCredential(ctx, credential)
	if err != nil {
		return nil, err
	}

	// Check if repo already exists.
	existing, err := g.findRepo(ctx, credential, username, fact.Name)
	if err == nil && existing != nil {
		return g.migrationActionFromRepo(existing, fact.FullName, credential), nil
	}

	// Create the repo.
	type createReq struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Private     bool   `json:"private"`
		AutoInit    bool   `json:"auto_init"`
	}

	body, _ := json.Marshal(createReq{
		Name:        fact.Name,
		Description: fact.Description,
		Private:     fact.Visibility != "public",
		AutoInit:    false, // push full history; don't seed with a README
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.github.com/user/repos", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	g.setHeaders(req, credential)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating GitHub repository: %w", err)
	}
	defer resp.Body.Close()

	// 422 Unprocessable Entity = repo already exists.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		existing, err := g.findRepo(ctx, credential, username, fact.Name)
		if err != nil {
			return nil, fmt.Errorf("repo already exists but couldn't be fetched: %w", err)
		}
		return g.migrationActionFromRepo(existing, fact.FullName, credential), nil
	}

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub create repo returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var repo ghRepo
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, fmt.Errorf("decoding GitHub repo response: %w", err)
	}
	return g.migrationActionFromRepo(&repo, fact.FullName, credential), nil
}

// RepoExists checks whether a repository exists under the authenticated user's
// account. Used during crash recovery.
func (g *gitHubImpl) RepoExists(ctx context.Context, credential, namespace, repoName string) (bool, error) {
	repo, err := g.findRepo(ctx, credential, namespace, repoName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return repo != nil, nil
}

// MigrateMetadata copies webhooks and branch protection rules to the GitHub
// repository. Best-effort.
func (g *gitHubImpl) MigrateMetadata(ctx context.Context, credential string, action *MigrationAction, fact githubscanner.RepoFact) error {
	var errs []string

	for _, wh := range fact.Webhooks {
		if err := g.createWebhook(ctx, credential, action.DestRepoID, wh); err != nil {
			errs = append(errs, fmt.Sprintf("webhook (%s): %v", wh.Domain, err))
		}
	}

	if fact.BranchProtection != nil {
		if err := g.applyBranchProtection(ctx, credential, action.DestRepoID, *fact.BranchProtection); err != nil {
			errs = append(errs, fmt.Sprintf("branch protection (%s): %v", fact.BranchProtection.Branch, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("metadata migration partial failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// — GitHub API types ──────────────────────────────────────────────────────────

type ghRepo struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	CloneURL string `json:"clone_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// — Internal helpers ──────────────────────────────────────────────────────────

func (g *gitHubImpl) findRepo(ctx context.Context, credential, owner, name string) (*ghRepo, error) {
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, name)
	var repo ghRepo
	if err := g.getJSON(ctx, credential, apiURL, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

func (g *gitHubImpl) migrationActionFromRepo(r *ghRepo, sourceFullName, credential string) *MigrationAction {
	// Build authenticated push URL: https://<token>@github.com/owner/repo.git
	pushURL := strings.Replace(r.CloneURL, "https://", "https://"+credential+"@", 1)
	return &MigrationAction{
		RepoFullName:  sourceFullName,
		DestRepoID:    r.FullName, // "owner/repo" — used in API paths
		PushURL:       pushURL,
		DestWebURL:    r.HTMLURL,
		DestNamespace: r.Owner.Login,
	}
}

func (g *gitHubImpl) createWebhook(ctx context.Context, credential, repoFullName string, wh githubscanner.WebhookFact) error {
	type hookConfig struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
	}
	type hookReq struct {
		Name   string     `json:"name"`
		Config hookConfig `json:"config"`
		Events []string   `json:"events"`
		Active bool       `json:"active"`
	}

	hookURL := "https://" + wh.Domain
	events := wh.Events
	if len(events) == 0 {
		events = []string{"push"}
	}

	body, _ := json.Marshal(hookReq{
		Name:   "web",
		Config: hookConfig{URL: hookURL, ContentType: "json"},
		Events: events,
		Active: wh.Active,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.github.com/repos/%s/hooks", repoFullName),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.setHeaders(req, credential)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (g *gitHubImpl) applyBranchProtection(ctx context.Context, credential, repoFullName string, bp githubscanner.BranchProtectionFact) error {
	type requiredPRReviews struct {
		RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
		DismissStaleReviews          bool `json:"dismiss_stale_reviews"`
	}
	type protectReq struct {
		RequiredStatusChecks       *struct{ Strict bool; Contexts []string } `json:"required_status_checks"`
		EnforceAdmins              bool                                      `json:"enforce_admins"`
		RequiredPullRequestReviews *requiredPRReviews                        `json:"required_pull_request_reviews"`
		Restrictions               *struct{}                                 `json:"restrictions"`
	}

	var pr *requiredPRReviews
	if bp.RequiresPRReview {
		count := bp.RequiredApprovalCount
		if count < 1 {
			count = 1
		}
		pr = &requiredPRReviews{RequiredApprovingReviewCount: count}
	}

	body, _ := json.Marshal(protectReq{
		EnforceAdmins:              bp.EnforcesAdmins,
		RequiredPullRequestReviews: pr,
		Restrictions:               nil, // no push restrictions — would need team/user lists
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("https://api.github.com/repos/%s/branches/%s/protection", repoFullName, bp.Branch),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.setHeaders(req, credential)
	req.Header.Set("Accept", "application/vnd.github.luke-cage-preview+json")

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (g *gitHubImpl) getJSON(ctx context.Context, credential, apiURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	g.setHeaders(req, credential)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid or expired GitHub token")
	case http.StatusForbidden:
		return fmt.Errorf("token lacks required scopes (need: repo)")
	case http.StatusNotFound:
		return fmt.Errorf("not found: %s", apiURL)
	default:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (g *gitHubImpl) setHeaders(req *http.Request, credential string) {
	req.Header.Set("Authorization", "token "+credential)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "shiftctl/0.1.0")
	req.Header.Set("Content-Type", "application/json")
}
