package extraction

import (
	"regexp"
	"strings"
)

// Generated file patterns — matched against relative file paths.
// These files are still indexed and reachable via graph traversal,
// but ranked LAST in search/explorer results when a hand-written
// symbol with the same name exists.
//
// Ported from official generated-detection.ts.
var generatedPatterns = []regexp.Regexp{
	// Go — protobuf / gRPC / pulsar
	*regexp.MustCompile(`\.pb\.go$`),
	*regexp.MustCompile(`\.pulsar\.go$`),
	*regexp.MustCompile(`_grpc\.pb\.go$`),
	// Go — mockgen / mockery output
	*regexp.MustCompile(`_mock\.go$`),
	*regexp.MustCompile(`_mocks\.go$`),
	*regexp.MustCompile(`^mock_[^/]+\.go$`),
	// TypeScript / JavaScript — codegen suffixes
	*regexp.MustCompile(`\.generated\.[jt]sx?$`),
	*regexp.MustCompile(`\.gen\.[jt]sx?$`),
	*regexp.MustCompile(`\.pb\.[jt]s$`),
	*regexp.MustCompile(`_pb\.[jt]s$`),
	*regexp.MustCompile(`_grpc_pb\.[jt]s$`),
	// Minified bundles
	*regexp.MustCompile(`\.min\.m?js$`),
	// Python — protobuf / gRPC
	*regexp.MustCompile(`_pb2(_grpc)?\.py$`),
	*regexp.MustCompile(`_pb2\.pyi$`),
	// C++ — protobuf
	*regexp.MustCompile(`\.pb\.(cc|h)$`),
	// C# — protobuf / gRPC
	*regexp.MustCompile(`\.g\.cs$`),
	*regexp.MustCompile(`Grpc\.cs$`),
	// Java — protobuf / gRPC
	*regexp.MustCompile(`OuterClass\.java$`),
	*regexp.MustCompile(`Grpc\.java$`),
	// Dart — protobuf
	*regexp.MustCompile(`\.g\.dart$`),
	// Ruby — protobuf
	*regexp.MustCompile(`_pb\.rb$`),
	// Rust — protobuf
	*regexp.MustCompile(`\.pb\.rs$`),
	// Kotlin — protobuf
	*regexp.MustCompile(`_grpc\.kt$`),
	// Swift — protobuf
	*regexp.MustCompile(`\.pb\.swift$`),
	*regexp.MustCompile(`\.grpc\.swift$`),
}

// IsGeneratedFile reports whether a file path looks like generated code.
// Path should be a relative path using forward slashes.
func IsGeneratedFile(path string) bool {
	// Also check for the canonical "// Code generated" or "//go:generate" header
	// would require reading the file; here we do path-based detection only.
	base := path
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		base = path[i+1:]
	}
	for i := range generatedPatterns {
		if generatedPatterns[i].MatchString(base) || generatedPatterns[i].MatchString(path) {
			return true
		}
	}
	return false
}
