//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------- fake PeerAuth ----------

// fakeProc is one fabricated /proc process.
type fakeProc struct {
	ppid   int
	env    map[string]string
	argv   []string
	uid    int
	noEnv  bool // ReadEnviron fails (ENOENT/EACCES/empty)
	noStat bool // ReadPPID fails
	noArgv bool // ReadArgv fails
	noUID  bool // ReadUID fails
}

// fakePeerAuth drives verifyPeerWriteAuth without real /proc, sockets or hc.
type fakePeerAuth struct {
	*PeerAuth
	procs       map[int]fakeProc
	statErr     error          // result of StatHcSocket (nil, os.ErrNotExist, other)
	hcInfo      HcTerminalInfo // canned terminal.get record
	hcOK        bool
	hcErr       bool // force query failure
	peerPID     int
	peerOK      bool
	selfEUID    int // fake os.Geteuid result
	queriedID   string
	queriedPath string
}

func newFakePeerAuth() *fakePeerAuth {
	f := &fakePeerAuth{procs: map[int]fakeProc{}}
	f.PeerAuth = &PeerAuth{
		PeerPID: func(net.Conn) (int, bool) { return f.peerPID, f.peerOK },
		ReadEnviron: func(pid int) (map[string]string, bool) {
			p, ok := f.procs[pid]
			if !ok || p.noEnv {
				return nil, false
			}
			return p.env, true
		},
		ReadPPID: func(pid int) (int, bool) {
			p, ok := f.procs[pid]
			if !ok || p.noStat {
				return 0, false
			}
			return p.ppid, true
		},
		ReadArgv: func(pid int) ([]string, bool) {
			p, ok := f.procs[pid]
			if !ok || p.noArgv {
				return nil, false
			}
			return p.argv, true
		},
		ReadUID: func(pid int) (int, bool) {
			p, ok := f.procs[pid]
			if !ok || p.noUID {
				return 0, false
			}
			return p.uid, true
		},
		SelfEUID:     func() int { return f.selfEUID },
		StatHcSocket: func(string) error { return f.statErr },
		QueryHcTerminal: func(path, termID string, _ time.Duration) (HcTerminalInfo, bool) {
			f.queriedPath, f.queriedID = path, termID
			if f.hcErr || !f.hcOK {
				return HcTerminalInfo{}, false
			}
			return f.hcInfo, true
		},
		HcSocketPath: hcSocketFile,
		HcTimeout:    50 * time.Millisecond,
		MaxHops:      ancestorMaxHops,
	}
	f.peerPID = 1000
	f.peerOK = true
	f.statErr = os.ErrNotExist // default: no hc environment (CI shape)
	// selfEUID defaults to 0 (root) — the production dispatch environment.
	return f
}

func (f *fakePeerAuth) add(pid, ppid int, env map[string]string, argv []string) {
	f.procs[pid] = fakeProc{ppid: ppid, env: env, argv: argv}
}

// addChain links peer -> a -> b -> ... -> 0 (last ppid=0 stops the walk).
func (f *fakePeerAuth) chain(env map[string]string, argv []string) {
	f.add(f.peerPID, 0, env, argv)
}

// ---------- helper ----------

func allowMsg(a WriteAuth) string {
	if a.Allowed {
		return "ALLOW(" + a.Reason + ")"
	}
	return "deny(" + a.Reason + ")"
}

// ---------- anti-shift invariant (observation item 1) ----------

// TestParseProcCmdlinePreservesInteriorEmpty reproduces the BuildPig
// `--append-system-prompt ""` shape: raw bytes
//
//	pig \x00 --append-system-prompt \x00 \x00 --name \x00 role-scout-a1b2c3d4 \x00
//
// must split with the interior "" KEPT so --name still pairs with
// role-scout-… (a filter-empty implementation would slide --name onto "" and
// miss the role marker entirely).
func TestParseProcCmdlinePreservesInteriorEmpty(t *testing.T) {
	raw := []byte("pig\x00--append-system-prompt\x00\x00--name\x00role-scout-a1b2c3d4\x00")
	argv := parseProcCmdline(raw)
	want := []string{"pig", "--append-system-prompt", "", "--name", "role-scout-a1b2c3d4"}
	if len(argv) != len(want) {
		t.Fatalf("argv len=%d want %d: %#v", len(argv), len(want), argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d]=%q want %q (full %#v)", i, argv[i], want[i], argv)
		}
	}
	if len(parseProcCmdline(nil)) != 0 {
		t.Fatal("empty cmdline must yield nil")
	}
}

