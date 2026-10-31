package extraction

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/config"
)

func TestShouldSkipDir(t *testing.T) {
	cases := []struct {
		path, name string
		skip       bool
	}{
		{"/home/u/node_modules", "node_modules", true},
		{"/proj/pkg", "pkg", false},
		{"/home/u/go/pkg/mod/github.com/x", "x", true},
		{"/home/u/myapp/mod", "mod", false},
		{"/root/go", "go", true},
		{"/root/go/pkg", "pkg", true},
		{"/root/go/bin", "bin", true},
		{"/root/go/pkg/mod/foo", "foo", true},
		{"/root/sdk/go", "go", false},
		{"/proj/internal/go", "go", false},
		{"/root/clients/go", "go", false},
		{"/root/codegraph-go", "codegraph-go", false},
		{"/root/codegraph-go/pkg", "pkg", false},
		{"/root/codegraph-go/pkg/mod", "mod", false},
		{"/home/u/codegraph-go/extraction", "extraction", false},
		{"/root/.cargo/registry/src", "src", true},
		{"/root/code_references/x", "x", true},
	}
	for _, c := range cases {
		got := ShouldSkipDir(c.path, c.name)
		if got != c.skip {
			t.Fatalf("ShouldSkipDir(%q, %q)=%v want %v", c.path, c.name, got, c.skip)
		}
	}
}

func TestHomeModeTopLevel(t *testing.T) {
	home := t.TempDir()
	// Fake broad workdir by using a real dir; IsBroadWorkdir checks UserHomeDir/ /root.
	// We test ShouldSkipDirIn logic via a manual scenario: workdir with project + junk.
	// Override: call with workdir that IsBroadWorkdir may not recognize — so unit-test
	// HasProjectMarker + relative top-level filter by temporarily setting HOME.

	oldHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", oldHome) })
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatal(err)
	}

	proj := filepath.Join(home, "myapp")
	junk := filepath.Join(home, "random-dump")
	hidden := filepath.Join(home, ".something")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	// nested inside project should not be home-filtered
	nested := filepath.Join(proj, "internal")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	if !IsBroadWorkdir(home) {
		t.Fatalf("expected home %s to be broad", home)
	}
	if ShouldSkipDirIn(home, proj, "myapp") {
		t.Fatal("project top-level should index")
	}
	if !ShouldSkipDirIn(home, junk, "random-dump") {
		t.Fatal("non-project top-level should skip in home mode")
	}
	if !ShouldSkipDirIn(home, hidden, ".something") {
		t.Fatal("hidden top-level should skip in home mode")
	}
	if ShouldSkipDirIn(home, nested, "internal") {
		t.Fatal("nested under project should not be home-filtered")
	}

	// HOME_INDEX_ALL opts out
	t.Setenv("CODEGRAPH_GO_HOME_INDEX_ALL", "1")
	if ShouldSkipDirIn(home, junk, "random-dump") {
		t.Fatal("HOME_INDEX_ALL should allow non-project top-level")
	}
}

// TestWtContainerSkippedButWorktreeRootKept: the .wt container dir is a
// built-in skip, yet when workdir IS a single worktree inside .wt the root
// itself (and its children) must never be skipped — otherwise a dispatched
// agent running codegraph inside its own worktree would index nothing.
func TestWtContainerSkippedButWorktreeRootKept(t *testing.T) {
	if !ShouldSkipDir("/root/workspace/.wt", ".wt") {
		t.Fatal(".wt container dir must be skipped by built-in rule")
	}
	wt := filepath.Join(t.TempDir(), ".wt", "repo", "feat-x")
	inner := filepath.Join(wt, "src")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// Root protection: workdir itself never skipped, even though its
	// parent chain contains a name in skipDirNames.
	if ShouldSkipDirIn(wt, wt, filepath.Base(wt)) {
		t.Fatal("worktree root as workdir must not be skipped")
	}
	if ShouldSkipDirIn(wt, inner, "src") {
		t.Fatal("normal subdir inside worktree workdir must not be skipped")
	}
	// ...but a .wt dir seen while walking a parent workdir is still skipped.
	if !ShouldSkipDirIn(filepath.Dir(filepath.Dir(filepath.Dir(wt))), filepath.Join(filepath.Dir(filepath.Dir(wt)), ".wt"), ".wt") {
		t.Fatal(".wt dir inside a broader workdir must be skipped")
	}
}

