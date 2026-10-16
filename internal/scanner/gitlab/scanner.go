// Package gitlab implements a repository scanner for GitLab using the GitLab
// REST API v4. It produces the same RepoFact type as the other scanners so the
// rest of the pipeline is platform-agnostic.
package gitlab

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
	apiBase   = "https://gitlab.com/api/v4"
	userAgent = "shiftctl/0.1.0"
	perPage   = 100
)

var errNotFound = errors.New("not found")

// Scanner reads repository facts from GitLab.com using a personal access token.
type Scanner struct {
	token  string
	client *http.Client
}

// NewScanner returns a Scanner ready to enumerate repositories accessible to
// the given personal access token (starts with "glpat-").
func NewScanner(token string) *Scanner {
	return &Scanner{
		token:  strings.TrimSpace(token),
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// ValidateAndGetUsername calls /api/v4/user and returns the authenticated username.
func (s *Scanner) ValidateAndGetUsername(ctx context.Context) (string, error) {
	var user glUser
	if err := s.getJSON(ctx, "/user", &user); err != nil {
		return "", fmt.Errorf("GitLab credential validation failed: %w", err)
	}
	if user.Username == "" {
		return "", fmt.Errorf("could not retrieve GitLab username — check token scopes (need: read_api)")
	}
	return user.Username, nil
}

// Scan enumerates all projects accessible to the token and returns structured,
// redacted facts for each one.
func (s *Scanner) Scan(ctx context.Context, progress func(current, total int, fullName string)) ([]githubscanner.RepoFact, error) {
	projects, err := s.listAllProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}

	facts := make([]githubscanner.RepoFact, 0, len(projects))
	for i, proj := range projects {
		fact := s.scanProject(ctx, proj)
		facts = append(facts, fact)
		if progress != nil {
			progress(i+1, len(projects), proj.PathWithNamespace)
		}
	}
	return facts, nil
}

// scanProject collects all facts for a single project in parallel.
func (s *Scanner) scanProject(ctx context.Context, proj glProject) githubscanner.RepoFact {
	fact := repoFactFromGL(proj)
	if proj.Archived || fact.IsEmpty {
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
		hooks, err := s.fetchWebhooks(ctx, proj.ID)
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
		keys, err := s.fetchDeployKeys(ctx, proj.ID)
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
		count, prot, err := s.fetchBranchInfo(ctx, proj.ID, proj.DefaultBranch)
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
		ci, err := s.fetchCIFiles(ctx, proj.ID, proj.DefaultBranch)
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
		depFiles, err := s.fetchDependencyFiles(ctx, proj.ID, proj.DefaultBranch)
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

func (s *Scanner) fetchWebhooks(ctx context.Context, projectID int) ([]githubscanner.WebhookFact, error) {
	path := fmt.Sprintf("/projects/%d/hooks?per_page=%d", projectID, perPage)
	var hooks []glHook
	if err := s.getJSON(ctx, path, &hooks); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]githubscanner.WebhookFact, 0, len(hooks))
	for _, h := range hooks {
		domain := extractDomain(redact.String(h.URL))
		events := hookEvents(h)
		facts = append(facts, githubscanner.WebhookFact{
			Events: events,
			Active: h.EnabledEvents.Push || h.EnabledEvents.MergeRequests,
			Domain: domain,
		})
	}
	return facts, nil
}

func (s *Scanner) fetchDeployKeys(ctx context.Context, projectID int) ([]githubscanner.DeployKeyFact, error) {
	path := fmt.Sprintf("/projects/%d/deploy_keys?per_page=%d", projectID, perPage)
	var keys []glDeployKey
	if err := s.getJSON(ctx, path, &keys); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	facts := make([]githubscanner.DeployKeyFact, 0, len(keys))
	for _, k := range keys {
		facts = append(facts, githubscanner.DeployKeyFact{
			Title:    k.Title,
			ReadOnly: !k.CanPush,
		})
	}
	return facts, nil
}

func (s *Scanner) fetchBranchInfo(ctx context.Context, projectID int, defaultBranch string) (int, *githubscanner.BranchProtectionFact, error) {
	// Count branches.
	count := 0
	page := 1
	for {
		path := fmt.Sprintf("/projects/%d/repository/branches?per_page=%d&page=%d", projectID, perPage, page)
		var branches []glBranch
		if err := s.getJSON(ctx, path, &branches); err != nil {
			return 0, nil, err
		}
		count += len(branches)
		if len(branches) < perPage {
			break
		}
		page++
	}

	// Protected branches (= branch protection).
	var prot *githubscanner.BranchProtectionFact
	if defaultBranch != "" {
		path := fmt.Sprintf("/projects/%d/protected_branches/%s",
			projectID, url.PathEscape(defaultBranch))
		var pb glProtectedBranch
		if err := s.getJSON(ctx, path, &pb); err == nil {
			prot = branchProtectionFromGL(pb)
		}
	}

	return count, prot, nil
}

func (s *Scanner) fetchCIFiles(ctx context.Context, projectID int, branch string) ([]string, error) {
	if branch == "" {
		branch = "main"
	}
	// GitLab CI uses .gitlab-ci.yml at the repo root.
	path := fmt.Sprintf("/projects/%d/repository/files/%s?ref=%s",
		projectID, url.PathEscape(".gitlab-ci.yml"), url.QueryEscape(branch))
	var f glFile
	if err := s.getJSON(ctx, path, &f); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return []string{".gitlab-ci.yml"}, nil
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

func (s *Scanner) fetchDependencyFiles(ctx context.Context, projectID int, branch string) ([]githubscanner.DependencyFileFact, error) {
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
			// GitLab raw file endpoint returns the file content directly.
			path := fmt.Sprintf("/projects/%d/repository/files/%s/raw?ref=%s",
				projectID, url.PathEscape(file), url.QueryEscape(branch))
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

// — Project listing ────────────────────────────────────────────────────────────

func (s *Scanner) listAllProjects(ctx context.Context) ([]glProject, error) {
	var all []glProject
	page := 1
	for {
		path := fmt.Sprintf("/projects?membership=true&per_page=%d&page=%d&order_by=id&sort=asc", perPage, page)
		var batch []glProject
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

// — GitLab API types ───────────────────────────────────────────────────────────

type glUser struct {
	Username string `json:"username"`
}

type glProject struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	PathWithNamespace string    `json:"path_with_namespace"`
	Description       string    `json:"description"`
	Visibility        string    `json:"visibility"` // "private", "internal", "public"
	Archived          bool      `json:"archived"`
	ForkedFromProject *struct{} `json:"forked_from_project"` // non-nil if fork
	DefaultBranch     string    `json:"default_branch"`
	Topics            []string  `json:"topics"`
	Language          string    `json:"-"` // resolved separately if needed
	Statistics        struct {
		RepositorySize int64 `json:"repository_size"`
		CommitCount    int   `json:"commit_count"`
	} `json:"statistics"`
	LastActivityAt time.Time `json:"last_activity_at"`
	OpenIssuesCount int      `json:"open_issues_count"`
}

type glHook struct {
	URL string `json:"url"`
	EnabledEvents struct {
		Push          bool `json:"push_events"`
		MergeRequests bool `json:"merge_requests_events"`
		Issues        bool `json:"issues_events"`
	} `json:"-"`
	PushEvents          bool `json:"push_events"`
	MergeRequestsEvents bool `json:"merge_requests_events"`
}

type glDeployKey struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	CanPush  bool   `json:"can_push"`
}

type glBranch struct {
	Name string `json:"name"`
}

type glProtectedBranch struct {
	Name             string `json:"name"`
	PushAccessLevels []struct {
		AccessLevel int `json:"access_level"` // 0=No access, 30=Developer, 40=Maintainer
	} `json:"push_access_levels"`
	MergeAccessLevels []struct {
		AccessLevel int `json:"access_level"`
	} `json:"merge_access_levels"`
	AllowForcePush bool `json:"allow_force_push"`
	// Approval rules are fetched via a separate endpoint in GitLab EE.
}

type glFile struct {
	FileName string `json:"file_name"`
}

// — HTTP helpers ──────────────────────────────────────────────────────────────

func (s *Scanner) getJSON(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", s.token)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitLab API request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(resp.Body).Decode(out)
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	case http.StatusUnauthorized:
		return fmt.Errorf("invalid or expired GitLab token (401)")
	case http.StatusForbidden:
		io.Copy(io.Discard, resp.Body)
		return errNotFound
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

func (s *Scanner) getRaw(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", s.token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitLab API request failed: %w", err)
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
		return nil, fmt.Errorf("GitLab API returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
}

// — Conversion helpers ────────────────────────────────────────────────────────

func repoFactFromGL(p glProject) githubscanner.RepoFact {
	parts := strings.SplitN(p.PathWithNamespace, "/", 2)
	owner, name := "", p.PathWithNamespace
	if len(parts) == 2 {
		owner = parts[0]
		name = parts[1]
	}

	visibility := "private"
	if p.Visibility == "public" {
		visibility = "public"
	}

	return githubscanner.RepoFact{
		Owner:          owner,
		Name:           name,
		FullName:       p.PathWithNamespace,
		Description:    redact.String(p.Description),
		Topics:         p.Topics,
		SizeKB:         int(p.Statistics.RepositorySize / 1024),
		Visibility:     visibility,
		IsArchived:     p.Archived,
		IsFork:         p.ForkedFromProject != nil,
		IsEmpty:        p.Statistics.CommitCount == 0,
		DefaultBranch:  p.DefaultBranch,
		LastPushedAt:   p.LastActivityAt,
		OpenIssueCount: p.OpenIssuesCount,
	}
}

func branchProtectionFromGL(pb glProtectedBranch) *githubscanner.BranchProtectionFact {
	fact := &githubscanner.BranchProtectionFact{Branch: pb.Name}
	// Access level 40 = Maintainer only (= requires PR/MR to merge).
	for _, level := range pb.MergeAccessLevels {
		if level.AccessLevel >= 40 {
			fact.RequiresPRReview = true
		}
	}
	for _, level := range pb.PushAccessLevels {
		if level.AccessLevel == 0 {
			fact.EnforcesAdmins = true
		}
	}
	return fact
}

func hookEvents(h glHook) []string {
	var events []string
	if h.PushEvents {
		events = append(events, "push")
	}
	if h.MergeRequestsEvents {
		events = append(events, "merge_requests")
	}
	return events
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
