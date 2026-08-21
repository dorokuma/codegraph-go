package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// --- rgOutputLines lifecycle (audit: no deferred Kill/Wait) ---

// writeMatchFile writes a file with n lines each containing "match".
func writeMatchFile(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "big.txt")
	var b strings.Builder
	b.Grow(n * 16)
	for i := 0; i < n; i++ {
		b.WriteString("match line number here\n")
	}
	if err := os.WriteFile(f, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestRgOutputLinesLineCap: when rg emits more lines than maxLines, the call
// must return exactly maxLines lines with truncated=true and no error — and
// the child must be reaped (no zombie), which is what the deferred Kill/Wait
// guarantees.
func TestRgOutputLinesLineCap(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available")
	}
	f := writeMatchFile(t, 5000)
	cmd := exec.Command("rg", "--line-number", "--no-heading", "--color=never", "--fixed-strings", "match line", f)
	lines, truncated, err := rgOutputLines(cmd, 100, 1<<30)
	if err != nil {
		t.Fatalf("rgOutputLines: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true when output exceeds maxLines")
	}
	if len(lines) != 100 {
		t.Fatalf("expected exactly maxLines=100 lines, got %d", len(lines))
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (zombie)")
	}
}

// TestRgOutputLinesByteCap: a single line longer than maxBytes must stop the
// read entirely (no partial line, no unbounded buffering) and report
// truncated, with the child reaped.
func TestRgOutputLinesByteCap(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(f, []byte(strings.Repeat("a", 10*1024)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rg", "--no-heading", "--color=never", "--fixed-strings", "aaaa", f)
	lines, truncated, err := rgOutputLines(cmd, 1<<30, 1024)
	if err != nil {
		t.Fatalf("rgOutputLines: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true for a line over the byte cap")
	}
	if len(lines) != 0 {
		t.Fatalf("over-budget line must not be returned, got %d lines", len(lines))
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (zombie)")
	}
}

// TestRgOutputLinesNoMatch: rg's exit code 1 (no matches) is not an error —
// it returns zero lines, truncated=false, nil error, child reaped.
func TestRgOutputLinesNoMatch(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("rg", "--no-heading", "--color=never", "--fixed-strings", "zzz-no-such-match", f)
	lines, truncated, err := rgOutputLines(cmd, 100, 1<<30)
	if err != nil {
		t.Fatalf("rg exit 1 (no matches) must not be an error, got: %v", err)
	}
	if truncated {
		t.Fatal("no-match run must not report truncated")
	}
	if len(lines) != 0 {
		t.Fatalf("expected no lines, got %d", len(lines))
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (zombie)")
	}
}

// TestRgOutputLinesError: a genuine rg failure (nonexistent search root,
// exit 2) must be surfaced as an error, with the child reaped.
func TestRgOutputLinesError(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not available")
	}
	cmd := exec.Command("rg", "--no-heading", "--color=never", "--fixed-strings", "x",
		filepath.Join(t.TempDir(), "does-not-exist"))
	if _, _, err := rgOutputLines(cmd, 100, 1<<30); err == nil {
		t.Fatal("expected error for nonexistent search root")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (zombie)")
	}
}

// TestRgOutputLinesKilledByContext: when the caller's ctx kills the child
// mid-stream, rgOutputLines must return an error promptly (no hang on Wait)
// and reap the child. The child is this test binary re-executed in helper
// mode, streaming lines forever — it cannot exit on its own, so the kill is
// the only way the call can return.
func TestRgOutputLinesKilledByContext(t *testing.T) {
	if os.Getenv("CG_RG_OUTPUT_HELPER") == "1" {
		w := bufio.NewWriter(os.Stdout)
		for i := 0; ; i++ {
			fmt.Fprintf(w, "line %d\n", i)
			w.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestRgOutputLinesKilledByContext")
	cmd.Env = append(os.Environ(), "CG_RG_OUTPUT_HELPER=1")

	start := time.Now()
	_, _, err := rgOutputLines(cmd, 1<<30, 1<<30)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error: child was killed by ctx")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("rgOutputLines hung after ctx kill: %v", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("child was not reaped (zombie)")
	}
}

// --- audit regression: clamp + negative max handling ---

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name string
		v    int
		def  int
		hard int
		want int
	}{
		{"zero falls back to default", 0, 70, 500, 70},
		{"negative falls back to default", -1, 70, 500, 70},
		{"very negative falls back to default", -100000, 70, 500, 70},
		{"in-range kept", 42, 70, 500, 42},
		{"over hard cap cut to cap", 1000000, 70, 500, 500},
		{"exactly at cap kept", 500, 70, 500, 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampLimit(tt.v, tt.def, tt.hard); got != tt.want {
				t.Fatalf("clampLimit(%d,%d,%d) = %d, want %d", tt.v, tt.def, tt.hard, got, tt.want)
			}
		})
	}
}