// TestScanArgvRoleMarkers covers the flag scan: --name/-n/--agent hitting a
// dispatched read-only role, the empty-value shift guard, and the "--" stop.
func TestScanArgvRoleMarkers(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"scout via --name separated", []string{"pig", "--name", "role-scout-a1b2c3d4"}, true},
		{"scout via --name=", []string{"pig", "--name=role-scout-a1b2c3d4"}, true},
		{"scout via -n", []string{"pig", "-n", "role-scout-zz"}, true},
		{"worker name not readonly", []string{"pig", "--name", "role-worker-a1b2c3d4"}, false},
		{"empty value then --name scout", []string{"pig", "--append-system-prompt", "", "--name", "role-scout-a1b2c3d4"}, true},
		{"--agent oracle", []string{"pig", "--agent", "oracle"}, true},
		{"--agent=verifier", []string{"pig", "--agent=verifier"}, true},
		{"--agent scout--worker sep", []string{"pig", "--agent", "scout--x"}, true},
		{"--agent worker", []string{"pig", "--agent", "worker"}, false},
		{"--name after -- ignored", []string{"pig", "--", "--name", "role-scout-a1b2c3d4"}, false},
		{"--name role no suffix", []string{"pig", "--name", "role-scout"}, true},
		{"--name rolefoo no match", []string{"pig", "--name", "role-scoutxyz"}, false},
		{"text mention not a flag", []string{"pig", "the --name role-scout-abc in prompt"}, false},
	}
	for _, c := range cases {
		if got := scanArgvReadonly(c.argv); got != c.want {
			t.Errorf("%s: scanArgvReadonly=%v want %v (argv %#v)", c.name, got, c.want, c.argv)
		}
	}
}

// ---------- provenance ----------

func TestVerifyClientWriteAuth_MissingHello(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, false, f.peerPID)
	if a.Allowed {
		t.Fatalf("missing client hello must deny, got %s", allowMsg(a))
	}
}

func TestVerifyClientWriteAuth_PIDMismatch(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID+1)
	if a.Allowed {
		t.Fatalf("hello pid mismatch must deny, got %s", allowMsg(a))
	}
}

func TestVerifyClientWriteAuth_NoPeerPID(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, 0, true, 0)
	if a.Allowed {
		t.Fatalf("no kernel peer pid must deny, got %s", allowMsg(a))
	}
}

// ---------- no-hc branch ----------

func TestNoHC_CleanChainAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{"USER": "u"}, []string{"bash"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("clean chain without hc must allow, got %s", allowMsg(a))
	}
}

func TestNoHC_ScoutArgvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{}, []string{"pig", "--name", "role-scout-a1b2c3d4"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("scout argv must deny, got %s", allowMsg(a))
	}
}

func TestNoHC_CtxmodeReadonlyDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{"CTXMODE_READONLY": "1"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("CTXMODE_READONLY must deny, got %s", allowMsg(a))
	}
}

func TestNoHC_NonWorkerRoleDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{"CTXMODE_ROLE": "researcher"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("CTXMODE_ROLE=researcher must deny, got %s", allowMsg(a))
	}
}

func TestNoHC_SubagentMarkerWithoutWorkerDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{"PI_SUBAGENT": "1"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("PI_SUBAGENT without worker must deny, got %s", allowMsg(a))
	}
}

func TestNoHC_SubagentMarkerWithWorkerAllows(t *testing.T) {
	f := newFakePeerAuth()
	// worker marker on the HOST hop (chain[1]): the peer itself is a
	// subagent child; its pig/agy host declared CTXMODE_ROLE=worker.
	f.add(1000, 600, map[string]string{"PI_SUBAGENT": "1"}, []string{"pig"})
	f.add(600, 0, map[string]string{"CTXMODE_ROLE": "worker"}, []string{"bash"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("worker under PI_SUBAGENT must allow, got %s", allowMsg(a))
	}
}

func TestNoHC_DispatchArgvMarkerWithoutWorkerDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{}, []string{"sh", "-c", "x /tmp/dispatch-foo"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("dispatch argv marker without worker must deny, got %s", allowMsg(a))
	}
}

// ---------- hc present ----------

func hcOKInfo(parent *string) HcTerminalInfo {
	return HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: parent, ParentHasKey: true}
}

// TestHC_TopLevelCleanAllows: parent=null top-level terminal, clean window.
func TestHC_TopLevelCleanAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	// chain: peer(1000) -> shell(500)
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("top-level clean terminal must allow, got %s", allowMsg(a))
	}
}

