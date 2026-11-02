package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dorokuma/codegraph-go/internal/config"
	"github.com/dorokuma/codegraph-go/internal/db"
	"github.com/dorokuma/codegraph-go/internal/extraction"
)

// TestRgExcludeArgs: the helper maps each exclusion rule class to the
// matching rg negative glob pair (directory-only "<glob>/" + contents
// "<glob>/**") and honours the no_ignore opt-out. The built-in rules come
// from extraction.BuiltinSkipGlobs (single source of truth); the test
// asserts the four config classes land after them in order.
func TestRgExcludeArgs(t *testing.T) {
	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"ref", "*-backup*", "./archive", "experiments/scratch", " "}, 0)

	got := rgExcludeArgs("/any/root", false)

	// The built-in single-source globs come first — node_modules, .wt,
	// code_references (bare names), **/.local/lib, **/go/pkg (fragments),
	// etc. They are sorted lexically by BuiltinSkipGlobs. Assert a few
	// representative built-ins are present as pairs, then the config tail.
	mustContainPair(t, got, "!node_modules/", "!node_modules/**")
	mustContainPair(t, got, "!.wt/", "!.wt/**")
	mustContainPair(t, got, "!code_references/", "!code_references/**")
	mustContainPair(t, got, "!**/.local/lib/", "!**/.local/lib/**")
	mustContainPair(t, got, "!**/go/pkg/", "!**/go/pkg/**")

	// Config rules land last, in declaration order, after the built-ins.
	tail := []string{
		"-g", "!ref/", "-g", "!ref/**",
		"-g", "!*-backup*/", "-g", "!*-backup*/**",
		"-g", "!/archive/", "-g", "!/archive/**",
		"-g", "!/experiments/scratch/", "-g", "!/experiments/scratch/**",
	}
	if len(got) < len(tail) || !reflect.DeepEqual(got[len(got)-len(tail):], tail) {
		t.Fatalf("rgExcludeArgs config tail = %v, want %v", got[len(got)-min(len(tail), len(got)):], tail)
	}

	if got := rgExcludeArgs("/any/root", true); got != nil {
		t.Fatalf("no_ignore must disable exclusion globs, got %v", got)
	}

	// noIgnore=true must not emit anything even with config rules.
	if got := rgExcludeArgs("/any/root", true); got != nil {
		t.Fatalf("no_ignore must disable all globs, got %v", got)
	}
}

// mustContainPair asserts a "!<g>/" + "!<g>/**" negative-glob pair appears
// consecutively somewhere in the -g flag list.
func mustContainPair(t *testing.T, args []string, dir, subtree string) {
	t.Helper()
	for i := 0; i+3 < len(args); i += 4 {
		if args[i] == "-g" && args[i+1] == dir && args[i+2] == "-g" && args[i+3] == subtree {
			return
		}
	}
	t.Fatalf("glob pair (%q, %q) not found in %v", dir, subtree, args)
}

