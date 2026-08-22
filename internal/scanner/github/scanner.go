package github

import (
	"bufio"
	"context"
	"encoding/base64"
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
)

const (
	apiBase   = "https://api.github.com"
	userAgent = "shiftctl/0.1.0"
	perPage   = 100
)

var errNotFound = errors.New("not found")

// Scanner reads repository facts from GitHub using a personal access token.
type Scanner struct {
	token  string
	client *http.Client
}

// NewScanner returns a Scanner ready to enumerate repositories accessible to
// the given personal access token.
func NewScanner(token string) *Scanner {
	return &Scanner{
		token:  strings.TrimSpace(token),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Scan enumerates all repositories accessible to the token and returns
// structured, redacted facts for each one. progress, if non-nil, is called
// after every repo is processed with (current index, total count, full name).
func (s *Scanner) Scan(ctx context.Context, progress func(current, total int, fullName string)) ([]RepoFact, error) {
	apiRepos, err := s.listAllRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}

	facts := make([]RepoFact, 0, len(apiRepos))
	for i, repo := range apiRepos {
		fact := s.scanRepo(ctx, repo)
		facts = append(facts, fact)
		if progress != nil {
			progress(i+1, len(apiRepos), repo.FullName)
		}
	}
	return facts, nil
}

// scanRepo collects all facts for a single repository. Errors in individual
// sub-calls are noted in fact.ScanError rather than aborting the whole scan.
func (s *Scanner) scanRepo(ctx context.Context, repo githubAPIRepo) RepoFact {
	fact := repoFactFromAPI(repo)
	if repo.Archived || fact.IsEmpty {
		return fact
	}

	// Run sub-calls in parallel: each writes into a specific field of fact.
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
		hooks, err := s.fetchWebhooks(ctx, repo.FullName)
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
		keys, err := s.fetchDeployKeys(ctx, repo.FullName)
		if err != nil {
			addErr("deploy keys: " + err.Error())
			return
		}
		mu.Lock()
		fact.DeployKeys = keys
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		count, err := s.countBranches(ctx, repo.FullName)
		if err != nil {
			addErr("branches: " + err.Error())
			return
		}
		prot, err := s.fetchBranchProtection(ctx, repo.FullName, repo.DefaultBranch)
		if err != nil && !errors.Is(err, errNotFound) {
			addErr("branch protection: " + err.Error())
		}
		mu.Lock()
		fact.BranchCount = count
		fact.BranchProtection = prot
		mu.Unlock()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ci, err := s.fetchCIFiles(ctx, repo.FullName)
		if err != nil && !errors.Is(err, errNotFound) {
			addErr("CI files: " + err.Error())
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
		depFiles, err := s.fetchDependencyFiles(ctx, repo.FullName)
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

func (s *Scanner) fetchWebhooks(ctx context.Context, fullName string) ([]WebhookFact, error) {
	var hooks []githubAPIHook
	if err := s.getJSON(ctx, "/repos/"+fullName+"/hooks", &hooks); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]WebhookFact, 0, len(hooks))
	for _, h := range hooks {
		domain := extractDomain(redact.String(h.Config.URL))
		facts = append(facts, WebhookFact{
			Events: h.Events,
			Active: h.Active,
			Domain: domain,
		})
	}
	return facts, nil
}

func (s *Scanner) fetchDeployKeys(ctx context.Context, fullName string) ([]DeployKeyFact, error) {
	var keys []githubAPIKey
	if err := s.getJSON(ctx, "/repos/"+fullName+"/keys", &keys); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]DeployKeyFact, 0, len(keys))
	for _, k := range keys {
		facts = append(facts, DeployKeyFact{
			Title:    k.Title,
			ReadOnly: k.ReadOnly,
		})
	}
	return facts, nil
}

func (s *Scanner) countBranches(ctx context.Context, fullName string) (int, error) {
	total := 0
	page := 1
	for {
		var branches []githubAPIBranch
		path := fmt.Sprintf("/repos/%s/branches?per_page=%d&page=%d", fullName, perPage, page)
		if err := s.getJSON(ctx, path, &branches); err != nil {
			return 0, err
		}
		total += len(branches)
		if len(branches) < perPage {
			break
		}
		page++
	}
	return total, nil
}

func (s *Scanner) fetchBranchProtection(ctx context.Context, fullName, branch string) (*BranchProtectionFact, error) {
	if branch == "" {
		return nil, nil
	}
	var prot githubAPIProtection
	path := fmt.Sprintf("/repos/%s/branches/%s/protection", fullName, url.PathEscape(branch))
	if err := s.getJSON(ctx, path, &prot); err != nil {
		return nil, err
	}

	fact := &BranchProtectionFact{Branch: branch}
	if prot.RequiredPullRequestReviews != nil {
		fact.RequiresPRReview = true
		fact.RequiredApprovalCount = prot.RequiredPullRequestReviews.RequiredApprovingReviewCount
	}
	if prot.RequiredStatusChecks != nil {
		fact.RequiredStatusChecks = prot.RequiredStatusChecks.Contexts
	}
	if prot.EnforceAdmins != nil {
		fact.EnforcesAdmins = prot.EnforceAdmins.Enabled
	}
	return fact, nil
}