// TestHC_TopLevelReadonlyEnvDenies (observation item 2): a top-level terminal
// is NOT automatically writable — CTXMODE_ROLE=scout under it still denies.
func TestHC_TopLevelReadonlyEnvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "scout"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("CTXMODE_ROLE=scout under top-level terminal must deny, got %s", allowMsg(a))
	}
}

func TestHC_TopLevelReadonlyFlagDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_READONLY": "true"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("CTXMODE_READONLY under top-level terminal must deny, got %s", allowMsg(a))
	}
}

func TestHC_TopLevelScoutArgvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig", "--name", "role-scout-1"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("--name role-scout under top-level terminal must deny, got %s", allowMsg(a))
	}
}

// TestHC_SubagentWorkerAllows (fatal 2a): shell_pid hop carries a dispatch
// marker ("/tmp/dispatch-") but NO CTXMODE_ROLE, and the lower pig hop has
// CTXMODE_ROLE=worker — the worker role must override the conditional
// marker and allow the write.
func TestHC_SubagentWorkerAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	parent := "t9top"
	f.add(1000, 600, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	// middle shell_pid 500? No — shell_pid must be an ANCESTOR of peer.
	// peer(1000) -> pig-host(600) -> shell(500). Put dispatch marker AND the
	// worker role on the host hop 600 (S2: only chain[1:] may declare it).
	f.add(600, 500, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"sh", "/tmp/dispatch-run"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("worker under subagent terminal with dispatch marker must allow, got %s", allowMsg(a))
	}
}

// TestHC_SubagentNonWorkerDenies: subagent terminal (parent non-null) with no
// worker role must deny.
func TestHC_SubagentNonWorkerDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	parent := "t9top"
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("subagent terminal without worker role must deny, got %s", allowMsg(a))
	}
}

// TestHC_WorkerTaskbookAllows (fatal 2b): a worker whose taskbook text merely
// MENTIONS "--name role-scout" / "--agent oracle" inside an argv element (not
// as the actual flag) must NOT be denied — the scanner reads flags, not
// arbitrary text.
func TestHC_WorkerTaskbookAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	parent := "t9top"
	// peer = worker child process (taskbook argv text); worker role sits on
	// the host hop chain[1] which is also shell_pid here.
	f.add(1000, 500,
		map[string]string{"HC_TERMINAL_ID": "t2zx"},
		[]string{"pig", "implement: the --name role-scout and --agent oracle markers are denied to non-workers"},
	)
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("worker taskbook mentioning role flags must allow, got %s", allowMsg(a))
	}
}

// ---------- malformed terminal.get (should-fix item 2) ----------

func TestHC_MalformedRecordsDeny(t *testing.T) {
	// The host hop (chain[1]) carries CTXMODE_ROLE=worker so that even a
	// SUBAGENT-shaped record reaching the parent decision would have been
	// allowed — any deny below is therefore caused by the malformed record
	// itself, not by a missing worker role (S1: no false-green).
	mkShell := func(f *fakePeerAuth) {
		f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
		f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"bash"})
	}
	cases := []struct {
		name        string
		info        HcTerminalInfo
		ok          bool
		reasonMatch string // substring the deny Reason must contain ("" = skip)
	}{
		{"state=finished", HcTerminalInfo{Address: "t2zx", State: "finished", PID: 500, Parent: nil, ParentHasKey: true}, true, "state"},
		{"missing parent key", HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: nil, ParentHasKey: false}, true, "parent"},
		{"parent empty string", HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: strp(""), ParentHasKey: true}, true, "parent"},
		{"parent whitespace", HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: strp("   "), ParentHasKey: true}, true, "parent"},
		{"empty address", HcTerminalInfo{Address: "", State: "live", PID: 500, Parent: strp("t9top"), ParentHasKey: true}, true, "address"},
		{"query fails", HcTerminalInfo{}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakePeerAuth()
			f.statErr = nil
			mkShell(f)
			f.hcOK = c.ok
			f.hcInfo = c.info
			a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
			if a.Allowed {
				t.Fatalf("%s: malformed record must deny, got %s", c.name, allowMsg(a))
			}
			if c.reasonMatch != "" && !strings.Contains(a.Reason, c.reasonMatch) {
				t.Fatalf("%s: deny reason %q must mention %q", c.name, a.Reason, c.reasonMatch)
			}
		})
	}
}

