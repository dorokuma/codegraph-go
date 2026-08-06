package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// ProxyResult is the outcome of RunProxy.
type ProxyResult struct {
	// Outcome is "proxied" (ran until disconnect) or "fallback-needed".
	Outcome string
	Reason  string
}

// ConnectHello dials socketPath, reads/verifies hello, returns the live conn
// with a bufio reader positioned after the hello line. On version mismatch or
// connect failure returns outcome fallback-needed and nil conn.
func ConnectHello(socketPath string) (net.Conn, *bufio.Reader, Hello, ProxyResult) {
	if socketPath == "" {
		return nil, nil, Hello{}, ProxyResult{Outcome: "fallback-needed", Reason: "empty socket path"}
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, nil, Hello{}, ProxyResult{Outcome: "fallback-needed", Reason: "socket file missing"}
	}
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return nil, nil, Hello{}, ProxyResult{Outcome: "fallback-needed", Reason: err.Error()}
	}
	br := bufio.NewReader(conn)
	hello, err := ReadHello(br)
	if err != nil {
		_ = conn.Close()
		return nil, nil, Hello{}, ProxyResult{Outcome: "fallback-needed", Reason: err.Error()}
	}
	if hello.Codegraph != PackageVersion {
		_ = conn.Close()
		return nil, nil, hello, ProxyResult{
			Outcome: "fallback-needed",
			Reason:  fmt.Sprintf("version mismatch daemon=%s ours=%s", hello.Codegraph, PackageVersion),
		}
	}
	return conn, br, hello, ProxyResult{Outcome: "proxied"}
}

// RunProxy pipes host stdio through a same-version daemon socket until either end closes.
// Call after ConnectHello succeeded; br must be the reader positioned after daemon hello.
func RunProxy(conn net.Conn, br *bufio.Reader, hello Hello) ProxyResult {
	if truthy(os.Getenv(EnvLogAttach)) {
		log.Printf("attached to shared daemon on %s (pid %d, v%s)", hello.SocketPath, hello.PID, hello.Codegraph)
	}
	if err := WriteClientHello(conn); err != nil {
		log.Printf("write client hello: %v", err)
	}

	// PPID watchdog: closing the socket ends both io.Copy legs (daemon
	// refcount--), so RunProxy returns normally and deferred cleanup runs.
	// No os.Exit here — graceful shutdown keeps defer chains intact (B3).
	stopWD := StartPPIDWatchdog(PPIDPollInterval(), func() {
		_ = conn.Close()
	})
	defer stopWD()

	errc := make(chan error, 2)
	// Host stdin → daemon (after optional leftover in br is empty).
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		_ = conn.Close()
		errc <- err
	}()
	// Daemon → host stdout. Drain br first (should be empty post-hello), then conn.
	go func() {
		// Anything already buffered after hello (shouldn't be) then the rest of the conn.
		mr := io.MultiReader(br, conn)
		_, err := io.Copy(os.Stdout, mr)
		errc <- err
	}()

	if err := <-errc; err != nil && err != io.EOF {
		log.Printf("proxy copy: %v", err)
	}
	_ = conn.Close()
	return ProxyResult{Outcome: "proxied"}
}

// DialAnyCandidate walks SocketCandidates and returns the first same-version connection.
// The returned reason is non-empty when a definitive version-mismatch daemon was found
// (callers use it to decide whether a stale daemon needs to be killed first).
func DialAnyCandidate(projectRoot string) (net.Conn, *bufio.Reader, Hello, bool, string) {
	for _, c := range SocketCandidates(projectRoot) {
		conn, br, hello, res := ConnectHello(c)
		if res.Outcome == "proxied" {
			return conn, br, hello, true, ""
		}
		if res.Reason != "" && contains(res.Reason, "version mismatch") {
			// Definitive — don't keep probing.
			return nil, nil, Hello{}, false, res.Reason
		}
	}
	return nil, nil, Hello{}, false, ""
}

// halfDeadStartupGrace is how long after a daemon wrote its pidfile we treat
// dial failures as a startup window instead of a half-dead process. The
// daemon writes the pidfile (TryAcquireLock) BEFORE it binds the socket
// (Daemon.Start), so a healthy daemon mid-startup looks exactly like a
// half-dead one for a few milliseconds. Killing inside this window would
// SIGTERM a healthy daemon that is merely still starting.
const halfDeadStartupGrace = 3 * time.Second

