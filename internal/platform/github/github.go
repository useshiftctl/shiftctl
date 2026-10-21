package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const userAgent = "shiftctl/0.1.0"

type platform struct{}

// New returns the GitHub platform implementation.
func New() *platform { return &platform{} }

func (p *platform) ID() string          { return "github" }
func (p *platform) DisplayName() string { return "GitHub" }
func (p *platform) IsImplemented() bool { return true }

func (p *platform) CredentialInstructions() string {
	return `To generate a GitHub personal access token:

  1. Open: https://github.com/settings/tokens
  2. Click "Generate new token" -> "Generate new token (classic)"
  3. Give it a descriptive name (e.g. "shiftctl-migration")
  4. Set an expiration appropriate for your migration timeline
  5. Select these scopes:
       repo          (read access to repositories and webhooks)
       read:org      (read organisation membership and teams)
  6. Click "Generate token" and copy the result

  The token will start with "ghp_" (classic) or "github_pat_" (fine-grained).
  Keep it somewhere safe. GitHub only shows it once.`
}

func (p *platform) CredentialPrompt() string {
	return "Paste your GitHub personal access token"
}

// ValidateCredential calls the GitHub REST API to confirm the token is valid
// and has the repository read access shiftctl requires for assessment.
func (p *platform) ValidateCredential(ctx context.Context, credential string) (string, error) {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", fmt.Errorf("token cannot be empty")
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Confirm token identity.
	user, err := githubGetUser(ctx, client, credential)
	if err != nil {
		return "", err
	}

	// Confirm repo read access by listing the first repo.
	if err := githubCheckRepoAccess(ctx, client, credential); err != nil {
		return "", err
	}

	return user, nil
}

func githubGetUser(ctx context.Context, client *http.Client, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", fmt.Errorf("could not build request: %w", err)
	}
	setGitHubHeaders(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach GitHub API. check your internet connection: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusUnauthorized:
		return "", fmt.Errorf("token is invalid or has been revoked. double-check you copied it in full")
	case http.StatusForbidden:
		return "", fmt.Errorf("GitHub returned 403. the token may have expired or been restricted")
	default:
		return "", fmt.Errorf("GitHub returned an unexpected status: %d", resp.StatusCode)
	}

	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("could not parse GitHub response: %w", err)
	}
	if body.Login == "" {
		return "", fmt.Errorf("GitHub returned a valid response but no username. please try again")
	}
	return body.Login, nil
}

func githubCheckRepoAccess(ctx context.Context, client *http.Client, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/user/repos?per_page=1&sort=updated", nil)
	if err != nil {
		return fmt.Errorf("could not build request: %w", err)
	}
	setGitHubHeaders(req, token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach GitHub API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"token doesn't have repository read access. ensure the 'repo' scope is selected when generating it",
		)
	}

	// For classic tokens, check the scopes header as an extra signal.
	scopes := resp.Header.Get("X-OAuth-Scopes")
	if scopes != "" && !strings.Contains(scopes, "repo") {
		return fmt.Errorf(
			"token is missing the 'repo' scope (current scopes: %s). regenerate it with 'repo' enabled",
			scopes,
		)
	}

	return nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", userAgent)
}