// TestHC_ConflictingTerminalIDsDeny: two hops advertising different
// HC_TERMINAL_ID values cannot both be the session terminal — fail closed.
func TestHC_ConflictingTerminalIDsDeny(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "tabc"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("conflicting HC_TERMINAL_ID across hops must deny, got %s", allowMsg(a))
	}
}

func TestHC_MalformedTerminalIDDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "not-a-term"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "not-a-term"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("malformed HC_TERMINAL_ID must deny, got %s", allowMsg(a))
	}
}

// TestHC_NoTerminalIDDenies (double-fork orphan / plain ssh): hc socket live
// but the chain carries no HC_TERMINAL_ID — unconditionally deny.
func TestHC_NoTerminalIDDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{}, []string{"pig"})
	f.add(500, 0, map[string]string{}, []string{"bash"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("no HC_TERMINAL_ID under live hc must deny, got %s", allowMsg(a))
	}
}

// TestHC_ShellPIDNotInChainDenies: terminal.get returns a pid that is not a
// real ancestor of the peer — forged/irrelevant record.
func TestHC_ShellPIDNotInChainDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	f.hcInfo.PID = 424242 // not an ancestor
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("shell_pid not in ancestry must deny, got %s", allowMsg(a))
	}
}

// TestHC_StatErrorDenies: a stat error that is NOT ErrNotExist (EACCES,
// ELOOP) cannot prove "no hc" — fail closed.
func TestHC_StatErrorDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = errors.New("EACCES")
	f.chain(map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("non-ENOENT stat error must deny, got %s", allowMsg(a))
	}
}

// TestUnreadableEnvDenies: peer environ unreadable (EACCES) — cannot prove
// absence of role markers.
func TestUnreadableEnvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.procs[1000] = fakeProc{ppid: 0, env: nil, argv: []string{"pig"}, noEnv: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable peer environ must deny, got %s", allowMsg(a))
	}
}

func strp(s string) *string { return &s }

// ---------- real queryHcTerminal over a fake unix socket ----------

// TestQueryHcTerminalStrictDecode runs the real decoder against a scripted
// unix-socket peer to prove the wire contract: live+null-parent+pid accepted;
// finished / missing parent / empty parent / mismatched address rejected.
func TestQueryHcTerminalStrictDecode(t *testing.T) {
	dir := t.TempDir()
	sock := dir + "/hc.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot bind unix socket: %v", err)
	}
	defer ln.Close()

	serve := func(resp string) {
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, 4096)
			_, _ = c.Read(buf)
			_, _ = c.Write([]byte(resp + "\n"))
		}()
	}

	good := `{"id":"codegraph-write-auth","jsonrpc":"2.0","result":{"address":"t2zx","state":"live","pid":500,"parent":null}}`
	serve(good)
	info, ok := queryHcTerminal(sock, "t2zx", time.Second)
	if !ok || info.PID != 500 || info.Parent != nil || !info.ParentHasKey {
		t.Fatalf("good record decode failed: %+v ok=%v", info, ok)
	}

	bad := []struct{ name, resp string }{
		{"state finished", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"finished","pid":500,"parent":null}}`},
		{"missing state", `{"id":"codegraph-write-auth","result":{"address":"t2zx","pid":500,"parent":null}}`},
		{"state null", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":null,"pid":500,"parent":null}}`},
		{"missing parent", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500}}`},
		{"parent empty", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":""}}`},
		{"parent whitespace", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":"   "}}`},
		{"parent number", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":5}}`},
		{"parent bool", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":true}}`},
		{"parent object", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":{}}}`},
		{"parent array", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":[]}}`},
		{"result null", `{"id":"codegraph-write-auth","result":null}`},
		{"bad address", `{"id":"codegraph-write-auth","result":{"address":"tOTHER","state":"live","pid":500,"parent":null}}`},
		{"pid zero", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":0,"parent":null}}`},
		{"pid fractional", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500.5,"parent":null}}`},
		{"pid string", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":"500","parent":null}}`},
		{"wrong reply id", `{"id":"other","result":{"address":"t2zx","state":"live","pid":500,"parent":null}}`},
	}
	for _, b := range bad {
		serve(b.resp)
		if _, ok := queryHcTerminal(sock, "t2zx", time.Second); ok {
			t.Errorf("%s: malformed record accepted", b.name)
		}
	}
}

// ---------- M1: non-root EACCES + unreadable ancestors above shell_pid ----------

// TestNoHC_NonRootEaccesUidBoundaryAllows (M1-a): a non-root process
// (euid=1000) cannot stat the root-owned /run/user/0 hc socket dir —
// EACCES must mean "hc unreachable for us" (no-hc branch), not a deny.
// The ancestry then crosses into root-owned sshd/PID 1, whose environ is
// unreadable; that uid boundary is a legitimate stop and must allow.
func TestNoHC_NonRootEaccesUidBoundaryAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.selfEUID = 1000
	f.statErr = os.ErrPermission // stat("/run/user/0/...") as uid 1000
	// peer(1000, uid 1000) -> shell(900, uid 1000) -> sshd(300, uid 0) ->
	// init(1, uid 0). The root hops' environ/argv are unreadable to us.
	f.add(1000, 900, map[string]string{"USER": "dev"}, []string{"codegraph-go"})
	f.procs[1000] = withUID(f.procs[1000], 1000)
	f.add(900, 300, map[string]string{"USER": "dev"}, []string{"bash"})
	f.procs[900] = withUID(f.procs[900], 1000)
	f.add(300, 1, nil, nil)
	f.procs[300] = fakeProc{ppid: 1, uid: 0, noEnv: true, noArgv: true}
	f.procs[1] = fakeProc{ppid: 0, uid: 0, noEnv: true, noArgv: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("non-root EACCES + uid boundary must allow, got %s", allowMsg(a))
	}
}

func withUID(p fakeProc, uid int) fakeProc { p.uid = uid; return p }

// TestHC_Pid1AboveShellPidUnreadableAllows (M1-b): under a live hc
// environment the window is [peerPID..shell_pid]; ancestors ABOVE
// shell_pid (herdr-core as root, PID 1) are legitimately unreadable and
// must not fail the verdict.
func TestHC_Pid1AboveShellPidUnreadableAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	f.add(500, 300, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"}) // shell_pid
	f.procs[300] = fakeProc{ppid: 1, uid: 0, noEnv: true, noArgv: true}            // herdr-core (root)
	f.procs[1] = fakeProc{ppid: 0, uid: 0, noEnv: true, noArgv: true}              // PID 1
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("unreadable ancestors above shell_pid must allow, got %s", allowMsg(a))
	}
}

