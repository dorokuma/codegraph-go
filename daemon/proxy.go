package daemon

import (
	"bufio"
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

	// PPID watchdog closes the socket (daemon refcount--) then we exit.
	stopWD := StartPPIDWatchdog(PPIDPollInterval(), func() {
		_ = conn.Close()
		os.Exit(0)
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
func DialAnyCandidate(projectRoot string) (net.Conn, *bufio.Reader, Hello, bool) {
	for _, c := range SocketCandidates(projectRoot) {
		conn, br, hello, res := ConnectHello(c)
		if res.Outcome == "proxied" {
			return conn, br, hello, true
		}
		if res.Reason != "" && contains(res.Reason, "version mismatch") {
			// Definitive — don't keep probing.
			return nil, nil, Hello{}, false
		}
	}
	return nil, nil, Hello{}, false
}

// EnsureAndDial probes for a live daemon, spawning one if needed, then dials.
// Returns ok=false when the daemon path is unavailable (caller → direct mode).
func EnsureAndDial(projectRoot string, wait time.Duration, poll time.Duration) (net.Conn, *bufio.Reader, Hello, bool) {
	if conn, br, hello, ok := DialAnyCandidate(projectRoot); ok {
		return conn, br, hello, true
	}
	if err := SpawnDetached(projectRoot); err != nil {
		log.Printf("spawn daemon: %v", err)
		return nil, nil, Hello{}, false
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		if conn, br, hello, ok := DialAnyCandidate(projectRoot); ok {
			return conn, br, hello, true
		}
	}
	return nil, nil, Hello{}, false
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
