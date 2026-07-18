package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
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
)

// EdgeKind constants
const (
	EdgeCalls      = "calls"
	EdgeImports    = "imports"
	EdgeExtends    = "extends"
	EdgeImplements = "implements"
	EdgeReferences = "references"
)

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
}

// Edge represents a relationship between two nodes.
type Edge struct {
	ID       int64
	SourceID int64
	TargetID int64
	Kind     string
	File     string
	Line     int
}

// UpsertNode inserts or updates a node. Returns the node ID.
func (d *DB) UpsertNode(n *Node) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		INSERT INTO nodes (kind, name, file, line, end_line, body, language)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file, line, kind, name) DO UPDATE SET
			end_line = excluded.end_line,
			body = excluded.body,
			language = excluded.language
	`, n.Kind, n.Name, n.File, n.Line, n.EndLine, n.Body, n.Language)
	if err != nil {
		return 0, fmt.Errorf("upsert node: %w", err)
	}
	return result.LastInsertId()
}

// UpsertEdge inserts or updates an edge. Returns the edge ID.
func (d *DB) UpsertEdge(e *Edge) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		INSERT INTO edges (source_id, target_id, kind, file, line)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, kind) DO UPDATE SET
			file = excluded.file,
			line = excluded.line
	`, e.SourceID, e.TargetID, e.Kind, e.File, e.Line)
	if err != nil {
		return 0, fmt.Errorf("upsert edge: %w", err)
	}
	return result.LastInsertId()
}

// UpsertFile records a file's indexing status.
func (d *DB) UpsertFile(path string, size int64, mtime float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec(`
		INSERT INTO files (path, size, mtime, indexed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size = excluded.size,
			mtime = excluded.mtime,
			indexed_at = excluded.indexed_at
	`, path, size, mtime, float64(time.Now().Unix()))
	return err
}

// GetNodeByName finds nodes by name (exact match).
func (d *DB) GetNodeByName(name string) ([]Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query(`
		SELECT id, kind, name, file, line, end_line, body, language
		FROM nodes WHERE name = ?
	`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNodeByFileLine finds the node at a specific file:line position.
func (d *DB) GetNodeByFileLine(file string, line int) (*Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	row := d.conn.QueryRow(`
		SELECT id, kind, name, file, line, end_line, body, language
		FROM nodes
		WHERE file = ? AND line <= ? AND (end_line >= ? OR end_line IS NULL)
		ORDER BY line DESC LIMIT 1
	`, file, line, line)
	return scanOneNode(row)
}

// GetCallers returns nodes that call the given node ID.
func (d *DB) GetCallers(nodeID int64) ([]Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language
		FROM edges e
		JOIN nodes n ON n.id = e.source_id
		WHERE e.target_id = ? AND e.kind = ?
	`, nodeID, EdgeCalls)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetCallees returns nodes that the given node ID calls.
func (d *DB) GetCallees(nodeID int64) ([]Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.source_id = ? AND e.kind = ?
	`, nodeID, EdgeCalls)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetImpact returns files that reference the given node, with match counts.
func (d *DB) GetImpact(nodeID int64) (map[string]int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query(`
		SELECT file, COUNT(*) as cnt
		FROM edges
		WHERE target_id = ? AND file IS NOT NULL
		GROUP BY file
		ORDER BY cnt DESC
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var file string
		var cnt int
		if err := rows.Scan(&file, &cnt); err != nil {
			return nil, err
		}
		result[file] = cnt
	}
	return result, nil
}

// FileNeedsReindex checks if a file needs reindexing based on size and mtime.
func (d *DB) FileNeedsReindex(path string, size int64, mtime float64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var dbSize int64
	var dbMtime float64
	err := d.conn.QueryRow("SELECT size, mtime FROM files WHERE path = ?", path).Scan(&dbSize, &dbMtime)
	if err == sql.ErrNoRows {
		return true, nil // new file
	}
	if err != nil {
		return false, err
	}
	return dbSize != size || dbMtime != mtime, nil
}

// ClearFile removes all nodes and edges for a file (before reindexing).
func (d *DB) ClearFile(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Get node IDs for this file
	rows, err := tx.Query("SELECT id FROM nodes WHERE file = ?", path)
	if err != nil {
		return err
	}
	var nodeIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		nodeIDs = append(nodeIDs, id)
	}
	rows.Close()

	// Delete edges referencing these nodes
	for _, id := range nodeIDs {
		if _, err := tx.Exec("DELETE FROM edges WHERE source_id = ? OR target_id = ?", id, id); err != nil {
			return err
		}
	}

	// Delete nodes for this file
	if _, err := tx.Exec("DELETE FROM nodes WHERE file = ?", path); err != nil {
		return err
	}

	// Delete file record
	if _, err := tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
		return err
	}

	return tx.Commit()
}

// Stats returns index statistics.
type Stats struct {
	NodeCount  int
	EdgeCount  int
	FileCount  int
	KindCounts map[string]int
}

// GetStats returns index statistics.
func (d *DB) GetStats() (*Stats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	s := &Stats{KindCounts: make(map[string]int)}

	d.conn.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&s.NodeCount)
	d.conn.QueryRow("SELECT COUNT(*) FROM edges").Scan(&s.EdgeCount)
	d.conn.QueryRow("SELECT COUNT(*) FROM files").Scan(&s.FileCount)

	rows, err := d.conn.Query("SELECT kind, COUNT(*) FROM nodes GROUP BY kind")
	if err != nil {
		return s, nil
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var cnt int
		if err := rows.Scan(&kind, &cnt); err == nil {
			s.KindCounts[kind] = cnt
		}
	}

	return s, nil
}

// ListFiles returns all indexed files.
func (d *DB) ListFiles() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query("SELECT path FROM files ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err == nil {
			files = append(files, path)
		}
	}
	return files, nil
}

// DeleteFile removes a file and its nodes/edges from the index.
func (d *DB) DeleteFile(path string) error {
	return d.ClearFile(path)
}

// FindImporters finds files that import the given package.
func (d *DB) FindImporters(targetPkg string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	rows, err := d.conn.Query(`
		SELECT DISTINCT e.file
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.kind = 'imports' AND (n.name = ? OR n.name LIKE ?)
	`, targetPkg, targetPkg+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var file string
		if err := rows.Scan(&file); err == nil {
			files = append(files, file)
		}
	}
	return files, nil
}

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

// FullTextSearch performs a full-text search using FTS5.
func (d *DB) FullTextSearch(query string, limit int) ([]Node, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	safe := escapeFTS5Query(query)
	if safe == "" {
		return nil, nil
	}

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language
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

func scanNodes(rows *sql.Rows) ([]Node, error) {
	var nodes []Node
	for rows.Next() {
		var n Node
		var endLine sql.NullInt64
		var body, lang sql.NullString
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.File, &n.Line, &endLine, &body, &lang); err != nil {
			return nil, err
		}
		n.EndLine = int(endLine.Int64)
		n.Body = body.String
		n.Language = lang.String
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func scanOneNode(row *sql.Row) (*Node, error) {
	var n Node
	var endLine sql.NullInt64
	var body, lang sql.NullString
	if err := row.Scan(&n.ID, &n.Kind, &n.Name, &n.File, &n.Line, &endLine, &body, &lang); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	n.EndLine = int(endLine.Int64)
	n.Body = body.String
	n.Language = lang.String
	return &n, nil
}
