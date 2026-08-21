package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// UpsertNode inserts or updates a node. Returns the real row ID.
// (SQLite LastInsertId is unreliable after ON CONFLICT DO UPDATE.)
// New optional fields are written when set; empty values are fine.
func (d *DB) UpsertNode(n *Node) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	body := TruncateBody(n.Body)
	exported := 0
	if n.IsExported {
		exported = 1
	}
	_, err := d.conn.Exec(`
		INSERT INTO nodes (
			kind, name, file, line, end_line, body, language,
			qualified_name, signature, docstring,
			start_column, end_column, visibility, is_exported, return_type
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file, line, kind, name) DO UPDATE SET
			end_line = excluded.end_line,
			body = excluded.body,
			language = excluded.language,
			qualified_name = excluded.qualified_name,
			signature = excluded.signature,
			docstring = excluded.docstring,
			start_column = excluded.start_column,
			end_column = excluded.end_column,
			visibility = excluded.visibility,
			is_exported = excluded.is_exported,
			return_type = excluded.return_type
	`, n.Kind, n.Name, n.File, n.Line, n.EndLine, body, n.Language,
		n.QualifiedName, n.Signature, n.Docstring,
		nullInt(n.StartColumn), nullInt(n.EndColumn), n.Visibility, exported, n.ReturnType)
	if err != nil {
		return 0, fmt.Errorf("upsert node: %w", err)
	}
	var id int64
	err = d.conn.QueryRow(`
		SELECT id FROM nodes WHERE file = ? AND line = ? AND kind = ? AND name = ?
	`, n.File, n.Line, n.Kind, n.Name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert node id lookup: %w", err)
	}
	return id, nil
}

// nullInt stores 0 columns as NULL so "unset" stays distinguishable later if needed.

