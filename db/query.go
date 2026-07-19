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
	EdgeContains   = "contains"
	EdgeBridge     = "bridge"
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
type FileRecord struct {
	Path        string
	Size        int64
	Mtime       float64
	ContentHash string
	Language    string
	NodeCount   int
}

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
func nullInt(v int) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// UpsertEdge inserts or updates an edge. Returns the edge ID.
func (d *DB) UpsertEdge(e *Edge) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, kind) DO UPDATE SET
			file = excluded.file,
			line = excluded.line,
			col = excluded.col,
			provenance = excluded.provenance,
			metadata = excluded.metadata
	`, e.SourceID, e.TargetID, e.Kind, e.File, e.Line, nullInt(e.Col), e.Provenance, e.Metadata)
	if err != nil {
		return 0, fmt.Errorf("upsert edge: %w", err)
	}
	return result.LastInsertId()
}

// UpsertFile records a file's indexing status (legacy signature; extra fields empty).
func (d *DB) UpsertFile(path string, size int64, mtime float64) error {
	return d.UpsertFileRecord(&FileRecord{Path: path, Size: size, Mtime: mtime})
}

// UpsertFileRecord writes a full files row including content_hash / language / node_count.
func (d *DB) UpsertFileRecord(f *FileRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.conn.Exec(`
		INSERT INTO files (path, size, mtime, indexed_at, content_hash, language, node_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			size = excluded.size,
			mtime = excluded.mtime,
			indexed_at = excluded.indexed_at,
			content_hash = excluded.content_hash,
			language = excluded.language,
			node_count = excluded.node_count
	`, f.Path, f.Size, f.Mtime, float64(time.Now().Unix()), f.ContentHash, f.Language, f.NodeCount)
	return err
}

// InsertUnresolvedRef stores a pending reference for later resolution.
func (d *DB) InsertUnresolvedRef(r *UnresolvedRef) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	status := r.Status
	if status == "" {
		status = "pending"
	}
	result, err := d.conn.Exec(`
		INSERT INTO unresolved_refs (
			from_node, reference_name, reference_kind, line, col,
			file_path, language, status, name_tail, candidates
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(from_node, reference_name, reference_kind, line, col) DO UPDATE SET
			file_path = excluded.file_path,
			language = excluded.language,
			status = excluded.status,
			name_tail = excluded.name_tail,
			candidates = excluded.candidates
	`, r.FromNode, r.ReferenceName, r.ReferenceKind, r.Line, r.Col,
		r.FilePath, r.Language, status, r.NameTail, r.Candidates)
	if err != nil {
		return 0, fmt.Errorf("insert unresolved_ref: %w", err)
	}
	return result.LastInsertId()
}

// CountUnresolvedRefs returns how many unresolved_refs rows match status
// (empty status = all rows).
func (d *DB) CountUnresolvedRefs(status string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var n int
	var err error
	if status == "" {
		err = d.conn.QueryRow(`SELECT COUNT(*) FROM unresolved_refs`).Scan(&n)
	} else {
		err = d.conn.QueryRow(`SELECT COUNT(*) FROM unresolved_refs WHERE status = ?`, status).Scan(&n)
	}
	return n, err
}

// ListUnresolvedRefs returns unresolved_refs rows, optionally filtered by file path
// and/or status (empty string = no filter).
func (d *DB) ListUnresolvedRefs(filePath, status string) ([]UnresolvedRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT id, from_node, reference_name, reference_kind, line, col,
		file_path, language, status, name_tail, COALESCE(candidates,'')
		FROM unresolved_refs WHERE 1=1`
	var args []interface{}
	if filePath != "" {
		q += ` AND file_path = ?`
		args = append(args, filePath)
	}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnresolvedRef
	for rows.Next() {
		var r UnresolvedRef
		if err := rows.Scan(&r.ID, &r.FromNode, &r.ReferenceName, &r.ReferenceKind,
			&r.Line, &r.Col, &r.FilePath, &r.Language, &r.Status, &r.NameTail, &r.Candidates); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetEdgeByEndpoints loads one edge by endpoints + kind (for tests / inspection).
func (d *DB) GetEdgeByEndpoints(sourceID, targetID int64, kind string) (*Edge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`
		SELECT id, source_id, target_id, kind, file, line, col, provenance, metadata
		FROM edges WHERE source_id = ? AND target_id = ? AND kind = ?
	`, sourceID, targetID, kind)
	var e Edge
	var file, provenance, metadata sql.NullString
	var line, col sql.NullInt64
	if err := row.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.Kind, &file, &line, &col, &provenance, &metadata); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.File = file.String
	e.Line = int(line.Int64)
	e.Col = int(col.Int64)
	e.Provenance = provenance.String
	e.Metadata = metadata.String
	return &e, nil
}

// nodeSelectCols is the shared column list for Node scans (keeps SELECT/scan aligned).
const nodeSelectCols = `id, kind, name, file, line, end_line, body, language,
	qualified_name, signature, docstring, start_column, end_column,
	visibility, is_exported, return_type`

// DeleteUnresolvedRef removes one unresolved_refs row (resolved successfully).
func (d *DB) DeleteUnresolvedRef(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`DELETE FROM unresolved_refs WHERE id = ?`, id)
	return err
}