// TestToolFilesNegativeMaxNoPanic: max<0 used to reach lines[:args.Max] and
// panic (audit critical #2), killing the whole daemon. Both the direct tool
// and the action router must clamp to the default instead.
func TestToolFilesNegativeMaxNoPanic(t *testing.T) {
	s, _ := setupToolServer(t)

	// Direct tool call.
	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "*.go", Max: -1})
	if err != nil {
		t.Fatalf("toolFiles with max=-1: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected alpha.go in files listing, got:\n%s", text)
	}

	// Through the router (the path a hostile MCP client actually takes).
	res, _, err = s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "files", Pattern: "*.go", Max: -5})
	if err != nil {
		t.Fatalf("router files with max=-5: %v", err)
	}
	if textContent(res) == "" {
		t.Fatal("expected non-empty files listing for max=-5")
	}
}

// TestToolSearchNegativeAndHugeMax: negative max_results must not panic
// (the FTS path feeds it straight into the SQL LIMIT); a huge max_results
// must be clamped to the server hard ceiling instead of amplifying work —
// and the result must actually be bounded by that ceiling, not merely
// "not an error".
func TestToolSearchNegativeAndHugeMax(t *testing.T) {
	s, _ := setupToolServer(t)

	if _, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha", MaxResults: -3}); err != nil {
		t.Fatalf("search with max_results=-3: %v", err)
	}
	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha", MaxResults: 10_000_000})
	if err != nil {
		t.Fatalf("search with max_results=10M: %v", err)
	}
	// Huge max is clamped to maxSearchResults: the FTS shortcut must not
	// return more lines than the ceiling (a regression here means the clamp
	// was bypassed and a hostile client can amplify the result set).
	lines := strings.Split(strings.TrimSpace(textContent(res)), "\n")
	if len(lines) == 0 {
		t.Fatal("expected at least one match for Alpha")
	}
	if len(lines) > maxSearchResults {
		t.Fatalf("search with max_results=10M returned %d lines, clamped cap is %d", len(lines), maxSearchResults)
	}
	res, _, err = s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "search", Pattern: "Alpha", MaxResults: -1})
	if err != nil {
		t.Fatalf("router search with max_results=-1: %v", err)
	}
	if textContent(res) == "" {
		t.Fatal("expected non-empty search result")
	}
}