// nullInt stores 0 columns as NULL so "unset" stays distinguishable later if needed.
func nullInt(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// InsertFact stores a new fact. contentHash is a SHA-256 hex string.
// The caller must already have computed it. Returns the new row ID and nil.

// nodeSelectCols is the shared column list for Node scans (keeps SELECT/scan aligned).
const nodeSelectCols = `id, kind, name, file, line, end_line, body, language,
	qualified_name, signature, docstring, start_column, end_column,
	visibility, is_exported, return_type`

// nodeLightSelectCols is the lightweight column list for Node scans omitting body.

// nodeLightSelectCols is the lightweight column list for Node scans omitting body.
const nodeLightSelectCols = `id, kind, name, file, line, end_line, '' AS body, language,
	qualified_name, signature, docstring, start_column, end_column,
	visibility, is_exported, return_type`

// DeleteUnresolvedRef removes one unresolved_refs row (resolved successfully).

// getNodesByFileCap bounds GetNodesByFile results to prevent unbounded reads.
// Test-only mutation: tests that change this value must restore it and must
// not run in parallel with other tests in this package.
var getNodesByFileCap = 10_000

// SetGetNodesByFileCapForTest overrides getNodesByFileCap for testing and returns the previous value.

// SetGetNodesByFileCapForTest overrides getNodesByFileCap for testing and returns the previous value.
func SetGetNodesByFileCapForTest(newCap int) int {
	old := getNodesByFileCap
	getNodesByFileCap = newCap
	return old
}

// GetNodesByFile returns nodes defined in a file path WITH bodies, capped at getNodesByFileCap.

// GetNodesByFile returns nodes defined in a file path WITH bodies, capped at getNodesByFileCap.
func (d *DB) GetNodesByFile(file string) ([]Node, error) {
	return d.GetNodesByFileContext(context.Background(), file)
}

// GetNodesByFileContext is the context-aware variant of GetNodesByFile (loads bodies).
// Delegates to the truncation-aware limited variant; the flag is discarded.

// GetNodesByFileContext is the context-aware variant of GetNodesByFile (loads bodies).
// Delegates to the truncation-aware limited variant; the flag is discarded.
func (d *DB) GetNodesByFileContext(ctx context.Context, file string) ([]Node, error) {
	nodes, _, err := d.GetNodesByFileBodiesLimitedContext(ctx, file, getNodesByFileCap)
	return nodes, err
}

// GetNodesByFileLightLimitedContext returns up to limit nodes defined in file (omitting body)
// and reports whether more nodes exist for that file.

// GetNodesByFileLightLimitedContext returns up to limit nodes defined in file (omitting body)
// and reports whether more nodes exist for that file.
func (d *DB) GetNodesByFileLightLimitedContext(ctx context.Context, file string, limit int) ([]Node, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = getNodesByFileCap
	}
	rows, err := d.conn.QueryContext(ctx, `
		SELECT `+nodeLightSelectCols+`
		FROM nodes WHERE file = ?
		ORDER BY id LIMIT ?
	`, file, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(nodes) > limit
	if truncated {
		nodes = nodes[:limit]
	}
	return nodes, truncated, nil
}

// GetNodesByFileLight returns nodes defined in a file path without bodies, capped at getNodesByFileCap.

// GetNodesByFileLight returns nodes defined in a file path without bodies, capped at getNodesByFileCap.
func (d *DB) GetNodesByFileLight(file string) ([]Node, error) {
	return d.GetNodesByFileLightContext(context.Background(), file)
}

// GetNodesByFileLightContext is the context-aware variant of GetNodesByFileLight (omits body).

// GetNodesByFileLightContext is the context-aware variant of GetNodesByFileLight (omits body).
func (d *DB) GetNodesByFileLightContext(ctx context.Context, file string) ([]Node, error) {
	nodes, _, err := d.GetNodesByFileLightLimitedContext(ctx, file, getNodesByFileCap)
	return nodes, err
}

// GetNodesByFileBodiesLimitedContext returns up to limit nodes defined in
// file WITH bodies and reports whether more nodes exist for that file.
// limit <= 0 defaults to getNodesByFileCap.

// GetNodesByFileBodiesLimitedContext returns up to limit nodes defined in
// file WITH bodies and reports whether more nodes exist for that file.
// limit <= 0 defaults to getNodesByFileCap.
func (d *DB) GetNodesByFileBodiesLimitedContext(ctx context.Context, file string, limit int) ([]Node, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = getNodesByFileCap
	}
	rows, err := d.conn.QueryContext(ctx, `
		SELECT `+nodeSelectCols+`
		FROM nodes WHERE file = ?
		ORDER BY id LIMIT ?
	`, file, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(nodes) > limit
	if truncated {
		nodes = nodes[:limit]
	}
	return nodes, truncated, nil
}

// GetNodesByFileBodiesLimited is the background-context variant of
// GetNodesByFileBodiesLimitedContext.

// GetNodesByFileBodiesLimited is the background-context variant of
// GetNodesByFileBodiesLimitedContext.
func (d *DB) GetNodesByFileBodiesLimited(file string, limit int) ([]Node, bool, error) {
	return d.GetNodesByFileBodiesLimitedContext(context.Background(), file, limit)
}

// ForEachNodeByFileLight iterates through all nodes in file without bodies using keyset pagination.

// ForEachNodeByFileLight iterates through all nodes in file without bodies using keyset pagination.
func (d *DB) ForEachNodeByFileLight(file string, fn func(n Node) error) error {
	return d.ForEachNodeByFileLightContext(context.Background(), file, fn)
}

// ForEachNodeByFileLightContext iterates through all nodes in file without bodies using keyset pagination.

// ForEachNodeByFileLightContext iterates through all nodes in file without bodies using keyset pagination.
func (d *DB) ForEachNodeByFileLightContext(ctx context.Context, file string, fn func(n Node) error) error {
	if file == "" {
		return nil
	}
	const batchSize = 1000
	var lastID int64
	for {
		nodes, err := func() ([]Node, error) {
			d.mu.RLock()
			defer d.mu.RUnlock()
			rows, err := d.conn.QueryContext(ctx, `
				SELECT `+nodeLightSelectCols+`
				FROM nodes WHERE file = ? AND id > ?
				ORDER BY id LIMIT ?
			`, file, lastID, batchSize)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanNodes(rows)
		}()
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			break
		}
		for _, n := range nodes {
			if err := fn(n); err != nil {
				return err
			}
			lastID = n.ID
		}
		if len(nodes) < batchSize {
			break
		}
	}
	return nil
}

