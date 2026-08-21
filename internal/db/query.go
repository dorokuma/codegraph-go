package db

import (
	"strings"
)

// NodeKind constants
const (
	KindFunction  = "function"
	KindClass     = "class"
	KindMethod    = "method"
	KindVariable  = "variable"
	KindConstant  = "constant"
	KindType      = "type"
	KindStruct    = "struct"
	KindInterface = "interface"
	KindFile      = "file"
	// KindSignature marks interface-method declaration nodes (extraction
	// emits them as first-class symbols). They are declarations, not
	// callable bodies: never call targets.
	KindSignature = "signature"
)

// EdgeKind constants

// EdgeKind constants
const (
	EdgeCalls      = "calls"
	EdgeImports    = "imports"
	EdgeExtends    = "extends"
	EdgeImplements = "implements"
	EdgeReferences = "references"
	EdgeContains   = "contains"
	EdgeBridge     = "bridge"
)

// Node represents a code symbol.

// Node represents a code symbol.
type Node struct {
	ID       int64
	Kind     string
	Name     string
	File     string
	Line     int
	EndLine  int
	Body     string
	Language string
	// Official-aligned optional fields (empty until extractors fill them).
	QualifiedName string
	Signature     string
	Docstring     string
	StartColumn   int
	EndColumn     int
	Visibility    string
	IsExported    bool
	ReturnType    string
}

// Edge represents a relationship between two nodes.

// Edge represents a relationship between two nodes.
type Edge struct {
	ID         int64
	SourceID   int64
	TargetID   int64
	Kind       string
	File       string
	Line       int
	Col        int
	Provenance string // exact / import / proximity / heuristic
	Metadata   string // JSON object
}

// FileRecord is an indexed source file row.

// FileRecord is an indexed source file row.
type FileRecord struct {
	Path        string
	Size        int64
	Mtime       float64
	ContentHash string
	Language    string
	NodeCount   int
}

// Fact is an agent-annotated fact attached to a code symbol.

// Fact is an agent-annotated fact attached to a code symbol.
type Fact struct {
	ID           int64
	TargetFile   string
	TargetSymbol string
	TargetLine   int
	Content      string
	ContentHash  string
	Author       string
	Status       string // active | superseded | retracted
	SupersededBy int64  // 0 = none
	CreatedAt    int64  // unix seconds
	UpdatedAt    int64  // unix seconds
}

// UnresolvedRef is a pending/failed reference awaiting resolution.

// UnresolvedRef is a pending/failed reference awaiting resolution.
type UnresolvedRef struct {
	ID            int64
	FromNode      int64
	ReferenceName string
	ReferenceKind string
	Line          int
	Col           int
	FilePath      string
	Language      string
	Status        string // pending | failed
	NameTail      string
	Candidates    string // JSON array
}

// UpsertNode inserts or updates a node. Returns the real row ID.
// (SQLite LastInsertId is unreliable after ON CONFLICT DO UPDATE.)
// New optional fields are written when set; empty values are fine.

// escapeLikePattern escapes LIKE wildcards (_ and %) so they match literally
// when used inside a LIKE pattern with ESCAPE '\'. Backslashes are escaped
// first so a literal backslash in the input cannot neutralize the escaping.
func escapeLikePattern(s string) string {
	return strings.NewReplacer("\\", "\\\\", "_", "\\_", "%", "\\%").Replace(s)
}

// statusOrDefault returns "active" when s is empty.

// statusOrDefault returns "active" when s is empty.
func statusOrDefault(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

// nullString returns nil for empty strings so SQLite stores NULL.

// nullString returns nil for empty strings so SQLite stores NULL.
func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nullInt64 returns nil for 0 so SQLite stores NULL.

// nullInt64 returns nil for 0 so SQLite stores NULL.
func nullInt64(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// UpsertEdge inserts or updates an edge. Returns the edge ID.
// NOTE: result.LastInsertId() is unreliable after ON CONFLICT DO UPDATE —
// it returns 0 on conflict. Callers that need the real ID should query it
// separately (like UpsertNode does). Current non-test callers discard the
// return value so this is harmless in practice.
// A3: uniqueness is per call-site (source,target,kind,line,col), so one
// source calling the same target from many lines keeps one row per site.