// TestExcludeDirsMatching: user-configured exclude_dirs rules — bare names,
// globs and workdir-relative slash paths — are honored by ShouldSkipDirIn.
func TestExcludeDirsMatching(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{
		"junk",
		"*-backup*",
		"experiments/scratch",
		" ",         // blank rules ignored
		"trailing/", // trailing slash stripped
	}, 0)

	mkdir := func(rel string) string {
		p := filepath.Join(wd, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		rel  string
		skip bool
	}{
		{"junk", true},
		{"nested/junk", true},
		{"proj-backup-2026", true},
		{"experiments/scratch", true},
		{"experiments/scratch/deeper", true},
		{"trailing", true},
		{"experiments", false}, // parent of a path rule still walks
		{"src", false},
		{"backup", false}, // glob needs a match
	}
	for _, c := range cases {
		p := mkdir(c.rel)
		got := ShouldSkipDirIn(wd, p, filepath.Base(p))
		if got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q)=%v want %v", c.rel, got, c.skip)
		}
	}

	// Path rules without a workdir never match (no rel to compare).
	config.SetRuntimeConfigForTesting([]string{"a/b"}, 0)
	if ShouldSkipDirIn("", "/x/a/b", "b") {
		t.Error("path rule must not match when workdir is empty")
	}
}

// TestExcludeDirsTopLevelAnchor: rules anchored with "./" (or "/") pin the
// exclusion to <workdir>/<sub> and its subtree only — unlike a bare "ref"
// they must NOT hit same-named deep directories (proj/internal/ref).
func TestExcludeDirsTopLevelAnchor(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"./ref", "/archive"}, 0)

	mkdir := func(rel string) string {
		p := filepath.Join(wd, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		rel  string
		skip bool
	}{
		{"ref", true},     // anchored top-level dir itself
		{"ref/sub", true}, // its subtree
		{"archive", true}, // "/archive" anchored the same way
		{"archive/deep/x", true},
		{"proj/internal/ref", false}, // deep same-name must NOT be hit
		{"pkg/ref", false},
		{"src", false},
	}
	for _, c := range cases {
		p := mkdir(c.rel)
		got := ShouldSkipDirIn(wd, p, filepath.Base(p))
		if got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q)=%v want %v", c.rel, got, c.skip)
		}
	}

	// Root protection still wins: workdir == the anchored dir itself.
	ref := filepath.Join(wd, "ref")
	if ShouldSkipDirIn(ref, ref, "ref") {
		t.Error("workdir root must never be skipped even when it matches an anchored rule")
	}
	// Mixed relative/absolute forms of the same paths agree.
	if ShouldSkipDirIn(wd, ".", filepath.Base(wd)) {
		t.Error("workdir as '.' must still be root-protected")
	}
}

// TestShouldSkipDirInRootProtectionRelative: the workdir-root protection
// must hold under REAL relative-path callers, not only absolute inputs —
// the startup narrowing (NarrowExcludedWorkdirs) builds absolute paths,
// but watchers/walkers can hand ShouldSkipDirIn a bare "." with cwd inside
// the excluded dir. If the root check only compared raw strings instead of
// Abs-normalized paths, "ShouldSkipDirIn(ref, '.', 'ref')" and the inverse
// "ShouldSkipDirIn('.', ref, 'ref')" would slip through and skip the very
// root the session was narrowed to — a blank index. These calls must
// return false with cwd inside the excluded dir; remove the Abs
// normalization and they return true (red without the fix).
func TestShouldSkipDirInRootProtectionRelative(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"ref", "./ref"}, 0)

	ref := filepath.Join(wd, "ref")
	if err := os.MkdirAll(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(ref, "ref")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(ref); err != nil {
		t.Fatal(err)
	}

	// workdir=<abs ref>, path="." → Abs(".") == workdir → root-protected.
	if ShouldSkipDirIn(ref, ".", "ref") {
		t.Error("root protection must hold for path='.' when cwd IS the workdir")
	}
	// workdir="." (relative), path=<abs ref> → Abs(".") resolves to ref →
	// same root, still protected even though 'ref' itself matches rules.
	if ShouldSkipDirIn(".", ref, "ref") {
		t.Error("root protection must hold for relative workdir='.' pointing at the same dir")
	}
	// A nested dir that legitimately matches the rules is still skipped —
	// the protection only covers the root itself.
	if !ShouldSkipDirIn(".", nested, "ref") {
		t.Error("nested ref under relative workdir='.' must still be skipped")
	}
}