// TestToolSearchLiteralByDefault: the pattern is handed to rg as a literal
// string (--fixed-strings) unless regex=true (audit M1). "Alpha(" is an
// invalid regex (unclosed group) — before the fix rg exited 2 and the tool
// errored; now it matches the literal text.
func TestToolSearchLiteralByDefault(t *testing.T) {
	s, _ := setupToolServer(t)
	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha("})
	if err != nil {
		t.Fatalf("literal search: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "alpha.go") {
		t.Fatalf("expected alpha.go to match literal pattern, got:\n%s", text)
	}

	// Same pattern as a regex must fail (unclosed group) — proving the
	// literal path really differs from regex semantics.
	if _, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha(", Regex: true}); err == nil {
		t.Fatal("expected error for invalid regex with regex=true")
	}
}

// TestToolSearchRegexExplicit: regex semantics only when regex=true.
func TestToolSearchRegexExplicit(t *testing.T) {
	s, _ := setupToolServer(t)

	// "^func Alpha" only matches as a regex; as a literal it finds nothing.
	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "^func Alpha"})
	if err != nil {
		t.Fatalf("literal search: %v", err)
	}
	if strings.Contains(textContent(res), "alpha.go") {
		t.Fatalf("literal pattern ^func Alpha must not match, got:\n%s", textContent(res))
	}

	res, _, err = s.toolSearch(context.Background(), nil, searchArgs{Pattern: "^func Alpha", Regex: true})
	if err != nil {
		t.Fatalf("regex search: %v", err)
	}
	if !strings.Contains(textContent(res), "alpha.go") {
		t.Fatalf("regex pattern ^func Alpha should match alpha.go, got:\n%s", textContent(res))
	}
}

// TestToolSearchNoIgnoreOptIn: searching a path must respect .gitignore by
// default (no more implicit --no-ignore that surfaced .env/private keys,
// audit M2); no_ignore=true re-enables the sweep.
func TestToolSearchNoIgnoreOptIn(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, dir := setupToolServer(t)
	// Make dir a git repo so rg actually applies ignore rules.
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %v %s", err, out)
	}
	_ = exec.Command("git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.Command("git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.env"), []byte("PRIVATE_KEY=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", ".gitignore").CombinedOutput(); err != nil {
		t.Skipf("git add failed: %v %s", err, out)
	}

	// Default: ignored file is not searched, even with an explicit path.
	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "PRIVATE_KEY", Path: "."})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if strings.Contains(textContent(res), "secret.env") {
		t.Fatalf("default search must respect .gitignore, got:\n%s", textContent(res))
	}

	// Opt-in: no_ignore=true surfaces the ignored file.
	res, _, err = s.toolSearch(context.Background(), nil, searchArgs{Pattern: "PRIVATE_KEY", Path: ".", NoIgnore: true})
	if err != nil {
		t.Fatalf("search with no_ignore: %v", err)
	}
	if !strings.Contains(textContent(res), "secret.env") {
		t.Fatalf("no_ignore=true must search ignored files, got:\n%s", textContent(res))
	}
}

// TestToolSearchNoIgnoreWithoutPath: empty Path + simple ident that FTS
// can hit must still fall through to rg when no_ignore=true, so gitignored
// files are searched.
func TestToolSearchNoIgnoreWithoutPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, dir := setupToolServer(t)
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init failed: %v %s", err, out)
	}
	_ = exec.Command("git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.Command("git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("secret.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.env"), []byte("Alpha hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "add", ".gitignore").CombinedOutput(); err != nil {
		t.Skipf("git add failed: %v %s", err, out)
	}

	// FTS shortcut (no Path, simple ident) still hits the indexed symbol.
	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha"})
	if err != nil {
		t.Fatalf("fts search: %v", err)
	}
	if !strings.Contains(textContent(res), "alpha.go") {
		t.Fatalf("FTS should hit Alpha in alpha.go, got:\n%s", textContent(res))
	}

	res, _, err = s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Alpha", NoIgnore: true})
	if err != nil {
		t.Fatalf("search no_ignore without path: %v", err)
	}
	if !strings.Contains(textContent(res), "secret.env") {
		t.Fatalf("no_ignore=true without Path must still search ignored files, got:\n%s", textContent(res))
	}
}

// TestToolStoreFactContentTooLarge: content over the 64KiB cap must be
// rejected before touching the DB (audit high M9).
func TestToolStoreFactContentTooLarge(t *testing.T) {
	s, _ := setupToolServer(t)
	big := strings.Repeat("x", maxFactContentLen+1)
	_, _, err := s.toolStoreFact(context.Background(), nil, storeFactArgs{
		TargetFile: "alpha.go",
		Content:    big,
	})
	if err == nil {
		t.Fatal("expected error for over-limit fact content")
	}
	if !strings.Contains(err.Error(), "content too large") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Nothing was written.
	facts, err := s.Database.SearchFacts("", "", "", "all", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("over-limit fact must not be stored, got %d facts", len(facts))
	}
}

