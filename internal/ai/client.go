// Package ai is the isolated boundary between shiftctl and the Anthropic API.
// Everything outside this package reasons over local facts; this package is the
// only place that makes outbound network calls to the model.
//
// The design is intentionally thin so that a local-model option can be wired in
// later by swapping this package's implementation behind the same function
// signatures.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicAPIURL = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	model            = "claude-sonnet-4-6"
)

// Client holds an authenticated Anthropic API connection.
type Client struct {
	apiKey      string
	workspaceID string // optional; required for identity-linked API keys
	httpClient  *http.Client
}

// NewClient returns a Client configured with the given API key. The key is
// read from ANTHROPIC_API_KEY by the caller; this package does not read env
// vars so the isolation is explicit. workspaceID is optional and only needed
// for identity-linked API keys (set via ANTHROPIC_WORKSPACE_ID).
func NewClient(apiKey, workspaceID string) *Client {
	return &Client{
		apiKey:      apiKey,
		workspaceID: workspaceID,
		httpClient: &http.Client{
			// Analysis calls can take longer than a typical HTTP request ,
			// large repo sets produce large prompts and long completions.
			Timeout: 3 * time.Minute,
		},
	}
}

// complete sends a single request to the Anthropic Messages API and returns
// the raw response. It is unexported; callers use the higher-level functions
// in analyze.go.
func (c *Client) complete(ctx context.Context, req anthropicRequest) (*anthropicResponse, error) {
	if strings.HasPrefix(c.apiKey, "wrkspc_") {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY is set to a workspace ID, not an API key.\n\n" +
			"  Your API key starts with 'sk-ant-...' , find it at https://console.anthropic.com/\n" +
			"  Your workspace ID (wrkspc_...) goes in ANTHROPIC_WORKSPACE_ID instead")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPIURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("user-agent", "shiftctl/0.1.0")
	if c.workspaceID != "" {
		httpReq.Header.Set("anthropic-workspace-id", c.workspaceID)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Anthropic API request failed , check your internet connection: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading Anthropic response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// ok, fall through
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("Anthropic API key is invalid , check your ANTHROPIC_API_KEY environment variable")
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("Anthropic API rate limit exceeded , wait a moment and try again")
	case 529:
		return nil, fmt.Errorf("Anthropic API is overloaded , try again in a moment")
	default:
		// Try to extract the error message from the response body.
		var errResp anthropicResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != nil {
			msg := errResp.Error.Message
			if strings.Contains(msg, "anthropic-workspace-id") {
				return nil, fmt.Errorf("your API key is an identity-linked key, which requires a workspace ID.\n\n" +
					"  Option 1 (recommended): use a standard API key from https://console.anthropic.com/\n" +
					"  Option 2: set ANTHROPIC_WORKSPACE_ID=<your-workspace-id> before running shiftctl assess")
			}
			return nil, fmt.Errorf("Anthropic API error (%d): %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("Anthropic API returned %d: %s", resp.StatusCode, truncateBody(respBody))
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing Anthropic response: %w", err)
	}
	return &result, nil
}

func truncateBody(b []byte) string {
	const max = 300
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}
