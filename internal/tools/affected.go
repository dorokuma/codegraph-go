package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/internal/db"
)

// AffectedArgs are the arguments for the affected tool.
type AffectedArgs struct {
	Files []string `json:"files" jsonschema:"list of changed source files"`
	// Stdin reads extra paths from process stdin (CLI/offline only).
	// MCP handlers must not set this — stdio is the protocol stream.
	Stdin  bool   `json:"stdin,omitempty" jsonschema:"CLI only: read file list from stdin; unsupported over MCP,optional"`
	Depth  int    `json:"depth,omitempty" jsonschema:"max dependency traversal depth (default 5),optional"`
	Filter string `json:"filter,omitempty" jsonschema:"custom glob to identify test files,optional"`
}

// AffectedResult is the result of the affected tool.
type AffectedResult struct {
	Content []ContentItem `json:"content"`
}

// Test file patterns by language
var testPatterns = map[string][]string{
	"go":         {"*_test.go"},
	"typescript": {"*.test.ts", "*.test.tsx", "*.spec.ts", "*.spec.tsx"},
	"javascript": {"*.test.js", "*.test.jsx", "*.spec.js", "*.spec.jsx"},
	"python":     {"test_*.py", "*_test.py"},
	"rust":       {"tests/*.rs", "src/*test*.rs"},
	"java":       {"*Test.java", "*Tests.java"},
	"csharp":     {"*Test.cs", "*Tests.cs"},
}

// Default and max affected depth (aligned with the graph impact depth cap).
const (
	defaultAffectedDepth = 5
	maxAffectedDepth     = 10
)

// clampedDepth validates the affected Depth argument (B4/W5): values in
// [1, maxAffectedDepth] are kept; invalid values (<=0 or over the cap) fall
// back to the default depth.
func clampedDepth(depth int) int {
	if depth < 1 || depth > maxAffectedDepth {
		return defaultAffectedDepth
	}
	return depth
}

// ToolAffected finds test files affected by changes.
// DB reads now accept context via Context variants; cancellation is supported.
// Stdin is for offline/CLI helpers only. MCP must pass files= and never set Stdin
// (server layer rejects stdin — process stdio is the MCP protocol stream).
func ToolAffected(ctx context.Context, database *db.DB, workdir string, args AffectedArgs) (*AffectedResult, error) {
	args.Depth = clampedDepth(args.Depth)

	files := args.Files
	if args.Stdin {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				files = append(files, line)
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("files list is required (or use stdin in CLI mode)")
	}

	// Resolve all files to absolute paths; reject escapes outside workdir.
	var absFiles []string
	wd := filepath.Clean(workdir)
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(wd, f)
		}
		f = filepath.Clean(f)
		if f != wd && !strings.HasPrefix(f, wd+string(filepath.Separator)) {
			continue // skip paths outside workspace
		}
		absFiles = append(absFiles, f)
	}

	// Find all transitive dependencies
	affected := make(map[string]bool)
	queue := make([]string, len(absFiles))
	copy(queue, absFiles)
	// M5: import-query failures used to be swallowed silently. Count them so
	// the result can carry a caveat — a DB failure for one file should not
	// block the traversal, but the user must know the set may be incomplete.
	importQueryFails := 0

	for depth := 0; depth < args.Depth && len(queue) > 0; depth++ {
		// Let cancellation/timeout interrupt the BFS early.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var nextQueue []string
		for _, file := range queue {
			if affected[file] {
				continue
			}
			affected[file] = true

			// Include same-directory test files. Go convention: same-package
			// _test.go files test the package but don't import it, so the
			// import-closure BFS would miss them.
			if !isTestFile(file, workdir, args.Filter) {
				dir := filepath.Dir(file)
				// files table stores workdir-relative paths; query with relative dir.
				relDir := db.StoragePath(workdir, dir)
				dirFiles, dirErr := database.ListFilesInDirContext(ctx, relDir)
				if dirErr == nil {
					for _, df := range dirFiles {
						absDf := db.AbsPath(workdir, df)
						if absDf != file && isTestFile(absDf, workdir, args.Filter) && !affected[absDf] {
							affected[absDf] = true
						}
					}
				}
			}

			// Find files that import this file's package
			importers, err := findImportersCtx(ctx, database, file)
			if err != nil {
				importQueryFails++ // DB failure for one file shouldn't block the whole traversal
				continue
			}
			for _, importer := range importers {
				// Normalize importer paths (relative storage keys → absolute).
				imp := importer
				if !filepath.IsAbs(imp) {
					imp = db.AbsPath(workdir, imp)
				}
				if !affected[imp] {
					nextQueue = append(nextQueue, imp)
				}
			}
		}
		queue = nextQueue
	}

	// Find test files among affected
	var testFiles []string
	for file := range affected {
		if isTestFile(file, workdir, args.Filter) {
			rel, _ := filepath.Rel(workdir, file)
			if rel == "" {
				rel = file
			}
			testFiles = append(testFiles, rel)
		}
	}

	// Deduplicate and sort
	testFiles = unique(testFiles)
	sort.Strings(testFiles)

	if len(testFiles) == 0 {
		text := "No affected test files found."
		if importQueryFails > 0 {
			text += fmt.Sprintf("\nnote: %d import-query failure(s); affected set may be incomplete", importQueryFails)
		}
		return &AffectedResult{
			Content: []ContentItem{{Type: "text", Text: text}},
		}, nil
	}

	var b strings.Builder
	for _, f := range testFiles {
		fmt.Fprintf(&b, "%s\n", f)
	}
	if importQueryFails > 0 {
		// M5: surface partial traversal failures instead of a silent gap.
		fmt.Fprintf(&b, "note: %d import-query failure(s); affected set may be incomplete\n", importQueryFails)
	}

	return &AffectedResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