// TestToolSearchFactsTruncated: search_facts must truncate per-fact content
// AND the final JSON payload (audit H3) — a huge max used to marshal
// unbounded output with no truncateOutput at all.
func TestToolSearchFactsTruncated(t *testing.T) {
	s, _ := setupToolServer(t)
	for i := 0; i < 15; i++ {
		content := strings.Repeat("fact-body-", 500) + fmt.Sprintf("#%d", i) // ~5KB each, distinct hash
		if _, err := s.Database.InsertFact(&db.Fact{
			TargetFile:   "alpha.go",
			TargetSymbol: "Alpha",
			Content:      content,
			ContentHash:  hashFactContent(content),
			Status:       "active",
		}); err != nil {
			t.Fatal(err)
		}
	}
	res, _, err := s.toolSearchFacts(context.Background(), nil, searchFactsArgs{Max: 10_000})
	if err != nil {
		t.Fatalf("search_facts: %v", err)
	}
	text := textContent(res)
	// Payload is cut to the output cap.
	if len(text) > defaultOutputChars+100 {
		t.Fatalf("search_facts payload too large: %d bytes (cap %d)", len(text), defaultOutputChars)
	}
	if !strings.Contains(text, "truncated") {
		t.Fatalf("expected truncation marker, got %d bytes:\n%s...", len(text), text[:200])
	}
}

// hashFactContent mirrors the server's store_fact hash (sha256 hex).
func hashFactContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TestToolCodegraphPanicRecover: a panic inside any action must become a
// single-call error, never a process crash (audit critical). A Server with a
// nil Database makes toolStatus dereference a nil pointer — the recover must
// convert that into an error return.
func TestToolCodegraphPanicRecover(t *testing.T) {
	s := &Server{Workdir: t.TempDir()}
	res, _, err := s.toolCodegraph(context.Background(), nil, codegraphArgs{Action: "status"})
	if err == nil {
		t.Fatal("expected error from panicking action")
	}
	if res != nil {
		t.Fatalf("expected nil result on panic, got %+v", res)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("expected panic-wrapped error, got: %v", err)
	}
}

// TestToolFilesCapListings: the files rg path must not return more lines
// than the (clamped) max even when the tree has more files.
func TestToolFilesCapListings(t *testing.T) {
	s, dir := setupToolServer(t)
	for i := 0; i < 30; i++ {
		_ = os.WriteFile(filepath.Join(dir, strings.ToLower(string(rune('a'+i%26)))+string(rune('0'+i/26))+".txt"), []byte("x\n"), 0o644)
	}
	res, _, err := s.toolFiles(context.Background(), nil, filesArgs{Pattern: "*.txt", Max: 10})
	if err != nil {
		t.Fatalf("toolFiles: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(textContent(res)), "\n")
	if len(lines) > 10 {
		t.Fatalf("files listing exceeded max: %d lines", len(lines))
	}
}

// TestToolSearchSimpleIdentLightweight verifies simple identifier searches
// query lightweight refs without requiring bodies and match line numbers accurately.
func TestToolSearchSimpleIdentLightweight(t *testing.T) {
	s, dir := setupToolServer(t)
	filePath := filepath.Join(dir, "calc.go")
	code := "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
	if err := os.WriteFile(filePath, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}

	// Insert a node without body in DB (mimicking lightweight projection or indexing)
	if _, err := s.Database.UpsertNode(&db.Node{
		Kind:     db.KindFunction,
		Name:     "Add",
		File:     filePath,
		Line:     3,
		EndLine:  5,
		Language: "go",
	}); err != nil {
		t.Fatal(err)
	}

	res, _, err := s.toolSearch(context.Background(), nil, searchArgs{Pattern: "Add"})
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	text := textContent(res)
	if !strings.Contains(text, "calc.go:3") {
		t.Fatalf("expected calc.go:3 in search results, got:\n%s", text)
	}
}

