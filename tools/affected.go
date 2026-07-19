package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dorokuma/codegraph-go/db"
)

// AffectedArgs are the arguments for the affected tool.
type AffectedArgs struct {
	Files  []string `json:"files" jsonschema:"list of changed source files"`
	Stdin  bool     `json:"stdin,omitempty" jsonschema:"read file list from stdin,optional"`
	Depth  int      `json:"depth,omitempty" jsonschema:"max dependency traversal depth (default 5),optional"`
	Filter string   `json:"filter,omitempty" jsonschema:"custom glob to identify test files,optional"`
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

// ToolAffected finds test files affected by changes.
func ToolAffected(ctx context.Context, database *db.DB, workdir string, args AffectedArgs) (*AffectedResult, error) {
	if len(args.Files) == 0 && !args.Stdin {
		return nil, fmt.Errorf("files list is required (or use stdin)")
	}

	if args.Depth == 0 {
		args.Depth = 5
	}

	// Read files from stdin if requested
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

	for depth := 0; depth < args.Depth && len(queue) > 0; depth++ {
		var nextQueue []string
		for _, file := range queue {
			if affected[file] {
				continue
			}
			affected[file] = true

			// Find files that import this file's package
			importers := findImporters(database, file)
			for _, importer := range importers {
				if !affected[importer] {
					nextQueue = append(nextQueue, importer)
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
		return &AffectedResult{
			Content: []ContentItem{{Type: "text", Text: "No affected test files found."}},
		}, nil
	}

	var b strings.Builder
	for _, f := range testFiles {
		fmt.Fprintf(&b, "%s\n", f)
	}

	return &AffectedResult{
		Content: []ContentItem{{Type: "text", Text: b.String()}},
	}, nil
}

// findImporters finds files that import the given file's package.
func findImporters(database *db.DB, targetFile string) []string {
	// Get the package/module path from the file
	targetPkg := fileToPackage(targetFile)
	if targetPkg == "" {
		return nil
	}

	files, err := database.FindImporters(targetPkg)
	if err != nil {
		return nil
	}
	return files
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
			// tiny parse: "name": "foo"
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, `"name"`) {
					parts := strings.SplitN(line, ":", 2)
					if len(parts) == 2 {
						n := strings.Trim(parts[1], ` ",`)
						if n != "" {
							return cur, n
						}
					}
				}
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

	// Custom filter
	if customFilter != "" {
		matched, _ := filepath.Match(customFilter, base)
		if matched {
			return true
		}
		matched, _ = filepath.Match(customFilter, rel)
		return matched
	}

	// Check if file is in a tests/ directory
	if strings.Contains(rel, "tests/") || strings.Contains(rel, "test/") || strings.Contains(rel, "__tests__/") {
		return true
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
