//go:build linux

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// WriteAuth is the per-connection (or per-process, for direct mode) verdict
// on whether store_fact writes are allowed. The zero value is the safe one:
// Allowed=false with an empty Reason fails closed — a session that never ran
// verification must be refused, so callers treat the zero value as deny.
type WriteAuth struct {
	Allowed bool
	Reason  string
}

// allow returns an allowed WriteAuth.
func allow(reason string) WriteAuth { return WriteAuth{Allowed: true, Reason: reason} }

// deny returns a refused WriteAuth.
func deny(reason string) WriteAuth { return WriteAuth{Allowed: false, Reason: reason} }

// ---------- dependency-injectable probes ----------
//
// PeerAuth bundles every host interaction the write-auth verdict needs so
// unit tests can drive the full decision tree without real /proc entries,
// real sockets, or a live herdr-core server. Production code uses
// defaultPeerAuth; every field is replaceable.

// procHop is one fully-recorded hop of the /proc ancestry chain. Every
// probe result is kept with its own ok flag — collectChain never aborts
// early on an unreadable ancestor, so the verdict layer can decide per
// branch (hc present vs absent) exactly how much of the chain must have
// been readable.
type procHop struct {
	PID    int
	PPID   int
	UID    int
	Env    map[string]string
	EnvOK  bool
	Argv   []string
	ArgvOK bool
	PPIDOK bool
	UIDOK  bool
}

// HcTerminalInfo mirrors the strict herdr-core terminal.get fields the
// verdict consumes. ParentHasKey reports whether the "parent" key was
// present at all: JSON null (top-level terminal) and a non-empty string
// (subagent terminal) are both legal, while a missing key, "" or any other
// type must fail closed — so bare *string cannot express it.
type HcTerminalInfo struct {
	Address      string
	State        string
	PID          int
	Parent       *string // nil = literal null (top-level); non-nil non-empty = subagent
	ParentHasKey bool
}

type PeerAuth struct {
	// PeerPID resolves the kernel-authenticated pid of a unix connection
	// (SO_PEERCRED). Non-unix connections return ok=false.
	PeerPID func(conn net.Conn) (int, bool)
	// ReadEnviron returns the KEY=VALUE map of /proc/<pid>/environ.
	// ok=false on ENOENT/EACCES/empty/read error — all fail closed.
	ReadEnviron func(pid int) (map[string]string, bool)
	// ReadPPID returns the parent pid from /proc/<pid>/stat.
	ReadPPID func(pid int) (int, bool)
	// ReadArgv returns the raw argv of /proc/<pid>/cmdline with interior
	// empty strings preserved (parseProcCmdline semantics). ok=false when
	// the cmdline cannot be read at all — never silently dropped.
	ReadArgv func(pid int) ([]string, bool)
	// ReadUID returns the owner uid of /proc/<pid> (syscall.Stat_t.Uid).
	// ok=false when the process cannot be stat'ed.
	ReadUID func(pid int) (int, bool)
	// SelfEUID returns the effective uid of this process (os.Geteuid) —
	// used to decide whether an EACCES on the root-owned hc socket means
	// "no hc for us" instead of fail-closed.
	SelfEUID func() int
	// StatHcSocket stats the herdr-core socket path. os.ErrNotExist means
	// "no hc environment" (CI/dev box); under root any other error fails
	// closed, while a non-root process treats EACCES/EPERM on the
	// root-owned /run/user/0 path as "hc unreachable for us" too.
	StatHcSocket func(path string) error
	// QueryHcTerminal runs terminal.get against the hc socket.
	QueryHcTerminal func(socketPath, termID string, timeout time.Duration) (HcTerminalInfo, bool)
	// HcSocketPath is the hard-coded host socket (not env-overridable).
	HcSocketPath string
	// HcTimeout bounds the terminal.get query.
	HcTimeout time.Duration
	// MaxHops bounds the ancestry walk (default 32).
	MaxHops int
}