// TestHC_UnreadableHopInsideWindowDenies: an unreadable hop INSIDE
// [peerPID..shell_pid] still fails closed — the above-shell_pid exemption
// does not leak downward.
func TestHC_UnreadableHopInsideWindowDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	// shell_pid hop itself unreadable: cannot prove its identity.
	f.procs[500] = fakeProc{ppid: 0, uid: 0, noEnv: true, noArgv: true}
	f.hcOK = true
	f.hcInfo = hcOKInfo(nil)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable hop inside window must deny, got %s", allowMsg(a))
	}
}

// ---------- M2: no-hc fail-closed gaps ----------

// TestNoHC_TruncatedChainDenies (M2-a): a chain reaching MaxHops without
// arriving at ppid<=0 or crossing a uid boundary cannot prove what hides
// above — fail closed (defeats the 32-hop burial bypass).
func TestNoHC_TruncatedChainDenies(t *testing.T) {
	f := newFakePeerAuth()
	// 32 hops all same-uid readable, none terminates the walk.
	pid := f.peerPID
	for i := 0; i < ancestorMaxHops; i++ {
		next := pid + 1
		f.add(pid, next, map[string]string{"USER": "u"}, []string{"sh"})
		f.procs[pid] = withUID(f.procs[pid], 1000)
		pid = next
	}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("MaxHops-truncated ancestry must deny, got %s", allowMsg(a))
	}
}

// TestNoHC_SameUIDAncestorUnreadableDenies (M2-b): a same-uid ancestor
// whose environ is unreadable sits inside the checked window — cannot
// prove it was not a read-only role.
func TestNoHC_SameUIDAncestorUnreadableDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.add(1000, 600, map[string]string{"USER": "u"}, []string{"pig"})
	f.procs[1000] = withUID(f.procs[1000], 1000)
	f.procs[600] = fakeProc{ppid: 0, uid: 1000, noEnv: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable same-uid ancestor must deny, got %s", allowMsg(a))
	}
}

// TestNoHC_SameUIDAncestorUnreadableArgvDenies (M2-b): same-uid ancestor
// argv unreadable — ReadArgv failure is no longer silently dropped.
func TestNoHC_SameUIDAncestorUnreadableArgvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.add(1000, 600, map[string]string{"USER": "u"}, []string{"pig"})
	f.procs[1000] = withUID(f.procs[1000], 1000)
	f.procs[600] = fakeProc{ppid: 0, uid: 1000, env: map[string]string{"USER": "u"}, noArgv: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable same-uid ancestor argv must deny, got %s", allowMsg(a))
	}
}

