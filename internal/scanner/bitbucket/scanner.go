// Package bitbucket implements a repository scanner for Bitbucket Cloud using
// the Bitbucket API v2. It produces the same RepoFact type as the GitHub
// scanner so the rest of the pipeline (AI analysis, plan, execute) is
// platform-agnostic.
package bitbucket

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

	"github.com/useshiftctl/shiftctl/internal/redact"
	githubscanner "github.com/useshiftctl/shiftctl/internal/scanner/github"
)

const (
	apiBase   = "https://api.bitbucket.org/2.0"
	userAgent = "shiftctl/0.1.0"
	pageLen   = 100
)

var errNotFound = errors.New("not found")

// Scanner reads repository facts from Bitbucket Cloud.
// Credential must be in the form "username:app_password".
type Scanner struct {
	username string
	password string
	client   *http.Client
}

// NewScanner returns a Scanner from a combined "username:app_password"
// credential string.
func NewScanner(credential string) (*Scanner, error) {
	username, password, ok := strings.Cut(credential, ":")
	if !ok || username == "" || password == "" {
		return nil, fmt.Errorf("Bitbucket credential must be in the format username:app_password")
	}
	return &Scanner{
		username: strings.TrimSpace(username),
		password: strings.TrimSpace(password),
		client:   &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// ValidateAndGetUsername calls /2.0/user and returns the authenticated username.
func (s *Scanner) ValidateAndGetUsername(ctx context.Context) (string, error) {
	var user bbUser
	if err := s.getJSON(ctx, "/user", &user); err != nil {
		return "", fmt.Errorf("Bitbucket credential validation failed: %w", err)
	}
	if user.AccountID == "" && user.Nickname == "" {
		return "", fmt.Errorf("could not retrieve Bitbucket user info. check token scopes")
	}
	name := user.Nickname
	if name == "" {
		name = user.AccountID
	}
	return name, nil
}

// Scan enumerates all repositories in the authenticated user's workspaces and
// returns structured, redacted facts for each one.
func (s *Scanner) Scan(ctx context.Context, progress func(current, total int, fullName string)) ([]githubscanner.RepoFact, error) {
	repos, err := s.listAllRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}

	facts := make([]githubscanner.RepoFact, 0, len(repos))
	for i, repo := range repos {
		fact := s.scanRepo(ctx, repo)
		facts = append(facts, fact)
		if progress != nil {
			progress(i+1, len(repos), repo.FullName)
		}
	}
	return facts, nil
}

// scanRepo collects all facts for a single repository in parallel.
func (s *Scanner) scanRepo(ctx context.Context, repo bbRepo) githubscanner.RepoFact {
	fact := repoFactFromBB(repo)
	if fact.IsArchived || fact.IsEmpty {
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
		count, prot, err := s.fetchBranchInfo(ctx, repo.FullName, repo.MainBranch)
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
		ci, err := s.fetchCIFiles(ctx, repo.FullName, repo.MainBranch)
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
		depFiles, err := s.fetchDependencyFiles(ctx, repo.FullName, repo.MainBranch)
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

// Sub-call implementations ─────────────────────────────────────────────────

func (s *Scanner) fetchWebhooks(ctx context.Context, fullName string) ([]githubscanner.WebhookFact, error) {
	path := "/repositories/" + fullName + "/hooks?pagelen=" + fmt.Sprint(pageLen)
	var page bbPage[bbHook]
	if err := s.getJSON(ctx, path, &page); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]githubscanner.WebhookFact, 0, len(page.Values))
	for _, h := range page.Values {
		domain := extractDomain(redact.String(h.URL))
		facts = append(facts, githubscanner.WebhookFact{
			Events: h.Events,
			Active: h.Active,
			Domain: domain,
		})
	}
	return facts, nil
}

func (s *Scanner) fetchDeployKeys(ctx context.Context, fullName string) ([]githubscanner.DeployKeyFact, error) {
	path := "/repositories/" + fullName + "/deploy-keys?pagelen=" + fmt.Sprint(pageLen)
	var page bbPage[bbDeployKey]
	if err := s.getJSON(ctx, path, &page); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]githubscanner.DeployKeyFact, 0, len(page.Values))
	for _, k := range page.Values {
		facts = append(facts, githubscanner.DeployKeyFact{
			Title:    k.Label,
			ReadOnly: !k.AddedOn.IsZero(), // Bitbucket deploy keys are always read-only
		})
	}
	return facts, nil
}

// fetchBranchInfo returns branch count and a protection fact derived from
// Bitbucket branch restrictions.
func (s *Scanner) fetchBranchInfo(ctx context.Context, fullName, mainBranch string) (int, *githubscanner.BranchProtectionFact, error) {
	// Count branches.
	count := 0
	nextURL := apiBase + "/repositories/" + fullName + "/refs/branches?pagelen=" + fmt.Sprint(pageLen)
	for nextURL != "" {
		var page bbPage[bbBranch]
		if err := s.getJSONAbsolute(ctx, nextURL, &page); err != nil {
			return 0, nil, err
		}
		count += len(page.Values)
		nextURL = page.Next
	}

	// Branch restrictions (protection rules).
	var prot *githubscanner.BranchProtectionFact
	if mainBranch != "" {
		var page bbPage[bbBranchRestriction]
		rPath := "/repositories/" + fullName + "/branch-restrictions?pagelen=" + fmt.Sprint(pageLen)
		if err := s.getJSON(ctx, rPath, &page); err == nil {
			prot = branchProtectionFromRestrictions(mainBranch, page.Values)
		}
	}

	return count, prot, nil
}

