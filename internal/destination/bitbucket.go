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

// bitbucketImpl is the Bitbucket Cloud implementation of Destination.
// Credential format: "username:app_password"
type bitbucketImpl struct {
	client *http.Client
}

func newBitbucket() *bitbucketImpl {
	return &bitbucketImpl{client: &http.Client{Timeout: 30 * time.Second}}
}

func (b *bitbucketImpl) ID() string          { return "bitbucket" }
func (b *bitbucketImpl) DisplayName() string { return "Bitbucket" }
func (b *bitbucketImpl) IsImplemented() bool { return true }

func (b *bitbucketImpl) CredentialInstructions() string {
	return `To generate a Bitbucket app password for migration:

  1. Open: https://bitbucket.org/account/settings/app-passwords/
  2. Click "Create app password"
  3. Give it a label (e.g. "shiftctl-migration") and select:
       Repositories: Read, Write, Admin
       Webhooks: Read and write
       SSH keys: Read and write
  4. Click "Create" and copy the result

  Enter your credential as:  username:app_password
  (your Bitbucket username, NOT your email address)`
}

func (b *bitbucketImpl) CredentialPrompt() string {
	return "Bitbucket credential (username:app_password)"
}

func (b *bitbucketImpl) ValidateCredential(ctx context.Context, credential string) (string, error) {
	username, _, err := splitBBCredential(credential)
	if err != nil {
		return "", err
	}
	// Verify the credential is valid by hitting the user endpoint.
	type bbUser struct {
		Nickname string `json:"nickname"`
	}
	var user bbUser
	if err := b.getJSON(ctx, credential, "https://api.bitbucket.org/2.0/user", &user); err != nil {
		return "", fmt.Errorf("Bitbucket credential validation failed: %w", err)
	}
	if user.Nickname == "" {
		return "", fmt.Errorf("could not retrieve Bitbucket username , check app password scopes")
	}
	return username, nil
}

func (b *bitbucketImpl) PrepareRepo(ctx context.Context, credential string, fact githubscanner.RepoFact) (*MigrationAction, error) {
	username, _, err := splitBBCredential(credential)
	if err != nil {
		return nil, err
	}

	// Check if repo already exists.
	existing, err := b.findRepo(ctx, credential, username, fact.Name)
	if err == nil && existing != nil {
		return b.migrationAction(existing, fact.FullName, credential, username), nil
	}

	isPrivate := fact.Visibility != "public"
	type createReq struct {
		Scm     string `json:"scm"`
		IsPrivate bool `json:"is_private"`
		Description string `json:"description,omitempty"`
	}
	body, _ := json.Marshal(createReq{
		Scm:       "git",
		IsPrivate: isPrivate,
		Description: fact.Description,
	})

	apiURL := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s",
		url.PathEscape(username), url.PathEscape(fact.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	b.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating Bitbucket repository: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// 200 = already exists (Bitbucket returns 200 for existing repos on POST).
		var repo bbRepo
		if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
			return nil, err
		}
		return b.migrationAction(&repo, fact.FullName, credential, username), nil
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Bitbucket create repo returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var repo bbRepo
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, err
	}
	return b.migrationAction(&repo, fact.FullName, credential, username), nil
}