// TestRgExcludeArgsFiltersMatches: end-to-end against a real rg — files
// under .wt and an exclude_dirs rule must not be matched while a normal
// file still is.
func TestRgExcludeArgsFiltersMatches(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go")
	write(".wt/repo/feat-x/src/dup.go")
	write("ref/data.go")
	write("proj/internal/ref/ok.go") // deep same-name under anchored rule: kept
	// Built-in skip trees the indexer would refuse: node_modules, .cache,
	// .local/lib (fragment), code_references (single-seg fragment), go/pkg.
	write("node_modules/pkg/lib/sub.go")
	write(".cache/a.go")
	write(".local/lib/b.go")
	write("code_references/c.go")
	write("go/pkg/mod/d.go")
	write("sub/go/pkg/e.go")
	// Same-named paths that must NOT be killed: a project dir *-go/pkg is
	// not a GOPATH segment, and sdk/go is not a toolchain dir.
	write("codegraph-go/pkg/keep.go")
	write("sdk/go/keep.go")

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"./ref"}, 0)

	args := append([]string{"--line-number", "--no-heading", "--color=never", "--fixed-strings"},
		rgExcludeArgs(root, false)...)
	args = append(args, "--", "needle", root)
	cmd := exec.Command("rg", args...)
	// "/"-anchored globs resolve against the child cwd, same contract the
	// production call sites honour (cmd.Dir = project root).
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			t.Fatalf("rg: %v", err)
		}
	}
	got := string(out)

	wantHit := []string{
		"src/main.go",
		"proj/internal/ref/ok.go",  // deep same-name under anchored rule: kept
		"codegraph-go/pkg/keep.go", // *-go/pkg is NOT a GOPATH segment
		"sdk/go/keep.go",           // sdk/go is NOT a toolchain dir
	}
	for _, w := range wantHit {
		if !strings.Contains(got, w) {
			t.Fatalf("expected %q in rg output, got: %q", w, got)
		}
	}
	wantMiss := []string{
		".wt/",          // .wt container
		"ref/data.go",   // anchored ./ref top-level dir
		"node_modules/", // built-in skip dir
		".cache/",       // built-in skip dir
		".local/lib/",   // skipPathFragments multi-seg
		"code_references/",
		"go/pkg/mod/d.go", // Go toolchain segment (top level)
		"sub/go/pkg/e.go", // Go toolchain segment (nested)
	}
	for _, m := range wantMiss {
		if strings.Contains(got, m) {
			t.Fatalf("%q must be excluded, got: %q", m, got)
		}
	}
}

