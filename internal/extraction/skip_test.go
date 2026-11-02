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

	// Mid-segment empty negated class: Go's `[^z-a]` matches ANY single
	// char — '/' included — so `par[^z-a]b` hits rel "par/b" on the
	// anchored/path branch (filepath.Match(`par[^z-a]b`, "par/b") is
	// true). The rg translation needs the {?,[/]} alternation there
	// because rg's `?` refuses the separator (measured rg 14.1.1).
	config.SetRuntimeConfigForTesting([]string{"./par[^z-a]b", "sub/par[^z-a]b"}, 0)
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"par/b", true},     // the empty class sits ON '/'
		{"parYb", true},     // 'Y' is a live complement member
		{"parxb", true},     // 'x' too ('x' is not in the empty range z-a)
		{"sub/par/b", true}, // path rule hits mid-path too
		{"sub/parYb", true},
		{"inner/par/b", false}, // sub/-anchored rule pins to sub/ only
		{"par/b/deep", false},  // '/' can only sit on the class char
		{"src", false},
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("anchored [^z-a] mid-segment: ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}

	// "///" and ".///" degenerate anchors name NOTHING — the walker
	// drops them before the comparison loop, so nothing is skipped and
	// rg must never see a "//" glob (measured rg 14.1.1: `!//a/` never
	// matches anything). "[/]" / "./[/]" are valid Go patterns whose
	// only match would be the bare rel "/" — impossible — but the
	// literal dir name "[/]" is still excluded via the exact-name
	// branch (dual emission covers it for rg).
	config.SetRuntimeConfigForTesting([]string{"///", ".///", "//", "[/]", "./[/]"}, 0)
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"a", false},
		{"src", false},
		{"[/]", true}, // literal dir named "[/]" — exact-name branch
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("degenerate anchor: ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}

	// Round 6 (normalizeExcludeRule single source, oracle-cg-r5 must-fix):
	// the walker used to do its own single TrimSuffix/TrimPrefix while the
	// rg side had already moved to TrimLeft/TrimRight — "trailb//", "./anchc//",
	// ".//anchd" and "//anche//" were normalized away for rg but left a
	// residual '/' on the walker side, so the directory was excluded for rg
	// yet walked by the indexer (a real split). With the shared
	// normalization both engines now read them identically:
	//   - "trailb//"  → bare name "trailb" — hits at ANY depth;
	//   - "./anchc//" → anchored "anchc" — top level only;
	//   - ".//anchd"  → anchored "anchd" — top level only;
	//   - "//anche//" → anchored "anche" — top level only;
	//   - "./." / "./.." name nothing — early return, never excluded.
	config.SetRuntimeConfigForTesting([]string{
		"trailb//", "./anchc//", ".//anchd", "//anche//", "./.", "./..",
	}, 0)
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"trailb", true},     // bare name after trailing-slash collapse
		{"sub/trailb", true}, // bare name still hits at depth
		{"anchc", true},      // anchored, top level
		{"sub/anchc", false}, // anchored must NOT hit the deep same-name
		{"anchd", true},      // ".//d" anchored the same way
		{"sub/anchd", false},
		{"anche", true}, // "//e//" anchored the same way
		{"sub/anche", false},
		{"src", false}, // "./." and "./.." exclude nothing
		{"sub", false}, // parent of kept candidates walks
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("slash-run normalization: ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
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

// TestRgExcludeGlobTranslation: the walker↔rg translation table. Shape
// (anchor vs bare) must match ShouldSkipDirIn's own reading of the rule, and
// every construct the two glob engines disagree on must come out neutralised
// so rg means the literal thing the walker means.
func TestRgExcludeGlobTranslation(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// Shape: bare names stay unanchored, path/anchored rules root-anchor.
		{"ref", "ref", true},
		{"  ref  ", "ref", true},
		{"trailing/", "trailing", true},
		{"./ref", "/ref", true},
		{"/archive", "/archive", true},
		{"experiments/scratch", "/experiments/scratch", true},
		{"a/b/", "/a/b", true},
		{" ", "", false},
		{"", "", false},
		{"/", "", false},
		{"./", "", false},
		{".", "", false},
		{"..", "", false},
		{"./.", "", false},
		{"./..", "", false},
		// Anchor runs with no path behind them normalize to the same
		// early return — emitting "//" would give rg a glob that never
		// matches (measured rg 14.1.1) while the walker reads it
		// differently.
		{"///", "", false},
		{".///", "", false},
		{"//", "", false},
		// Trailing-slash runs collapse before the shape decision (the
		// collapsed "a" stays a bare name — anchored was decided on the
		// raw rule).
		{"a//", "a", true},
		{"./a//", "/a", true},
		// "..foo" is a legal directory name — never early-returned.
		{"..foo", "..foo", true},
		// Shared glob syntax survives untouched (both engines agree).
		{"*-backup*", "*-backup*", true},
		{"q?x", "q?x", true},
		{"[ab]x", "[ab]x", true},
		{"with space", "with space", true},
		{"-dash", "-dash", true},
		{"!bang", "!bang", true},
		// Go escape of an ordinary char: the backslash is dropped for rg
		// (both engines read `\s` as the literal "s" — the translated form
		// stays canonical).
		{"back\\slash", `back\slash`, true},
		// Go negated class [^a] → rg [!a/]: under a bare-name rule the
		// basename can never be '/', but rg's [!a] WOULD span the
		// separator (measured rg 14.1.1) and kill a two-level path — '/' is
		// injected into the excluded set, and the caller emits the "**/"
		// prefix because the glob now carries a '/' (a bare glob containing
		// '/' is root-anchored in rg's gitignore engine). Go literal-!
		// class [!a] → the POSIX form [a!] ('!' is only special FIRST).
		{"[^a]", "**/[!a/]", true},
		{"[!a]", `[a!]`, true},
		// Go has no globstar: a/**/b matches only a/<one-seg>/b, which is
		// exactly rg's single-segment '*'.
		{"a/**/b", "/a/*/b", true},
		{"a/***/b", "/a/*/b", true},
		// Brace alternation: rg would expand it, the walker never does.
		// The class form [{]/[}] is used because `\{` is NOT an escape in
		// rg's brace lexer ("unclosed alternate group", exit 2).
		{"{foo,bar}", `[{]foo,bar[}]`, true},
		{"a{b,c}d", `a[{]b,c[}]d`, true},
		{"./{a,b}/x", `/[{]a,b[}]/x`, true},
		{"foo{", `foo[{]`, true},
		// Escaped brace → class form too: `\{foo` must NOT become `\\{foo`
		// (literal backslash + unclosed alternate group, exit 2).
		// RgExcludeGlob returns the FIRST (wildcard) glob; the literal
		// partner is covered by TestRgExcludeGlobsDualEmission.
		{`\{foo`, `[{]foo`, true},
		{`foo\{bar`, `foo[{]bar`, true},
		{`foo\}bar`, `foo[}]bar`, true},
		// Escaped wildcard metachars keep their `\c` escape on both sides.
		{`\*`, `\*`, true},
		{`\?`, `\?`, true},
		{`\[`, `\[`, true},
		{`foo\[bar`, `foo\[bar`, true},
		{`x\]y`, `x\]y`, true},
		// A literal backslash (Go `\\`) becomes the class `[\\]` — a bare
		// "\\" cannot survive the "!\<glob>/" form ("dangling '\\'").
		{`a\\b`, `a[\\]b`, true},
		// Patterns filepath.Match rejects: the walker can only ever match them
		// literally, so every metacharacter must go literal for rg too — and
		// rg must not be handed an unparsable glob.
		{"data[", `data\[`, true},
		// "[a-]" is a bad RANGE for filepath.Match (ErrBadPattern) but a valid
		// class for rg — literal fallback it is. "-" needs no escape once it
		// sits outside a class.
		{"[a-]", `\[a-\]`, true},
		{"x[a-]y", `x\[a-\]y`, true},
		// An unsatisfiable positive class ([z-a]: Go silently treats the
		// reversed range as EMPTY, rg exits 2 with "invalid range") has no
		// wildcard form at all — RgExcludeGlob then returns the literal
		// translation (the walker's exact-name branch still excludes the
		// literal dir "[z-a]").
		{"[z-a]", `\[z-a\]`, true},
		// A negated class with no members ([^z-a]) is Go's "any single
		// char" — the only shape class syntax cannot express; compiled to
		// `?`, plus the literal form for the exact-name dir. `?` never
		// matches '/' for rg, so no member injection is needed.
		{"[^z-a]", "?", true},
		// Bare-name rule: a negated class that would otherwise span '/'.
		// `cmd[^x]codegraph-go` must NOT exclude the two-level
		// `cmd/codegraph-go` (the walker never sees '/' in a basename),
		// so '/' joins the excluded member set and the "**/" prefix
		// keeps the glob matching at any depth.
		{"cmd[^x]codegraph-go", "**/cmd[!x/]codegraph-go", true},
		// The `\` inside a class is a LITERAL backslash for rg — never an
		// escape. Go's `[a\]b]` (members 'a',']','b') emits the POSIX
		// ']'-first form; `[A-\]]` (Go range 'A'..']') keeps the range with
		// ']' peeled into the leading literal slot.
		{`[a\]b]`, `[]ab]`, true},
		{`[A-\]]`, `[]A-\]`, true},
		// `-` first is literal for rg (a positive class with '-' but no
		// other safe-first char puts it first); only-'!' classes cannot be
		// written as a class at all and compile to `\!` / `{\!,\^}`.
		{"[!]", `\!`, true},
		{"[!^]", `{\!,\^}`, true},
		// Bare-name rule: `[\--0]` is Go's range '-'..'0' (the first '-' is
		// the escaped lo, the second '-' the connector) which SPANS '/'
		// (Go class ranges do not exclude it — filepath.Match(`[\--0]`,
		// "/") is true). Under a bare-name rule the emitted class must
		// refuse '/', so the range is split at the separator into
		// '-'..'.' and '0'..'0' — `[.0-]` — and a two-level
		// `cmd/codegraph-go` is never swept.
		{`[\--0]`, `[.0-]`, true},
		// `a[\--0]b` has no literal '/' so it stays a bare-name rule
		// (allowSlash=false): the range IS split at the separator into
		// '-'..'.' and '0'..'0' and emitted `[.0-]` — a two-level
		// `a/b` is never swept, matching the walker's basename branch.
		{`a[\--0]b`, `a[.0-]b`, true},
		// `a[/]b` DOES carry a literal '/' (path rule): '/' is a legal
		// class member for Go on the rel-path branch
		// (filepath.Match(`a[/]b`, "a/b") is true) and rg honours it
		// the same way, so it is kept verbatim.
		{`a[/]b`, `/a[/]b`, true},
		{`a[\/]b`, `/a[/]b`, true},
		{`a[^/x]b`, `/a[!/x]b`, true},
		// `a[^z-a]b` has no literal '/', so the walker treats it as a
		// bare-name rule (basename match: `axb`→true, `a/b`→true via the
		// basename 'b' never satisfying 'a*'). The empty negated class
		// compiles to `?` which never matches '/' for rg either way.
		{`a[^z-a]b`, `a?b`, true},
		// `./[^z-a]` and `a/[^z-a]` carry a real '/' — path rules; the
		// negated class still compiles to `?` because it sits ADJACENT
		// to the separator (or at the pattern edge), where no rel path
		// can put a second '/' for the class to sit on.
		{`./[^z-a]`, `/?`, true},
		{`a/[^z-a]`, `/a/?`, true},
		// `par[^z-a]b` mid-segment is different: rel `par/b` puts the
		// class right on '/' and the walker DOES match
		// (filepath.Match(`par[^z-a]b`, "par/b") is true) while rg's
		// `?` refuses the separator — measured rg 14.1.1 leaves
		// `par/b/` listed under `!/par?b/`. The emitted alternation
		// `{?,[/]}` adds the '/'-sitting arm the walker also matches.
		{`./par[^z-a]b`, `/par{?,[/]}b`, true},
		{`sub/par[^z-a]b`, `/sub/par{?,[/]}b`, true},
		{`par/[^z-a]b`, `/par/?b`, true}, // segment edge: '/' slot impossible
		{`par[^z-a]/b`, `/par?/b`, true}, // '/' follows: '//' would result
		{`par[^z-a]`, `par?`, true},      // bare name + pattern end
		{`par[^z-a]*b`, `par?*b`, true},  // bare name: basename never has '/'
		// `[/]` / `./[/]` are VALID Go patterns that match nothing the
		// walker can ever see (no rel is the bare "/"), so rg gets the
		// class AND the literal name — dual emission; the FIRST glob
		// here is the wildcard form, the literal partner is covered by
		// TestRgExcludeGlobsDualEmission.
		{`[/]`, `/[/]`, true},
		{`./[/]`, `/[/]`, true},
		// Bare-name negated class over a two-part name: the member '/'
		// is injected so `[!x]` becomes `[!x/]` and refuses the
		// separator just like the walker does.
		{"a[^x]b", "**/a[!x/]b", true},
		// '{' and ',' need no escaping inside a class for rg (its brace
		// lexer does not see through the class — measured rg 14.1.1).
		{"[a{b]", `[a{b]`, true},
		{"[{a,b}]", `[{a,b}]`, true},
		// A dangling escape: filepath.Match rejects it, so the walker can
		// only match the literal name. The literal translation turns the
		// trailing backslash into the class `[\\]` — parseable in the
		// "!\<glob>/" form and still literal (a trailing bare `\` would
		// be "dangling '\\'", exit 2).
		{`a\`, `a[\\]`, true},
		{`a\\`, `a[\\]`, true},
		{`data\`, `data[\\]`, true},
		// filepath.Match(pat, "") short-circuits at the FIRST '*' chunk
		// whose prefix fails to match the empty name, so a malformed
		// tail (`*\`, `*[`, `*[a-`) used to slip past the validator and
		// feed the bad pattern into the literal-escape path with a panic
		// in translateRgGlob. isValidGoPattern scans every '*' chunk, so
		// these rules are now correctly rejected and fall back to literal.
		{`foo*\`, `foo\*[\\]`, true},
		{`foo*[`, `foo\*\[`, true},
		{`foo*[a-`, `foo\*\[a-`, true},
		{`*a\`, `\*a[\\]`, true},
		{`[a]*\`, `\[a\]\*[\\]`, true},
		// Literal fallback keeps '/' as the separator (never escaped away).
		{"dir/data[", `/dir/data\[`, true},
		// Slash-path rule with a '\'-escape: the '\' is dropped for rg
		// (both engines read `\s` as the literal "s"), and '/' members
		// inside a class keep their meaning on the rel-path branch.
		{`a[\/]b`, `/a[/]b`, true},
	}
	for _, c := range cases {
		got, ok := RgExcludeGlob(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("RgExcludeGlob(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestRgExcludeGlobsDualEmission: a rule that is a VALID Go pattern whose
// own literal name does NOT satisfy the pattern (`\{foo`, `[ab]`, `[!a]`,
// `foo\[bar`) is still excluded by the walker's exact-name branch, so
// RgExcludeGlobs must emit BOTH the wildcard translation and the literal
// translation (deduplicated, wildcard first). Rules whose literal name
// self-matches emit the single wildcard form only.
func TestRgExcludeGlobsDualEmission(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`\{foo`, []string{`[{]foo`, `[\\][{]foo`}},
		{`foo\[bar`, []string{`foo\[bar`, `foo[\\]\[bar`}},
		{`[ab]`, []string{`[ab]`, `\[ab\]`}},
		{`[!a]`, []string{`[a!]`, `\[!a\]`}},
		// Bare-name negated class: '/' joins the excluded member set and
		// the "**/" prefix keeps the glob unanchored (it now carries '/').
		{`[^a]`, []string{`**/[!a/]`, `\[^a\]`}},
		{`q?x`, []string{`q?x`}}, // literal name self-matches
		{`back\slash`, []string{`back\slash`, `back[\\]slash`}},
		// Self-matching / invalid rules stay single-glob.
		{`ref`, []string{`ref`}},
		{`*-backup*`, []string{`*-backup*`}},
		{`{foo,bar}`, []string{`[{]foo,bar[}]`}},
		{`data[`, []string{`data\[`}}, // parse-invalid: literal only
		{`a\`, []string{`a[\\]`}},     // literal dir a\ (dangling escape)
		// Early-return rules emit nothing at all.
		{` `, nil},
		{`/`, nil},
		{`.`, nil},
		{`..`, nil},
		{`///`, nil},
		{`.///`, nil},
		// `[/]` matches nothing the walker can see (no rel is the bare
		// "/") but the literal dir name is still excluded through the
		// exact-name branch — dual emission, wildcard first.
		{`[/]`, []string{`/[/]`, `/\[/\]`}},
	}
	for _, c := range cases {
		got := RgExcludeGlobs(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("RgExcludeGlobs(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("RgExcludeGlobs(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

// TestExcludeDirsGlobMetaRulesAreLiteral: the walker side of the must-fix —
// "{foo,bar}" and "data[" name ONE literal directory each. rg used to expand
// the braces (dropping foo/ and bar/, which the indexer still walks) and to
// abort on the unclosed class; the translation now keeps rg on this side.
func TestExcludeDirsGlobMetaRulesAreLiteral(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{
		"{foo,bar}", "data[", "with space", "-dash", "!bang",
		`\{foo`, `foo\[bar`, "./[!a]", "./[^a]", "gr/a/**/b",
	}, 0)

	mkdir := func(rel string) string {
		p := filepath.Join(wd, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	skip := map[string]bool{
		"{foo,bar}":  true, // the literal directory the rule names
		"data[":      true,
		"with space": true,
		"-dash":      true,
		"!bang":      true,
		// Dual-emission cases: the literal name itself is excluded by the
		// walker's exact-name branch even though the pattern does not
		// self-match.
		`\{foo`:    true, // literal backslash-brace dir (name == pat)
		"{foo":     true, // what `\{foo` MATCHES as a Go pattern
		`foo\[bar`: true, // literal backslash-bracket dir
		"foo[bar":  true, // what `foo\[bar` matches
		// NOT excluded: what rg's brace expansion used to hit instead.
		"foo":       false,
		"bar":       false,
		"data":      false,
		"data[x":    false,
		"withspace": false,
		"dash":      false,
		"bang":      false,
	}
	for name, want := range skip {
		p := mkdir(name)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != want {
			t.Errorf("ShouldSkipDirIn(%q) = %v, want %v", name, got, want)
		}
	}

	// Class semantics under anchored path rules ./[!a] and ./[^a] (the
	// anchor pins the match to the top-level rel path, so a single-char
	// class cannot sweep every segment of the tree):
	//   - [!a] is NOT a negation for filepath.Match — it is the literal
	//     members '!' or 'a' — while rg would read it as negation; the
	//     translation emits the POSIX form [a!] ('!' is only special in
	//     first position).
	//   - [^a] IS Go's negation (any single char except 'a') and maps to
	//     rg's [!a].
	// Literal dirs named "[!a]" / "[^a]" are still excluded via the
	// walker's `rel == sub` exact-name branch (dual-glob emission covers
	// them for rg).
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"a", true},      // [!a] member 'a'
		{"!", true},      // [!a] member '!'; [^a] hits too
		{"b", true},      // [^a] hits
		{"c", true},      // [^a] hits
		{"zz", false},    // two chars: neither class matches
		{"ab", false},    // same
		{"[!a]", true},   // literal "[!a]" dir: exact rel == "[!a]"
		{"[^a]", true},   // literal "[^a]" dir: exact rel == "[^a]"
		{"sub/a", false}, // anchored rules pin to top level only
		{"sub/b", false},
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}
	// Globstar rule "gr/a/**/b" under the walker's slash-path semantics
	// (matched against the full rel path): filepath.Match has no '**',
	// so only gr/a/<one-seg>/b matches as a PATTERN — but the
	// `HasPrefix(rel, pat+"/")` literal-subtree branch also excludes
	// everything UNDER gr/a/**/b. Kept: gr/a/b (zero middle segments),
	// gr/a/x/y/b (two), and the parent dirs themselves.
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"gr/a/x/b", true},
		// "gr/a/x/b/deep" is NOT skipped: the subtree prefix check applies
		// to the LITERAL rule path ("gr/a/**/b/"...), not to a
		// pattern-matched path — Go's '*' does not reach deeper rels and
		// there is no globstar. The rg translation /gr/a/*/b + the
		// "/**" contents pair behaves the same way (verified end-to-end
		// in the server-side test).
		{"gr/a/b", false},
		{"gr/a/x/y/b", false},
		{"gr/a", false},
		{"gr", false},
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}
}

// TestExcludeDirsPosixClassRules: the walker side of the POSIX class
// normalization (reviewer-cg-r3) — bare-name class rules matched per
// basename, separated from TestExcludeDirsGlobMetaRulesAreLiteral
// because several classes are too broad to mix with the slash-path
// cases ([^z-a] is Go's any-single-char class and would sweep 'a','b'
// in the anchored loop; `[\--0]`'s range '-','.','/','0' spans '/' for
// Go — class ranges do not exclude it — but under a bare-name rule the
// emitted class is split at the separator into `[.0-]` so a two-level
// path can never be swept; 'gr' subtree entries still land in its
// member set because '-' and '0' are real members).
func TestExcludeDirsPosixClassRules(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{
		"[z-a]", "[^z-a]", "[z-ab]", "[!]", "[!^]",
		`[a\]b]`, `[A-\]]`, `[\--0]`, "[a{b]", "[{a,b}]",
	}, 0)

	mkdir := func(rel string) string {
		p := filepath.Join(wd, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"[z-a]", true},   // exact name: the rule IS the literal dir
		{"[z-ab]", true},  // exact name
		{`[a\]b]`, true},  // exact name
		{`[A-\]]`, true},  // exact name
		{`[\--0]`, true},  // exact name
		{"[a{b]", true},   // exact name
		{"[{a,b}]", true}, // exact name
		{"[!]", true},     // exact name
		{"[!^]", true},    // exact name
		{"^", true},       // [!^] member '^'
		{"y", true},       // [^z-a] any-single-char hits
		{"w", true},       // [^z-a] hits
		{"zz", false},     // two chars: no rule matches
		{`\`, true},       // '\' is a member of TWO rules for Go itself:
		// [A-\]]'s range 'A'..']' spans it (92) and [\--0]'s `--0` leaves
		// '\' the lo of the range '-'..'0' — the walker genuinely
		// excludes it; the point of this rule set is that rg must too
		// (proven in the server-side end-to-end test) while the OLD
		// in-class-escape emission [\!] would have swept it spuriously.
		{"A", true}, // [A-\]] range member
		{"Z", true}, // [A-\]] range member
		{"[", true}, // [A-\]] range member ('[' = 91 < ']' = 93)
		{"0", true}, // [\--0] range member
		{"-", true}, // [\--0] range member ('/' would be a Go member too,
		// but a basename can never be '/' — under a bare-name rule the
		// emitted class is split at the separator into `[.0-]` so the
		// rg glob refuses '/' exactly like the walker does)
		{",", true}, // [{a,b}] member
		{"{", true}, // [a{b] and [{a,b}] member
		{"}", true}, // [{a,b}] member
		{"keep", false},
		{"src", false},
	} {
		p := mkdir(c.rel)
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}
}

// TestRgExcludeGlobStarSuffixBadSyntax: filepath.Match(pat, "") returns
// (false, nil) at the FIRST '*' chunk whose prefix fails to match the
// empty name, so a syntax error in a LATER chunk used to be invisible to
// rgSafeGlob's validity oracle — `foo*\` then reached translateRgGlob's
// `pat[i+1]` with i+1==len(pat) and panicked. isValidGoPattern scans every
// '*' chunk, so each rule below is now correctly rejected and translated
// as a PURE literal directory name — no panic, no wildcard, rg-parseable.
func TestRgExcludeGlobStarSuffixBadSyntax(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`foo*\`, `foo\*[\\]`},   // dangling escape after a '*' chunk
		{`foo*[`, `foo\*\[`},     // unclosed class after a '*' chunk
		{`foo*[a-`, `foo\*\[a-`}, // unclosed range after a '*' chunk
		{`*a\`, `\*a[\\]`},       // dangling escape in the LAST chunk
		{`[a]*\`, `\[a\]\*[\\]`}, // valid class then bad tail
	}
	for _, c := range cases {
		got, ok := RgExcludeGlob(c.in)
		if !ok || got != c.want {
			t.Errorf("RgExcludeGlob(%q) = (%q, %v), want (%q, true)", c.in, got, ok, c.want)
		}
	}
	// The literal directory itself is still excluded by the walker's
	// exact-name branch — while a same-prefix dir (`fooX`) is NOT.
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{`foo*\`}, 0)
	for _, c := range []struct {
		name string
		skip bool
	}{
		{`foo*\`, true}, // the literal directory the rule names
		{"fooX", false}, // a '*' chunk does not turn the rule into a glob
		{"foo", false},
		{"src", false},
	} {
		p := filepath.Join(wd, filepath.FromSlash(c.name))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("ShouldSkipDirIn(%q) = %v, want %v", c.name, got, c.skip)
		}
	}
}

// TestExcludeDirsSlashClassBareVsPath: the bare-name branch of
// ShouldSkipDirIn feeds filepath.Match a basename — it can never contain
// '/' — while the slash-path branch feeds the rel path where a class CAN
// hit '/' (Go getEsc does not exclude it). rg's globset reads both
// engines the same way, but rg's `[!x]` spans the separator where the
// walker's bare-name match cannot. The emitted glob must therefore inject
// '/' into the excluded member set for bare-name rules (`[!x]` → `[!x/]`,
// emitted `**/`-prefixed because it now carries '/') and split positive
// ranges at the separator (`[\--0]` → `[.0-]`), while path rules keep
// '/' whole (`a[/]b` excludes `a/b` only).
func TestExcludeDirsSlashClassBareVsPath(t *testing.T) {
	wd := t.TempDir()
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })

	mkdir := func(rel string) string {
		p := filepath.Join(wd, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Layout: the two-level `cmd/codegraph-go` the bare-name rules must
	// NOT kill, plus the single-segment dirs they must hit at top level
	// AND at depth.
	for _, rel := range []string{
		"cmd/codegraph-go",
		"cmdYcodegraph-go",
		"sub/cmdYcodegraph-go",
		"cmd-codegraph-go",
		"sub/cmd.codegraph-go",
		"src",
	} {
		mkdir(rel)
	}

	// Bare-name rule cmd[^x]codegraph-go: the walker matches the basename
	// 'codegraph-go' against 'cmd[^x]codegraph-go' — false (the '^x' class
	// rejects 'x' but the name is 13 chars, not 1). The two-level rel
	// 'cmd/codegraph-go' is the real adversary: rg's [!x] spans '/' and
	// would exclude it; the walker never sees '/' in a basename, so it
	// must NOT be skipped.
	config.SetRuntimeConfigForTesting([]string{"cmd[^x]codegraph-go"}, 0)
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"cmd/codegraph-go", false}, // two-level: basename match impossible
		{"cmdYcodegraph-go", true},
		{"sub/cmdYcodegraph-go", true},
		{"cmd-codegraph-go", true}, // '-' IS in the [^x] complement
		{"src", false},
	} {
		p := filepath.Join(wd, filepath.FromSlash(c.rel))
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("bare-name [^x]: ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}

	// Bare-name rule cmd[\--0]codegraph-go: the range '-'..'0' spans '/'
	// for Go, but a basename can never BE '/', so the emitted class must
	// refuse the separator — `[.0-]` — while still hitting '-', '.', '0'.
	config.SetRuntimeConfigForTesting([]string{`cmd[\--0]codegraph-go`}, 0)
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"cmd/codegraph-go", false}, // '/' must be refused
		{"cmd-codegraph-go", true},  // '-' in range
		{"sub/cmd.codegraph-go", true},
		{"src", false},
	} {
		p := filepath.Join(wd, filepath.FromSlash(c.rel))
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("bare-name [\\--0]: ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
	}

	// Slash-path rule a[/]b: '/' is a legitimate class member on the
	// rel-path branch — the walker excludes `a/b` and rg must too. The
	// rule only has the literal and wildcard translations; neither should
	// panic on the '/' member or crash rg.
	config.SetRuntimeConfigForTesting([]string{`a[/]b`, `a[\/]b`, `a[^/x]b`, `a[^z-a]b`}, 0)
	for _, rel := range []string{"a/b", "aYb", "axb"} {
		mkdir(rel)
	}
	for _, c := range []struct {
		rel  string
		skip bool
	}{
		{"a/b", true}, // a[/]b, a[\/]b and a[^z-a]b all hit
		{"aYb", true}, // a[^/x]b and a[^z-a]b hit
		{"axb", true}, // a[^z-a]b hits; a[^/x]b does NOT ('x' excluded)
		{"src", false},
	} {
		p := filepath.Join(wd, filepath.FromSlash(c.rel))
		if got := ShouldSkipDirIn(wd, p, filepath.Base(p)); got != c.skip {
			t.Errorf("path-rule '/': ShouldSkipDirIn(%q) = %v, want %v", c.rel, got, c.skip)
		}
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
