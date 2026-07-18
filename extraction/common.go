package extraction

import (
	"path/filepath"
	"strings"
)

// DetectLanguage returns the language for a file based on extension.
func DetectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".cs":
		return "csharp"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hxx":
		return "cpp"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".scala":
		return "scala"
	case ".dart":
		return "dart"
	case ".lua":
		return "lua"
	case ".luau":
		return "luau"
	case ".r", ".R":
		return "r"
	case ".m", ".mm":
		return "objective-c"
	case ".svelte":
		return "svelte"
	case ".vue":
		return "vue"
	case ".astro":
		return "astro"
	case ".liquid":
		return "liquid"
	case ".pas", ".dpr", ".dpk", ".lpr":
		return "pascal"
	}
	return ""
}

// IsSupportedLanguage returns true if we can extract symbols from this language.
func IsSupportedLanguage(lang string) bool {
	switch lang {
	case "go", "typescript", "javascript", "python", "rust", "java", "csharp",
		"ruby", "php", "c", "cpp", "swift", "kotlin", "scala", "dart", "lua", "luau", "r",
		"objective-c", "svelte", "vue", "astro", "liquid", "pascal":
		return true
	}
	return false
}