// TestNarrowedWorkdirChildrenNotKilledByAncestors is the regression for
// ancestor-path false positives: after startup narrowing lands the
// workdir INSIDE a tree whose ancestors match a skip fragment or
// toolchain segment (/ws/node_modules/pkg, /.cache/tool,
// /root/go/pkg/mod/foo), the walker's fragment check must compare the
// workdir-RELATIVE path — otherwise every normal child of that root
// would be rejected just because an ancestor segment contains
// "/node_modules/", "/.cache/" or "/go/pkg/". Genuinely excluded
// subtrees nested INSIDE the narrowed root must still be skipped.
func TestNarrowedWorkdirChildrenNotKilledByAncestors(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// workdir roots narrowed into skip-fragment / toolchain trees.
	nmWd := mkdir("ws/node_modules/pkg")
	cacheWd := mkdir(".cache/tool")
	gomodWd := mkdir("go/pkg/mod/foo")

	// Normal children of the narrowed workdir are NOT skipped.
	for _, wd := range []string{nmWd, cacheWd, gomodWd} {
		lib := mkdir(filepath.Join(mustRel(t, root, wd), "lib"))
		if ShouldSkipDirIn(wd, lib, "lib") {
			t.Errorf("normal subdir %q under narrowed workdir %q must not be skipped", lib, wd)
		}
	}

	// Subtrees that re-hit a skip fragment / built-in name INSIDE the
	// narrowed workdir are still skipped.
	nestedNM := mkdir("ws/node_modules/pkg/node_modules")
	if !ShouldSkipDirIn(nmWd, nestedNM, "node_modules") {
		t.Error("nested node_modules inside narrowed workdir must still be skipped")
	}
	nestedNMDep := mkdir("ws/node_modules/pkg/node_modules/dep")
	if !ShouldSkipDirIn(nmWd, nestedNMDep, "dep") {
		t.Error("dir under a nested node_modules inside the narrowed workdir must still be skipped")
	}
	nestedCache := mkdir(".cache/tool/.cache/x")
	if !ShouldSkipDirIn(cacheWd, nestedCache, "x") {
		t.Error("nested /.cache/ subtree inside narrowed workdir must still be skipped")
	}
	// GOPATH root itself is still skipped when workdir is its parent:
	// walking /root must skip /root/go — the rel=="go" fallback to
	// isGopathRoot keeps this (literal paths, no FS access needed).
	if !ShouldSkipDirIn("/root", "/root/go", "go") {
		t.Error("GOPATH root /root/go must still be skipped when workdir is /root")
	}

	// And with no workdir at all the absolute-path behaviour is untouched.
	if !ShouldSkipDir(filepath.Join(root, "ws/node_modules/pkg/lib"), "lib") {
		t.Error("absolute path under node_modules must still be skipped without a workdir")
	}
}

