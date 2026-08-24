// Package azuredevops implements a repository scanner for Azure DevOps using
// the Azure DevOps REST API v7.1. It produces the same RepoFact type as the
// other scanners so the rest of the pipeline is platform-agnostic.
//
// Credential format: "organization:personal_access_token"
// ADO auth uses HTTP Basic with an empty username and the PAT as the password.
package azuredevops

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shiftctl/shiftctl/internal/redact"
	githubscanner "github.com/shiftctl/shiftctl/internal/scanner/github"
)

const (
	apiVersion = "7.1"
	userAgent  = "shiftctl/0.1.0"
	pageTop    = 200
)

var errNotFound = errors.New("not found")

// Scanner reads repository facts from Azure DevOps.
// Credential must be in the form "organization:personal_access_token".
type Scanner struct {
	org    string
	pat    string
	client *http.Client
}

// NewScanner returns a Scanner from a combined "org:pat" credential string.
func NewScanner(credential string) (*Scanner, error) {
	org, pat, ok := strings.Cut(credential, ":")
	if !ok || org == "" || pat == "" {
		return nil, fmt.Errorf("Azure DevOps credential must be in the format organization:personal_access_token")
	}
	return &Scanner{
		org:    strings.TrimSpace(org),
		pat:    strings.TrimSpace(pat),
		client: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// ValidateAndGetUsername calls the ADO profile API and returns the display name.
func (s *Scanner) ValidateAndGetUsername(ctx context.Context) (string, error) {
	var profile adoProfile
	if err := s.getJSONAbsolute(ctx,
		"https://app.vssps.visualstudio.com/_apis/profile/me?api-version="+apiVersion,
		&profile,
	); err != nil {
		return "", fmt.Errorf("Azure DevOps credential validation failed: %w", err)
	}
	name := profile.DisplayName
	if name == "" {
		name = profile.EmailAddress
	}
	if name == "" {
		return "", fmt.Errorf("could not retrieve Azure DevOps user info — check token scopes")
	}
	return name, nil
}

// Scan enumerates all git repositories in all projects and returns structured,
// redacted facts for each one.
func (s *Scanner) Scan(ctx context.Context, progress func(current, total int, fullName string)) ([]githubscanner.RepoFact, error) {
	projects, err := s.listProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}

	// Collect all repos across projects first so we know the total.
	type projectRepo struct {
		project string
		repo    adoRepo
	}
	var all []projectRepo
	for _, proj := range projects {
		repos, err := s.listRepos(ctx, proj.Name)
		if err != nil {
			// Non-fatal: note the project but continue.
			continue
		}
		for _, r := range repos {
			all = append(all, projectRepo{project: proj.Name, repo: r})
		}
	}

	facts := make([]githubscanner.RepoFact, 0, len(all))
	for i, pr := range all {
		fact := s.scanRepo(ctx, pr.project, pr.repo)
		facts = append(facts, fact)
		if progress != nil {
			progress(i+1, len(all), fact.FullName)
		}
	}
	return facts, nil
}

// scanRepo collects all facts for a single repository in parallel.
func (s *Scanner) scanRepo(ctx context.Context, project string, repo adoRepo) githubscanner.RepoFact {
	fact := repoFactFromADO(s.org, project, repo)
	if fact.IsEmpty {
		return fact
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []string

	addErr := func(msg string) {
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		hooks, err := s.fetchWebhooks(ctx, project, repo.ID)
		if err != nil {
			addErr("webhooks: " + err.Error())
			return
		}
		mu.Lock()
		fact.Webhooks = hooks
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		count, prot, err := s.fetchBranchInfo(ctx, project, repo.ID, repo.DefaultBranch)
		if err != nil {
			addErr("branches: " + err.Error())
			return
		}
		mu.Lock()
		fact.BranchCount = count
		fact.BranchProtection = prot
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ci, err := s.fetchCIFiles(ctx, project, repo.ID, repo.DefaultBranch)
		if err != nil && !errors.Is(err, errNotFound) {
			addErr("CI: " + err.Error())
			return
		}
		mu.Lock()
		if len(ci) > 0 {
			fact.HasCI = true
			fact.CIFiles = ci
		}
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		depFiles, err := s.fetchDependencyFiles(ctx, project, repo.ID, repo.DefaultBranch)
		if err != nil {
			addErr("dependency files: " + err.Error())
			return
		}
		mu.Lock()
		fact.DependencyFiles = depFiles
		mu.Unlock()
	}()

	wg.Wait()

	if len(errs) > 0 {
		fact.ScanError = strings.Join(errs, "; ")
	}
	return fact
}

// — Sub-call implementations ─────────────────────────────────────────────────

func (s *Scanner) fetchWebhooks(ctx context.Context, project, repoID string) ([]githubscanner.WebhookFact, error) {
	// ADO service hooks are org-level, filterable by publisher and resource.
	u := s.orgURL() + "/_apis/hooks/subscriptions?api-version=" + apiVersion
	var result adoListResult[adoServiceHook]
	if err := s.getJSONAbsolute(ctx, u, &result); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var facts []githubscanner.WebhookFact
	for _, h := range result.Value {
		// Filter to hooks for this specific repository.
		if h.PublisherInputs.Repository != repoID && h.PublisherInputs.Repository != "" {
			continue
		}
		consumerURL := redact.String(h.ConsumerInputs.URL)
		domain := extractDomain(consumerURL)
		if domain == "" {
			continue
		}
		facts = append(facts, githubscanner.WebhookFact{
			Events: []string{h.EventType},
			Active: h.Status == "enabled",
			Domain: domain,
		})
	}
	return facts, nil
}

func (s *Scanner) fetchBranchInfo(ctx context.Context, project, repoID, defaultBranch string) (int, *githubscanner.BranchProtectionFact, error) {
	// Count branches via refs API.
	refsURL := fmt.Sprintf("%s/%s/_apis/git/repositories/%s/refs?filter=heads/&api-version=%s&$top=%d",
		s.orgURL(), url.PathEscape(project), repoID, apiVersion, pageTop)
	var refs adoListResult[adoRef]
	if err := s.getJSONAbsolute(ctx, refsURL, &refs); err != nil {
		return 0, nil, err
	}

	// Branch policies (protection).
	var prot *githubscanner.BranchProtectionFact
	if defaultBranch != "" {
		polURL := fmt.Sprintf("%s/%s/_apis/policy/configurations?api-version=%s",
			s.orgURL(), url.PathEscape(project), apiVersion)
		var policies adoListResult[adoPolicyConfig]
		if err := s.getJSONAbsolute(ctx, polURL, &policies); err == nil {
			prot = branchProtectionFromPolicies(defaultBranch, policies.Value)
		}
	}

	return refs.Count, prot, nil
}

func (s *Scanner) fetchCIFiles(ctx context.Context, project, repoID, branch string) ([]string, error) {
	if branch == "" {
		branch = "main"
	}
	// Check for Azure Pipelines YAML at common paths.
	ciCandidates := []string{"azure-pipelines.yml", "azure-pipelines.yaml", ".azure-pipelines.yml"}
	var found []string
	for _, f := range ciCandidates {
		itemURL := fmt.Sprintf("%s/%s/_apis/git/repositories/%s/items?path=/%s&api-version=%s&versionDescriptor.version=%s&versionDescriptor.versionType=branch",
			s.orgURL(), url.PathEscape(project), repoID, f, apiVersion, url.QueryEscape(branch))
		var item adoItem
		if err := s.getJSONAbsolute(ctx, itemURL, &item); err == nil {
			found = append(found, f)
		}
	}
	// Also check for build definitions (YAML pipelines attached to the repo).
	buildsURL := fmt.Sprintf("%s/%s/_apis/build/definitions?repositoryId=%s&repositoryType=TfsGit&api-version=%s",
		s.orgURL(), url.PathEscape(project), repoID, apiVersion)
	var builds adoListResult[adoBuildDef]
	if err := s.getJSONAbsolute(ctx, buildsURL, &builds); err == nil && builds.Count > 0 {
		for _, b := range builds.Value {
			if b.Process.YamlFilename != "" && !containsStr(found, b.Process.YamlFilename) {
				found = append(found, b.Process.YamlFilename)
			}
		}
	}
	return found, nil
}

var depFileCandidates = []string{
	"go.mod",
	"package.json",
	"requirements.txt",
	"Dockerfile",
	"docker-compose.yml",
	"Gemfile",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"pyproject.toml",
}

func (s *Scanner) fetchDependencyFiles(ctx context.Context, project, repoID, branch string) ([]githubscanner.DependencyFileFact, error) {
	if branch == "" {
		branch = "main"
	}
	type result struct {
		fact *githubscanner.DependencyFileFact
		err  error
	}
	results := make(chan result, len(depFileCandidates))

	for _, file := range depFileCandidates {
		file := file
		go func() {
			itemURL := fmt.Sprintf(
				"%s/%s/_apis/git/repositories/%s/items?path=/%s&api-version=%s&$format=text&versionDescriptor.version=%s&versionDescriptor.versionType=branch",
				s.orgURL(), url.PathEscape(project), repoID, file, apiVersion, url.QueryEscape(branch),
			)
			raw, err := s.getRaw(ctx, itemURL)
			if errors.Is(err, errNotFound) {
				results <- result{}
				return
			}
			if err != nil {
				results <- result{err: fmt.Errorf("%s: %w", file, err)}
				return
			}
			deps, _ := parseDependencyContent(file, string(raw))
			results <- result{fact: &githubscanner.DependencyFileFact{File: file, Dependencies: deps}}
		}()
	}

	var facts []githubscanner.DependencyFileFact
	var errs []string
	for range depFileCandidates {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err.Error())
			continue
		}
		if r.fact != nil {
			facts = append(facts, *r.fact)
		}
	}
	if len(errs) > 0 {
		return facts, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return facts, nil
}

// — Repository and project listing ────────────────────────────────────────────

func (s *Scanner) listProjects(ctx context.Context) ([]adoProject, error) {
	u := s.orgURL() + "/_apis/projects?api-version=" + apiVersion + "&$top=" + fmt.Sprint(pageTop)
	var result adoListResult[adoProject]
	if err := s.getJSONAbsolute(ctx, u, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

func (s *Scanner) listRepos(ctx context.Context, project string) ([]adoRepo, error) {
	u := fmt.Sprintf("%s/%s/_apis/git/repositories?api-version=%s",
		s.orgURL(), url.PathEscape(project), apiVersion)
	var result adoListResult[adoRepo]
	if err := s.getJSONAbsolute(ctx, u, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

// — Azure DevOps API types ─────────────────────────────────────────────────────

type adoListResult[T any] struct {
	Count int `json:"count"`
	Value []T `json:"value"`
}

type adoProfile struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type adoProject struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type adoRepo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"` // e.g. "refs/heads/main"
	Size          int    `json:"size"`
	IsDisabled    bool   `json:"isDisabled"`
	Project       struct {
		Name string `json:"name"`
	} `json:"project"`
	RemoteURL string `json:"remoteUrl"`
}

type adoRef struct {
	Name string `json:"name"`
}

type adoPolicyConfig struct {
	ID       int    `json:"id"`
	IsEnabled bool  `json:"isEnabled"`
	Type     struct {
		DisplayName string `json:"displayName"`
	} `json:"type"`
	Settings struct {
		MinimumApproverCount int `json:"minimumApproverCount"`
		Scope                []struct {
			RefName string `json:"refName"`
		} `json:"scope"`
		BuildDefinitionID int `json:"buildDefinitionId"`
	} `json:"settings"`
}

type adoServiceHook struct {
	EventType      string `json:"eventType"`
	Status         string `json:"status"`
	PublisherInputs struct {
		Repository string `json:"repository"`
	} `json:"publisherInputs"`
	ConsumerInputs struct {
		URL string `json:"url"`
	} `json:"consumerInputs"`
}

type adoItem struct {
	Path string `json:"path"`
}

type adoBuildDef struct {
	Process struct {
		YamlFilename string `json:"yamlFilename"`
	} `json:"process"`
}

// — HTTP helpers ──────────────────────────────────────────────────────────────

func (s *Scanner) orgURL() string {
	return "https://dev.azure.com/" + s.org
}

func (s *Scanner) getJSONAbsolute(ctx context.Context, fullURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	// ADO Basic auth: empty username, PAT as password.
	req.SetBasicAuth("", s.pat)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("Azure DevOps API request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid Azure DevOps token (401) — check your PAT has Code: Read scope")
	case http.StatusForbidden:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusNonAuthoritativeInfo: // 203 — redirect to login page
		return fmt.Errorf("invalid Azure DevOps credentials — organisation may not exist or token is invalid")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Azure DevOps API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

func (s *Scanner) getRaw(ctx context.Context, fullURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth("", s.pat)
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Azure DevOps API request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return io.ReadAll(resp.Body)
	case http.StatusNotFound:
		return nil, errNotFound
	case http.StatusForbidden:
		return nil, errNotFound
	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Azure DevOps API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// — Conversion helpers ────────────────────────────────────────────────────────

func repoFactFromADO(org, project string, r adoRepo) githubscanner.RepoFact {
	defaultBranch := strings.TrimPrefix(r.DefaultBranch, "refs/heads/")
	return githubscanner.RepoFact{
		Owner:         org + "/" + project,
		Name:          r.Name,
		FullName:      project + "/" + r.Name,
		SizeKB:        r.Size / 1024,
		IsEmpty:       r.Size == 0,
		IsArchived:    r.IsDisabled,
		DefaultBranch: defaultBranch,
		Visibility:    "private", // ADO repos are private by default; public projects are rare
	}
}

func branchProtectionFromPolicies(defaultBranch string, policies []adoPolicyConfig) *githubscanner.BranchProtectionFact {
	branchRef := "refs/heads/" + defaultBranch
	fact := &githubscanner.BranchProtectionFact{Branch: defaultBranch}
	hasAny := false

	for _, p := range policies {
		if !p.IsEnabled {
			continue
		}
		// Check if any scope matches our branch.
		applies := false
		for _, sc := range p.Settings.Scope {
			if sc.RefName == branchRef || sc.RefName == "" {
				applies = true
				break
			}
		}
		if !applies {
			continue
		}
		hasAny = true
		switch p.Type.DisplayName {
		case "Minimum number of reviewers":
			fact.RequiresPRReview = true
			if p.Settings.MinimumApproverCount > fact.RequiredApprovalCount {
				fact.RequiredApprovalCount = p.Settings.MinimumApproverCount
			}
		case "Build":
			fact.RequiredStatusChecks = append(fact.RequiredStatusChecks, "build")
		case "Required reviewers":
			fact.RequiresPRReview = true
		}
	}
	if !hasAny {
		return nil
	}
	return fact
}

func extractDomain(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// — Dependency parsers ────────────────────────────────────────────────────────

func parseDependencyContent(filename, content string) ([]string, error) {
	switch filename {
	case "go.mod":
		return parseGoMod(content), nil
	case "package.json":
		return parsePackageJSON(content), nil
	case "requirements.txt", "pyproject.toml":
		return parseRequirements(content), nil
	case "Dockerfile":
		return parseDockerfile(content), nil
	case "Gemfile":
		return parseGemfile(content), nil
	default:
		return nil, nil
	}
}

func parseGoMod(content string) []string {
	var deps []string
	inRequire := false
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		if strings.HasPrefix(line, "require ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				deps = append(deps, parts[1])
			}
			continue
		}
		if inRequire && line != "" && !strings.HasPrefix(line, "//") {
			parts := strings.Fields(line)
			if len(parts) >= 1 && !strings.HasPrefix(parts[0], "//") {
				deps = append(deps, parts[0])
			}
		}
	}
	return deps
}

func parsePackageJSON(content string) []string {
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(content), &pkg); err != nil {
		return nil
	}
	var deps []string
	for k := range pkg.Dependencies {
		deps = append(deps, k)
	}
	for k := range pkg.DevDependencies {
		deps = append(deps, k)
	}
	return deps
}

func parseRequirements(content string) []string {
	var deps []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		name := line
		for _, sep := range []string{"==", ">=", "<=", "~=", "!=", ">", "<", "[", ";"} {
			if i := strings.Index(name, sep); i >= 0 {
				name = name[:i]
			}
		}
		name = strings.TrimSpace(name)
		if name != "" {
			deps = append(deps, name)
		}
	}
	return deps
}

func parseDockerfile(content string) []string {
	var images []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.ToUpper(parts[1]) != "SCRATCH" {
				images = append(images, parts[1])
			}
		}
	}
	return images
}

func parseGemfile(content string) []string {
	var gems []string
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "gem ") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			name := strings.Trim(parts[1], `'",`)
			if name != "" {
				gems = append(gems, name)
			}
		}
	}
	return gems
}