// getNodeByNameCap bounds GetNodeByName results to prevent unbounded reads.

// getNodeByNameCap bounds GetNodeByName results to prevent unbounded reads.
var getNodeByNameCap = 10_000

// GetNodeByNameLimited finds nodes by name (exact match) up to limit, and reports truncation.

// GetNodeByNameLimited finds nodes by name (exact match) up to limit, and reports truncation.
func (d *DB) GetNodeByNameLimited(name string, limit int) ([]Node, bool, error) {
	return d.GetNodeByNameLimitedContext(context.Background(), name, limit)
}

// GetNodeByNameLimitedContext finds nodes by name up to limit, and reports truncation.

// GetNodeByNameLimitedContext finds nodes by name up to limit, and reports truncation.
func (d *DB) GetNodeByNameLimitedContext(ctx context.Context, name string, limit int) ([]Node, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = getNodeByNameCap
	}
	rows, err := d.conn.QueryContext(ctx, `
		SELECT `+nodeSelectCols+`
		FROM nodes WHERE name = ?
		ORDER BY id LIMIT ?
	`, name, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(nodes) > limit
	if truncated {
		nodes = nodes[:limit]
	}
	return nodes, truncated, nil
}

// GetNodeByName finds nodes by name (exact match).

// GetNodeByName finds nodes by name (exact match).
func (d *DB) GetNodeByName(name string) ([]Node, error) {
	return d.GetNodeByNameContext(context.Background(), name)
}

// GetNodeByNameContext is the context-aware variant of GetNodeByName.

// GetNodeByNameContext is the context-aware variant of GetNodeByName.
func (d *DB) GetNodeByNameContext(ctx context.Context, name string) ([]Node, error) {
	nodes, _, err := d.GetNodeByNameLimitedContext(ctx, name, getNodeByNameCap)
	return nodes, err
}

// ForEachNodeByName iterates through all nodes matching name using keyset pagination.

// ForEachNodeByName iterates through all nodes matching name using keyset pagination.
func (d *DB) ForEachNodeByName(name string, fn func(n Node) error) error {
	return d.ForEachNodeByNameContext(context.Background(), name, fn)
}

// ForEachNodeByNameContext iterates through all nodes matching name using keyset pagination.

// ForEachNodeByNameContext iterates through all nodes matching name using keyset pagination.
func (d *DB) ForEachNodeByNameContext(ctx context.Context, name string, fn func(n Node) error) error {
	if name == "" {
		return nil
	}
	const batchSize = 1000
	var lastID int64
	for {
		nodes, err := func() ([]Node, error) {
			d.mu.RLock()
			defer d.mu.RUnlock()
			rows, err := d.conn.QueryContext(ctx, `
				SELECT `+nodeSelectCols+`
				FROM nodes WHERE name = ? AND id > ?
				ORDER BY id LIMIT ?
			`, name, lastID, batchSize)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			return scanNodes(rows)
		}()
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			break
		}
		for _, n := range nodes {
			if err := fn(n); err != nil {
				return err
			}
			lastID = n.ID
		}
		if len(nodes) < batchSize {
			break
		}
	}
	return nil
}

// GetNodeByID loads one node by primary key.

// GetNodeByID loads one node by primary key.
func (d *DB) GetNodeByID(id int64) (*Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`SELECT `+nodeSelectCols+` FROM nodes WHERE id = ?`, id)
	return scanOneNode(row)
}

// getNodesByKindCap bounds GetNodesByKind results. A single kind can hold
// hundreds of thousands of rows (each with a body up to MaxBodyChars) in very
// large indexes; whole-graph synthesis passes call this per kind. A var so
// tests can lower it to exercise truncation.