// hcSocketFile is the hard-coded host herdr-core socket, aligned with
// exclude-tools-guard's HcSocketFile. Never derived from the environment.
const hcSocketFile = "/run/user/0/herdr-core/default.sock"

// hcQueryTimeout is the single terminal.get budget (1500ms, aligned with
// IdentityQueryTimeout in exclude-tools-guard).
const hcQueryTimeout = 1500 * time.Millisecond

// ancestorMaxHops bounds the /proc ancestry walk.
const ancestorMaxHops = 32

func defaultPeerAuth() *PeerAuth {
	return &PeerAuth{
		PeerPID:         peerPIDUnix,
		ReadEnviron:     readProcEnviron,
		ReadPPID:        readProcPPID,
		ReadArgv:        readProcArgv,
		ReadUID:         readProcUID,
		SelfEUID:        os.Geteuid,
		StatHcSocket:    statHcSocket,
		QueryHcTerminal: queryHcTerminal,
		HcSocketPath:    hcSocketFile,
		HcTimeout:       hcQueryTimeout,
		MaxHops:         ancestorMaxHops,
	}
}

// ---------- kernel peer credentials ----------

// peerPIDUnix extracts the kernel peer pid of a *net.UnixConn via
// SO_PEERCRED. Anything else (TCP, pipes, non-unix) returns ok=false.
func peerPIDUnix(conn net.Conn) (int, bool) {
	u, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return 0, false
	}
	var pid int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		cred, opErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if opErr == nil {
			pid = int(cred.Pid)
		}
	}); err != nil {
		return 0, false
	}
	if opErr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// ---------- /proc readers ----------

// readProcEnviron reads /proc/<pid>/environ into a map. ok=false when the
// file is missing, unreadable (EACCES), or empty — the caller treats every
// one of those as fail-closed.
func readProcEnviron(pid int) (map[string]string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	env := make(map[string]string)
	for _, kv := range strings.Split(string(data), "\x00") {
		if kv == "" {
			continue
		}
		k, v, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		env[k] = v
	}
	return env, true
}

// readProcPPID parses /proc/<pid>/stat for the parent pid: fields after the
// final ')' are state(1) ppid(2) — matching exclude-tools-guard's
// ReadProcPpid so a comm containing ')' cannot shift the parse.
func readProcPPID(pid int) (int, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	stat := string(data)
	closeIdx := strings.LastIndex(stat, ")")
	if closeIdx < 0 || closeIdx+1 >= len(stat) {
		return 0, false
	}
	rest := strings.TrimSpace(stat[closeIdx+1:])
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil || ppid < 0 {
		return 0, false
	}
	return ppid, true
}

// readProcArgv reads /proc/<pid>/cmdline via parseProcCmdline. ok=false
// when the file cannot be read — the caller must see the failure, not an
// empty argv that looks like a kernel thread.
func readProcArgv(pid int) ([]string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, false
	}
	return parseProcCmdline(data), true
}

// readProcUID returns the owner uid of /proc/<pid> itself — the same
// credential the kernel would report for the process.
func readProcUID(pid int) (int, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(fmt.Sprintf("/proc/%d", pid), &st); err != nil {
		return 0, false
	}
	return int(st.Uid), true
}

// parseProcCmdline splits a NUL-separated cmdline while preserving interior
// empty strings. Only the single trailing NUL is trimmed
// (bytes.TrimSuffix(data, []byte{0})); interior "" elements are kept.
//
// This is a deliberate anti-shift invariant: BuildPig can legitimately emit
// `--append-system-prompt ""` — an EMPTY argument. Filtering "" away would
// slide `--name` (or `--agent`) one slot left so the flag scanner mistakes
// the following value for the flag's own argument and skips the real role
// marker (`role-scout-…`). Keep every element verbatim.
func parseProcCmdline(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	trimmed := bytes.TrimSuffix(data, []byte{0})
	parts := strings.Split(string(trimmed), "\x00")
	return parts
}

// ---------- herdr-core terminal.get ----------