// TestBuiltinSkipGlobs: the single-source exporter must produce the three
// glob shapes the rg exclusion layer consumes — bare names (skipDirNames
// + single-seg skipPathFragments), "**/a/b" multi-segment fragments
// (skipPathFragments + go/pkg|bin|src), and a top-level "/go" anchor only
// when <root>/go is itself a GOPATH workspace. Sorted + deduplicated.
func TestBuiltinSkipGlobs(t *testing.T) {
	globs := BuiltinSkipGlobs("/tmp/nonexistent-root-xyz")
	has := map[string]bool{}
	for _, g := range globs {
		has[g] = true
	}
	// Bare single-segment names.
	for _, want := range []string{"node_modules", ".wt", ".cache", "vendor", "code_references", "__pycache__"} {
		if !has[want] {
			t.Errorf("missing bare-name glob %q in %v", want, globs)
		}
	}
	// Multi-segment fragments must be "**/"-prefixed (any depth).
	for _, want := range []string{
		"**/.local/lib", "**/.local/share", "**/.cargo/registry",
		"**/Library/Caches", "**/gopath/pkg", "**/gopath/src/golang.org",
		"**/AppData/Local/Go",
		"**/go/pkg", "**/go/bin", "**/go/src",
	} {
		if !has[want] {
			t.Errorf("missing fragment glob %q in %v", want, globs)
		}
	}
	// No top-level "/go" anchor for a non-GOPATH root.
	if has["/go"] {
		t.Errorf("non-GOPATH root must not emit /go anchor, got %v", globs)
	}
	// Sorted + deduped.
	for i := 1; i < len(globs); i++ {
		if globs[i-1] >= globs[i] {
			t.Fatalf("globs not strictly sorted/deduped at %d: %q >= %q", i, globs[i-1], globs[i])
		}
	}

	// GOPATH root emits the anchored "/go".
	g2 := BuiltinSkipGlobs("/root")
	found := false
	for _, g := range g2 {
		if g == "/go" {
			found = true
		}
	}
	if !found {
		t.Errorf("GOPATH parent root /root must emit /go anchor, got %v", g2)
	}
	// $HOME too.
	if home, err := os.UserHomeDir(); err == nil {
		found = false
		for _, g := range BuiltinSkipGlobs(home) {
			if g == "/go" {
				found = true
			}
		}
		if !found {
			t.Errorf("GOPATH parent root $HOME must emit /go anchor")
		}
	}
}

