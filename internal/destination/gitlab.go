package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
)

// gitLabImpl is the GitLab implementation of Destination.
type gitLabImpl struct {
	client *http.Client
}

func newGitLab() *gitLabImpl {
	return &gitLabImpl{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *gitLabImpl) ID() string          { return "gitlab" }
func (g *gitLabImpl) DisplayName() string { return "GitLab" }
func (g *gitLabImpl) IsImplemented() bool { return true }

func (g *gitLabImpl) CredentialInstructions() string {
	return `To generate a GitLab personal access token:

  1. Go to https://gitlab.com/-/user_settings/personal_access_tokens
     (or your self-hosted GitLab instance: https://<host>/-/user_settings/personal_access_tokens)
  2. Click "Add new token"
  3. Give it a name (e.g. "shiftctl-migration")
  4. Set an expiry date far enough out to cover the migration
  5. Select these scopes: api  (includes read_api, read_user, read_repository, write_repository)
  6. Click "Create personal access token"
  7. Copy the token , it starts with "glpat-" and won't be shown again`
}

func (g *gitLabImpl) CredentialPrompt() string {
	return "GitLab personal access token (glpat-...)"
}

// ValidateCredential checks the token against the GitLab API and returns the
// authenticated username. Requires the "api" scope.
func (g *gitLabImpl) ValidateCredential(ctx context.Context, credential string) (string, error) {
	type gitlabUser struct {
		Username string `json:"username"`
	}
	var user gitlabUser
	if err := g.getJSON(ctx, credential, "https://gitlab.com/api/v4/user", &user); err != nil {
		return "", fmt.Errorf("GitLab credential validation failed: %w", err)
	}
	if user.Username == "" {
		return "", fmt.Errorf("GitLab returned an empty username , check token scopes (need: api)")
	}
	return user.Username, nil
}

// PrepareRepo creates the GitLab project (repository) and returns the details
// needed to push git content into it. Idempotent: if the project already
// exists, its details are returned rather than failing.
func (g *gitLabImpl) PrepareRepo(ctx context.Context, credential string, fact githubscanner.RepoFact) (*MigrationAction, error) {
	// Resolve the authenticated user's namespace.
	username, err := g.ValidateCredential(ctx, credential)
	if err != nil {
		return nil, err
	}

	// Build the namespace path: use the source owner if it matches the
	// authenticated user (common for personal repos); otherwise use the
	// authenticated user's namespace so we don't need group admin rights.
	namespace := username

	// Check whether the project already exists.
	projectPath := namespace + "/" + fact.Name
	existing, err := g.findProject(ctx, credential, projectPath)
	if err == nil && existing != nil {
		return g.migrationActionFromProject(existing, fact.FullName, credential), nil
	}

	// Create the project.
	type createReq struct {
		Name        string `json:"name"`
		Path        string `json:"path"`
		Description string `json:"description,omitempty"`
		Visibility  string `json:"visibility"`
		// Initialise without a README so we can push the full history in.
		InitializeWithReadme bool `json:"initialize_with_readme"`
	}
	visibility := "private"
	if fact.Visibility == "public" {
		visibility = "public"
	}

	body, _ := json.Marshal(createReq{
		Name:                 fact.Name,
		Path:                 fact.Name,
		Description:          fact.Description,
		Visibility:           visibility,
		InitializeWithReadme: false,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://gitlab.com/api/v4/projects", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating GitLab project: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Race: project appeared between our check and our create; fetch it.
		p, err := g.findProject(ctx, credential, projectPath)
		if err != nil {
			return nil, fmt.Errorf("project already exists but couldn't be fetched: %w", err)
		}
		return g.migrationActionFromProject(p, fact.FullName, credential), nil
	}

	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitLab create project returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var project gitlabProject
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return nil, fmt.Errorf("decoding GitLab project response: %w", err)
	}
	return g.migrationActionFromProject(&project, fact.FullName, credential), nil
}

// RepoExists checks whether a GitLab project exists under the authenticated
// user's namespace. Used during crash recovery.
func (g *gitLabImpl) RepoExists(ctx context.Context, credential, namespace, repoName string) (bool, error) {
	projectPath := namespace + "/" + repoName
	p, err := g.findProject(ctx, credential, projectPath)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return p != nil, nil
}

// MigrateMetadata copies webhooks and branch protection rules from the source
// facts to the GitLab project. Best-effort.
func (g *gitLabImpl) MigrateMetadata(ctx context.Context, credential string, action *MigrationAction, fact githubscanner.RepoFact) error {
	var errs []string

	// Webhooks.
	for _, wh := range fact.Webhooks {
		if err := g.createWebhook(ctx, credential, action.DestRepoID, wh); err != nil {
			errs = append(errs, fmt.Sprintf("webhook (%s): %v", wh.Domain, err))
		}
	}

	// Branch protection rules.
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

// , GitLab API types ──────────────────────────────────────────────────────────

type gitlabProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	Namespace         struct {
		Path string `json:"path"`
	} `json:"namespace"`
}

// , Internal helpers ──────────────────────────────────────────────────────────

func (g *gitLabImpl) findProject(ctx context.Context, credential, projectPath string) (*gitlabProject, error) {
	encoded := url.PathEscape(projectPath)
	apiURL := "https://gitlab.com/api/v4/projects/" + encoded
	var p gitlabProject
	if err := g.getJSON(ctx, credential, apiURL, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (g *gitLabImpl) migrationActionFromProject(p *gitlabProject, sourceFullName, credential string) *MigrationAction {
	// Build an authenticated push URL: https://oauth2:<token>@gitlab.com/...
	pushURL := strings.Replace(p.HTTPURLToRepo, "https://", "https://oauth2:"+credential+"@", 1)
	return &MigrationAction{
		RepoFullName:  sourceFullName,
		DestRepoID:    fmt.Sprintf("%d", p.ID),
		PushURL:       pushURL,
		DestWebURL:    p.WebURL,
		DestNamespace: p.Namespace.Path,
	}
}

func (g *gitLabImpl) createWebhook(ctx context.Context, credential, projectID string, wh githubscanner.WebhookFact) error {
	type hookReq struct {
		URL    string `json:"url"`
		PushEvents bool `json:"push_events"`
	}
	// We only have the domain , reconstruct a best-effort URL.
	hookURL := "https://" + wh.Domain
	body, _ := json.Marshal(hookReq{URL: hookURL, PushEvents: true})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://gitlab.com/api/v4/projects/"+url.PathEscape(projectID)+"/hooks",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", credential)
	req.Header.Set("Content-Type", "application/json")

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

func (g *gitLabImpl) applyBranchProtection(ctx context.Context, credential, projectID string, bp githubscanner.BranchProtectionFact) error {
	type protectReq struct {
		Name                      string `json:"name"`
		PushAccessLevel           int    `json:"push_access_level"`
		MergeAccessLevel          int    `json:"merge_access_level"`
		UnprotectAccessLevel      int    `json:"unprotect_access_level"`
		AllowForcePush            bool   `json:"allow_force_push"`
		CodeOwnerApprovalRequired bool   `json:"code_owner_approval_required"`
	}

	// GitLab access levels: 0=No access, 30=Developer, 40=Maintainer
	pushLevel := 40 // Maintainer only (mirrors "require PR review")
	if !bp.RequiresPRReview {
		pushLevel = 30 // Developer
	}

	body, _ := json.Marshal(protectReq{
		Name:             bp.Branch,
		PushAccessLevel:  pushLevel,
		MergeAccessLevel: 40,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://gitlab.com/api/v4/projects/"+url.PathEscape(projectID)+"/protected_branches",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil // already protected , idempotent
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (g *gitLabImpl) getJSON(ctx context.Context, credential, apiURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", credential)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid or expired GitLab token")
	case http.StatusForbidden:
		return fmt.Errorf("token lacks required scopes (need: api)")
	case http.StatusNotFound:
		return fmt.Errorf("not found: %s", apiURL)
	default:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}
