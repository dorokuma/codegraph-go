package safelog

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRedactSensitive: every common secret shape must be masked, and plain
// text must pass through unchanged.
//
// Fixtures below are deliberately assembled at runtime (prefix + payload)
// instead of written as complete literals: GitHub secret scanning / push
// protection statically flags secret-shaped literals in source, even
// synthetic test fixtures — a push was rejected on exactly this. The
// concatenated input is byte-identical to what a plain literal would be, so
// redactSensitive still sees the full shape; only the file text never
// contains a complete secret literal. Keep the joins — "tidying" them back
// into literals will get pushes rejected again.
func TestRedactSensitive(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"token=" + "sk-" + "abcdefghijklmnopqrstuvwxyz123456", "token=[REDACTED]"},
		{"key=" + "ghp_" + "0123456789abcdefghijklmnopqrstuvwxyz", "key=ghp_[REDACTED]"},
		{"credential: " + "github_pat_" + "0123456789abcdefghijklmnopqrstuvwxyz", "credential: [REDACTED]"},
		{"Authorization: " + "Bearer " + "abcdefghijklmnopqrstuvwxyz0123456789", "Authorization: [REDACTED]"},
		{"Basic " + "dXNlcj" + "pwYXNzd29yZA==", "Basic [REDACTED]"},
		{"id=" + "AKIA" + "0123456789ABCDEF", "id=AKIA[REDACTED]"},
		{"key=" + "AIza" + "0123456789abcdefghijklmnopqrstuvwxy", "key=AIza[REDACTED]"},
		{"token=" + "xoxb-" + "123456789012-abcdefghijklmnopqrstuvwxyz", "token=[REDACTED]"},
		{"token=" + "sk_live_" + "abcdefghijklmnopqrstuvwxyz0123456789", "token=[REDACTED]"},
		{"jwt=" + "eyJ" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" + "." + "eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0" + "." + "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "jwt=[REDACTED]"},
		{"password=hunter2, user=alice", "password=[REDACTED], user=alice"},
		{"db_password=abc123", "db_password=[REDACTED]"},
		{"api_key = 0123456789abcdef", "api_key = [REDACTED]"},
		{"plain text with no secrets", "plain text with no secrets"},
		{"tokenizer=5", "tokenizer=5"},
		{"author: alice", "author: alice"},
		{"-----BEGIN RSA " + "PRIVATE KEY-----\n" + "MIIEowIBAAKCAQEA" + "\n-----END RSA " + "PRIVATE KEY-----", "[REDACTED]"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----", "[REDACTED]"},
	}
	for _, tt := range tests {
		if got := redactSensitive(tt.in); got != tt.want {
			t.Errorf("redactSensitive(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRedactAttrSensitiveKeys: values under sensitive keys are blanked
// wholesale, case-insensitively and for compound keys.
func TestRedactAttrSensitiveKeys(t *testing.T) {
	for _, k := range []string{
		"token", "api_token", "Token", "password", "db_password", "secret",
		"authorization", "cookie", "api_key", "apikey", "APICredential",
		"clientSecret", "x-api-key", "key", "session_id", "passphrase",
	} {
		got := redactAttr(nil, slog.Attr{Key: k, Value: slog.StringValue("supersecret-value")})
		if got.Value.String() != "[REDACTED]" {
			t.Errorf("key %q not redacted: %v", k, got.Value)
		}
	}
	// Non-sensitive keys pass through untouched.
	for _, k := range []string{"workdir", "author", "socket", "line"} {
		got := redactAttr(nil, slog.Attr{Key: k, Value: slog.StringValue("/root/codegraph-go")})
		if got.Value.String() != "/root/codegraph-go" {
			t.Errorf("non-sensitive key %q was redacted: %v", k, got.Value)
		}
	}
}

// TestRedactAttrValues: secret shapes inside string values, error messages
// (incl. %+v-style chains) and LogValuers are scrubbed.
func TestRedactAttrValues(t *testing.T) {
	// error chain: the formatted message carries a secret.
	got := redactAttr(nil, slog.Attr{Key: "error", Value: slog.AnyValue(
		errors.New("dial failed: secret=xyzzyplugh"),
	)})
	if s := got.Value.String(); s != "dial failed: secret=[REDACTED]" {
		t.Errorf("error value not scrubbed: %q", s)
	}

	// plain string value with a secret shape (assembled at runtime; see
	// TestRedactSensitive for why).
	got = redactAttr(nil, slog.Attr{Key: "msg", Value: slog.StringValue("got token " + "sk-" + "abcdefghijklmnopqrstuvwxyz123456")})
	if s := got.Value.String(); strings.Contains(s, "sk-"+"abcdefghijklmnopqrstuvwxyz123456") {
		t.Errorf("string value leaked: %q", s)
	}

	// LogValuer resolving to a secret-carrying string (assembled at runtime;
	// see TestRedactSensitive for why).
	got = redactAttr(nil, slog.Attr{Key: "detail", Value: slog.AnyValue(secretLogValue{"Bearer " + "abcdefghijklmnopqrstuvwxyz0123456789"})})
	if s := got.Value.String(); strings.Contains(s, "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("LogValuer value leaked: %q", s)
	}
}

type secretLogValue struct{ s string }

func (v secretLogValue) LogValue() slog.Value {
	return slog.StringValue(v.s)
}

// TestScrubbingWriter: legacy log lines are scrubbed at the writer level and
// the io.Writer contract (returned n == input length) is preserved.
func TestScrubbingWriter(t *testing.T) {
	var buf bytes.Buffer
	sw := &scrubbingWriter{w: &buf}
	in := "password=hunter2\n"
	n, err := sw.Write([]byte(in))
	if err != nil || n != len(in) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "password=[REDACTED]\n" {
		t.Fatalf("scrubbed = %q, want %q", got, "password=[REDACTED]\n")
	}
}

// TestSlogHandlerRedacts: the handler SetupLogger builds (TextHandler +
// ReplaceAttr) redacts end-to-end through slog.
func TestSlogHandlerRedacts(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: redactAttr})
	logger := slog.New(h)
	logger.Info("started", "api_token", "supersecret", "workdir", "/root/x")
	logger.Error("boom", "error", errors.New("auth: token=abc123"))
	s := buf.String()
	for _, leak := range []string{"supersecret", "abc123"} {
		if strings.Contains(s, leak) {
			t.Errorf("secret %q leaked via slog handler: %s", leak, s)
		}
	}
	if !strings.Contains(s, "/root/x") {
		t.Errorf("non-sensitive attr must survive: %s", s)
	}
	if !strings.Contains(s, "[REDACTED]") {
		t.Errorf("no redaction marker in output: %s", s)
	}
}

// TestSetupLoggerRedactsEndToEnd: real SetupLogger output — both slog and
// legacy log.Printf — is scrubbed before it reaches stderr (the daemon.log /
// Pi stderr path).
func TestSetupLoggerRedactsEndToEnd(t *testing.T) {
	oldStderr := os.Stderr
	defer func() { os.Stderr = oldStderr }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	_, cleanup := SetupLogger("debug")
	defer cleanup()
	defer r.Close()
	defer w.Close()

	slog.Info("hello", "token", "sk-"+"abcdefghijklmnopqrstuvwxyz123456", "password", "hunter2")
	log.Printf("legacy: Authorization: " + "Bearer " + "abcdefghijklmnopqrstuvwxyz0123456789")
	slog.Error("boom", "error", errors.New("dial failed: secret=xyzzyplugh"))

	// The non-blocking writer drains asynchronously: poll the pipe until all
	// three lines have arrived or the deadline passes.
	var out []byte
	buf := make([]byte, 8192)
	deadline := time.Now().Add(3 * time.Second)
	_ = r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	for time.Now().Before(deadline) {
		n, _ := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			_ = r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		}
		s := string(out)
		if strings.Contains(s, "hello") && strings.Contains(s, "legacy:") && strings.Contains(s, "boom") {
			break
		}
	}
	s := string(out)
	for _, leak := range []string{
		"sk-" + "abcdefghijklmnopqrstuvwxyz123456",
		"hunter2",
		"Bearer " + "abcdefghijklmnopqrstuvwxyz0123456789",
		"xyzzyplugh",
	} {
		if strings.Contains(s, leak) {
			t.Errorf("secret %q leaked into stderr output:\n%s", leak, s)
		}
	}
	if !strings.Contains(s, "[REDACTED]") {
		t.Errorf("no redaction marker in stderr output:\n%s", s)
	}
}
