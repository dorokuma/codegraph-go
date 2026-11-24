package daemon

import "strings"

// selfEnvWriteAuthFrom applies the platform-independent read-only markers
// against the environment supplied by getenv (production: os.Getenv; tests
// inject a fake). A process carrying an explicit marker denies; anything
// else allows — off Linux there is no ancestry provenance to consult, so
// the marker set is the only provable signal and a clean environment must
// not break normal development.
//
// Kept in a file without a build tag so the verdict logic itself can be
// unit-tested on every platform (GOOS=linux CI included); only the os.Getenv
// wiring lives in writeauth_other.go behind the !linux tag.
func selfEnvWriteAuthFrom(getenv func(string) string) WriteAuth {
	if envTruthyOther(getenv("CTXMODE_READONLY")) {
		return WriteAuth{Allowed: false, Reason: "CTXMODE_READONLY set"}
	}
	if strings.TrimSpace(getenv("PI_ROLE")) != "" {
		return WriteAuth{Allowed: false, Reason: "PI_ROLE set"}
	}
	if v := strings.TrimSpace(getenv("CTXMODE_ROLE")); v != "" && strings.ToLower(v) != "worker" {
		return WriteAuth{Allowed: false, Reason: "CTXMODE_ROLE=" + v}
	}
	if strings.TrimSpace(getenv("PI_SUBAGENT")) == "1" &&
		strings.ToLower(strings.TrimSpace(getenv("CTXMODE_ROLE"))) != "worker" {
		return WriteAuth{Allowed: false, Reason: "PI_SUBAGENT without worker role"}
	}
	return WriteAuth{Allowed: true, Reason: "non-linux: no read-only marker in own environment"}
}

// envTruthyOther mirrors the Linux environTruthy truth-value set
// ("1/true/yes/on", case-insensitive, whitespace-trimmed). It lives here
// (not in writeauth_other.go) so selfEnvWriteAuthFrom compiles on every
// platform for the shared unit test.
func envTruthyOther(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
