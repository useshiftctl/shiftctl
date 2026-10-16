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

// azureDevOpsImpl is the Azure DevOps implementation of Destination.
// Credential format: "organization:personal_access_token"
type azureDevOpsImpl struct {
	client *http.Client
}

func newAzureDevOps() *azureDevOpsImpl {
	return &azureDevOpsImpl{client: &http.Client{Timeout: 30 * time.Second}}
}

func (a *azureDevOpsImpl) ID() string          { return "azuredevops" }
func (a *azureDevOpsImpl) DisplayName() string { return "Azure DevOps" }
func (a *azureDevOpsImpl) IsImplemented() bool { return true }

func (a *azureDevOpsImpl) CredentialInstructions() string {
	return `To generate an Azure DevOps PAT for migration:

  1. Open: https://dev.azure.com/<your-org>/_usersSettings/tokens
  2. Click "New Token"
  3. Give it a name (e.g. "shiftctl-migration") and select:
       Code: Read & write
       Build: Read & execute (optional, for pipeline metadata)
  4. Click "Create" and copy the token

  Enter your credential as:  organization:personal_access_token
  (your Azure DevOps organization name, e.g. "contoso")`
}

func (a *azureDevOpsImpl) CredentialPrompt() string {
	return "Azure DevOps credential (organization:personal_access_token)"
}

func (a *azureDevOpsImpl) ValidateCredential(ctx context.Context, credential string) (string, error) {
	_, _, err := splitADOCredential(credential)
	if err != nil {
		return "", err
	}
	type profile struct {
		DisplayName string `json:"displayName"`
		EmailAddress string `json:"emailAddress"`
	}
	var p profile
	if err := a.getJSON(ctx, credential, "https://app.vssps.visualstudio.com/_apis/profile/me?api-version=7.1", &p); err != nil {
		return "", fmt.Errorf("Azure DevOps credential validation failed: %w", err)
	}
	if p.DisplayName == "" && p.EmailAddress == "" {
		return "", fmt.Errorf("could not retrieve Azure DevOps profile — check PAT scopes")
	}
	name := p.DisplayName
	if name == "" {
		name = p.EmailAddress
	}
	return name, nil
}

func (a *azureDevOpsImpl) PrepareRepo(ctx context.Context, credential string, fact githubscanner.RepoFact) (*MigrationAction, error) {
	org, _, err := splitADOCredential(credential)
	if err != nil {
		return nil, err
	}

	// Azure DevOps FullName is "project/repo".
	project, repoName := adoSplitFullName(fact.FullName)
	if project == "" {
		project = org
		repoName = fact.Name
	}

	// Check if the repository already exists.
	existing, err := a.findRepo(ctx, credential, org, project, repoName)
	if err == nil && existing != nil {
		return a.migrationAction(existing, fact.FullName, credential, org, project), nil
	}

	// Ensure the project exists (create if needed).
	if err := a.ensureProject(ctx, credential, org, project); err != nil {
		return nil, fmt.Errorf("ensuring project %q: %w", project, err)
	}

	// Create the repository.
	type createReq struct {
		Name    string      `json:"name"`
		Project adoProject  `json:"project"`
	}
	proj, err := a.getProject(ctx, credential, org, project)
	if err != nil {
		return nil, fmt.Errorf("fetching project %q: %w", project, err)
	}

	body, _ := json.Marshal(createReq{
		Name:    repoName,
		Project: *proj,
	})

	apiURL := fmt.Sprintf("https://dev.azure.com/%s/%s/_apis/git/repositories?api-version=7.1",
		url.PathEscape(org), url.PathEscape(project))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating Azure DevOps repository: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// Repository already exists — fetch it.
		existing, err := a.findRepo(ctx, credential, org, project, repoName)
		if err != nil {
			return nil, fmt.Errorf("repository already exists but could not be fetched: %w", err)
		}
		return a.migrationAction(existing, fact.FullName, credential, org, project), nil
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Azure DevOps create repo returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var repo adoRepo
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		return nil, err
	}
	return a.migrationAction(&repo, fact.FullName, credential, org, project), nil
}