// statHcSocket stats the hc socket path for the production defaultPeerAuth.
func statHcSocket(path string) error {
	_, err := os.Stat(path)
	return err
}

// queryHcTerminal asks the herdr-core server for a terminal record via
// JSON-RPC 2.0 terminal.get — the same raw map[string]json.RawMessage
// strict decode as exclude-tools-guard's QueryHcTerminalInfo (extension.go
// :328-392), with two deliberate tightenings for write auth:
//
//   - "state" is REQUIRED and must be the string "live" (a record without a
//     state key cannot prove the terminal is alive).
//   - "address" when present must equal the requested termID; when absent we
//     fall back to "id" (and "id" then must match) — never trust blindly.
//   - "pid" must be a positive integer (float64 with integral value or int).
//   - "parent" must be an explicit key: literal null => Parent=nil,
//     ParentHasKey=true (top-level terminal); non-empty string => the
//     parent's address (subagent terminal). Missing key, "" or any other
//     type => ParentHasKey=false and ok=false.
//
// Any transport, deadline, decode or validation failure is (zero, false).
func queryHcTerminal(socketPath, termID string, timeout time.Duration) (HcTerminalInfo, bool) {
	deadline := time.Now().Add(timeout)
	dialTimeout := time.Until(deadline)
	if dialTimeout <= 0 {
		return HcTerminalInfo{}, false
	}
	conn, err := net.DialTimeout("unix", socketPath, dialTimeout)
	if err != nil {
		return HcTerminalInfo{}, false
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	req := map[string]any{
		"id":      "codegraph-write-auth",
		"jsonrpc": "2.0",
		"method":  "terminal.get",
		"params":  map[string]any{"caller": termID, "terminal": termID},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return HcTerminalInfo{}, false
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return HcTerminalInfo{}, false
	}

	reader := bufio.NewReader(io.LimitReader(conn, 1048576))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return HcTerminalInfo{}, false
	}

	var reply struct {
		ID     string                     `json:"id"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return HcTerminalInfo{}, false
	}
	if reply.ID != "codegraph-write-auth" || reply.Result == nil {
		return HcTerminalInfo{}, false
	}

	info := HcTerminalInfo{}

	// address: must match the requested termID when the key exists; when it
	// is absent fall back to "id" (which then must match). Both missing or
	// mismatched => fail.
	rawAddr, hasAddr := reply.Result["address"]
	if hasAddr {
		if err := json.Unmarshal(rawAddr, &info.Address); err != nil || info.Address != termID {
			return HcTerminalInfo{}, false
		}
	} else {
		rawID, hasID := reply.Result["id"]
		if !hasID {
			return HcTerminalInfo{}, false
		}
		if err := json.Unmarshal(rawID, &info.Address); err != nil || info.Address != termID {
			return HcTerminalInfo{}, false
		}
	}

	// state: REQUIRED and must be exactly "live".
	rawState, hasState := reply.Result["state"]
	if !hasState {
		return HcTerminalInfo{}, false
	}
	if err := json.Unmarshal(rawState, &info.State); err != nil || info.State != "live" {
		return HcTerminalInfo{}, false
	}

	// pid: positive integer (JSON float64 with integral value, or int).
	rawPid, hasPid := reply.Result["pid"]
	if !hasPid {
		return HcTerminalInfo{}, false
	}
	var pidValue any
	if err := json.Unmarshal(rawPid, &pidValue); err != nil {
		return HcTerminalInfo{}, false
	}
	switch p := pidValue.(type) {
	case float64:
		info.PID = int(p)
		if float64(info.PID) != p {
			return HcTerminalInfo{}, false
		}
	case int:
		info.PID = p
	default:
		return HcTerminalInfo{}, false
	}
	if info.PID <= 0 {
		return HcTerminalInfo{}, false
	}

	// parent: explicit key required. null => top-level; non-empty string =>
	// subagent parent's address. "" / other type / missing => fail closed.
	rawParent, hasParent := reply.Result["parent"]
	if !hasParent {
		return HcTerminalInfo{}, false
	}
	var parent *string
	if err := json.Unmarshal(rawParent, &parent); err != nil {
		return HcTerminalInfo{}, false
	}
	info.ParentHasKey = true
	if parent != nil {
		if strings.TrimSpace(*parent) == "" {
			return HcTerminalInfo{}, false
		}
		info.Parent = parent
	}
	return info, true
}

// ---------- verdict ----------

// hcTerminalIDPattern is the herdr-core terminal address shape ("t12t").
var hcTerminalIDPattern = regexp.MustCompile(`^t[0-9a-z]+$`)

// roleNameFlagPattern matches a dispatched read-only role name passed to
// --name/-n: "role-scout-a1b2c3d4", "role-verifier", etc. The (?:-|$) tail
// keeps a plain "--name role" or "--name rolefoo" from matching.
var roleNameFlagPattern = regexp.MustCompile(`^role-(scout|planner|researcher|verifier|reviewer|oracle)(?:-|$)`)

// agentFlagPattern matches a dispatched read-only role passed to --agent:
// "scout", "oracle", or the "--" agent-spec separator form ("scout--worker").
var agentFlagPattern = regexp.MustCompile(`^(scout|planner|researcher|verifier|reviewer|oracle)(?:--|$)`)

// skipValueFlags are CLI flags whose following argv element is their VALUE
// (never a flag), so the scanner must skip argv[i+1] verbatim — an empty
// value ("--append-system-prompt ”") must still consume its slot.
var skipValueFlags = map[string]bool{
	"--system-prompt":        true,
	"-i":                     true,
	"--settings":             true,
	"--mcp-config":           true,
	"--append-system-prompt": true,
	"--prompt-file":          true,
	"--session-dir":          true,
	"--allowedTools":         true,
	"--disallowedTools":      true,
	"--tools":                true,
	"--exclude-tools":        true,
}

// flagValue extracts the value of "--flag value" or "--flag=value" (or the
// short "-n value" / "-n=value" forms). Returns (value, consumed) where
// consumed reports how many argv elements the flag used (1 for the = form,
// 2 for the separated form when a next element exists). A flag that is the
// last element yields ("", 1, false-ish) — handled by the caller as
// "no value to inspect".
func flagValue(argv []string, i int, names ...string) (val string, consumed int, matched bool) {
	a := argv[i]
	for _, n := range names {
		if a == n {
			if i+1 < len(argv) {
				return argv[i+1], 2, true
			}
			return "", 1, true
		}
		if strings.HasPrefix(a, n+"=") {
			return a[len(n)+1:], 1, true
		}
	}
	return "", 0, false
}

// scanArgvReadonly runs the 2b structured flag scan over one hop's raw argv:
// --name/-n whose value matches roleNameFlagPattern, or --agent whose value
// matches agentFlagPattern, marks the process a dispatched read-only role.
//
// Scanning starts at argv[1] (argv[0] is the program). Value-taking flags in
// skipValueFlags consume their next element verbatim — including an empty
// string — so `--append-system-prompt "" --name role-scout-…` still sees
// --name (the "" is skipped as a value, not dropped by cmdline filtering).
// A literal "--" ends flag parsing.
func scanArgvReadonly(argv []string) bool {
	for i := 1; i < len(argv); {
		a := argv[i]
		if a == "--" {
			return false
		}
		if skipValueFlags[a] {
			i += 2 // skip flag + its value element (even when "")
			continue
		}
		// Also handle "--flag=value" forms of value-taking flags so the
		// combined token itself is consumed (it is a single element — just
		// don't misread it as a role flag).
		isValueFlagEq := false
		for f := range skipValueFlags {
			if strings.HasPrefix(a, f+"=") {
				isValueFlagEq = true
				break
			}
		}
		if isValueFlagEq {
			i++
			continue
		}
		if val, consumed, matched := flagValue(argv, i, "--name", "-n"); matched {
			if roleNameFlagPattern.MatchString(val) {
				return true
			}
			i += consumed
			continue
		}
		if val, consumed, matched := flagValue(argv, i, "--agent"); matched {
			if agentFlagPattern.MatchString(val) {
				return true
			}
			i += consumed
			continue
		}
		i++
	}
	return false
}

// environTruthy reports a "1/true/yes/on" truthy value.
func environTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hopIsUnconditionalReadonly is the FIRST 2a segment: any single hop whose
// environ or argv proves a dispatched read-only role refuses writes no
// matter what any other hop says.
func hopIsUnconditionalReadonly(e procHop) bool {
	if scanArgvReadonly(e.Argv) {
		return true
	}
	env := e.Env
	if environTruthy(env["CTXMODE_READONLY"]) {
		return true
	}
	if v := strings.TrimSpace(env["PI_ROLE"]); v != "" {
		return true
	}
	if v := strings.TrimSpace(env["CTXMODE_ROLE"]); v != "" && strings.ToLower(v) != "worker" {
		return true
	}
	return false
}

// hopIsWorker reports whether this hop's environ declares a worker role.
func hopIsWorker(e procHop) bool {
	v := strings.TrimSpace(e.Env["CTXMODE_ROLE"])
	return v != "" && strings.ToLower(v) == "worker"
}

// hopIsConditionalReadonly is the SECOND 2a segment: markers that only
// refuse when NO hop in the checked window declared CTXMODE_ROLE=worker —
// a dispatched worker launched through a subagent shell keeps those
// intermediate markers legitimately.
func hopIsConditionalReadonly(e procHop) bool {
	if strings.TrimSpace(e.Env["PI_SUBAGENT"]) == "1" {
		return true
	}
	for _, a := range e.Argv {
		if strings.Contains(a, "/tmp/herdr-role-sessions") || strings.Contains(a, "/tmp/dispatch-") {
			return true
		}
	}
	return false
}

// collectChain walks /proc from peerPID upward (bounded by MaxHops) and
// records EVERY hop's probe results independently — environ, argv, ppid,
// uid — without dying mid-walk. The walk ends when ppid <= 0, a cycle
// repeats a pid, or MaxHops is reached (truncated=true). Whether an
// unreadable hop is fatal is a per-branch decision left to the verdict:
// hops above the hc shell_pid (herdr-core / PID 1) are legitimately
// unreadable, while hops inside the checked window must all be readable.
func collectChain(pa *PeerAuth, peerPID int) (chain []procHop, truncated bool) {
	hops := pa.MaxHops
	if hops <= 0 {
		hops = ancestorMaxHops
	}
	seen := make(map[int]bool)
	current := peerPID
	for hop := 0; hop < hops; hop++ {
		if current <= 0 || seen[current] {
			return chain, false
		}
		seen[current] = true
		h := procHop{PID: current}
		h.Env, h.EnvOK = pa.ReadEnviron(current)
		h.Argv, h.ArgvOK = pa.ReadArgv(current)
		h.PPID, h.PPIDOK = pa.ReadPPID(current)
		h.UID, h.UIDOK = pa.ReadUID(current)
		chain = append(chain, h)
		switch {
		case !h.PPIDOK:
			// Parent unreadable — the chain cannot be walked further; the
			// caller decides whether that is a safe stop or fail-closed.
			return chain, false
		case h.PPID <= 0 || h.PPID == current:
			return chain, false
		}
		current = h.PPID
	}
	// MaxHops reached without the walk terminating on its own: the window
	// below may still be fine, but a no-hc verdict must treat this as
	// fail-closed (a 32-hop-deep caller could otherwise hide its roots).
	return chain, true
}

// readablePrefixEnd returns the number of leading chain hops whose environ
// AND argv are both readable, scanning upward from peerPID. Hop 0 (the
// peer itself) not fully readable yields 0 — the caller denies
// unconditionally in that case.
func readablePrefixEnd(chain []procHop) int {
	i := 0
	for i < len(chain) && chain[i].EnvOK && chain[i].ArgvOK {
		i++
	}
	return i
}

// terminalIDs collects every non-empty HC_TERMINAL_ID across the given
// hops. Any hop whose value is malformed, or two hops disagreeing, fails
// closed. Hops the caller did not include (unreadable, outside the
// collection window) contribute nothing.
func terminalIDs(chain []procHop) (string, bool) {
	var id string
	for _, e := range chain {
		v := strings.TrimSpace(e.Env["HC_TERMINAL_ID"])
		if v == "" {
			continue
		}
		if !hcTerminalIDPattern.MatchString(v) {
			return "", false
		}
		if id == "" {
			id = v
		} else if id != v {
			return "", false
		}
	}
	return id, true
}

// runTwoA applies the two-segment read-only scan over the given window and
// reports (unconditionalHit, conditionalHit, hasWorkerRole). The worker
// role must be declared by a HOST hop (index >= 1) — a bare process that
// merely exports CTXMODE_ROLE=worker itself (chain[0]) cannot self-elevate;
// only an ancestor carrying the marker (the dispatch-agent spawned pig/agy/
// claude-code host) proves a real worker dispatch.
func runTwoA(window []procHop) (uncond, cond, hasWorker bool) {
	for i, e := range window {
		if i >= 1 && hopIsWorker(e) {
			hasWorker = true
		}
		if hopIsUnconditionalReadonly(e) {
			uncond = true
		}
		if hopIsConditionalReadonly(e) {
			cond = true
		}
	}
	return uncond, cond, hasWorker
}

// verifyPeerWriteAuth is the shared decision tree for both the per-
// connection socket path (VerifyClientWriteAuth, peerPID from the kernel)
// and the direct-mode path (VerifyDirectWriteAuth, peerPID = os.Getpid).
//
// Invariant ordering (audited shape):
//  1. helloOK / peerPID provenance — refuse unauthenticated or mismatched peers.
//  2. chain collection — unreadable environ anywhere fails closed.
//  3. HC_TERMINAL_ID collection — malformed or conflicting => fail closed.
//  4. hc socket stat — ErrNotExist => no-hc branch; a non-root process also
//     treats EACCES/EPERM on the root-owned /run/user/0 path as no-hc;
//     under root any other error => deny.
//     5a. hc present: HC_TERMINAL_ID is collected over the readable env+argv
//     prefix (peer itself must be readable); a live/matching/parent-keyed
//     terminal.get record must place shell_pid in the chain; the strict
//     window is then [peerPID..shell_pid] where EVERY hop must be fully
//     readable (anything above shell_pid — herdr-core, PID 1 — is
//     ignored); run 2a over the window, THEN decide on parent (null =>
//     allow; non-empty => allow only when hasWorkerRole).
//     5b. hc absent: peer must be fully readable; the checked window is the
//     run of same-UID ancestors (stops safely at PID 1, a different uid,
//     or an unreadable ppid/uid boundary). Any unreadable hop inside the
//     window, or a walk that truncated at MaxHops without reaching a
//     boundary, fails closed. 2a then runs over the window.
func verifyPeerWriteAuth(pa *PeerAuth, peerPID int, helloOK bool, helloPID int) WriteAuth {
	if !helloOK {
		return deny("missing client hello")
	}
	if peerPID <= 0 {
		return deny("no kernel peer pid")
	}
	// Kernel pid is authoritative; a client-hello pid that disagrees (or is
	// absent — a hello that passes parse must carry its real pid) means a
	// forged identity.
	if helloPID != peerPID {
		return deny(fmt.Sprintf("client hello pid %d != kernel peer pid %d", helloPID, peerPID))
	}

	chain, truncated := collectChain(pa, peerPID)
	if len(chain) == 0 {
		return deny("cannot read /proc ancestry")
	}
	// The peer's own environ + argv are the minimum provable identity —
	// unreadable means nothing downstream can be trusted.
	if !chain[0].EnvOK || !chain[0].ArgvOK {
		return deny("cannot read /proc ancestry environ")
	}

	statErr := pa.StatHcSocket(pa.HcSocketPath)
	hcAbsent := errors.Is(statErr, os.ErrNotExist)
	if !hcAbsent && statErr != nil && pa.SelfEUID() != 0 &&
		(errors.Is(statErr, os.ErrPermission) || errors.Is(statErr, syscall.EPERM)) {
		// Non-root cannot stat the root-owned /run/user/0 socket dir
		// (0700 root:root). That EACCES proves nothing about whether hc
		// runs — for THIS process hc is simply unreachable, so the no-hc
		// branch applies rather than a fail-closed deny.
		hcAbsent = true
	}
	if statErr != nil && !hcAbsent {
		// Root (or a non-permission error): cannot prove "no hc" — deny.
		return deny(fmt.Sprintf("hc socket stat failed: %v", statErr))
	}

	if hcAbsent {
		// No hc environment (CI/dev box, or non-root where the root-owned
		// hc socket is unreachable): the checked window is the run of
		// ancestors owned by the SAME uid as the peer, stopping safely at
		// PID 1, a uid boundary (e.g. non-root shell under root
		// sshd/systemd), or an unreadable ppid/uid probe. Inside that
		// window every hop must be fully readable and the walk must have
		// terminated at a real boundary — a MaxHops truncation cannot
		// prove what hides above, so it fails closed.
		if !chain[0].UIDOK {
			return deny("cannot read /proc peer uid")
		}
		window := []procHop{chain[0]}
		boundaryOK := false
		for i := 1; i < len(chain); i++ {
			h := chain[i]
			if !h.UIDOK {
				// Cannot determine this ancestor's owner: safe stop — it is
				// not provably a same-uid ancestor, so it cannot be part of
				// the checked window.
				boundaryOK = true
				break
			}
			if h.PID == 1 || h.UID != window[0].UID {
				// Init / different-owner ancestor (root sshd, systemd):
				// legitimate stop, its environ is not required.
				boundaryOK = true
				break
			}
			if !h.EnvOK || !h.ArgvOK || !h.PPIDOK {
				// Same-uid ancestor inside the window we cannot fully read:
				// cannot prove it was not a read-only role — fail closed.
				return deny(fmt.Sprintf("unreadable same-uid ancestor pid %d", h.PID))
			}
			window = append(window, h)
		}
		if !boundaryOK {
			// The whole chain entered the window and the loop never found a
			// uid/PID-1 boundary. If the walk truncated at MaxHops the
			// caller's real ancestry is unknowable — fail closed. An
			// untruncated full consumption means collectChain ended on a
			// genuine termination (ppid <= 0, self-parent or a cycle back
			// into the same already-checked window) — a safe boundary.
			if truncated {
				return deny("ancestry walk truncated before uid boundary")
			}
		}
		uncond, cond, hasWorker := runTwoA(window)
		if uncond {
			return deny("read-only role marker in process ancestry")
		}
		if cond && !hasWorker {
			return deny("subagent dispatch marker without worker role")
		}
		return allow("no hc environment; no read-only marker in ancestry")
	}

	// hc present: collect HC_TERMINAL_ID over the readable env+argv prefix
	// only — unreadable ancestors above shell_pid (herdr-core, PID 1)
	// legitimately carry no environ we can see. Malformed or conflicting
	// ids inside the readable prefix fail closed.
	readable := chain[:readablePrefixEnd(chain)]
	termID, ok := terminalIDs(readable)
	if !ok {
		return deny("conflicting or malformed HC_TERMINAL_ID in ancestry")
	}
	// A session with no provable terminal id can never write — double-fork
	// orphans and plain ssh shells land here.
	if termID == "" {
		return deny("no HC_TERMINAL_ID in ancestry under a live hc environment")
	}
	info, ok := pa.QueryHcTerminal(pa.HcSocketPath, termID, pa.HcTimeout)
	if !ok {
		return deny(fmt.Sprintf("hc terminal.get %s failed or record invalid", termID))
	}
	// Defensive re-check at the decision point (not only inside the probe):
	// a record that is not provably live, addressed to this terminal, and
	// carrying a real ancestor pid can never grant writes.
	if info.State != "live" {
		return deny(fmt.Sprintf("hc terminal %s state=%q not live", termID, info.State))
	}
	if info.Address != termID {
		return deny(fmt.Sprintf("hc terminal record address %q != requested %q", info.Address, termID))
	}
	if !info.ParentHasKey {
		return deny(fmt.Sprintf("hc terminal %s record lacks explicit parent key", termID))
	}
	if info.Parent != nil && strings.TrimSpace(*info.Parent) == "" {
		return deny(fmt.Sprintf("hc terminal %s record parent is blank", termID))
	}
	// shell_pid must be a real ancestor of peerPID.
	shellIndex := -1
	for i, e := range chain {
		if e.PID == info.PID {
			shellIndex = i
			break
		}
	}
	if shellIndex < 0 {
		return deny(fmt.Sprintf("hc terminal %s shell_pid %d not in ancestry", termID, info.PID))
	}
	// Anchor the checked window at [peerPID .. shell_pid] (inclusive).
	// EVERY hop inside the window must be fully readable — an unreadable
	// environ/argv there cannot be proven not to be a read-only role.
	// Hops ABOVE shell_pid (herdr-core itself, PID 1) are intentionally
	// ignored: they are outside the session's own ancestry.
	window := chain[:shellIndex+1]
	for _, h := range window {
		if !h.EnvOK || !h.ArgvOK {
			return deny(fmt.Sprintf("unreadable ancestor pid %d inside session window", h.PID))
		}
	}
	// Cross-hop terminal-id check over the window (same rule as the
	// readable prefix — defensive, values were already collected there).
	if _, ok := terminalIDs(window); !ok {
		return deny("conflicting or malformed HC_TERMINAL_ID in session window")
	}
	// Run 2a FIRST — a top-level terminal is not automatically writable:
	// a scout/researcher role dispatched under a top-level shell still
	// refuses. Only after 2a passes does `parent` decide top-level vs
	// worker-subagent.
	uncond, cond, hasWorker := runTwoA(window)
	if uncond {
		return deny("read-only role marker in session ancestry")
	}
	if cond && !hasWorker {
		return deny("subagent dispatch marker without worker role")
	}
	if info.Parent == nil {
		// parent: literal null => top-level terminal.
		return allow(fmt.Sprintf("top-level hc terminal %s", termID))
	}
	// Subagent terminal: only a declared worker may write.
	if hasWorker {
		return allow(fmt.Sprintf("worker role under hc subagent terminal %s", termID))
	}
	return deny(fmt.Sprintf("hc subagent terminal %s (parent=%s) without worker role", termID, *info.Parent))
}

// VerifyClientWriteAuth computes the write verdict for one accepted socket
// connection. helloOK/helloPID come from TryReadClientHello; peerPID is the
// kernel credential — never the client's claim.
func VerifyClientWriteAuth(conn net.Conn, chlo ClientHello, helloOK bool) WriteAuth {
	return verifyPeerWriteAuth(defaultPeerAuth(), mustPeerPID(conn), helloOK, chlo.PID)
}

func mustPeerPID(conn net.Conn) int {
	pid, _ := peerPIDUnix(conn)
	return pid
}

// VerifyDirectWriteAuth computes the write verdict for a direct (non-daemon)
// stdio process — CODEGRAPH_NO_DAEMON=1 must not be a write backdoor, so the
// process verifies its own identity with the same logic. A stdio process has
// no client hello; its own pid is the provenance.
func VerifyDirectWriteAuth() WriteAuth {
	return verifyPeerWriteAuth(defaultPeerAuth(), os.Getpid(), true, os.Getpid())
}
