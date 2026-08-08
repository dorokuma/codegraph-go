package safelog

import (
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Redaction strategy (H1): two layers, both wired in SetupLogger.
//
//  1. slog.ReplaceAttr (redactAttr) blanks the WHOLE value of attrs whose key
//     is sensitive (sensitiveKey), and runs redactSensitive over string
//     values, error messages and the record message.
//  2. scrubbingWriter applies redactSensitive to every byte stream that goes
//     to the non-blocking writer, so legacy log.Printf lines — which bypass
//     the slog handler entirely — get the same shape masking.
//
// Capability boundary (documented in the package comment): this is best
// effort. A secret that matches no known shape AND appears under a
// non-sensitive key (e.g. a random-looking 32-char string in "msg"), a pure
// numeric secret, or a multi-line private key split across several legacy log
// writes can still reach the log. Do not rely on safelog as the only line of
// defense; avoid logging secrets at the source.

// sensitiveKeyParts are case-insensitive substrings that mark a slog attr key
// as sensitive; the entire value is replaced with [REDACTED].
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "credential", "authorization",
	"cookie", "apikey", "api_key", "api-key", "sessionid", "session_id",
	"access_key", "accesskey", "private_key", "privatekey", "client_secret",
	"passphrase",
}

// sensitiveKeyExact are keys that must match the whole key name
// (case-insensitive). "key"/"auth" alone would false-positive as substrings
// ("monkey", "author"), so they are exact-match only.
var sensitiveKeyExact = map[string]bool{
	"key":          true,
	"keys":         true,
	"pwd":          true,
	"auth":         true,
	"bearer":       true,
	"x-api-key":    true,
	"x-auth-token": true,
}

// sensitiveKey reports whether an attr key must be redacted wholesale.
func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return sensitiveKeyExact[lower]
}

// secretPatterns are applied in order to every logged string. Each pattern
// either replaces the whole secret with [REDACTED] or keeps a non-secret
// prefix (e.g. "Bearer ") so the log stays readable. Order matters: the
// key=value pattern runs before the prefix patterns so "authorization: Basic
// dXNlcjpwYXNz" is consumed wholesale instead of being redacted twice.
var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Full PEM private key blocks (multi-line; slog escapes newlines so a
	// block inside one string value still matches here).
	{regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`), "[REDACTED]"},
	// BEGIN/END markers on their own, in case a block is split across
	// several legacy log writes (the base64 body lines then still leak —
	// documented boundary).
	{regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`), "[REDACTED]"},
	{regexp.MustCompile(`-----END (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`), "[REDACTED]"},
	// key=value / key: value pairs with sensitive names (no word boundary, so
	// "db_password=..." is caught too). The value runs to the next comma,
	// semicolon or newline; quotes around the value are included.
	{regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret|private[_-]?key|authorization|credential|session[_-]?id)\s*[=:]\s*)[^,;\n]+`), "${1}[REDACTED]"},
	// Basic auth base64.
	{regexp.MustCompile(`(?i)(\bbasic\s+)[a-z0-9+/=]{8,}`), "${1}[REDACTED]"},
	// Bearer tokens.
	{regexp.MustCompile(`(?i)(\bbearer\s+)[-a-z0-9._~+/=:_]{8,}`), "${1}[REDACTED]"},
	// JWTs (eyJ… header).
	{regexp.MustCompile(`(?i)\beyJ[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\b`), "[REDACTED]"},
	// OpenAI / Anthropic-style sk- keys.
	{regexp.MustCompile(`(?i)\b(sk-)[a-z0-9_-]{8,}`), "${1}[REDACTED]"},
	// GitHub classic tokens.
	{regexp.MustCompile(`\b(gh[pousr]_)[a-z0-9]{20,}`), "${1}[REDACTED]"},
	// GitHub fine-grained PATs.
	{regexp.MustCompile(`\b(github_pat_)[a-z0-9_]{20,}`), "${1}[REDACTED]"},
	// AWS access key IDs.
	{regexp.MustCompile(`\b(AKIA)[0-9A-Z]{16}\b`), "${1}[REDACTED]"},
	// Google API keys.
	{regexp.MustCompile(`\b(AIza)[0-9A-Za-z_-]{35}\b`), "${1}[REDACTED]"},
	// Slack tokens.
	{regexp.MustCompile(`\b(xox[baprs]-)[a-z0-9-]{10,}`), "${1}[REDACTED]"},
	// Stripe sk_live_/sk_test_/rk_… keys.
	{regexp.MustCompile(`\b((?:sk|rk)_(?:live|test)_)[a-z0-9]{16,}`), "${1}[REDACTED]"},
}

// redactSensitive masks every known secret shape in s.
func redactSensitive(s string) string {
	for _, p := range secretPatterns {
		if p.re.MatchString(s) {
			s = p.re.ReplaceAllString(s, p.repl)
		}
	}
	return s
}

// redactAttr is the slog.ReplaceAttr: it blanks values under sensitive keys
// before formatting, and scrubs known secret shapes out of string values,
// error messages, and the record message.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Key == "" {
		return a
	}
	if sensitiveKey(a.Key) {
		a.Value = slog.StringValue("[REDACTED]")
		return a
	}
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(redactSensitive(a.Value.String()))
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			// %+v / error chains: the formatted message is scrubbed.
			a.Value = slog.StringValue(redactSensitive(v.Error()))
		case string:
			a.Value = slog.StringValue(redactSensitive(v))
		case fmt.Stringer:
			a.Value = slog.StringValue(redactSensitive(v.String()))
		}
	case slog.KindLogValuer:
		// Resolve LogValuer chains, then re-dispatch so the resolved value
		// gets the same treatment.
		v := a.Value
		for v.Kind() == slog.KindLogValuer {
			v = v.Resolve()
		}
		a.Value = v
		return redactAttr(nil, a)
	}
	return a
}

// scrubbingWriter passes every write through redactSensitive, so legacy
// log.Printf lines (which bypass the slog handler) are scrubbed too. The
// returned byte count is the input length regardless of scrubbing, per the
// io.Writer contract.
type scrubbingWriter struct {
	w io.Writer
}

func (s *scrubbingWriter) Write(p []byte) (int, error) {
	_, err := s.w.Write([]byte(redactSensitive(string(p))))
	return len(p), err
}