// halfDeadDaemonHoldsLock reports whether the pidfile names a LIVE process
// while DialAnyCandidate has just failed on every candidate socket. That is
// the half-dead state: the daemon's Stop path got stuck (listener closed —
// which unlinks the socket file — but Stop wedged in d.wg.Wait() behind a
// session/accept goroutine that never returns, so cleanupArtifacts never ran
// and the pidfile is still there) before the process exited. The lock is
// held by a process that will never accept this client; it keeps blocking
// spawns and direct-mode fallback (which refuses to run on top of a live
// writer), so treat it like a version mismatch — kill and replace — instead
// of burning the spawn + 6s poll cycle.
//
// Two guards prevent killing a healthy daemon:
//   - Startup grace: a pidfile written less than halfDeadStartupGrace ago is
//     a daemon still binding its socket — dial failures there are transient
//     and the spawn loop below retries, so it is never half-dead.
//   - Legacy pidfiles without startedAt cannot be age-gated; refusing to
//     classify them as half-dead is the safe choice (a false kill would
//     SIGTERM a healthy daemon, while a missed kill only costs one extra
//     spawn attempt, which the normal lock-taken path resolves).
func halfDeadDaemonHoldsLock(projectRoot string) bool {
	raw, err := os.ReadFile(PidPath(projectRoot))
	if err != nil {
		return false // no pidfile → nothing holds the lock
	}
	info := DecodeLock(raw)
	if info == nil || info.PID <= 0 || !IsProcessAlive(info.PID) {
		return false
	}
	// Startup window: pidfile written, socket not yet bound (or not yet
	// answering). Never kill inside the grace window.
	if info.StartedAt > 0 && time.Since(time.UnixMilli(info.StartedAt)) < halfDeadStartupGrace {
		return false
	}
	// Old pidfile without startedAt: cannot verify age — conservative
	// no-kill (see comment above).
	if info.StartedAt <= 0 {
		return false
	}
	return true
}

// killStaleDaemonForSpawn terminates the recorded daemon so the caller can
// spawn a replacement, mapping failures to the caller's contract:
// ErrStaleDaemonRefused is wrapped (fatal — an unidentified live process holds
// the lock; do NOT spawn), any other kill failure (e.g. signal sent but exit
// wait timed out) is logged and the caller proceeds with a fresh spawn
// (historical behavior).
func killStaleDaemonForSpawn(projectRoot string) error {
	if err := KillStaleDaemon(projectRoot); err != nil {
		if errors.Is(err, ErrStaleDaemonRefused) {
			// G1: the PID-reuse guard refused to signal the recorded
			// process because its identity could not be confirmed. The
			// lock is still held by a live, unidentified process: spawning
			// on top of it or falling back to direct mode would risk
			// double-writing the index, so stop and hand the refusal to
			// the caller (main prints an actionable message and exits).
			return fmt.Errorf("stale daemon at %s failed identity check: %w", PidPath(projectRoot), err)
		}
		// Ordinary failure (e.g. signal sent but exit wait timed out):
		// keep historical behavior — log and retry via a fresh spawn.
		log.Printf("kill stale daemon: %v", err)
	}
	return nil
}

// EnsureAndDial probes for a live daemon, spawning one if needed, then dials.
// Returns ok=false when the daemon path is unavailable (caller → direct mode).
// The returned error is non-nil when the daemon path is unusable for a reason
// the caller must surface: it wraps ErrStaleDaemonRefused when the stale-daemon
// kill was REFUSED (PID-reuse guard — an unidentified live process holds the
// lock; do NOT spawn and do NOT fall back to direct mode), and it carries the
// spawn error when SpawnDetached itself failed (safe to fall back to direct).
// opts is passed to SpawnDetached so -config / -no-sync match the parent.
func EnsureAndDial(projectRoot string, wait time.Duration, poll time.Duration, opts *SpawnOpts) (net.Conn, *bufio.Reader, Hello, bool, error) {
	conn, br, hello, ok, reason := DialAnyCandidate(projectRoot)
	if ok {
		return conn, br, hello, true, nil
	}
	if contains(reason, "version mismatch") {
		// Definitive version mismatch (B1): the running daemon belongs to a
		// different build and will never accept us. Kill it and clear its
		// pidfile so the fresh spawn below can acquire the lock; the old
		// daemon must not keep writing an index this build is about to
		// migrate/replace.
		if err := killStaleDaemonForSpawn(projectRoot); err != nil {
			return nil, nil, Hello{}, false, err
		}
	} else if halfDeadDaemonHoldsLock(projectRoot) {
		// No candidate socket answered, yet the pidfile names a live
		// process: a half-dead daemon holds the lock but is not serving
		// (its Stop got stuck after the listener closed and the socket file
		// was unlinked, before cleanupArtifacts removed the pidfile). It
		// will never accept this client, so replace it the same way as a
		// version mismatch — the fresh spawn below needs the lock and the
		// DB. Identity verification is unchanged: KillStaleDaemon still
		// runs verifyDaemonIdentity before signaling (PID-reuse guard), and
		// halfDeadDaemonHoldsLock's startup grace keeps a healthy daemon
		// mid-bind from being misclassified.
		if err := killStaleDaemonForSpawn(projectRoot); err != nil {
			return nil, nil, Hello{}, false, err
		}
	}
	if err := SpawnDetached(projectRoot, opts); err != nil {
		log.Printf("spawn daemon: %v", err)
		return nil, nil, Hello{}, false, err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if conn, br, hello, ok, _ := DialAnyCandidate(projectRoot); ok {
			return conn, br, hello, true, nil
		}
	}
	return nil, nil, Hello{}, false, nil
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