// getNodesByKindCap bounds GetNodesByKind results. A single kind can hold
// hundreds of thousands of rows (each with a body up to MaxBodyChars) in very
// large indexes; whole-graph synthesis passes call this per kind. A var so
// tests can lower it to exercise truncation.
var getNodesByKindCap = 50_000

// GetNodesByKind returns nodes of a given kind (for whole-graph synthesis
// passes), capped at getNodesByKindCap rows to bound memory and RLock hold
// time. Callers that need the full set with explicit truncation reporting
// should use GetNodesByKindLimited.

// GetNodesByKind returns nodes of a given kind (for whole-graph synthesis
// passes), capped at getNodesByKindCap rows to bound memory and RLock hold
// time. Callers that need the full set with explicit truncation reporting
// should use GetNodesByKindLimited.
func (d *DB) GetNodesByKind(kind string) ([]Node, error) {
	nodes, _, err := d.GetNodesByKindLimited(kind, getNodesByKindCap)
	return nodes, err
}

// GetNodesByKindLimited returns up to limit nodes of a given kind (ordered by
// id for deterministic truncation) plus whether more rows exist. limit <= 0
// falls back to the getNodesByKindCap default.

// GetNodesByKindLimited returns up to limit nodes of a given kind (ordered by
// id for deterministic truncation) plus whether more rows exist. limit <= 0
// falls back to the getNodesByKindCap default.
func (d *DB) GetNodesByKindLimited(kind string, limit int) ([]Node, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = getNodesByKindCap
	}
	rows, err := d.conn.Query(`SELECT `+nodeSelectCols+` FROM nodes WHERE kind = ? ORDER BY id LIMIT ?`, kind, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(nodes) > limit
	if truncated {
		nodes = nodes[:limit]
	}
	return nodes, truncated, nil
}

// GetIncomingEdges returns edges targeting nodeID, optionally filtered by kinds.

// GetNodeByFileLine finds the node at a specific file:line position.
func (d *DB) GetNodeByFileLine(file string, line int) (*Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`
		SELECT `+nodeSelectCols+`
		FROM nodes
		WHERE file = ? AND line <= ? AND (end_line >= ? OR end_line IS NULL)
		ORDER BY line DESC LIMIT 1
	`, file, line, line)
	return scanOneNode(row)
}

// structuralEdgeKinds are relationship kinds that count for callers/callees/impact.
// Official CodeGraph walks calls + references (routes→handlers) + bridges.

// escapeFTS5Query turns free-text input into a safe FTS5 MATCH expression.
// Each whitespace-separated token is quoted as a phrase so operators like AND
// and punctuation like : or " cannot trigger FTS5 syntax errors.
func escapeFTS5Query(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r)
	})
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		// Preserve a single trailing * as prefix match: foo* → "foo"*
		prefix := false
		if strings.HasSuffix(f, "*") && len(f) > 1 {
			prefix = true
			f = strings.TrimSuffix(f, "*")
		}
		escaped := strings.ReplaceAll(f, `"`, `""`)
		if escaped == "" {
			continue
		}
		if prefix {
			parts = append(parts, `"`+escaped+`"*`)
		} else {
			parts = append(parts, `"`+escaped+`"`)
		}
	}
	return strings.Join(parts, " ")
}

// ftsNameOrBodyQuery wraps escapeFTS5Query tokens so MATCH only hits the
// name and body columns, not language (searching "go" must not match every
// Go node). Each token becomes (name:tok OR body:tok); multiple tokens AND.

// ftsNameOrBodyQuery wraps escapeFTS5Query tokens so MATCH only hits the
// name and body columns, not language (searching "go" must not match every
// Go node). Each token becomes (name:tok OR body:tok); multiple tokens AND.
func ftsNameOrBodyQuery(escaped string) string {
	escaped = strings.TrimSpace(escaped)
	if escaped == "" {
		return ""
	}
	fields := strings.Fields(escaped)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, "(name:"+f+" OR body:"+f+")")
	}
	return strings.Join(parts, " AND ")
}

