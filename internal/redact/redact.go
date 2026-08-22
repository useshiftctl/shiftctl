// Package redact strips known secret patterns from strings before any content
// leaves the local machine for analysis. This is a defence-in-depth measure:
// the scanner is already designed to send structured facts rather than raw
// content, but any string field that passes through (e.g. webhook URLs,
// descriptions) is redacted here as well.
package redact

import "regexp"

// pattern pairs a compiled regex with its replacement string.
type pattern struct {
	re          *regexp.Regexp
	replacement string
}

var patterns = []pattern{
	// GitHub tokens: classic (ghp_, ghs_, ghu_) and fine-grained (github_pat_).
	{
		re:          regexp.MustCompile(`(ghp_|ghs_|ghu_|github_pat_)[A-Za-z0-9_]{10,}`),
		replacement: "[REDACTED:github-token]",
	},
	// GitLab personal access tokens.
	{
		re:          regexp.MustCompile(`glpat-[A-Za-z0-9\-_]{10,}`),
		replacement: "[REDACTED:gitlab-token]",
	},
	// AWS access key IDs.
	{
		re:          regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		replacement: "[REDACTED:aws-key-id]",
	},
	// Generic API keys/tokens/secrets in key=value form.
	{
		re:          regexp.MustCompile(`(?i)(api[_-]?key|apikey|access[_-]?token|secret[_-]?key|auth[_-]?token)\s*[:=]\s*["']?[A-Za-z0-9\-_\.]{16,}["']?`),
		replacement: "[REDACTED:api-key]",
	},
	// PEM private keys.
	{
		re:          regexp.MustCompile(`-----BEGIN\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE KEY-----[\s\S]*?-----END\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE KEY-----`),
		replacement: "[REDACTED:private-key]",
	},
	// Connection string passwords: scheme://user:password@host
	{
		re:          regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+\-.]*://[^:@/\s]+):([^@\s]{3,})@`),
		replacement: `$1:[REDACTED:password]@`,
	},
	// URL query parameters named token, secret, key, or password.
	{
		re:          regexp.MustCompile(`(?i)([?&](token|secret|key|password|api_key|access_token)=)[^&\s"']{4,}`),
		replacement: `$1[REDACTED]`,
	},
	// Slack webhook tokens (last path segment after /services/).
	{
		re:          regexp.MustCompile(`(hooks\.slack\.com/services/)[A-Z0-9]+/[A-Z0-9]+/[A-Za-z0-9]+`),
		replacement: `$1[REDACTED]`,
	},
}

// String returns s with all known secret patterns replaced by redaction
// placeholders. Safe to call on any user-controlled string before it is
// marshalled into a fact that will be sent for analysis.
func String(s string) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.replacement)
	}
	return s
}

// Strings applies String to every element of ss in place, returning the
// modified slice.
func Strings(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = String(s)
	}
	return out
}