// TestRgExcludeArgsWeirdRulesAgreeWithWalker is the must-fix regression for
// the two glob engines disagreeing about one exclude_dirs rule. The raw rule
// used to be pasted into rg's gitignore globset, which reads it differently
// from the indexer's walker (filepath.Match + exact name):
//
//   - "{foo,bar}" became an alternation over foo/ and bar/ — trees the
//     indexer still walks, so rg dropped matches the tools should show;
//   - "data[" aborted the entire call: `rg: error parsing glob '!data[/':
//     unclosed character class`, exit 2, every search/files result lost.
//
// All rules now go through extraction.RgExcludeGlob. This drives real
// `rg --files` invocations and requires, for every candidate directory, that
// rg's keep/drop verdict equals ShouldSkipDirIn's, with zero rg stderr and no
// exit-2 parse error.
func TestRgExcludeArgsWeirdRulesAgreeWithWalker(t *testing.T) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg not on PATH")
	}
	root := t.TempDir()

	rules := []string{
		"{foo,bar}", // brace alternation rg expands, walker does not
		"data[",     // unclosed class rg refuses, walker matches literally
		"foo{",      // unclosed alternation rg refuses
		"[a-]",      // bad range for filepath.Match, valid class for rg
		"with space",
		"-dash", // leading dash must not look like an rg flag
		"!bang", // leading bang after the negation marker
		"*-backup*",
		"q?x",
		`\{foo`,        // valid Go pattern (literal {foo); needs BOTH globs
		`foo\[bar`,     // valid Go pattern (literal foo[bar); both globs
		"[!a]",         // Go: literal '!' or 'a'; rg would read it as negation
		"[^a]",         // Go negation → rg [!a]
		"a/**/b",       // Go has no globstar: only a/<one>/b matches
		"./archive",    // anchored: only <root>/archive
		"deep/scratch", // workdir-relative path rule
		// POSIX class normalization — the reviewer-cg-r3 crash set. Every
		// one of these either killed rg outright ("invalid range" / an
		// in-class '\' escape that silently widened the member set →
		// exit 2 or wrong tree) or matched a different member set than
		// filepath.Match. rg 14.1.1 must now parse every glob pair and
		// land on the same keep/drop verdict as ShouldSkipDirIn.
		"[z-a]",   // Go: silently EMPTY range → literal dir only
		"[^z-a]",  // Go: any single char → `?` (+ literal dir)
		"[z-ab]",  // empty range + live member 'b' → [b]
		"[!]",     // Go: member '!' → \! (no class form exists)
		"[!^]",    // Go: members '!','^' → alternation {\!,\^}
		`[a\]b]`,  // Go members a,],b → POSIX ']'-first []ab]
		`[A-\]]`,  // Go range 'A'..']' → peeled endpoint []A-\]
		`[\--0]`,  // bare-name: range split at '/' → [.0-]
		"[a{b]",   // '{' literal in-class for rg (no escape needed)
		"[{a,b}]", // ',' literal in-class too
		// Round 5 (oracle-cg-r4): '*' -suffix malformed patterns used to
		// panic translateRgGlob (filepath.Match(pat,"") short-circuits
		// at the first failing '*' chunk and never sees the bad tail),
		// and bare-name rules let a negated class or a '/-spanning range
		// match the separator the walker never sees.
		`foo*\`, // dangling escape after '*' → literal dir only
		`foo*[`, // unclosed class after '*' → literal dir only
		`foo*[a-`,
		`*a\`,
		`[a]*\`,
		// Bare-name vs slash-path '/' handling: `cmd[^x]codegraph-go`
		// must NOT kill the two-level `cmd/codegraph-go` (basename never
		// has '/'), while `a[/]b` must exclude exactly `a/b`.
		"cmd[^x]codegraph-go",
		`cmd[\--0]codegraph-go`,
		"par[/]b",
		`par[\/]b`,
		"par[^/x]b",
		"par/[^z-a]",
		"./[^z-a]",
		// Round 5b (worker-cg-r5b): an empty negated class MID-segment
		// can legitimately sit ON '/' for Go (filepath.Match(
		// `par[^z-a]b`, "par/b") is true) while rg's `?` refuses the
		// separator — the {?,[/]} alternation covers it. Degenerate
		// anchor runs ("///", ".///") normalize away and must emit no
		// "//" glob at all; `[/]`/`./[/]` are valid Go patterns whose
		// only match is the bare rel "/" — impossible — so rg just
		// needs parseable globs (measured rg 14.1.1: `!/[/]/` exit 0).
		"./par[^z-a]b",
		"sub/par[^z-a]b",
		"///",
		".///",
		"//",
		"[/]",
		"./[/]",
		// Round 6 (oracle-cg-r5 must-fix): the walker's single
		// TrimSuffix/TrimPrefix used to leave a residual '/' on rules like
		// "trailb//", "./anchc//", ".//anchd", "//anche//" — rg already
		// excluded them while the indexer kept walking (a real walker↔rg
		// split). normalizeExcludeRule is now the shared normalization;
		// every candidate below must get the same verdict from both sides.
		"trailb//",
		"./anchc//",
		".//anchd",
		"//anche//",
		"./.",
		"./..",
	}
	candidates := []string{
		"{foo,bar}", "foo", "bar",
		"data[", "data", "data[x",
		"foo{", "[a-]", "a", "a-b",
		"with space", "withspace", "keep with space",
		"-dash", "dash",
		"!bang", "bang",
		"proj-backup-2026", "backup",
		"q?x", "qx", "qxx",
		`\{foo`, "{foo", `foo\[bar`, "foo[bar",
		"!a", "a", "b", "x",
		"a/b", "a/x/b", "a/x/y/b",
		"archive", "sub/archive", "deep/scratch", "deep/other", "deep/scratch/inner", "src",
		// Candidates for the POSIX class normalization rules: the literal
		// dirs the rules NAME (exact-name branch), the member sets the
		// classes mean for Go, and near-misses they must not hit. `\\`
		// (a dir literally named backslash) is included because in-class
		// '\\' is literal for rg — Go sweeps it via [A-\]]/[\--0], and
		// the emitted globs must too, but only through those ranges, not
		// through the old spurious `\!`/`\]` in-class escapes.
		"[z-a]", "[^z-a]", "[z-ab]", "b", "z", "y", "w",
		"[!]", "!", "[!^]", "^",
		`[a\]b]`, `a\]b`, "]", `\`,
		`[A-\]]`, "A", "Z", "[", "_",
		`[\--0]`, "-", "0", ".x", "0x",
		"[a{b]", "{", "[{a,b}]", ",", "}",
		// Round-5 candidates: literal dirs the bad-`*`-suffix rules name,
		// plus the two-level `cmd/codegraph-go` that bare-name
		// `cmd[^x]codegraph-go` / `cmd[\--0]codegraph-go` must NOT hit,
		// and the slash-path members `a/b` / `aYb` / `axb` for the
		// `/`-in-class path rules.
		`foo*\`, `foo*[`, `foo*[a-`, `*a\`, `[a]*\`, "fooX",
		"cmd/codegraph-go", "cmdYcodegraph-go", "sub/cmdYcodegraph-go",
		"cmd-codegraph-go", "sub/cmd.codegraph-go",
		// 'par' stays searchable on purpose — the path rules below need a
		// live multi-char parent to prove `par/b` is excluded only by
		// `par[/]b`, never by a bare-name rule sweeping the parent.
		"par/b", "parYb", "parxb",
		// Round-5b candidates: the mid-segment empty negated class must
		// exclude `par/b` (class sits on '/') alongside `parYb`/`parxb`,
		// and the literal dir `par[^z-a]b` the rule names via the
		// exact-name branch.
		"par[^z-a]b", "sub/par/b", "sub/parYb", "sub/par[^z-a]b",
		"inner/par/b", "a",
		// Round-6 candidates: "trailb//" collapses to a bare name and hits
		// at any depth; the three anchored forms pin their rule to the top
		// level — the sub/ twins must stay searchable on BOTH engines.
		"trailb", "sub/trailb",
		"anchc", "sub/anchc",
		"anchd", "sub/anchd",
		"anche", "sub/anche",
	}
	// A dangling-escape rule is verified separately in phase two below:
	// `a\` is not a valid Go pattern, so the equality loop would compare
	// nothing meaningful — the isolated second phase instead asserts the
	// literal a[\\] glob excludes exactly the literal dir `a\` while
	// keeping `src` searchable (see below).
	candidates = append(candidates, `a\`)
	for _, d := range candidates {
		p := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "in.txt"), []byte("keepme\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })

	// runRg runs one real `rg --files` with the currently configured rules and
	// returns the set of directories still listed. Any rg stderr or an exit
	// code beyond 0/1 (2 = glob parse error) fails the test outright.
	runRg := func() map[string]bool {
		t.Helper()
		args := append([]string{"--files", "--hidden", "--sort=path"}, rgExcludeArgs(root, false)...)
		args = append(args, ".")
		cmd := exec.Command(rgPath, args...)
		cmd.Dir = root // "/"-anchored globs resolve against the project root
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) || ee.ExitCode() != 1 {
				t.Fatalf("rg must exit 0/1 (1 = no matches), got %v; stderr=%q", err, stderr.String())
			}
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			t.Fatalf("rg stderr must be empty — an exclude_dirs rule still breaks the call: %q\nargs: %v", s, args)
		}
		kept := map[string]bool{}
		// Split strictly on newlines: strings.Fields would silently break a
		// directory whose name contains a space ("keep with space" → two
		// bogus paths) and turn a real divergence into a false pass.
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				continue
			}
			rel := strings.TrimPrefix(line, "."+string(filepath.Separator))
			kept[filepath.ToSlash(filepath.Dir(rel))] = true
		}
		return kept
	}

	config.SetRuntimeConfigForTesting(rules, 0)
	kept := runRg()
	for _, d := range candidates {
		if d == `a\` {
			continue // phase two
		}
		p := filepath.Join(root, filepath.FromSlash(d))
		wantSkip := extraction.ShouldSkipDirIn(root, p, filepath.Base(p))
		if wantSkip == kept[d] {
			// wantSkip=true ⇒ rg must have dropped the dir's file (the dir is
			// absent from the listing); wantSkip=false ⇒ it must be listed.
			t.Errorf("dir %q: walker ShouldSkipDirIn=%v but rg kept it=%v — rg and the indexer disagree", d, wantSkip, kept[d])
		}
	}
	// Sanity: the plain source dir stays searchable, so a glob that
	// accidentally excluded the whole tree cannot pass the loop above by
	// making every candidate look "excluded".
	if !kept["src"] {
		t.Fatalf("src must stay searchable, rg listing: %v", kept)
	}
	// A not-excluded directory whose name contains a space must come back
	// as ONE listing entry — the newline-split above is what makes this
	// assertion honest (strings.Fields would split it into two bogus
	// paths and silently miskey the map).
	if !kept["keep with space"] {
		t.Fatalf("the 'keep with space' dir must stay searchable, rg listing: %v", kept)
	}

	// Phase three — globstar collapse in isolation: with ONLY the a/**/b
	// rule active (the [!a]/[^a] bare-class rules above would mask the
	// single-segment distinction through the basename branches), rg's
	// single-`*` translation /a/*/b must keep a/b and a/x/y/b (Go '**' is
	// one segment, not zero or two) while dropping a/x/b.
	config.SetRuntimeConfigForTesting([]string{"a/**/b"}, 0)
	kept = runRg()
	if kept["a/x/b"] {
		t.Errorf("a/**/b must exclude a/x/b (single middle segment), rg listing: %v", kept)
	}
	if !kept["a/b"] {
		t.Errorf("a/**/b must NOT exclude a/b (Go has no zero-segment '**'), rg listing: %v", kept)
	}
	if !kept["a/x/y/b"] {
		t.Errorf("a/**/b must NOT exclude a/x/y/b (Go '**' is single-segment), rg listing: %v", kept)
	}

	// Phase two — the dangling-escape rule `a\`: filepath.Match rejects it,
	// so the walker can only match the literal directory "a\". A trailing
	// bare backslash is inexpressible in the "!\<glob>/" form ("dangling
	// '\\'", exit 2), so the literal translation emits the class form
	// `a[\\]` — which rg parses and which correctly excludes the literal
	// dir, matching the walker's exact-name semantics. Zero stderr, zero
	// exit-2, and no unrelated tree is dropped.
	config.SetRuntimeConfigForTesting([]string{`a\`}, 0)
	kept = runRg()
	if kept[`a\`] {
		t.Errorf("rg must exclude the literal `a\\` dir via the a[\\\\] class glob")
	}
	if !kept["src"] {
		t.Errorf("src must stay searchable under the `a\\` rule")
	}
}

// TestToolSearchNarrowedRootInSkipAncestor: the project root itself has
// been narrowed INTO a tree whose ancestor segments match built-in skip
// rules (/tmp/.../node_modules/pkg). The indexer protects the root via
// ShouldSkipDirIn's workdir-relative check — the rg path must do the same:
// files directly inside the narrowed root are still found, while a nested
// node_modules INSIDE it is still excluded. Regression for ancestor-path
// false positives when rg is handed an absolute path inside a skip tree.
func TestToolSearchNarrowedRootInSkipAncestor(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, "node_modules", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\nfunc Needle() {}\n")
	// A nested node_modules inside the narrowed root is still excluded.
	mkdir("lib/node_modules/dep")
	write("lib/node_modules/dep/x.go", "package dep\nfunc Needle() {}\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Needle"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed root must still surface lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, "node_modules/dep/x.go") {
		t.Fatalf("nested node_modules must still be excluded, got: %q", text)
	}
}

// TestToolFilesNarrowedRootInSkipAncestor is the files-listing twin of
// TestToolSearchNarrowedRootInSkipAncestor: toolFiles (rg --files) with
// the project root narrowed into node_modules/pkg must list lib/sub.go
// but not the nested node_modules content.
func TestToolFilesNarrowedRootInSkipAncestor(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, "node_modules", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\n")
	mkdir("lib/node_modules/dep")
	write("lib/node_modules/dep/x.go", "package dep\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "**", Max: 100})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed root must list lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, "node_modules/dep/x.go") {
		t.Fatalf("nested node_modules must be excluded, got: %q", text)
	}
}

// TestToolSearchNarrowedRootInLocalLibFragment: same narrowed-root
// protection but the ancestor match is a multi-segment skipPathFragments
// entry (.local/lib) — the fragment glob "**/.local/lib" must not blank
// the narrowed root's own tree even though its absolute prefix contains
// /.local/lib/.
func TestToolSearchNarrowedRootInLocalLibFragment(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	base := t.TempDir()
	root := filepath.Join(base, ".local", "lib", "pkg")
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lib")
	write("lib/sub.go", "package lib\nfunc Needle() {}\n")
	mkdir("lib/.cache/x")
	write("lib/.cache/x/c.go", "package c\nfunc Needle() {}\n")

	s, _ := setupToolServerAt(t, root)

	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Needle"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "sub.go") {
		t.Fatalf("narrowed .local/lib root must still surface lib/sub.go, got: %q", text)
	}
	if strings.Contains(text, ".cache/x/c.go") {
		t.Fatalf("nested .cache must be excluded, got: %q", text)
	}
}

// setupToolServerAt is setupToolServer with Workdir pointed at an arbitrary
// root (used by the narrowed-root tests where the root sits inside a skip
// ancestor tree). The DB lives in the same dir; Workdirs=[root].
func setupToolServerAt(t *testing.T, root string) (*Server, string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	s := &Server{
		Workdir:  root,
		Workdirs: []string{root},
		Database: database,
	}
	return s, root
}

// TestToolFilesAnchoredExcludeKeepsProjRoot: a narrowed files search
// (path=subproj) must still evaluate "/"-anchored exclusion globs against
// the PROJECT root, not the subdirectory. exclude_dirs=["./ref"] excludes
// <root>/ref but must leave <root>/subproj/ref searchable — and the output
// paths stay relative to the requested subdirectory.
func TestToolFilesAnchoredExcludeKeepsProjRoot(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	s, root := setupToolServer(t)
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("ref")
	mkdir("subproj/ref")
	write("ref/x.go", "package ref\n")
	write("subproj/ref/keep.go", "package ref\n")

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"./ref"}, 0)

	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Path: "subproj", Pattern: "**/*.go", Max: 50})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "ref/keep.go") {
		t.Fatalf("subproj/ref must NOT be excluded by anchored ./ref rule (rg dir must stay projRoot), got: %q", text)
	}
	if strings.Contains(text, "ref/x.go") {
		t.Fatalf("top-level ref/x.go must be excluded by anchored ./ref rule, got: %q", text)
	}
}

// TestToolFilesBareRuleDirOnly: a bare-name rule (exclude_dirs=["vendor2"])
// excludes the directory <root>/sub/vendor2/ but must not kill a plain
// FILE named vendor2 elsewhere in the tree — the "!<glob>/" gitignore
// directory semantics only match directories. (A file and a directory
// cannot share the same path, so the same-named file lives in a sibling
// dir; the gitignore pattern is name-based either way.)
func TestToolFilesBareRuleDirOnly(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH")
	}
	s, root := setupToolServer(t)
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("sub/vendor2")
	write("sub/vendor2/a.go", "package vendor2\n")
	mkdir("sub2")
	write("sub2/vendor2", "plain file, not a directory\n") // same-named regular file

	t.Cleanup(func() { config.SetRuntimeConfigForTesting(nil, 0) })
	config.SetRuntimeConfigForTesting([]string{"vendor2"}, 0)

	// Search the whole project: the directory's contents are excluded…
	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "**", Max: 100})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	text := textContent(res)
	if strings.Contains(text, "vendor2/a.go") {
		t.Fatalf("dir sub/vendor2 must be excluded by bare rule, got: %q", text)
	}
	// …while the same-named regular file survives (output line is the
	// root-relative path "sub2/vendor2").
	if !strings.Contains(text, "sub2/vendor2") {
		t.Fatalf("plain file named vendor2 must NOT be excluded by a directory rule, got: %q", text)
	}
}