// MarkUnresolvedFailed parks a ref as failed so a later pass can retry.
func (d *DB) MarkUnresolvedFailed(id int64, nameTail string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`
		UPDATE unresolved_refs SET status = 'failed', name_tail = ? WHERE id = ?
	`, nameTail, id)
	return err
}

// GetNodesByFile returns all nodes defined in a file path.
func (d *DB) GetNodesByFile(file string) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`SELECT `+nodeSelectCols+` FROM nodes WHERE file = ?`, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNodeByName finds nodes by name (exact match).
func (d *DB) GetNodeByName(name string) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT `+nodeSelectCols+`
		FROM nodes WHERE name = ?
	`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNodeByID loads one node by primary key.
func (d *DB) GetNodeByID(id int64) (*Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`SELECT `+nodeSelectCols+` FROM nodes WHERE id = ?`, id)
	return scanOneNode(row)
}

// GetNodesByKind returns all nodes of a given kind (for whole-graph synthesis passes).
func (d *DB) GetNodesByKind(kind string) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`SELECT `+nodeSelectCols+` FROM nodes WHERE kind = ?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetIncomingEdges returns edges targeting nodeID, optionally filtered by kinds.
func (d *DB) GetIncomingEdges(nodeID int64, kinds []string) ([]Edge, error) {
	return d.listEdges(`target_id = ?`, nodeID, kinds)
}

// GetOutgoingEdges returns edges originating at nodeID, optionally filtered by kinds.
func (d *DB) GetOutgoingEdges(nodeID int64, kinds []string) ([]Edge, error) {
	return d.listEdges(`source_id = ?`, nodeID, kinds)
}

func (d *DB) listEdges(endpointClause string, nodeID int64, kinds []string) ([]Edge, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT id, source_id, target_id, kind, file, line, col, provenance, metadata FROM edges WHERE ` + endpointClause
	args := []interface{}{nodeID}
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = "?"
			args = append(args, k)
		}
		q += ` AND kind IN (` + strings.Join(ph, ",") + `)`
	}
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		var file, provenance, metadata sql.NullString
		var line, col sql.NullInt64
		if err := rows.Scan(&e.ID, &e.SourceID, &e.TargetID, &e.Kind, &file, &line, &col, &provenance, &metadata); err != nil {
			return nil, err
		}
		e.File = file.String
		e.Line = int(line.Int64)
		e.Col = int(col.Int64)
		e.Provenance = provenance.String
		e.Metadata = metadata.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteSynthesizedEdges removes edges created by synthesis passes so a re-run
// is idempotent and doesn't keep stale dispatcher→callback links. Resolution
// heuristic edges (no synthesizedBy metadata) are left alone.
func (d *DB) DeleteSynthesizedEdges() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`
		DELETE FROM edges
		WHERE provenance = 'heuristic'
		  AND metadata IS NOT NULL
		  AND metadata LIKE '%synthesizedBy%'
	`)
	return err
}

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
const structuralEdgeSQL = `('calls','references','bridge')`

// GetCallers returns nodes that call/reference the given node ID.
// Includes: call sites, route→handler references (reversed), bridge sources.
func (d *DB) GetCallers(nodeID int64) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type
		FROM edges e
		JOIN nodes n ON n.id = e.source_id
		WHERE e.target_id = ? AND e.kind IN `+structuralEdgeSQL+`
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetCallees returns nodes that the given node ID calls/references.
// For a route node this surfaces the handler via references edges.
func (d *DB) GetCallees(nodeID int64) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.source_id = ? AND e.kind IN `+structuralEdgeSQL+`
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetCallersWithKind is like GetCallers but also returns the edge kind per hit.
func (d *DB) GetCallersWithKind(nodeID int64) ([]NodeRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type, e.kind
		FROM edges e
		JOIN nodes n ON n.id = e.source_id
		WHERE e.target_id = ? AND e.kind IN `+structuralEdgeSQL+`
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRefs(rows)
}

// GetCalleesWithKind is like GetCallees but also returns the edge kind per hit.
func (d *DB) GetCalleesWithKind(nodeID int64) ([]NodeRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type, e.kind
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.source_id = ? AND e.kind IN `+structuralEdgeSQL+`
	`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRefs(rows)
}

