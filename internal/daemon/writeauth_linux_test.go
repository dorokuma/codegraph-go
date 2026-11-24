//go:build linux

package daemon

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// ---------- fake PeerAuth ----------

// fakeProc is one fabricated /proc process.
type fakeProc struct {
	ppid   int
	env    map[string]string
	argv   []string
	noEnv  bool // ReadEnviron fails (ENOENT/EACCES/empty)
	noStat bool // ReadPPID fails
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
		ReadArgv: func(pid int) []string {
			return f.procs[pid].argv
		},
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
	f.chain(map[string]string{"PI_SUBAGENT": "1", "CTXMODE_ROLE": "worker"}, []string{"pig"})
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
	f.add(1000, 600, map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"}, []string{"pig"})
	// middle shell_pid 500? No — shell_pid must be an ANCESTOR of peer.
	// peer(1000) -> pig-host(600) -> shell(500). Put dispatch marker on 600.
	f.add(600, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"sh", "/tmp/dispatch-run"})
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
	f.add(1000, 500,
		map[string]string{"HC_TERMINAL_ID": "t2zx", "CTXMODE_ROLE": "worker"},
		[]string{"pig", "implement: the --name role-scout and --agent oracle markers are denied to non-workers"},
	)
	f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	f.hcOK = true
	f.hcInfo = hcOKInfo(&parent)
	a := verifyPeerWriteAuth(f.PeerAuth, f.peerPID, true, f.peerPID)
	if !a.Allowed {
		t.Fatalf("worker taskbook mentioning role flags must allow, got %s", allowMsg(a))
	}
}

// ---------- malformed terminal.get (should-fix item 2) ----------

func TestHC_MalformedRecordsDeny(t *testing.T) {
	mkShell := func(f *fakePeerAuth) {
		f.add(1000, 500, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"pig"})
		f.add(500, 0, map[string]string{"HC_TERMINAL_ID": "t2zx"}, []string{"bash"})
	}
	cases := []struct {
		name string
		info HcTerminalInfo
		ok   bool
	}{
		{"state=finished", HcTerminalInfo{Address: "t2zx", State: "finished", PID: 500, Parent: nil, ParentHasKey: true}, true},
		{"missing parent key", HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: nil, ParentHasKey: false}, true},
		{"parent empty string", HcTerminalInfo{Address: "t2zx", State: "live", PID: 500, Parent: strp(""), ParentHasKey: true}, true},
		{"query fails", HcTerminalInfo{}, false},
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
		{"missing parent", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500}}`},
		{"parent empty", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":500,"parent":""}}`},
		{"bad address", `{"id":"codegraph-write-auth","result":{"address":"tOTHER","state":"live","pid":500,"parent":null}}`},
		{"pid zero", `{"id":"codegraph-write-auth","result":{"address":"t2zx","state":"live","pid":0,"parent":null}}`},
		{"wrong reply id", `{"id":"other","result":{"address":"t2zx","state":"live","pid":500,"parent":null}}`},
	}
	for _, b := range bad {
		serve(b.resp)
		if _, ok := queryHcTerminal(sock, "t2zx", time.Second); ok {
			t.Errorf("%s: malformed record accepted", b.name)
		}
	}
}