// TestNoHC_SameUIDAncestorUnreadablePPIDDenies (M2-b): same-uid ancestor
// whose ppid is unreadable prevents finding where the window ends — fail
// closed instead of silently treating the walk as finished.
func TestNoHC_SameUIDAncestorUnreadablePPIDDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.add(1000, 600, map[string]string{"USER": "u"}, []string{"pig"})
	f.procs[1000] = withUID(f.procs[1000], 1000)
	f.procs[600] = fakeProc{uid: 1000, env: map[string]string{"USER": "u"}, argv: []string{"sh"}, noStat: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable same-uid ancestor ppid must deny, got %s", allowMsg(a))
	}
}

// TestNoHC_PeerUnreadableArgvDenies (M2-b): even hop 0's argv failure is
// fail-closed now (previously a nil argv slipped through the 2a scan).
func TestNoHC_PeerUnreadableArgvDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.procs[1000] = fakeProc{ppid: 0, uid: 1000, env: map[string]string{"USER": "u"}, noArgv: true}
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("unreadable peer argv must deny, got %s", allowMsg(a))
	}
}

// ---------- S2: worker role cannot be self-declared by chain[0] ----------

// TestHC_Chain0SelfDeclaredWorkerDenies (S2): a bare `hc run --bg` child
// that exports CTXMODE_ROLE=worker itself — with no ancestor carrying the
// marker — must NOT self-elevate on a subagent terminal.
func TestHC_Chain0SelfDeclaredWorkerDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	parent := "t9top"
	f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"codegraph-go"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("self-declared worker on chain[0] must deny, got %s", allowMsg(a))
	}
}

// TestHC_HostWorkerAllows (S2 positive): CTXMODE_ROLE=worker on chain[1]
// (the dispatch-agent spawned host) under a subagent terminal allows.
func TestHC_HostWorkerAllows(t *testing.T) {
	f := newFakePeerAuth()
	f.statErr = nil
	parent := "t9top"
	f.add(1000, 600, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"codegraph-go"})
	f.add(600, 500, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"pig"})
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("ancestor-declared worker must allow, got %s", allowMsg(a))
	}
}

// TestNoHC_Chain0SelfDeclaredWorkerPI_SubagentDenies (S2 no-hc shape): a
// lone PI_SUBAGENT=1 + CTXMODE_ROLE=worker process cannot self-elevate
// without an ancestor worker marker.
func TestNoHC_Chain0SelfDeclaredWorkerPI_SubagentDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.chain(map[string]string{"PI_SUBAGENT": "1", "CTXMODE_ROLE": "worker"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("self-declared worker under PI_SUBAGENT must deny, got %s", allowMsg(a))
	}
}

// ---------- M1 stat policy edge ----------

// TestHC_RootEaccesStillDenies: under euid 0 an EACCES/EPERM on the hc
// socket is NOT excused — only non-root gets the unreachable-socket
// exemption.
func TestHC_RootEaccesStillDenies(t *testing.T) {
	f := newFakePeerAuth() // selfEUID defaults to 0
	f.statErr = os.ErrPermission
	f.chain(map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("root EACCES stat must deny, got %s", allowMsg(a))
	}
}

// TestNoHC_NonRootEpermTreatedAbsent: EPERM from a non-root stat is the
// same unreachable-root-socket shape as EACCES.
func TestNoHC_NonRootEpermTreatedAbsent(t *testing.T) {
	f := newFakePeerAuth()
	f.selfEUID = 1000
	f.statErr = syscall.EPERM
	f.chain(map[string]string{"USER": "u"}, []string{"bash"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("non-root EPERM stat must enter no-hc branch, got %s", allowMsg(a))
	}
}

// TestHC_NonRootOtherStatErrorDenies: a non-root stat error that is NOT a
// permission failure (e.g. ELOOP) still fails closed — it cannot be
// explained by the root-owned socket dir.
func TestHC_NonRootOtherStatErrorDenies(t *testing.T) {
	f := newFakePeerAuth()
	f.selfEUID = 1000
	f.statErr = fmt.Errorf("stat: %w", syscall.ELOOP)
	f.chain(map[string]string{"USER": "u"}, []string{"bash"})
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if a.Allowed {
		t.Fatalf("non-root non-permission stat error must deny, got %s", allowMsg(a))
	}
}