// NodeRef is a node plus the edge kind that connected it.
type NodeRef struct {
	Node
	EdgeKind string
}

func scanNodeRefs(rows *sql.Rows) ([]NodeRef, error) {
	var out []NodeRef
	for rows.Next() {
		var n NodeRef
		if err := scanNodeRow(rows, &n.Node, &n.EdgeKind); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetImpact returns files that reference the given node, with match counts.
func (d *DB) GetImpact(nodeID int64) (map[string]int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT file, COUNT(*) as cnt
		FROM edges
		WHERE target_id = ? AND file IS NOT NULL
		  AND kind IN `+structuralEdgeSQL+`
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
	d.mu.RLock()
	defer d.mu.RUnlock()

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

// FileHasContentHash reports whether path is already indexed with the given content hash.
// Empty hash never matches (forces reindex when caller has no hash).
func (d *DB) FileHasContentHash(path, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	var dbHash sql.NullString
	err := d.conn.QueryRow(`SELECT content_hash FROM files WHERE path = ?`, path).Scan(&dbHash)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return dbHash.Valid && dbHash.String == hash && dbHash.String != "", nil
}

// TouchFileMeta refreshes size/mtime/content_hash without changing node_count.
// Used when content is unchanged but the filesystem timestamp moved.
func (d *DB) TouchFileMeta(path string, size int64, mtime float64, contentHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`
		UPDATE files
		SET size = ?, mtime = ?, content_hash = ?, indexed_at = ?
		WHERE path = ?
	`, size, mtime, contentHash, float64(time.Now().Unix()), path)
	return err
}

// GetFileContentHash returns the stored content hash for path, or "" if missing.
func (d *DB) GetFileContentHash(path string) (string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var dbHash sql.NullString
	err := d.conn.QueryRow(`SELECT content_hash FROM files WHERE path = ?`, path).Scan(&dbHash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !dbHash.Valid {
		return "", nil
	}
	return dbHash.String, nil
}

// ClearFile removes all nodes, edges, and unresolved_refs for a file (before reindexing).
func (d *DB) ClearFile(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Drop refs anchored on this file path first (covers any orphan status rows).
	if _, err := tx.Exec(`DELETE FROM unresolved_refs WHERE file_path = ?`, path); err != nil {
		return err
	}

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

	// Delete edges + any remaining refs by from_node (FK cascade may also do this).
	for _, id := range nodeIDs {
		if _, err := tx.Exec("DELETE FROM unresolved_refs WHERE from_node = ?", id); err != nil {
			return err
		}
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
	d.mu.RLock()
	defer d.mu.RUnlock()

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
	d.mu.RLock()
	defer d.mu.RUnlock()

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

// GetFileDependents returns distinct other files that have a structural edge
// into a symbol defined in filePath (who depends on this file).
func (d *DB) GetFileDependents(filePath string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT DISTINCT src.file
		FROM edges e
		JOIN nodes tgt ON tgt.id = e.target_id
		JOIN nodes src ON src.id = e.source_id
		WHERE tgt.file = ?
		  AND e.kind IN `+structuralEdgeSQL+`
		  AND src.file != ?
		ORDER BY src.file
	`, filePath, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err == nil && f != "" {
			files = append(files, f)
		}
	}
	return files, rows.Err()
}

// DeleteFile removes a file and its nodes/edges from the index.
func (d *DB) DeleteFile(path string) error {
	return d.ClearFile(path)
}

// GetImportTargetNames returns module/symbol names imported by a source file.
func (d *DB) GetImportTargetNames(filePath string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT DISTINCT n.name
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.kind = 'imports' AND e.file = ?
	`, filePath)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	return names, rows.Err()
}

// FindImporters finds files that import the given package.
func (d *DB) FindImporters(targetPkg string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

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
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	safe := escapeFTS5Query(query)
	if safe == "" {
		return nil, nil
	}

	rows, err := d.conn.Query(`
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