func (s *Scanner) fetchCIFiles(ctx context.Context, fullName string) ([]string, error) {
	var entries []githubAPIContent
	if err := s.getJSON(ctx, "/repos/"+fullName+"/contents/.github/workflows", &entries); err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type == "file" && (strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml")) {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

// depFileCandidates lists the specific files shiftctl checks for in each repo.
var depFileCandidates = []string{
	"go.mod",
	"package.json",
	"requirements.txt",
	"Dockerfile",
	"docker-compose.yml",
	"docker-compose.yaml",
	"Gemfile",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"pyproject.toml",
}

func (s *Scanner) fetchDependencyFiles(ctx context.Context, fullName string) ([]DependencyFileFact, error) {
	type result struct {
		fact *DependencyFileFact
		err  error
	}
	results := make(chan result, len(depFileCandidates))

	for _, file := range depFileCandidates {
		file := file
		go func() {
			var content githubAPIContent
			path := fmt.Sprintf("/repos/%s/contents/%s", fullName, file)
			err := s.getJSON(ctx, path, &content)
			if errors.Is(err, errNotFound) {
				results <- result{}
				return
			}
			if err != nil {
				results <- result{err: fmt.Errorf("%s: %w", file, err)}
				return
			}
			deps, parseErr := parseDependencyContent(file, content.Content)
			if parseErr != nil {
				// Non-fatal: note the file exists but we couldn't parse deps.
				results <- result{fact: &DependencyFileFact{File: file}}
				return
			}
			results <- result{fact: &DependencyFileFact{File: file, Dependencies: deps}}
		}()
	}

	var facts []DependencyFileFact
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

// — Dependency content parsers ───────────────────────────────────────────────

func parseDependencyContent(filename, b64content string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(b64content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	content := string(raw)

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
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}
		// Single-line require: "require github.com/foo/bar v1.2.3"
		if strings.HasPrefix(line, "require ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				deps = append(deps, parts[1])
			}
			continue
		}
		if inRequire && line != "" && !strings.HasPrefix(line, "//") {
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				pkg := parts[0]
				// Strip indirect comments.
				if !strings.HasPrefix(pkg, "//") {
					deps = append(deps, pkg)
				}
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
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		// Extract package name: everything before ==, >=, <=, ~=, !=, [, ;, space.
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
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "FROM ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 && strings.ToUpper(parts[1]) != "SCRATCH" {
				// Strip the AS alias: "FROM node:20 AS builder" → "node:20"
				images = append(images, parts[1])
			}
		}
	}
	return images
}

func parseGemfile(content string) []string {
	var gems []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "gem ") {
			continue
		}
		// gem 'rails', '~> 7.0'  or  gem "devise"
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

// — HTTP helpers ─────────────────────────────────────────────────────────────

func (s *Scanner) getJSON(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+s.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub API request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("token rejected by GitHub (401)")
	case http.StatusForbidden:
		// Branch protection endpoints return 403 when protection is not set.
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusUnprocessableEntity:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// — Helpers ──────────────────────────────────────────────────────────────────

func repoFactFromAPI(r githubAPIRepo) RepoFact {
	visibility := "private"
	if !r.Private {
		visibility = "public"
	}
	isEmpty := r.Size == 0

	parts := strings.SplitN(r.FullName, "/", 2)
	owner := ""
	if len(parts) == 2 {
		owner = parts[0]
	}

	return RepoFact{
		Owner:           owner,
		Name:            r.Name,
		FullName:        r.FullName,
		Description:     redact.String(r.Description),
		PrimaryLanguage: r.Language,
		Topics:          r.Topics,
		SizeKB:          r.Size,
		Visibility:      visibility,
		IsArchived:      r.Archived,
		IsFork:          r.Fork,
		IsEmpty:         isEmpty,
		DefaultBranch:   r.DefaultBranch,
		LastPushedAt:    r.PushedAt,
		OpenIssueCount:  r.OpenIssues,
	}
}

// listAllRepos fetches every repository the token can access, paginating until
// GitHub returns fewer than perPage results.
func (s *Scanner) listAllRepos(ctx context.Context) ([]githubAPIRepo, error) {
	var all []githubAPIRepo
	page := 1
	for {
		var batch []githubAPIRepo
		path := fmt.Sprintf("/user/repos?per_page=%d&page=%d&sort=updated", perPage, page)
		if err := s.getJSON(ctx, path, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			break
		}
		page++
	}
	return all, nil
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
