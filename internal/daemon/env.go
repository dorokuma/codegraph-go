package daemon

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Env vars (aligned with official naming where practical).
const (
	EnvNoDaemon       = "CODEGRAPH_NO_DAEMON"
	EnvDaemonInternal = "CODEGRAPH_DAEMON_INTERNAL"
	EnvIdleTimeoutMS  = "CODEGRAPH_DAEMON_IDLE_TIMEOUT_MS"
	EnvPPIDPollMS     = "CODEGRAPH_PPID_POLL_MS"
	EnvLogAttach      = "CODEGRAPH_MCP_LOG_ATTACH"
)

const (
	defaultIdleTimeout = 300 * time.Second
	defaultPPIDPoll    = 5 * time.Second
)

// OptOut reports CODEGRAPH_NO_DAEMON truthy.
func OptOut() bool {
	return truthy(os.Getenv(EnvNoDaemon))
}

// Internal reports CODEGRAPH_DAEMON_INTERNAL truthy (this process IS the daemon).
func Internal() bool {
	return truthy(os.Getenv(EnvDaemonInternal))
}

// IdleTimeout from env; 0 means never idle-exit. Default 300s.
func IdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvIdleTimeoutMS))
	if raw == "" {
		return defaultIdleTimeout
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return defaultIdleTimeout
	}
	return time.Duration(n) * time.Millisecond
}

// PPIDPollInterval from env; 0 disables. Default 5s.
func PPIDPollInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(EnvPPIDPollMS))
	if raw == "" {
		return defaultPPIDPoll
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return defaultPPIDPoll
	}
	return time.Duration(n) * time.Millisecond
}

func truthy(raw string) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" || raw == "0" || raw == "false" || raw == "no" || raw == "off" {
		return false
	}
	return true
}
