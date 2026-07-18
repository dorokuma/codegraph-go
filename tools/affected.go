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

	// Resolve all files to absolute paths
	var absFiles []string
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(workdir, f)
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
	b.WriteString(fmt.Sprintf("Affected test files (%d):\n", len(testFiles)))
	for _, f := range testFiles {
		b.WriteString(fmt.Sprintf("- %s\n", f))
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
func fileToPackage(file string) string {
	dir := filepath.Dir(file)

	// Try to read go.mod for Go projects
	gomod := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(gomod); err == nil {
		// Read the module line from go.mod
		f, err := os.Open(gomod)
		if err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if strings.HasPrefix(line, "module ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "module"))
				}
			}
		}
	}

	// For other languages, use directory name as package
	return filepath.Base(dir)
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