func (a *azureDevOpsImpl) RepoExists(ctx context.Context, credential, namespace, repoName string) (bool, error) {
	org, _, err := splitADOCredential(credential)
	if err != nil {
		return false, err
	}
	project, slug := adoSplitFullName(repoName)
	if project == "" {
		project = namespace
		slug = repoName
	}
	repo, err := a.findRepo(ctx, credential, org, project, slug)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return repo != nil, nil
}

func (a *azureDevOpsImpl) MigrateMetadata(ctx context.Context, credential string, action *MigrationAction, fact githubscanner.RepoFact) error {
	org, _, err := splitADOCredential(credential)
	if err != nil {
		return err
	}

	var errs []string

	for _, wh := range fact.Webhooks {
		if err := a.createWebhook(ctx, credential, org, action.DestRepoID, wh); err != nil {
			errs = append(errs, fmt.Sprintf("webhook (%s): %v", wh.Domain, err))
		}
	}

	if fact.BranchProtection != nil {
		if err := a.applyBranchPolicy(ctx, credential, org, action.DestNamespace, action.DestRepoID, *fact.BranchProtection); err != nil {
			errs = append(errs, fmt.Sprintf("branch policy (%s): %v", fact.BranchProtection.Branch, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("metadata migration partial failures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// — Azure DevOps API types ─────────────────────────────────────────────────────

type adoRepo struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Project adoProject `json:"project"`
	RemoteURL string   `json:"remoteUrl"`
	WebURL    string   `json:"webUrl"`
}

type adoProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// — Internal helpers ───────────────────────────────────────────────────────────

func splitADOCredential(credential string) (org, pat string, err error) {
	o, p, ok := strings.Cut(credential, ":")
	if !ok || o == "" || p == "" {
		return "", "", fmt.Errorf("Azure DevOps credential must be in the format organization:personal_access_token")
	}
	return o, p, nil
}

// adoSplitFullName splits "project/repo" into (project, repo).
// Returns ("", fullName) if there is no slash.
func adoSplitFullName(fullName string) (project, repo string) {
	p, r, ok := strings.Cut(fullName, "/")
	if !ok {
		return "", fullName
	}
	return p, r
}

func (a *azureDevOpsImpl) findRepo(ctx context.Context, credential, org, project, repoName string) (*adoRepo, error) {
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/%s/_apis/git/repositories/%s?api-version=7.1",
		url.PathEscape(org), url.PathEscape(project), url.PathEscape(repoName))
	var repo adoRepo
	if err := a.getJSON(ctx, credential, apiURL, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

func (a *azureDevOpsImpl) getProject(ctx context.Context, credential, org, project string) (*adoProject, error) {
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/projects/%s?api-version=7.1",
		url.PathEscape(org), url.PathEscape(project))
	var proj adoProject
	if err := a.getJSON(ctx, credential, apiURL, &proj); err != nil {
		return nil, err
	}
	return &proj, nil
}

func (a *azureDevOpsImpl) ensureProject(ctx context.Context, credential, org, project string) error {
	// If the project already exists, getProject will succeed.
	_, err := a.getProject(ctx, credential, org, project)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "not found") {
		return err
	}

	// Create the project.
	type createProj struct {
		Name        string     `json:"name"`
		Visibility  string     `json:"visibility"`
		Capabilities struct {
			VersionControl  map[string]string `json:"versioncontrol"`
			ProcessTemplate map[string]string `json:"processTemplate"`
		} `json:"capabilities"`
	}
	payload := createProj{
		Name:       project,
		Visibility: "private",
	}
	payload.Capabilities.VersionControl = map[string]string{"sourceControlType": "Git"}
	payload.Capabilities.ProcessTemplate = map[string]string{"templateTypeId": "6b724908-ef14-45cf-84f8-768b5384da45"} // Agile

	body, _ := json.Marshal(payload)
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/projects?api-version=7.1", url.PathEscape(org))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	a.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create project returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (a *azureDevOpsImpl) migrationAction(repo *adoRepo, sourceFullName, credential, org, project string) *MigrationAction {
	_, pat, _ := splitADOCredential(credential)

	// Build authenticated push URL.
	remoteURL := repo.RemoteURL
	if remoteURL == "" {
		remoteURL = fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s", org, project, repo.Name)
	}
	pushURL := strings.Replace(remoteURL, "https://", "https://"+org+":"+pat+"@", 1)

	webURL := repo.WebURL
	if webURL == "" {
		webURL = fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s", org, project, repo.Name)
	}

	return &MigrationAction{
		RepoFullName:  sourceFullName,
		DestRepoID:    repo.ID,
		PushURL:       pushURL,
		DestWebURL:    webURL,
		DestNamespace: project,
	}
}

func (a *azureDevOpsImpl) createWebhook(ctx context.Context, credential, org, repoID string, wh githubscanner.WebhookFact) error {
	type publisherInputs struct {
		Repository string `json:"repository"`
	}
	type consumerInputs struct {
		URL string `json:"url"`
	}
	type hookReq struct {
		PublisherID     string          `json:"publisherId"`
		EventType       string          `json:"eventType"`
		ResourceVersion string          `json:"resourceVersion"`
		ConsumerID      string          `json:"consumerId"`
		ConsumerActionID string         `json:"consumerActionId"`
		PublisherInputs publisherInputs `json:"publisherInputs"`
		ConsumerInputs  consumerInputs  `json:"consumerInputs"`
	}

	body, _ := json.Marshal(hookReq{
		PublisherID:      "tfs",
		EventType:        "git.push",
		ResourceVersion:  "1.0",
		ConsumerID:       "webHooks",
		ConsumerActionID: "httpRequest",
		PublisherInputs:  publisherInputs{Repository: repoID},
		ConsumerInputs:   consumerInputs{URL: "https://" + wh.Domain},
	})

	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/hooks/subscriptions?api-version=7.1", url.PathEscape(org))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	a.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (a *azureDevOpsImpl) applyBranchPolicy(ctx context.Context, credential, org, project, repoID string, bp githubscanner.BranchProtectionFact) error {
	if !bp.RequiresPRReview && !bp.EnforcesAdmins {
		return nil
	}

	type policyTypeRef struct {
		ID string `json:"id"`
	}
	type scopeEntry struct {
		RepositoryID string `json:"repositoryId"`
		RefName      string `json:"refName"`
		MatchKind    string `json:"matchKind"`
	}
	type policySettings struct {
		MinimumApproverCount int          `json:"minimumApproverCount"`
		CreatorVoteCounts    bool         `json:"creatorVoteCounts"`
		Scope                []scopeEntry `json:"scope"`
	}
	type policyReq struct {
		IsEnabled  bool           `json:"isEnabled"`
		IsBlocking bool           `json:"isBlocking"`
		Type       policyTypeRef  `json:"type"`
		Settings   policySettings `json:"settings"`
	}

	count := bp.RequiredApprovalCount
	if count < 1 {
		count = 1
	}
	refName := "refs/heads/" + bp.Branch

	payload := policyReq{
		IsEnabled:  true,
		IsBlocking: true,
		// Minimum number of reviewers policy type ID (consistent across ADO instances).
		Type: policyTypeRef{ID: "fa4e907d-c16b-452d-8106-7efa0cb84489"},
		Settings: policySettings{
			MinimumApproverCount: count,
			CreatorVoteCounts:    false,
			Scope: []scopeEntry{
				{RepositoryID: repoID, RefName: refName, MatchKind: "Exact"},
			},
		},
	}

	body, _ := json.Marshal(payload)
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/%s/_apis/policy/configurations?api-version=7.1",
		url.PathEscape(org), url.PathEscape(project))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	a.setAuth(req, credential)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d applying branch policy: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (a *azureDevOpsImpl) getJSON(ctx context.Context, credential, apiURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	a.setAuth(req, credential)

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid Azure DevOps credentials")
	case http.StatusForbidden:
		return fmt.Errorf("PAT lacks required scopes")
	case http.StatusNotFound:
		return fmt.Errorf("not found: %s", apiURL)
	default:
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Azure DevOps API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (a *azureDevOpsImpl) setAuth(req *http.Request, credential string) {
	_, pat, _ := splitADOCredential(credential)
	req.SetBasicAuth("", pat)
	req.Header.Set("User-Agent", "shiftctl/0.1.0")
}
