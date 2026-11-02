package extraction

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dorokuma/codegraph-go/internal/config"
)

// Directory basenames that are never project source for indexing/watching.
// Expanding this list does not remove any MCP tools — it only avoids scanning
// dependency caches and build outputs that blow up cold-start time.
//
// Go module/toolchain trees (HOME/go/pkg, etc.) are handled by isGoToolchainPath
// with path-segment matching so project dirs like "codegraph-go/pkg" stay indexed.
var skipDirNames = map[string]struct{}{
	".git": {}, "node_modules": {}, "vendor": {}, ".codegraph": {},
	"dist": {}, "build": {}, "out": {}, "coverage": {},
	"__pycache__": {}, ".venv": {}, "venv": {}, "target": {},
	".gradle": {}, ".cargo": {}, ".rustup": {}, ".npm": {}, ".nvm": {},
	".cache": {}, ".pub-cache": {}, ".dartServer": {}, ".android": {},
	".tox": {}, ".mypy_cache": {}, ".pytest_cache": {}, ".ruff_cache": {},
	".next": {}, ".nuxt": {}, ".turbo": {}, ".yarn": {}, "bower_components": {},
	"site-packages": {}, ".eggs": {}, ".idea": {}, ".terraform": {},
	"Pods": {}, "Carthage": {}, ".bundle": {}, "DerivedData": {},
	// .wt is the git-worktree container dir used by dispatched sub-agents
	// (/root/workspace/.wt/<repo>/<branch>); each child worktree is a
	// duplicate of a project that is already indexed under the workspace
	// root, so walking it again doubles cold-start cost for zero new code.
	".wt": {},
}

// Path fragments (slash-normalized) that mark cache/toolchain trees.
// Matched as substring of the absolute path so nested layouts are covered.
// Do NOT put "/go/pkg/" here — it false-positives on projects named "*-go/pkg".
var skipPathFragments = []string{
	"/.gradle/",
	"/.cargo/registry/",
	"/.cargo/git/",
	"/.rustup/",
	"/.npm/",
	"/.nvm/",
	"/.cache/",
	"/.pub-cache/",
	"/node_modules/",
	"/.local/share/uv/",
	"/.local/share/virtualenv/",
	"/.local/lib/",
	"/.local/share/",
	"/Library/Caches/",
	"/.gradle/caches/",
	"/AppData/Local/Go/",
	"/AppData/Local/npm-cache/",
	// Bulk non-source dumps often present under home when ~ is indexed by mistake
	"/code_references/",
	"/gopath/pkg/",
	"/gopath/src/golang.org/",
}

// Markers that mean "this top-level folder is a real project" when workdir is $HOME.
var projectMarkers = []string{
	".git",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"composer.json",
	"Gemfile",
	"mix.exs",
	"CMakeLists.txt",
	"Makefile",
	"Cargo.lock",
	"pnpm-workspace.yaml",
	"setup.py",
	"requirements.txt",
	"Pipfile",
	"build.sbt",
	"Package.swift",
}

// isGoToolchainPath reports GOPATH / module-cache style trees:
//
//	.../go/pkg/...   .../go/bin/...   .../go/src/...
//
// Uses path segments so "codegraph-go/pkg" is NOT treated as GOPATH.
func isGoToolchainPath(slash string) bool {
	parts := strings.Split(slash, "/")
	for i := 0; i < len(parts); i++ {
		if parts[i] != "go" {
			continue
		}
		if i+1 >= len(parts) {
			// path ends at .../go — skip only known GOPATH roots, not
			// project dirs named "go" (sdk/go, internal/go, clients/go).
			return isGopathRoot(slash)
		}
		next := parts[i+1]
		if next == "pkg" || next == "bin" || next == "src" {
			return true
		}
	}
	return false
}

// isGopathRoot reports whether slash-normalized path is a GOPATH workspace
// root: /root/go, $HOME/go, /home/<user>/go, or /Users/<user>/go.
func isGopathRoot(slash string) bool {
	slash = strings.TrimSuffix(slash, "/")
	if slash == "/root/go" {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if slash == filepath.ToSlash(filepath.Join(home, "go")) {
			return true
		}
	}
	parts := strings.Split(slash, "/")
	// "/home/u/go" → ["", "home", "u", "go"]
	if len(parts) == 4 && (parts[1] == "home" || parts[1] == "Users") && parts[2] != "" && parts[3] == "go" {
		return true
	}
	return false
}