// findImporters finds files that import the given file's package.
func findImporters(database *db.DB, targetFile string) ([]string, error) {
	return findImportersCtx(context.Background(), database, targetFile)
}

// findImportersCtx is like findImporters but accepts a context for cancellation.
func findImportersCtx(ctx context.Context, database *db.DB, targetFile string) ([]string, error) {
	// Get the package/module path from the file
	targetPkg := fileToPackage(targetFile)
	if targetPkg == "" {
		return nil, nil
	}

	files, err := database.FindImportersContext(ctx, targetPkg)
	if err != nil {
		return nil, err
	}
	return files, nil
}

// fileToPackage converts a file path to its likely package/import path.
// For Go: walk up to go.mod and return modulePath + relative dir.
// For JS/TS: prefer package.json "name" + relative path when present.
// Fallback: directory basename (weak but better than empty).
func fileToPackage(file string) string {
	dir := filepath.Dir(file)

	// --- Go: module path + rel ---
	modDir, modPath := findGoModule(dir)
	if modPath != "" {
		rel, err := filepath.Rel(modDir, dir)
		if err == nil && rel != "." && rel != "" {
			return modPath + "/" + filepath.ToSlash(rel)
		}
		return modPath
	}

	// --- JS/TS: nearest package.json name ---
	if pkgDir, pkgName := findNPMPackage(dir); pkgName != "" {
		rel, err := filepath.Rel(pkgDir, dir)
		if err == nil && rel != "." && rel != "" {
			return pkgName + "/" + filepath.ToSlash(rel)
		}
		return pkgName
	}

	// Fallback: directory name
	return filepath.Base(dir)
}

func findGoModule(start string) (modDir, modulePath string) {
	cur := start
	for i := 0; i < 24; i++ {
		gomod := filepath.Join(cur, "go.mod")
		if f, err := os.Open(gomod); err == nil {
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if strings.HasPrefix(line, "module ") {
					mod := strings.TrimSpace(strings.TrimPrefix(line, "module"))
					_ = f.Close()
					return cur, mod
				}
			}
			_ = f.Close()
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", ""
}

func findNPMPackage(start string) (pkgDir, name string) {
	cur := start
	for i := 0; i < 24; i++ {
		pj := filepath.Join(cur, "package.json")
		data, err := os.ReadFile(pj)
		if err == nil {
			var pkg struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(data, &pkg); err == nil && pkg.Name != "" {
				return cur, pkg.Name
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", ""
}

// isTestFile checks if a file is a test file.
func isTestFile(file string, workdir string, customFilter string) bool {
	base := filepath.Base(file)
	rel, _ := filepath.Rel(workdir, file)
	// Normalize to forward slashes so path-segment matching is OS-independent.
	rel = filepath.ToSlash(rel)

	// Custom filter
	if customFilter != "" {
		matched, _ := filepath.Match(customFilter, base)
		if matched {
			return true
		}
		matched, _ = filepath.Match(customFilter, rel)
		return matched
	}

	// Check if file is in a tests/ directory (exact segment match, not substring).
	for _, seg := range strings.Split(rel, "/") {
		if seg == "tests" || seg == "test" || seg == "__tests__" {
			return true
		}
	}

	// Check default patterns
	for _, patterns := range testPatterns {
		for _, pattern := range patterns {
			// Match against filename
			matched, _ := filepath.Match(pattern, base)
			if matched {
				return true
			}
		}
	}

	return false
}

// unique removes duplicate strings from a slice.
func unique(slice []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, s := range slice {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