// FullTextSearch performs a full-text search using FTS5.

// FullTextSearch performs a full-text search using FTS5.
func (d *DB) FullTextSearch(query string, limit int) ([]Node, error) {
	return d.FullTextSearchContext(context.Background(), query, limit)
}

// FullTextSearchContext is the context-aware variant of FullTextSearch.

// FullTextSearchContext is the context-aware variant of FullTextSearch.
func (d *DB) FullTextSearchContext(ctx context.Context, query string, limit int) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	safe := ftsNameOrBodyQuery(escapeFTS5Query(query))
	if safe == "" {
		return nil, nil
	}

	rows, err := d.conn.QueryContext(ctx, `
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type
		FROM nodes_fts fts
		JOIN nodes n ON n.id = fts.rowid
		WHERE nodes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safe, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search %q: %w", query, err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

// FullTextSearchRefs is a lightweight FullTextSearch for callers that only
// need file:line references (search result listings). It returns the same
// []Node shape but WITHOUT bodies — bodies can be up to MaxBodyChars each and
// dominate the memory cost of a result set. Use FullTextSearch when the body
// is actually needed.

// FullTextSearchRefs is a lightweight FullTextSearch for callers that only
// need file:line references (search result listings). It returns the same
// []Node shape but WITHOUT bodies — bodies can be up to MaxBodyChars each and
// dominate the memory cost of a result set. Use FullTextSearch when the body
// is actually needed.
func (d *DB) FullTextSearchRefs(query string, limit int) ([]Node, error) {
	return d.FullTextSearchRefsContext(context.Background(), query, limit)
}

// FullTextSearchRefsContext is the context-aware variant of FullTextSearchRefs.

// FullTextSearchRefsContext is the context-aware variant of FullTextSearchRefs.
func (d *DB) FullTextSearchRefsContext(ctx context.Context, query string, limit int) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	safe := ftsNameOrBodyQuery(escapeFTS5Query(query))
	if safe == "" {
		return nil, nil
	}

	// Same query as FullTextSearchContext but with a constant '' in place of
	// n.body; scanNodeRow keeps every other field populated.
	rows, err := d.conn.QueryContext(ctx, `
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, '' AS body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type
		FROM nodes_fts fts
		JOIN nodes n ON n.id = fts.rowid
		WHERE nodes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, safe, limit)
	if err != nil {
		return nil, fmt.Errorf("fts search %q: %w", query, err)
	}
	defer rows.Close()
	return scanNodes(rows)
}

// rowScanner is shared by *sql.Rows and *sql.Row.

// rowScanner is shared by *sql.Rows and *sql.Row.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanNodeRow(row rowScanner, n *Node, edgeKind *string) error {
	var endLine, startCol, endCol, exported sql.NullInt64
	var body, lang, qn, sig, doc, vis, ret sql.NullString
	dests := []interface{}{
		&n.ID, &n.Kind, &n.Name, &n.File, &n.Line, &endLine, &body, &lang,
		&qn, &sig, &doc, &startCol, &endCol, &vis, &exported, &ret,
	}
	if edgeKind != nil {
		dests = append(dests, edgeKind)
	}
	if err := row.Scan(dests...); err != nil {
		return err
	}
	n.EndLine = int(endLine.Int64)
	n.Body = body.String
	n.Language = lang.String
	n.QualifiedName = qn.String
	n.Signature = sig.String
	n.Docstring = doc.String
	n.StartColumn = int(startCol.Int64)
	n.EndColumn = int(endCol.Int64)
	n.Visibility = vis.String
	n.IsExported = exported.Int64 != 0
	n.ReturnType = ret.String
	return nil
}

func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		if err := scanNodeRow(rows, &n, nil); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func scanOneNode(row *sql.Row) (*Node, error) {
	var n Node
	if err := scanNodeRow(row, &n, nil); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &n, nil
}

// GraphSnapshot contains all nodes and edges loaded from the index for
// graph-level algorithms like community detection.