func (s *Scanner) fetchCIFiles(ctx context.Context, fullName, branch string) ([]string, error) {
	if branch == "" {
		branch = "HEAD"
	}
	// Bitbucket Pipelines uses a single file at the repo root.
	path := "/repositories/" + fullName + "/src/" + url.PathEscape(branch) + "/bitbucket-pipelines.yml"
	_, err := s.getRaw(ctx, path)
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []string{"bitbucket-pipelines.yml"}, nil
}

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

func (s *Scanner) fetchDependencyFiles(ctx context.Context, fullName, branch string) ([]githubscanner.DependencyFileFact, error) {
	if branch == "" {
		branch = "HEAD"
	}
	type result struct {
		fact *githubscanner.DependencyFileFact
		err  error
	}
	results := make(chan result, len(depFileCandidates))

	for _, file := range depFileCandidates {
		file := file
		go func() {
			path := "/repositories/" + fullName + "/src/" + url.PathEscape(branch) + "/" + file
			raw, err := s.getRaw(ctx, path)
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

// Repository listing ────────────────────────────────────────────────────────

func (s *Scanner) listAllRepos(ctx context.Context) ([]bbRepo, error) {
	var all []bbRepo
	// List repos where the user is a member across all workspaces.
	nextURL := apiBase + "/repositories?role=member&pagelen=" + fmt.Sprint(pageLen)
	for nextURL != "" {
		var page bbPage[bbRepo]
		if err := s.getJSONAbsolute(ctx, nextURL, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Values...)
		nextURL = page.Next
	}
	return all, nil
}

// Bitbucket API types ───────────────────────────────────────────────────────

type bbPage[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next"`
}

type bbUser struct {
	AccountID string `json:"account_id"`
	Nickname  string `json:"nickname"`
}

type bbRepo struct {
	FullName    string    `json:"full_name"`
	Name        string    `json:"slug"`
	Description string    `json:"description"`
	IsPrivate   bool      `json:"is_private"`
	Language    string    `json:"language"`
	SCM         string    `json:"scm"`
	Size        int       `json:"size"`
	UpdatedOn   time.Time `json:"updated_on"`
	CreatedOn   time.Time `json:"created_on"`
	MainBranch  string    `json:"-"` // populated separately
	Project     struct {
		Key string `json:"key"`
	} `json:"project"`
	Links struct {
		Clone []struct {
			Href string `json:"href"`
			Name string `json:"name"`
		} `json:"clone"`
	} `json:"links"`
}

// UnmarshalJSON for bbRepo to extract main_branch from the nested object.
func (r *bbRepo) UnmarshalJSON(data []byte) error {
	type Alias bbRepo
	aux := &struct {
		MainBranch *struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	if aux.MainBranch != nil {
		r.MainBranch = aux.MainBranch.Name
	}
	return nil
}

type bbHook struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
	Active bool     `json:"active"`
}

type bbDeployKey struct {
	Label    string    `json:"label"`
	AddedOn  time.Time `json:"added_on"`
}

type bbBranch struct {
	Name string `json:"name"`
}

type bbBranchRestriction struct {
	Kind    string `json:"kind"`    // "push", "require_approvals_to_merge", etc.
	Pattern string `json:"pattern"` // branch name / glob
	Value   int    `json:"value"`   // approval count for require_approvals_to_merge
	Users   []struct {
		Nickname string `json:"nickname"`
	} `json:"users"`
	Groups []struct {
		Name string `json:"name"`
	} `json:"groups"`
}

// HTTP helpers ──────────────────────────────────────────────────────────────

func (s *Scanner) getJSON(ctx context.Context, path string, out interface{}) error {
	return s.getJSONAbsolute(ctx, apiBase+path, out)
}

func (s *Scanner) getJSONAbsolute(ctx context.Context, fullURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.username, s.password)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("Bitbucket API request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid Bitbucket credentials (401). check your username and app password")
	case http.StatusForbidden:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Bitbucket API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

func (s *Scanner) getRaw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.username, s.password)
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Bitbucket API request failed: %w", err)
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
		return nil, fmt.Errorf("Bitbucket API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// Conversion helpers ────────────────────────────────────────────────────────

func repoFactFromBB(r bbRepo) githubscanner.RepoFact {
	parts := strings.SplitN(r.FullName, "/", 2)
	owner := ""
	name := r.FullName
	if len(parts) == 2 {
		owner = parts[0]
		name = parts[1]
	}

	visibility := "private"
	if !r.IsPrivate {
		visibility = "public"
	}

	return githubscanner.RepoFact{
		Owner:           owner,
		Name:            name,
		FullName:        r.FullName,
		Description:     redact.String(r.Description),
		PrimaryLanguage: r.Language,
		SizeKB:          r.Size / 1024,
		Visibility:      visibility,
		IsEmpty:         r.Size == 0,
		DefaultBranch:   r.MainBranch,
		LastPushedAt:    r.UpdatedOn,
	}
}

func branchProtectionFromRestrictions(branch string, restrictions []bbBranchRestriction) *githubscanner.BranchProtectionFact {
	fact := &githubscanner.BranchProtectionFact{Branch: branch}
	hasAny := false
	for _, r := range restrictions {
		// Match restrictions that apply to the main branch (exact or glob "*").
		if r.Pattern != branch && r.Pattern != "*" {
			continue
		}
		hasAny = true
		switch r.Kind {
		case "require_approvals_to_merge":
			fact.RequiresPRReview = true
			if r.Value > fact.RequiredApprovalCount {
				fact.RequiredApprovalCount = r.Value
			}
		case "push":
			// Restricting who can push = effectively enforces admins-only push.
			fact.EnforcesAdmins = true
		case "require_passing_builds_to_merge":
			fact.RequiredStatusChecks = append(fact.RequiredStatusChecks, "ci")
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

// Dependency parsers (re-implemented here to avoid cross-package dep) ───────

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