// HasProjectMarker reports whether dir looks like a software project root.
func HasProjectMarker(dir string) bool {
	for _, m := range projectMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// IsBroadWorkdir is true for $HOME and multi-user roots where a full recursive
// index would suck in toolchains and caches. Callers use home-mode filtering.
func IsBroadWorkdir(workdir string) bool {
	if workdir == "" {
		return false
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return true
	}
	// Multi-user roots (not a single user's home)
	slash := filepath.ToSlash(abs)
	if slash == "/home" || slash == "/Users" {
		return true
	}
	home, err := os.UserHomeDir()
	if err == nil && filepath.Clean(home) == abs {
		return true
	}
	// Explicit server home
	if slash == "/root" {
		return true
	}
	return false
}

// homeIndexAll enables indexing every non-cache top-level dir under $HOME.
// Default is off: only project-like top-level dirs are entered.
func homeIndexAll() bool {
	return config.HomeIndexAll()
}

// NarrowExcludedWorkdirs is the single source of truth for the startup
// workdir narrowing: when the session target — an explicit -workdir the
// user passed (explicitWorkdir, takes precedence) or the process cwd —
// sits inside a configured workdir's subtree that the parent root
// refuses to index (a built-in skip name like .wt, an exclude_dirs rule,
// or a home-mode non-project top level), the parent workdir's index is
// blind to that subtree by construction. Returning []string{target}
// shrinks the workdir list to the target itself so the session indexes
// its own tree instead of a blind parent index — and does NOT keep the
// parent as a secondary (ExtraDBs) workdir.
//
// The rule semantics are NOT re-implemented here: every intermediate
// path segment between the workdir root and the target is checked with
// ShouldSkipDirIn(wdAbs, segPath, basename) — the same function the
// indexer's walker applies — so any layer the walk would skip triggers
// the narrowing. ShouldSkipDirIn's root protection makes the check
// safe: a target equal to the workdir (or reached through a protected
// intermediate) never narrows.
//
// explicitWorkdir == "" means the caller did not pass -workdir, so cwd
// is the target; when cwd is itself a configured workdir nothing
// narrows. An explicit target equal to a workdir root (or outside every
// configured root) likewise leaves the list untouched. workdirs is
// returned unchanged when the target is empty or the list is empty.
func NarrowExcludedWorkdirs(explicitWorkdir, cwd string, workdirs []string) []string {
	target := explicitWorkdir
	if target == "" {
		target = cwd
	}
	if target == "" || len(workdirs) == 0 {
		return workdirs
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return workdirs
	}
	targetAbs = filepath.Clean(targetAbs)
	for _, wd := range workdirs {
		wdAbs, err := filepath.Abs(wd)
		if err != nil {
			continue
		}
		wdAbs = filepath.Clean(wdAbs)
		if wdAbs == targetAbs {
			continue // target IS a configured workdir: nothing to narrow
		}
		rel, err := filepath.Rel(wdAbs, targetAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		segPath := wdAbs
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if seg == "" || seg == "." {
				continue
			}
			segPath = filepath.Join(segPath, seg)
			if ShouldSkipDirIn(wdAbs, segPath, filepath.Base(segPath)) {
				return []string{target}
			}
		}
	}
	return workdirs
}

// ShouldSkipDir reports whether a directory should be skipped during walk.
// name is the base name; path is the full path (absolute or relative).
// Prefer ShouldSkipDirIn when the workspace root is known (enables home-mode).
func ShouldSkipDir(path, name string) bool {
	return ShouldSkipDirIn("", path, name)
}

// normalizeExcludeRule is the single source of truth both the walker
// (ShouldSkipDirIn) and the rg translation (RgExcludeGlobs) read a raw
// exclude_dirs rule through — they must never apply two different
// trim/anchor routines to the same string or the two engines silently
// disagree about what a rule means ("a//" matching on the rg side but
// never on the walker side was the drift this helper eliminates).
//
// It returns the normalized pattern (a slash-cleaned rel path or bare
// name), whether the rule is anchored to the workdir root ("./sub" or
// "/sub"), and ok=false when the rule names nothing (blank, a bare
// "/" / "./" / "///" anchor, or the . / .. segments themselves —
// emitting "." would exclude the search root and ".." its parent;
// a legal name like "..foo" is untouched).
//
// Normalization, in order: TrimSpace + ToSlash, detect the leading
// "./" or "/" anchor, strip the anchor plus every remaining leading
// '/' ("//", ".//a"), then collapse every trailing '/' ("a//" → "a").
// anchored is decided on the raw rule — "./a//" stays anchored even
// after the trailing run is stripped.
func normalizeExcludeRule(raw string) (pat string, anchored bool, ok bool) {
	pat = filepath.ToSlash(strings.TrimSpace(raw))
	anchored = strings.HasPrefix(pat, "./") || strings.HasPrefix(pat, "/")
	if anchored {
		// Strip "./" once, then every remaining leading '/': "///" and
		// ".//a" both carry an anchor prefix, and a leftover '/' would
		// otherwise produce a "//" glob — which rg 14.1.1 silently
		// treats as NEVER matching (measured: `!//a/` leaves `a/`
		// listed) while the walker would read it differently.
		pat = strings.TrimLeft(strings.TrimPrefix(pat, "./"), "/")
	}
	// Repeated trailing '/' collapses the same way — "a//" names "a".
	pat = strings.TrimRight(pat, "/")
	if pat == "" || pat == "." || pat == ".." {
		return "", false, false
	}
	return pat, anchored, true
}

// ShouldSkipDirIn is like ShouldSkipDir but applies home-mode rules when
// workdir is $HOME / broad: only descend into top-level dirs that look like
// projects (go.mod, package.json, .git, …), unless CODEGRAPH_GO_HOME_INDEX_ALL=1.
//
// With a known workdir it also applies the user-configured exclusions
// (config.yaml exclude_dirs / CODEGRAPH_EXCLUDE_DIRS): bare names and globs
// match the directory's base name; patterns containing '/' match the
// workdir-relative slash path; patterns anchored with a leading "./" or "/"
// (e.g. "./ref") pin the exclusion to <workdir>/<sub> and its subtree only
// instead of hitting a same-named deep directory. The workdir root itself is
// never skipped, even when its own base name matches a rule (an agent may
// point the workdir at a single worktree).
//
// This lets users keep workdir=/root (their real workspace) without indexing
// GOPATH, package caches, random dumps, and dotfile trees.
func ShouldSkipDirIn(workdir, path, name string) bool {
	if name == "" {
		name = filepath.Base(path)
	}
	// Normalize workdir and path to absolute + clean once so relative
	// comparisons (root protection, exclude_dirs rel rules) are consistent
	// even when callers hand us "." or mixed relative/absolute paths.
	var workdirAbs, pathAbs string
	if workdir != "" {
		wAbs, err := filepath.Abs(workdir)
		if err != nil {
			wAbs = workdir
		}
		workdirAbs = filepath.Clean(wAbs)
		pAbs, err := filepath.Abs(path)
		if err != nil {
			pAbs = path
		}
		pathAbs = filepath.Clean(pAbs)
	}
	// Root protection: the workdir root itself is never skipped. An agent
	// may legitimately point workdir at a single worktree whose own name
	// is listed in skipDirNames or matches an exclude_dirs rule — skipping
	// the root would blank-index the whole project.
	if workdir != "" && pathAbs == workdirAbs {
		return false
	}
	if _, ok := skipDirNames[name]; ok {
		return true
	}
	// User-configured exclusions (config.yaml exclude_dirs +
	// CODEGRAPH_EXCLUDE_DIRS). Each rule is either a bare directory name /
	// glob matched against the base name, or a workdir-relative slash
	// path matched against the path's rel from the workdir root.
	for _, raw := range config.ExcludeDirs() {
		// Same normalizeExcludeRule the rg translation consumes — one
		// rule, one interpretation on both engines.
		pat, anchored, ok := normalizeExcludeRule(raw)
		if !ok {
			continue
		}
		if !anchored && !strings.Contains(pat, "/") {
			// Bare name / glob: matched against the basename only.
			if name == pat {
				return true
			}
			if ok, err := filepath.Match(pat, name); err == nil && ok {
				return true
			}
			continue
		}
		// Anchored ("./sub", "/sub") or slash-containing path rule:
		// matched against the workdir-relative slash path — the exact
		// allowSlash = anchored || strings.Contains(pat, "/") shape
		// RgExcludeGlobs applies, so the two engines classify a rule
		// identically. An anchored "./ref" pins the exclusion to
		// <workdir>/ref and its subtree only — a bare "ref" would also
		// hit nested dirs like <workdir>/proj/internal/ref.
		if workdirAbs == "" {
			continue
		}
		rel, err := filepath.Rel(workdirAbs, pathAbs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		if rel == pat || strings.HasPrefix(rel, pat+"/") {
			return true
		}
		if ok, mErr := filepath.Match(pat, rel); mErr == nil && ok {
			return true
		}
	}
	// Toolchain / cache path-fragment checks run on the workdir-RELATIVE
	// path when the target sits inside the workdir: a narrowed workdir
	// root like /root/go/pkg/mod/foo or /ws/node_modules/pkg must not
	// get its normal subdirs rejected just because an ANCESTOR segment
	// happened to contain "/go/pkg/" or "/node_modules/" — the parent
	// root's walk was already responsible for that exclusion. When
	// workdir is empty (ShouldSkipDir) or the path escapes it, the full
	// absolute path is checked as before.
	checkTarget := ""
	if workdirAbs != "" {
		if rel, err := filepath.Rel(workdirAbs, pathAbs); err == nil &&
			rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			checkTarget = "/" + strings.Trim(filepath.ToSlash(rel), "/")
			if isGoToolchainPath(checkTarget) ||
				(rel == "go" && isGopathRoot(filepath.ToSlash(pathAbs))) {
				return true
			}
			check := checkTarget + "/"
			for _, frag := range skipPathFragments {
				if strings.Contains(check, frag) {
					return true
				}
			}
		}
	}
	if checkTarget == "" {
		slash := filepath.ToSlash(path)
		if !strings.HasPrefix(slash, "/") && !strings.Contains(slash, ":/") {
			slash = "/" + slash
		}
		trimmed := strings.TrimSuffix(slash, "/")
		if isGoToolchainPath(trimmed) {
			return true
		}
		check := slash
		if !strings.HasSuffix(check, "/") {
			check += "/"
		}
		for _, frag := range skipPathFragments {
			if strings.Contains(check, frag) {
				return true
			}
		}
	}

	// Home / broad workdir: only index project-like top-level folders.
	if workdirAbs != "" && IsBroadWorkdir(workdir) && !homeIndexAll() {
		rel, err := filepath.Rel(workdirAbs, pathAbs)
		if err == nil {
			rel = filepath.ToSlash(rel)
			// Direct child of workdir only (top-level entry).
			if rel != "." && !strings.Contains(rel, "/") {
				// Hidden top-level dirs under home are almost never project roots.
				if strings.HasPrefix(name, ".") {
					return true
				}
				if !HasProjectMarker(pathAbs) {
					return true
				}
			}
		}
	}
	return false
}

// RgExcludeGlob translates ONE user-configured directory-exclusion rule
// (a config.yaml exclude_dirs / CODEGRAPH_EXCLUDE_DIRS entry) into the glob
// string the rg negative-glob pair "!\<glob>/" + "!\<glob>/**" consumes, so
// the rg backstop and the indexer's walker (ShouldSkipDirIn) agree on what a
// rule means, and NO rule can ever crash rg.
//
// ok=false means "emit nothing for this rule" (blank entry, a bare "/" or
// "./" anchor with no path behind it). The shape decision mirrors the
// walker: an entry anchored with "./" or "/" — and any entry containing a
// separator — becomes a search-root-anchored "/a/b" glob, a bare entry stays
// unanchored so it matches a same-named directory at any depth.
//
// The escaping half is the must-fix. The walker interprets a rule with
// filepath.Match plus an exact-name comparison; rg feeds the raw string to
// its gitignore globset, and the two engines do not agree:
//
//   - "{foo,bar}" is ONE literal directory name for filepath.Match but an
//     alternation for rg, so rg excluded foo/ and bar/ while the indexer kept
//     walking them (and the literal {foo,bar} dir stayed searchable in rg);
//   - "data[" is an unclosed character class rg refuses outright
//     ("error parsing glob … unclosed character class", exit 2 — the whole
//     search tool fails), while filepath.Match just reports ErrBadPattern and
//     the walker still matches the LITERAL name via the exact-name rule.
//
// rgSafeGlob below resolves both, so a rule is either honoured identically or
// degrades to a literal directory name — never to a rg parse error.
func RgExcludeGlob(pattern string) (string, bool) {
	globs := RgExcludeGlobs(pattern)
	if len(globs) == 0 {
		return "", false
	}
	return globs[0], true
}

// RgExcludeGlobs is the multi-glob form of RgExcludeGlob: one rule can need
// BOTH a wildcard translation and a literal translation to reproduce the
// walker's "exact-name OR filepath.Match" dual semantics. A rule like
// `\{foo` or `[ab]` is a valid Go pattern whose own literal directory name
// does NOT satisfy the pattern — ShouldSkipDirIn still excludes it through
// the `name == pat` / `rel == sub` exact comparison, so rg needs the literal
// glob too. Both forms are deduplicated and returned in deterministic order
// (wildcard translation first, literal second); the caller emits a
// "!<g>/" + "!<g>/**" pair per entry.
func RgExcludeGlobs(pattern string) []string {
	// normalizeExcludeRule is the shared normalization the walker uses —
	// there must not be a second hand-rolled trim/anchor pass here or the
	// rg side and the walker side silently diverge (oracle-cg-r5).
	pat, anchored, ok := normalizeExcludeRule(pattern)
	if !ok {
		return nil
	}
	// Escaping never adds, removes or rewrites a "/", so the separator test
	// is valid on the raw rule. allowSlash routes the class translator:
	// a bare name (no '/' anywhere) is matched against basenames only —
	// filepath.Match can never see a '/' there, so the emitted glob must
	// refuse '/' too; a path/anchored rule is matched against the rel path
	// where filepath.Match DOES let classes span '/'. The two engines must
	// be told apart or a bare `cmd[^x]codegraph-go` would silently exclude
	// the two-level `cmd/codegraph-go` for rg while the walker keeps it.
	allowSlash := anchored || strings.Contains(pat, "/")
	g, hasWild := rgSafeGlob(pat, allowSlash)
	var out []string
	if hasWild {
		if !allowSlash && strings.Contains(g, "/") {
			// The wildcard translation itself now carries a '/' (a negated
			// class expanded to [!x/] so it refuses the separator). A bare
			// glob containing '/' is root-anchored for rg's gitignore
			// engine — "**/" must lead it to keep matching at any depth.
			out = append(out, "**/"+g)
		} else if allowSlash {
			out = append(out, "/"+g)
		} else {
			out = append(out, g)
		}
	}
	// filepath.Match accepted the rule but the literal name does not match
	// the pattern (a backslash escape `\{foo`, a char class `[ab]`, an
	// unsatisfiable class like `[z-a]`, a `?`/`*` the literal name never
	// satisfies): the walker still excludes that exact name, so rg needs
	// the literal translation alongside (or instead of) the wildcard one.
	needLiteral := false
	if isValidGoPattern(pat) {
		if ok, mErr := filepath.Match(pat, pat); mErr == nil && !ok {
			needLiteral = true
		}
	}
	if needLiteral {
		var lit string
		if allowSlash {
			lit = "/" + fixRgTrailingEscape(escapeRgLiteral(pat))
		} else {
			lit = fixRgTrailingEscape(escapeRgLiteral(pat))
		}
		if len(out) == 0 || out[0] != lit {
			out = append(out, lit)
		}
	}
	return out
}

// rgSafeGlob rewrites a single rule so ripgrep's glob engine means what
// ShouldSkipDirIn means (see RgExcludeGlob). satisfiable=false means the
// wildcard side denotes NOTHING (an unsatisfiable positive class such as
// `[z-a]` — Go silently treats `z-a` as an empty range while rg's globset
// rejects the whole glob with "invalid range; 'z' > 'a'", exit 2); the
// caller then emits only the literal form when the walker's exact-name
// branch still excludes the literal directory.
func rgSafeGlob(pat string, allowSlash bool) (glob string, satisfiable bool) {
	// filepath.Match is the interpreter the walker actually uses. A pattern
	// it refuses (ErrBadPattern — unclosed "[", bad range, dangling escape)
	// can only ever match LITERALLY there, through the walker's exact-name
	// rule; rg would reject the same pattern outright and take the whole tool
	// call down with it. So every metacharacter — INCLUDING "\" itself —
	// loses its meaning for rg too.
	//
	// filepath.Match(pat, "") cannot be used as the validity oracle here:
	// it short-circuits at the FIRST '*' chunk whose prefix does not match
	// the empty name, so a malformed tail like `foo*\`, `foo*[` or
	// `foo*[a-` reports (false, nil) — no error — while the same malformed
	// tail against a matching candidate name DOES return ErrBadPattern.
	// isValidGoPattern scans every '*' chunk so no suffix error slips in.
	if !isValidGoPattern(pat) {
		return fixRgTrailingEscape(escapeRgLiteral(pat)), true
	}
	// Interpreter-compatible pattern: translate token by token under Go
	// filepath.Match (Unix) lexing so the resulting rg glob denotes exactly
	// the same strings.
	return translateRgGlob(pat, allowSlash)
}

// isValidGoPattern reports whether pat is a well-formed Go filepath.Match
// pattern: every chunk scanChunk would hand to matchChunk must parse with
// no error. filepath.Match(pat, "") alone is NOT a safe oracle — its
// star-skip loop returns (false, nil) at the first non-empty chunk that
// fails to match the empty name, so a syntax error sitting in a LATER
// chunk (`foo*\`, `foo*[`, `foo*[a-`, `a*b\`) is never seen. Walking the
// chunk boundaries ourselves (leading '*' run stripped, '\' consumes the
// next byte, '*' outside a '['…']' class splits chunks) lets the tail be
// validated too — a malformed chunk is only legal for the walker when it
// is matched literally, never as a pattern.
func isValidGoPattern(pat string) bool {
	rest := pat
	for len(rest) > 0 {
		// Strip the leading star run exactly like scanChunk does.
		for len(rest) > 0 && rest[0] == '*' {
			rest = rest[1:]
		}
		// Find the chunk end: the next '*' outside a character class.
		inrange := false
		i := 0
		for i < len(rest) {
			switch rest[i] {
			case '\\':
				// '\' consumes the next byte — the chunk syntax check
				// happens in matchChunk, not here, but the byte still
				// must be skipped so an escaped '*' or ']' does not end
				// the chunk early.
				if i+1 < len(rest) {
					i++
				}
			case '[':
				inrange = true
			case ']':
				inrange = false
			case '*':
				if !inrange {
					goto chunkEnd
				}
			}
			i++
		}
	chunkEnd:
		chunk := rest[:i]
		rest = rest[i:]
		if chunk == "" {
			// Trailing '*'-only tail: nothing left to validate.
			return len(rest) == 0
		}
		// filepath.Match on a star-free single chunk is exactly
		// matchChunk — the syntax pass runs even on a failed match,
		// so a malformed class / dangling escape still reports
		// ErrBadPattern against the empty name.
		if _, err := filepath.Match(chunk, ""); err != nil {
			return false
		}
		// rest[0] == '*': loop back to strip the next star run.
	}
	return true
}

// escapeRgLiteral makes the whole string literal for rg's globset.
// Every glob metacharacter is neutralised:
//
//   - `\` becomes the class `[\\]` — a class containing the single
//     escaped backslash. A bare backslash CANNOT be emitted in the
//     "!\<glob>/" pair: a trailing `\` swallows the appended "/" and
//     rg exits 2 with "dangling '\\'", while an escaped "\\" before
//     that "/" still leaves the run EVEN — the one case
//     fixRgTrailingEscape cannot pad around. The class form is immune.
//   - `*`, `?`, `[`, `]` take a plain backslash escape (`\[` is a
//     literal "[" — a DOUBLE backslash there would feed the "[" to the
//     class parser: `foo\\[bar` → "unclosed character class", exit 2).
//   - `{`, `}` become the classes `[{]` / `[}]` — rg's brace expansion
//     runs before escape resolution, so `\{` still unbalances an
//     alternate group ("unclosed alternate group", exit 2) while a
//     class form is never seen by the brace lexer.
func escapeRgLiteral(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`[\\]`)
		case '{':
			b.WriteString(`[{]`)
		case '}':
			b.WriteString(`[}]`)
		case '*', '?', '[', ']':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// translateRgGlob converts a Go filepath.Match-valid pattern into the rg
// glob denoting the same strings, scanning with Go's lexer so semantics
// are preserved token by token rather than byte by byte. satisfiable=false
// means the pattern denotes no string at all (an unsatisfiable positive
// class) and the caller must not emit the wildcard form.
//
//   - `\c` (Go escape, always a complete pair — the pattern parsed):
//     `\\` becomes the class `[\\]` (a bare backslash cannot survive
//     the "!\<glob>/" form — "dangling '\\'", exit 2 — and would
//     escape the next char), `\{`/`\}` become `[{]`/`[}]` (rg brace
//     expansion runs BEFORE escape resolution, so `\{` still unbalances
//     an alternate group → exit 2), `\*` `\?` `\[` `\]` keep the `\c`
//     escape, and an ordinary `\c` is emitted verbatim (both engines
//     read it as the literal char).
//   - `**` (or a longer star run) folds to `*`: Go has no globstar, so
//     `a/**/b` matches only `a/<one-seg>/b`; rg's `*` means exactly the
//     same single segment, while rg's `**` would ALSO match `a/b` and
//     `a/x/y/b` — an over-exclusion the walker never applies.
//   - `[...]` classes are scanned by parseGoClass with Go class lexing
//     (`\c` escape pairs consumed as units, `^` in first position is
//     negation), normalized by normalizeRgRangeEndpoints, then emitted
//     by emitRgClass under rg's POSIX-ish class rules (all measured
//     against ripgrep 14.1.1): in-class `\` is a LITERAL char (ASCII
//     92), never an escape; a reversed range `lo-hi` with lo > hi is
//     dropped (Go silently empty, rg "invalid range" → exit 2), an
//     empty positive class is dropped (the pattern is unsatisfiable)
//     and an empty negated class `[^...]` compiles to `?` — or to
//     `{?,[/]}` at an allowSlash mid-segment position, where Go lets
//     the empty class match '/' itself while rg's `?` refuses it; a
//     literal
//     ']' is emitted first, '!' and '^' are kept off the first slot
//     (or the whole class collapses to `\!` / `\^` / `{\!,\^}` when
//     nothing else can lead it), and '-' lands first or last; a range
//     spanning '/' is kept whole (both engines let class ranges match
//     '/' — filepath.Match(`[\--0]`, "/") is true); in-class
//     `{`, `}` and ',' need no escape at all because rg's brace
//     expansion does not see class contents.
//   - unescaped `{`/`}` outside classes are escaped — rg expands braces,
//     filepath.Match does not.
func translateRgGlob(pat string, allowSlash bool) (string, bool) {
	var b strings.Builder
	b.Grow(len(pat) + 8)
	for i := 0; i < len(pat); {
		switch c := pat[i]; c {
		case '\\':
			// Go escape pair: i+1 < len(pat) normally holds (a dangling escape
			// would have failed isValidGoPattern), but the check stays
			// defensive — a malformed input must never read out of bounds.
			//   - `\\` (literal backslash) → class `[\\]`: a bare "\\" would
			//     escape the NEXT char in rg, and at end of the glob the
			//     "!\<glob>/" form cannot hold a trailing escape at all
			//     ("dangling '\\'", exit 2).
			//   - `\{` / `\}` → classes `[{]` / `[}]`: `\{` is NOT an escape
			//     for rg — brace expansion runs first and still counts it as
			//     an alternation opener (exit 2).
			//   - `\*` `\?` `\[` `\]` keep the `\c` escape (rg literal).
			//   - any other `\c`: Go matches the bare char, rg's `\c` also
			//     matches the bare char — keep it.
			if i+1 >= len(pat) {
				// Defensive: isValidGoPattern should have caught this. Emit
				// the bare char so the output is still a valid rg glob.
				b.WriteByte('\\')
				i++
				continue
			}
			switch n := pat[i+1]; n {
			case '\\':
				b.WriteString(`[\\]`)
			case '{':
				b.WriteString(`[{]`)
			case '}':
				b.WriteString(`[}]`)
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			i += 2
		case '*':
			j := i
			for j < len(pat) && pat[j] == '*' {
				j++
			}
			b.WriteByte('*') // collapse the run: Go has no globstar
			i = j
		case '[':
			start := i // an empty negated class below needs the position
			neg, singles, ranges, next := parseGoClass(pat, i, allowSlash)
			i = next
			// Bare-name rule (allowSlash=false): filepath.Match only ever
			// sees a basename, so a negated class can never match '/'.
			// rg's globset would let `[!x]` span the separator and kill a
			// two-level `cmd/codegraph-go` — measured rg 14.1.1. Inject '/'
			// into the excluded member set (`[!x]` → `[!x/]`) so the emitted
			// glob refuses '/' too. The caller then adds the "**/" prefix
			// because the glob itself now carries a '/' (a bare glob with
			// '/' is root-anchored in rg's gitignore engine). An empty
			// negated class still compiles to `?` below — `?` never matches
			// '/' either, so nothing is injected there.
			if neg && !allowSlash && (len(singles) > 0 || len(ranges) > 0) {
				singles = append(singles, '/')
			}
			if ok := emitRgClass(&b, neg, singles, ranges); !ok {
				if neg {
					// Go semantics: a negated class with no members (e.g.
					// `[^z-a]`, the z-a range being silently empty for Go)
					// matches ANY single char — '/' included
					// (filepath.Match(`par[^z-a]b`, "par/b") is true).
					// On the rel-path branch (allowSlash) a class NOT
					// adjacent to '/' can therefore legitimately sit on the
					// separator, while rg's `?` refuses it — measured rg
					// 14.1.1: `!/par?b/` leaves `par/b/` listed. Emit the
					// alternation `{?,[/]}` there (the `[/]` arm covers
					// exactly the one-char path the walker also matches);
					// the position check keeps the `[/]` arm away from an
					// existing '/' or the pattern's edges, where it could
					// only produce "//" or match past the end of the rel
					// string. The alternation lands inside the normalized
					// glob (always anchored or "**/"-prefixed by the
					// caller), never at the start of a "!"-prefixed user
					// glob, so rg's leading-"!" alternation restriction
					// cannot bite. Everywhere else `?` is exact — it never
					// matches '/' either.
					if allowSlash && start > 0 && pat[start-1] != '/' &&
						i < len(pat) && pat[i] != '/' {
						b.WriteString(`{?,[/]}`)
					} else {
						b.WriteByte('?')
					}
					continue
				}
				// Unsatisfiable positive class ([z-a], foo[z-a]bar): the
				// pattern matches nothing — emitting any class syntax would
				// crash rg (invalid range) or match chars Go never matches.
				return "", false
			}
		case '{', '}':
			// Outside a class an unescaped brace is alternation syntax for
			// rg. `\{` is NOT a safe escape — brace expansion runs before
			// escape resolution, so it still counts as an alternation
			// opener ("unclosed alternate group", exit 2). The class form
			// is literal by construction.
			if c == '{' {
				b.WriteString(`[{]`)
			} else {
				b.WriteString(`[}]`)
			}
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), true
}

// parseGoClass parses the `[...]` class starting at pat[i]=='[' under Go
// filepath.Match class lexing (matchChunk + getEsc: on Unix a backslash
// escapes the next UTF-8 rune, a leading '^' negates, and a ']' with no
// members seen yet still terminates the class — there is no POSIX
// "literal ] in first position" rule in Go). It returns the negation flag,
// the deduplicated member runes (singles), and the surviving lo<hi ranges
// normalized for rg's globset:
//
//   - `lo-hi` with lo > hi (e.g. `z-a`, `a-\]`): Go silently treats it as
//     an EMPTY range (filepath.Match("[z-a]","") is no error) while rg
//     aborts the whole call ("invalid range; 'z' > 'a'", exit 2). Go
//     semantics win: the range is dropped.
//   - `lo == hi` folds into singles.
//   - '/' membership depends on allowSlash: the walker's bare-name branch
//     feeds filepath.Match a basename (never contains '/'), but its
//     slash-path branch feeds the whole rel path where a class CAN hit
//     '/' — Go's getEsc does not exclude it (measured:
//     filepath.Match(`a[/]b`, "a/b") is true) and rg's globset behaves
//     identically (rg 14.1.1: `a[/]b` and `a[a/]b` exclude `a/b` only).
//   - ']' / '-' / '!' / '^' sitting at a range endpoint are moved into
//     singles and the range shrunk, because each is position-sensitive for
//     rg's POSIX-ish class lexer: ']' closes the class, 'lo -' would
//     re-attach as a range start, and '!'/'^' first would flip the
//     negation bit.
//
// allowSlash=false additionally strips '/' out of the emitted member set:
// a bare-name rule is matched against basenames only, so a negated class
// like `cmd[^x]codegraph-go` must NOT be allowed to match the two-level
// path `cmd/codegraph-go` (rg's `[!x]` DOES span '/'; measured rg
// 14.1.1). The fix is to add '/' to the negated class's member set —
// `[!x/]` then refuses the separator — and to split any positive range
// that crosses '/' at the separator so the emitted class can never reach
// it.
func parseGoClass(pat string, i int, allowSlash bool) (neg bool, singles []rune, ranges [][2]rune, next int) {
	body := pat[i+1:]
	consumed := 1 // bytes of pat consumed so far (the '[')
	// Go (matchChunk): a leading '^' negates; everything else is a member.
	if strings.HasPrefix(body, "^") {
		neg = true
		body = body[1:]
		consumed++
	}
	seen := map[rune]bool{}
	addSingle := func(r rune) {
		// Under allowSlash=false '/' can never be a class member — the
		// basename filepath.Match sees never contains one. Under
		// allowSlash=true it IS a legal member: Go's getEsc accepts it
		// and rg's globset honours it the same way.
		if (!allowSlash && r == '/') || r == utf8.RuneError || seen[r] {
			return
		}
		seen[r] = true
		singles = append(singles, r)
	}
	// goClassRune decodes one class member: Go's getEsc consumes '\\' +
	// the next rune; anything else is the rune itself. The pattern is
	// already known valid (filepath.Match accepted it), so escapes are
	// always complete and never hit a raw '/'.
	goClassRune := func(s string, j int) (rune, int) {
		if s[j] == '\\' {
			r, sz := utf8.DecodeRuneInString(s[j+1:])
			return r, j + 1 + sz
		}
		r, sz := utf8.DecodeRuneInString(s[j:])
		return r, j + sz
	}
	for j := 0; j < len(body); {
		if body[j] == ']' {
			return neg, singles, ranges, i + consumed + j + 1
		}
		lo, j2 := goClassRune(body, j)
		j = j2
		// Go's matchChunk reads the class member by member: after the lo
		// rune it asks `chunk[0] == '-' && getEsc(chunk[1:])` — a '-'
		// becomes a range connector only when ANOTHER member follows it
		// (a trailing '-' before ']' or a dangling escape stays literal).
		if j < len(body) && body[j] == '-' && j+1 < len(body) && body[j+1] != ']' &&
			!(body[j+1] == '\\' && j+2 >= len(body)) {
			hi, j3 := goClassRune(body, j+1)
			j = j3
			switch {
			case lo > hi:
				// Go: silently empty. rg: "invalid range", exit 2. Drop it.
			case lo == hi:
				addSingle(lo)
			default:
				// Go class ranges CAN match '/' (matchChunk's range check is
				// `lo <= r && r <= hi` with no '/' exclusion — measured:
				// filepath.Match(`[\--0]`, "/") == true because the range
				// '-'..'0' spans it). rg's class ranges behave the same way
				// — but only when the walker is comparing a rel path
				// (allowSlash=true). For a bare-name rule the basename can
				// never be '/', so the range must be split at it: drop the
				// sub-range that contains '/' and keep the two sides
				// ([lo,'.'] and ['0',hi]) — `[\--0]` then emits `[.0-]`,
				// refusing '/' exactly like the walker does.
				if !allowSlash && lo <= '/' && '/' <= hi {
					if lo <= '/'-1 {
						ranges = append(ranges, [2]rune{lo, '/' - 1})
					}
					if '/'+1 <= hi {
						ranges = append(ranges, [2]rune{'/' + 1, hi})
					}
				} else {
					ranges = append(ranges, [2]rune{lo, hi})
				}
			}
			continue
		}
		addSingle(lo)
	}
	// Class is known to close (filepath.Match accepted the pattern); an
	// unterminated scan just means the input was not a valid class — keep
	// the index at the pattern end, which makes the caller emit what was
	// already written (degenerate, but unreachable in practice).
	return neg, singles, ranges, len(pat)
}

// normalizeRgRangeEndpoints peels rg's position-sensitive characters off
// the endpoints of every range: ']' would close the class early, a '-' at
// hi would look like the start of another range, '!'/'^' at lo can
// re-negate. The peeled char is moved into singles; the range shrinks by
// one (dropping into a single when lo==hi, disappearing when it empties).
// Runs until no range carries a sensitive endpoint, keeping every range
// strictly lo<hi with endpoints free of ']','-','!','^'. '/' is NOT
// peeled — it is a legitimate member/endpoint under allowSlash=true, and
// under allowSlash=false the ranges were already split at '/' upstream.
func normalizeRgRangeEndpoints(singles *[]rune, ranges *[][2]rune) {
	addSingle := func(r rune) {
		for _, s := range *singles {
			if s == r {
				return
			}
		}
		*singles = append(*singles, r)
	}
	endpoint := func(r rune) bool {
		return r == ']' || r == '-' || r == '!' || r == '^'
	}
	out := (*ranges)[:0]
	for _, rg := range *ranges {
		lo, hi := rg[0], rg[1]
		for lo < hi && (endpoint(lo) || endpoint(hi)) {
			if endpoint(lo) {
				addSingle(lo)
				lo++
			} else {
				addSingle(hi)
				hi--
			}
		}
		switch {
		case lo > hi:
			// emptied entirely
		case lo == hi:
			addSingle(lo)
		default:
			out = append(out, [2]rune{lo, hi})
		}
	}
	*ranges = out
}

// emitRgClass writes the normalized `[...]` class for rg's globset from
// the members parseGoClass produced. It returns false when the class has
// no emit form in class syntax: an EMPTY positive class (the caller then
// fails the whole pattern) and an EMPTY negated class (which means "any
// single char" for Go — '/' included — the caller emits `?`, or
// `{?,[/]}` where a '/' is genuinely reachable).
//
// rg's class lexer is POSIX-ish and differs from Go's in ways that shaped
// this emitter (all measured against ripgrep 14.1.1):
//   - '\' inside a class is a LITERAL char (ASCII 92), never an escape —
//     it is written like any other member (the old `\!`/`\]` in-class
//     escapes silently added a real backslash to the member set);
//   - a ']' first (after '[' or '[!') is a literal member;
//   - '!' or '^' first negate — a positive class whose ONLY members are
//     '!' and/or '^' cannot be expressed as a class (no char can sit in
//     first position to de-special them), so those cases compile to `\!`,
//     `\^` or the alternation `{\!,\^}` outside the class;
//   - a '-' is literal only first or last — a positive class containing
//     '-' but no other safe-first char emits it first.
//
// Ranges arrive already normalized: strictly lo<hi, endpoints free of
// ']','-','!','^','/'. Singles are deduplicated, order preserved.
func emitRgClass(b *strings.Builder, neg bool, singles []rune, ranges [][2]rune) bool {
	normalizeRgRangeEndpoints(&singles, &ranges)
	has := func(r rune) bool {
		for _, s := range singles {
			if s == r {
				return true
			}
		}
		return false
	}
	if !neg && len(ranges) == 0 {
		// A positive class whose only members are '!'/'^' has no legal
		// first char for the class syntax — compile to top-level forms.
		bang, caret := has('!'), has('^')
		other := false
		for _, s := range singles {
			if s != '!' && s != '^' {
				other = true
			}
		}
		if !other {
			switch {
			case bang && caret:
				b.WriteString(`{\!,\^}`)
				return true
			case bang:
				b.WriteString(`\!`)
				return true
			case caret:
				b.WriteString(`\^`)
				return true
			default:
				// The empty class — unsatisfiable as a pattern fragment.
				return false
			}
		}
	}
	if neg && len(singles) == 0 && len(ranges) == 0 {
		// "any char except /" — the caller emits `?`.
		return false
	}
	b.WriteByte('[')
	if neg {
		b.WriteByte('!')
	}
	// POSIX literal-']' first: a ']' directly after '[' or '[!' is a
	// member, not the closer.
	if has(']') {
		b.WriteByte(']')
	}
	// A '-' can only be literal first or last. With no ']' to sit in first
	// position, no ordinary singles and no ranges to precede it — and only
	// '!'/'^' otherwise (a positive class cannot write those first without
	// flipping negation) — the '-' goes first: [-!], [-^], [-!^].
	minusFirst := false
	if !neg && has('-') && !has(']') && len(ranges) == 0 {
		ordinary := false
		for _, s := range singles {
			switch s {
			case '-', '!', '^', ']':
			default:
				ordinary = true
			}
		}
		if !ordinary {
			b.WriteByte('-')
			minusFirst = true
		}
	}
	// Ordinary singles first — every char goes in verbatim, including
	// '\\', '{', '}', ',', '[' (in-class escaping does not exist for rg and
	// none of these is position-sensitive).
	for _, s := range singles {
		switch s {
		case ']', '-', '!', '^':
			continue
		}
		b.WriteString(string(s))
	}
	for _, rg := range ranges {
		b.WriteString(string(rg[0]))
		b.WriteByte('-')
		b.WriteString(string(rg[1]))
	}
	// '!' and '^' are safe anywhere except the first slot — by here either
	// neg already wrote '!' or a ']'/ordinary/range leads the class.
	if has('!') {
		b.WriteByte('!')
	}
	if has('^') {
		b.WriteByte('^')
	}
	if has('-') && !minusFirst {
		b.WriteByte('-')
	}
	b.WriteByte(']')
	return true
}

// fixRgTrailingEscape guarantees that the emitted glob does not end in an
// EVEN, non-empty run of backslashes. Measured against ripgrep 14.1.1 with
// the "!\<glob>/" directory form this call site appends:
//
//	trailing run 1 / 3 / 5 → accepted (the last "\" escapes the "/")
//	trailing run 2 / 4     → rg: error parsing glob '!a\\/': dangling '\' (exit 2)
//
// Appending one more backslash turns an even run odd. That removes the crash
// and keeps the meaning: `a\\` (rg: literal backslash) becomes `a\\\`, which
// still denotes the directory "a\\" — exactly what filepath.Match matches for
// the same rule. A run that was already odd (a dangling escape, which only
// ever reaches here from a pattern filepath.Match rejected) is left alone and
// degrades to a harmless no-op instead of an error.
func fixRgTrailingEscape(g string) string {
	if n := trailingBackslashes(g); n > 0 && n%2 == 0 {
		return g + `\`
	}
	return g
}

// trailingBackslashes counts the run of backslashes at the end of s.
func trailingBackslashes(s string) int {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n
}

// BuiltinSkipGlobs returns the indexer's built-in directory exclusions
// (skipDirNames + skipPathFragments + Go toolchain/GOPATH segments) as a
// deduplicated, lexically sorted list of gitignore-style glob strings.
// It is the single source of truth the rg exclusion args consume, so the
// rg fallback and the walker can never drift apart.
//
// Each entry carries the semantics rg needs — a leading "/" anchors the
// rule to the search root's top level, an unanchored multi-segment
// fragment must apply at any depth, and a bare name is directory-only.
// The caller maps each entry to a "!" negative glob pair (dir + "/**").
//
// The three shapes emitted are:
//
//	name        bare directory basename   (node_modules, .cache, …)
//	**/a/b      multi-segment fragment    (.local/lib, go/pkg, …)
//	/name       top-level anchored dir    (./ref, /go under a GOPATH root)
//
// root is the workdir/project root. It is only used to decide whether the
// top-level "go" directory is itself a GOPATH workspace (isGopathRoot on
// <root>/go) — e.g. workdir=/root, ~, /home/u or /Users/u. All other
// rules are global.
func BuiltinSkipGlobs(root string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(g string) {
		if g == "" {
			return
		}
		if _, ok := seen[g]; ok {
			return
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}

	// a) Bare directory basenames — a same-named dir at ANY depth is
	// excluded, but never a same-named plain file (gitignore "dir/").
	for name := range skipDirNames {
		add(name)
	}
	// Single-segment fragments reduce to bare names too
	// (e.g. "/code_references/" -> "code_references").
	for _, frag := range skipPathFragments {
		f := strings.Trim(frag, "/")
		if f != "" && !strings.Contains(f, "/") {
			add(f)
		}
	}

	// b) Multi-segment path fragments — unanchored "**/a/b" so the
	// fragment matches at the root top level AND at any depth below it.
	for _, frag := range skipPathFragments {
		f := strings.Trim(frag, "/")
		if f != "" && strings.Contains(f, "/") {
			add("**/" + f)
		}
	}
	// Go toolchain segments (.../go/pkg, .../go/bin, .../go/src) — the
	// isGoToolchainPath rule, as glob fragments. A leading "go" path
	// element is required so "codegraph-go/pkg" or "sdk/go" is never hit.
	for _, seg := range []string{"go/pkg", "go/bin", "go/src"} {
		add("**/" + seg)
	}

	// c) Top-level anchored GOPATH dir: only when <root>/go would itself
	// be a GOPATH workspace root (root is /root, $HOME, /home/<u> or
	// /Users/<u>) — the "rel == go && isGopathRoot" walker case.
	if root != "" {
		abs, err := filepath.Abs(root)
		if err != nil {
			abs = root
		}
		if isGopathRoot(filepath.ToSlash(filepath.Join(filepath.Clean(abs), "go"))) {
			add("/go")
		}
	}

	sort.Strings(out)
	return out
}