func (b *bitbucketImpl) RepoExists(ctx context.Context, credential, namespace, repoName string) (bool, error) {
	repo, err := b.findRepo(ctx, credential, namespace, repoName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return repo != nil, nil
}

func (b *bitbucketImpl) MigrateMetadata(ctx context.Context, credential string, action *MigrationAction, fact githubscanner.RepoFact) error {
	var errs []string

	for _, wh := range fact.Webhooks {
		if err := b.createWebhook(ctx, credential, action.DestRepoID, wh); err != nil {
			errs = append(errs, fmt.Sprintf("webhook (%s): %v", wh.Domain, err))
		}
	}
	// Bitbucket branch restrictions require knowing the branch name and
	// the workspace; best-effort.
	if fact.BranchProtection != nil {
		if err := b.applyBranchRestriction(ctx, credential, action.DestRepoID, *fact.BranchProtection); err != nil {
			errs = append(errs, fmt.Sprintf("branch restriction (%s): %v", fact.BranchProtection.Branch, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("metadata migration partial failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// , Bitbucket API types ───────────────────────────────────────────────────────

type bbRepo struct {
	FullName string `json:"full_name"`
	Links    struct {
		Clone []struct {
			Href string `json:"href"`
			Name string `json:"name"`
		} `json:"clone"`
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

// , Internal helpers ──────────────────────────────────────────────────────────

func splitBBCredential(credential string) (username, password string, err error) {
	u, p, ok := strings.Cut(credential, ":")
	if !ok || u == "" || p == "" {
		return "", "", fmt.Errorf("Bitbucket credential must be in the format username:app_password")
	}
	return u, p, nil
}

func (b *bitbucketImpl) findRepo(ctx context.Context, credential, workspace, repoSlug string) (*bbRepo, error) {
	apiURL := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/%s",
		url.PathEscape(workspace), url.PathEscape(repoSlug))
	var repo bbRepo
	if err := b.getJSON(ctx, credential, apiURL, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

func (b *bitbucketImpl) migrationAction(repo *bbRepo, sourceFullName, credential, workspace string) *MigrationAction {
	// Find the HTTPS clone URL and embed credentials.
	cloneURL := ""
	for _, link := range repo.Links.Clone {
		if link.Name == "https" {
			cloneURL = link.Href
			break
		}
	}
	// Embed username:app_password into URL.
	pushURL := ""
	if cloneURL != "" {
		// Replace https:// with https://user:pass@
		pushURL = strings.Replace(cloneURL, "https://", "https://"+credential+"@", 1)
	}

	return &MigrationAction{
		RepoFullName:  sourceFullName,
		DestRepoID:    repo.FullName,
		PushURL:       pushURL,
		DestWebURL:    repo.Links.HTML.Href,
		DestNamespace: workspace,
	}
}

func (b *bitbucketImpl) createWebhook(ctx context.Context, credential, repoFullName string, wh githubscanner.WebhookFact) error {
	type hookReq struct {
		Description string   `json:"description"`
		URL         string   `json:"url"`
		Active      bool     `json:"active"`
		Events      []string `json:"events"`
	}
	events := wh.Events
	if len(events) == 0 {
		events = []string{"repo:push"}
	}
	body, _ := json.Marshal(hookReq{
		Description: "Migrated by shiftctl",
		URL:         "https://" + wh.Domain,
		Active:      wh.Active,
		Events:      events,
	})

	apiURL := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/hooks",
		url.PathEscape(repoFullName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	b.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
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

func (b *bitbucketImpl) applyBranchRestriction(ctx context.Context, credential, repoFullName string, bp githubscanner.BranchProtectionFact) error {
	type restriction struct {
		Kind    string `json:"kind"`
		Pattern string `json:"pattern"`
		Value   int    `json:"value,omitempty"`
	}

	var reqs []restriction
	if bp.RequiresPRReview {
		count := bp.RequiredApprovalCount
		if count < 1 {
			count = 1
		}
		reqs = append(reqs, restriction{
			Kind:    "require_approvals_to_merge",
			Pattern: bp.Branch,
			Value:   count,
		})
	}
	if bp.EnforcesAdmins {
		reqs = append(reqs, restriction{
			Kind:    "push",
			Pattern: bp.Branch,
		})
	}

	apiURL := fmt.Sprintf("https://api.bitbucket.org/2.0/repositories/%s/branch-restrictions",
		url.PathEscape(repoFullName))

	for _, r := range reqs {
		body, _ := json.Marshal(r)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		b.setAuth(req, credential)
		req.Header.Set("Content-Type", "application/json")

		resp, err := b.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d applying %s restriction", resp.StatusCode, r.Kind)
		}
	}
	return nil
}

func (b *bitbucketImpl) getJSON(ctx context.Context, credential, apiURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	b.setAuth(req, credential)

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid Bitbucket credentials")
	case http.StatusForbidden:
		return fmt.Errorf("app password lacks required scopes")
	case http.StatusNotFound:
		return fmt.Errorf("not found: %s", apiURL)
	default:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Bitbucket API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (b *bitbucketImpl) setAuth(req *http.Request, credential string) {
	username, password, _ := splitBBCredential(credential)
	req.SetBasicAuth(username, password)
	req.Header.Set("User-Agent", "shiftctl/0.1.0")
}