// TestShouldSkipDirDotDotBoundary: a dir literally named "..foo" under a
// narrowed root is a LEGAL root-relative name — filepath.Rel yields
// "..foo", which is neither ".." nor "../...". The rel-check must be
// `rel != ".." && !strings.HasPrefix(rel, ".."+sep)` so such a directory
// is evaluated by the workdir-RELATIVE fragment check instead of being
// dumped onto the absolute-path check (which would false-positive on the
// root's own skip-fragment ancestors). With the old
// `!strings.HasPrefix(rel, "..")`, "..foo" was treated as an out-of-root
// escape: under a narrowed root inside node_modules the absolute check
// sees the "/node_modules/" ancestor segment and wrongly skips it.
func TestShouldSkipDirDotDotBoundary(t *testing.T) {
	// Root narrowed INSIDE a skip tree — its own ancestors carry
	// "/node_modules/" which the absolute-path fragment check would hit.
	root := filepath.Join(t.TempDir(), "node_modules", "proj")
	legit := filepath.Join(root, "..foo")
	if err := os.MkdirAll(filepath.Join(legit, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	// "..foo" and its child are ordinary dirs under the narrowed root:
	// the workdir-relative check must see them (rel "..foo", "..foo/lib")
	// and NOT skip — the absolute-path check would see the node_modules
	// ancestor and return true.
	if ShouldSkipDirIn(root, legit, "..foo") {
		t.Error("dir named '..foo' under narrowed root must not be skipped via ancestor fragment")
	}
	if ShouldSkipDirIn(root, filepath.Join(legit, "lib"), "lib") {
		t.Error("normal dir under '..foo' must not be skipped via ancestor fragment")
	}
	// A real skip dir inside '..foo' is still skipped by the same
	// workdir-relative check (proves the branch was reached, not bypassed).
	nm := filepath.Join(legit, "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if !ShouldSkipDirIn(root, nm, "node_modules") {
		t.Error("node_modules under '..foo' must still be skipped by the in-root rel check")
	}
	// Sanity: a genuine out-of-root escape is still handed to the absolute
	// check — a sibling OUTSIDE the root under node_modules stays skipped.
	outside := filepath.Join(filepath.Dir(root), "other")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if !ShouldSkipDirIn(root, outside, "other") {
		t.Error("path escaping the narrowed root must still hit the absolute fragment check")
	}
}

// mustRel returns the path of p relative to base, failing on error.
func mustRel(t *testing.T, base, p string) string {
	t.Helper()
	rel, err := filepath.Rel(base, p)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// TestNarrowExcludedWorkdirs covers the startup narrowing single source
// (called from cmd/codegraph-go main after workdir canonicalization):
// a session target inside a subtree its configured workdir excludes —
// the .wt container, an exclude_dirs rule, or a built-in skip dir —
// collapses Workdirs to exactly [target] (never keeping the parent as a
// blind secondary), while an explicit -workdir pointing at the parent or
// a normal indexed subtree leaves the list alone.
func TestNarrowExcludedWorkdirs(t *testing.T) {
	ws := t.TempDir() // plays the role of /root/workspace

	mkdir := func(rel string) string {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wt := mkdir(".wt/repo/feat-x")
	refSub := mkdir("ref/sub")
	junkSub := mkdir("junk/sub")
	nmSub := mkdir("node_modules/pkg")
	proj := mkdir("proj")
	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })

	// (a) no explicit -workdir, cwd inside <ws>/.wt/repo/branch → [cwd].
	got := NarrowExcludedWorkdirs("", wt, []string{ws})
	if len(got) != 1 || got[0] != wt {
		t.Fatalf("cwd in .wt subtree must narrow to [cwd], got %v", got)
	}

	// (b) daemon-style explicit -workdir <ws>/.wt/repo/branch with config
	// workdirs [<ws>] → [target] only — the parent must NOT survive at
	// Workdirs[1] as a blind ExtraDBs root.
	got = NarrowExcludedWorkdirs(wt, ws, []string{ws})
	if len(got) != 1 || got[0] != wt {
		t.Fatalf("explicit -workdir under excluded subtree must narrow to exactly [target], got %v", got)
	}

	// (c) explicit -workdir <ws> while cwd sits in the worktree → target
	// is the explicit dir, which equals the workdir root → untouched.
	got = NarrowExcludedWorkdirs(ws, wt, []string{ws})
	if len(got) != 1 || got[0] != ws {
		t.Fatalf("explicit -workdir at the root must not narrow, got %v", got)
	}

	// Anchored exclude_dirs ./ref: cwd inside <ws>/ref/sub → narrowed.
	config.SetRuntimeConfigForTesting([]string{"./ref"}, 0)
	got = NarrowExcludedWorkdirs("", refSub, []string{ws})
	if len(got) != 1 || got[0] != refSub {
		t.Fatalf("cwd under anchored ./ref must narrow, got %v", got)
	}

	// (d) env-only rule CODEGRAPH_EXCLUDE_DIRS=junk (published through the
	// runtime snapshot like LoadConfig does) narrows <ws>/junk/sub even
	// though no YAML exclude_dirs was set.
	config.SetRuntimeConfigForTesting([]string{"junk"}, 0)
	got = NarrowExcludedWorkdirs("", junkSub, []string{ws})
	if len(got) != 1 || got[0] != junkSub {
		t.Fatalf("cwd under env-only exclude rule must narrow, got %v", got)
	}
	got = NarrowExcludedWorkdirs(junkSub, ws, []string{ws})
	if len(got) != 1 || got[0] != junkSub {
		t.Fatalf("explicit target under env-only rule must narrow, got %v", got)
	}
	config.SetRuntimeConfigForTesting(nil, 0)

	// (e) built-in skip names narrow the same way: node_modules/pkg cwd.
	got = NarrowExcludedWorkdirs("", nmSub, []string{ws})
	if len(got) != 1 || got[0] != nmSub {
		t.Fatalf("cwd under built-in skip dir must narrow, got %v", got)
	}

	// A normal project subdirectory (with go.mod) is NOT blind → kept.
	got = NarrowExcludedWorkdirs("", proj, []string{ws})
	if len(got) != 1 || got[0] != ws {
		t.Fatalf("normal subdir cwd must keep the parent workdir, got %v", got)
	}

	// Edge: empty inputs never narrow.
	if got := NarrowExcludedWorkdirs("", "", []string{ws}); len(got) != 1 || got[0] != ws {
		t.Fatalf("empty target must return workdirs unchanged, got %v", got)
	}
	if got := NarrowExcludedWorkdirs("", wt, nil); got != nil {
		t.Fatalf("empty workdirs must return unchanged, got %v", got)
	}
	// Target outside every workdir → unchanged.
	outside := t.TempDir()
	if got := NarrowExcludedWorkdirs("", outside, []string{ws}); len(got) != 1 || got[0] != ws {
		t.Fatalf("target outside all workdirs must return unchanged, got %v", got)
	}
}
