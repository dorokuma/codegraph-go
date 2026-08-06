package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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

// InsertFact stores a new fact. contentHash is a SHA-256 hex string.
// The caller must already have computed it. Returns the new row ID and nil.
func (d *DB) InsertFact(f *Fact) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().Unix()
	result, err := d.conn.Exec(`
		INSERT INTO facts (target_file, target_symbol, target_line, content, content_hash,
			author, status, superseded_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, f.TargetFile, nullString(f.TargetSymbol), nullInt(f.TargetLine),
		f.Content, f.ContentHash, nullString(f.Author),
		statusOrDefault(f.Status), nullInt64(f.SupersededBy), now, now)
	if err != nil {
		return 0, fmt.Errorf("insert fact: %w", err)
	}
	return result.LastInsertId()
}

// GetFactByHash looks up a fact by its content hash. Returns nil when not found.
func (d *DB) GetFactByHash(hash string) (*Fact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`
		SELECT id, target_file, target_symbol, target_line, content, content_hash,
			author, status, COALESCE(superseded_by,0), created_at, updated_at
		FROM facts WHERE content_hash = ?
	`, hash)
	return scanFact(row)
}

// GetFactsByTarget returns all facts for a given target_file and optionally target_symbol.
// Pass symbol="" to ignore symbol filter.
func (d *DB) GetFactsByTarget(file, symbol string) ([]Fact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT id, target_file, target_symbol, target_line, content, content_hash,
		author, status, COALESCE(superseded_by,0), created_at, updated_at
		FROM facts WHERE target_file = ?`
	args := []interface{}{file}
	if symbol != "" {
		q += ` AND target_symbol = ?`
		args = append(args, symbol)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFacts(rows)
}

// SearchFacts searches facts by content substring (case-insensitive LIKE),
// optionally filtered by target_file, target_symbol, status, and max rows.
// status "" returns all statuses.
func (d *DB) SearchFacts(query, file, symbol, status string, max int) ([]Fact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if max <= 0 {
		max = 20
	}
	var wheres []string
	var args []interface{}
	if query != "" {
		wheres = append(wheres, `content LIKE ?`)
		args = append(args, "%"+query+"%")
	}
	if file != "" {
		wheres = append(wheres, `target_file = ?`)
		args = append(args, file)
	}
	if symbol != "" {
		wheres = append(wheres, `target_symbol = ?`)
		args = append(args, symbol)
	}
	if status != "" && status != "all" {
		wheres = append(wheres, `status = ?`)
		args = append(args, status)
	}
	q := `SELECT id, target_file, target_symbol, target_line, content, content_hash,
		author, status, COALESCE(superseded_by,0), created_at, updated_at
		FROM facts`
	if len(wheres) > 0 {
		q += ` WHERE ` + strings.Join(wheres, ` AND `)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, max)
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFacts(rows)
}

// SupersedeFact marks oldID as superseded and links it to newID (the replacing fact).
// Both IDs must exist and oldID must be active.
func (d *DB) SupersedeFact(oldID, newID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		UPDATE facts SET status = 'superseded', superseded_by = ?, updated_at = ?
		WHERE id = ? AND status = 'active'
	`, newID, time.Now().Unix(), oldID)
	if err != nil {
		return fmt.Errorf("supersede fact: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("fact %d not found or not active", oldID)
	}
	return nil
}

// RetractFact marks a fact as retracted (agent later determined it was wrong).
func (d *DB) RetractFact(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		UPDATE facts SET status = 'retracted', updated_at = ?
		WHERE id = ? AND status = 'active'
	`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("retract fact: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("fact %d not found or not active", id)
	}
	return nil
}

// scanFact scans a single fact row. Returns nil when the row is sql.ErrNoRows.
func scanFact(row *sql.Row) (*Fact, error) {
	var f Fact
	var targetSymbol, author sql.NullString
	var targetLine, supersededBy sql.NullInt64
	if err := row.Scan(&f.ID, &f.TargetFile, &targetSymbol, &targetLine,
		&f.Content, &f.ContentHash, &author, &f.Status, &supersededBy,
		&f.CreatedAt, &f.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	f.TargetSymbol = targetSymbol.String
	f.TargetLine = int(targetLine.Int64)
	f.Author = author.String
	f.SupersededBy = supersededBy.Int64
	return &f, nil
}

// scanFacts scans fact rows.
func scanFacts(rows *sql.Rows) ([]Fact, error) {
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		var targetSymbol, author sql.NullString
		var targetLine, supersededBy sql.NullInt64
		if err := rows.Scan(&f.ID, &f.TargetFile, &targetSymbol, &targetLine,
			&f.Content, &f.ContentHash, &author, &f.Status, &supersededBy,
			&f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.TargetSymbol = targetSymbol.String
		f.TargetLine = int(targetLine.Int64)
		f.Author = author.String
		f.SupersededBy = supersededBy.Int64
		out = append(out, f)
	}
	return out, rows.Err()
}

// statusOrDefault returns "active" when s is empty.
func statusOrDefault(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

// nullString returns nil for empty strings so SQLite stores NULL.
func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

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
func (d *DB) UpsertEdge(e *Edge) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result, err := d.conn.Exec(`
		INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, kind, line, col) DO UPDATE SET
			col = excluded.col,
			file = excluded.file,
			provenance = excluded.provenance,
			metadata = excluded.metadata
	`, e.SourceID, e.TargetID, e.Kind, e.File, e.Line, e.Col, e.Provenance, e.Metadata)
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
// NOTE: result.LastInsertId() is unreliable after ON CONFLICT DO UPDATE —
// it returns 0 on conflict. Current callers discard the return value.
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

// ListUnresolvedRefsByFiles returns unresolved_refs rows for multiple file
// paths, optionally filtered by status (empty string = no filter).
// This avoids loading all unresolved refs into memory and filtering in Go.
func (d *DB) ListUnresolvedRefsByFiles(files []string, status string) ([]UnresolvedRef, error) {
	if len(files) == 0 {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	q := `SELECT id, from_node, reference_name, reference_kind, line, col,
		file_path, language, status, name_tail, COALESCE(candidates,'')
		FROM unresolved_refs WHERE file_path IN (`
	ph := make([]string, len(files))
	args := make([]interface{}, 0, len(files)+1)
	for i, f := range files {
		ph[i] = "?"
		args = append(args, f)
	}
	q += strings.Join(ph, ",") + `)`
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

// ListUnresolvedRefsByNames returns unresolved_refs rows whose reference_name
// or name_tail exactly matches one of names, optionally filtered by status
// (empty statuses = no status filter). The name filter is pushed down to SQL
// instead of loading every row and matching in Go (F1), reusing
// idx_unresolved_name and idx_unresolved_failed_tail. Names are chunked so
// the IN lists stay under SQLite's variable-number ceiling; a row matching
// both branches is returned once.
func (d *DB) ListUnresolvedRefsByNames(names []string, statuses []string) ([]UnresolvedRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	const maxNamesPerChunk = 400 // 2 IN lists x 400 + 1 status arg stays under SQLite's 999-var cap
	selectCols := `SELECT id, from_node, reference_name, reference_kind, line, col,
		file_path, language, status, name_tail, COALESCE(candidates,'')
		FROM unresolved_refs`

	run := func(where string, args []interface{}) ([]UnresolvedRef, error) {
		rows, err := d.conn.Query(where, args...)
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

	var out []UnresolvedRef
	seen := make(map[int64]bool)
	add := func(rows []UnresolvedRef) {
		for _, r := range rows {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
	}

	if len(statuses) == 0 {
		statuses = []string{""}
	}
	for _, status := range statuses {
		// Split the name match into two index-friendly branches instead of one
		// OR query: reference_name IN (...) can use idx_unresolved_name, and
		// name_tail IN (...) can use idx_unresolved_failed_tail for failed
		// refs. Known statuses are inlined as literals so the partial index
		// (WHERE status = 'failed') stays usable by the planner.
		prefix := ""
		var statusArgs []interface{}
		switch status {
		case "pending", "failed":
			prefix = "status = '" + status + "' AND "
		case "":
			// no status filter
		default:
			prefix = "status = ? AND "
			statusArgs = []interface{}{status}
		}
		for start := 0; start < len(names); start += maxNamesPerChunk {
			end := start + maxNamesPerChunk
			if end > len(names) {
				end = len(names)
			}
			chunk := names[start:end]
			ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			args := make([]interface{}, 0, len(chunk)+len(statusArgs))
			args = append(args, statusArgs...)
			for _, n := range chunk {
				args = append(args, n)
			}
			nameRows, err := run(selectCols+" WHERE "+prefix+`reference_name IN (`+ph+`)`, args)
			if err != nil {
				return nil, err
			}
			add(nameRows)
			tailRows, err := run(selectCols+" WHERE "+prefix+`name_tail IN (`+ph+`)`, args)
			if err != nil {
				return nil, err
			}
			add(tailRows)
		}
	}
	return out, nil
}

// ListUnresolvedRefsEmptyTail returns unresolved_refs rows whose name_tail is
// empty (historical/anomalous rows that store the full qualified name in
// reference_name), optionally filtered by status (empty statuses = no status
// filter). The F1 SQL pushdown in ListUnresolvedRefsByNames matches stored
// name_tail exactly and cannot see these rows through their tail segment;
// callers re-apply nameTail(reference_name) matching in Go (S2). Empty-tail
// rows are rare, so this is deliberately a simple filtered scan (it can use
// idx_unresolved_status); the normal extraction path always writes a non-empty
// name_tail and is never touched by it.
func (d *DB) ListUnresolvedRefsEmptyTail(statuses []string) ([]UnresolvedRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(statuses) == 0 {
		statuses = []string{""}
	}
	var out []UnresolvedRef
	seen := make(map[int64]bool)
	for _, status := range statuses {
		prefix := ""
		var statusArgs []interface{}
		switch status {
		case "pending", "failed":
			prefix = "status = '" + status + "' AND "
		case "":
			// no status filter
		default:
			prefix = "status = ? AND "
			statusArgs = []interface{}{status}
		}
		rows, err := d.conn.Query(`
			SELECT id, from_node, reference_name, reference_kind, line, col,
				file_path, language, status, name_tail, COALESCE(candidates,'')
			FROM unresolved_refs WHERE `+prefix+`name_tail = ''`, statusArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r UnresolvedRef
			if err := rows.Scan(&r.ID, &r.FromNode, &r.ReferenceName, &r.ReferenceKind,
				&r.Line, &r.Col, &r.FilePath, &r.Language, &r.Status, &r.NameTail, &r.Candidates); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
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
	return d.GetNodesByFileContext(context.Background(), file)
}

// GetNodesByFileContext is the context-aware variant of GetNodesByFile.
func (d *DB) GetNodesByFileContext(ctx context.Context, file string) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `SELECT `+nodeSelectCols+` FROM nodes WHERE file = ?`, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetNodeByName finds nodes by name (exact match).
func (d *DB) GetNodeByName(name string) ([]Node, error) {
	return d.GetNodeByNameContext(context.Background(), name)
}

// GetNodeByNameContext is the context-aware variant of GetNodeByName.
func (d *DB) GetNodeByNameContext(ctx context.Context, name string) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `
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

// ReplaceSynthesizedEdges atomically replaces all synthesized edges in one
// transaction (F2): it deletes the old heuristic/synthesizedBy edges and then
// upserts the new edge set. Previously the delete and the per-edge upserts ran
// in separate autocommit transactions, so a mid-batch failure left a partially
// cleared graph and, between delete and first upsert, a window where the
// synthesized edges were missing entirely. On any error the whole batch rolls
// back and the old synthesized edges stay intact.
func (d *DB) ReplaceSynthesizedEdges(edges []Edge) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		DELETE FROM edges
		WHERE provenance = 'heuristic'
		  AND metadata IS NOT NULL
		  AND metadata LIKE '%synthesizedBy%'
	`); err != nil {
		return fmt.Errorf("replace synthesized edges delete: %w", err)
	}
	for _, e := range edges {
		if _, err := tx.Exec(`
			INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, target_id, kind, line, col) DO UPDATE SET
				col = excluded.col,
				file = excluded.file,
				provenance = excluded.provenance,
				metadata = excluded.metadata
		`, e.SourceID, e.TargetID, e.Kind, e.File, e.Line, e.Col, e.Provenance, e.Metadata); err != nil {
			return fmt.Errorf("replace synthesized edges upsert: %w", err)
		}
	}
	return tx.Commit()
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
// A3: multiple call-site edges to the same node exist now; callers are the
// DISTINCT source nodes (use GetIncomingEdges for per-call-site rows).
func (d *DB) GetCallers(nodeID int64) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT DISTINCT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
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
// A3: distinct callee nodes (per-call-site rows via GetOutgoingEdges).
func (d *DB) GetCallees(nodeID int64) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.Query(`
		SELECT DISTINCT n.id, n.kind, n.name, n.file, n.line, n.end_line, n.body, n.language,
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
	return d.GetCallersWithKindContext(context.Background(), nodeID)
}

// GetCallersWithKindContext is the context-aware variant of GetCallersWithKind.
func (d *DB) GetCallersWithKindContext(ctx context.Context, nodeID int64) ([]NodeRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `
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
	return d.GetCalleesWithKindContext(context.Background(), nodeID)
}

// GetCalleesWithKindContext is the context-aware variant of GetCalleesWithKind.
func (d *DB) GetCalleesWithKindContext(ctx context.Context, nodeID int64) ([]NodeRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `
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
// A3: COUNT(*) counts call sites (one source may hit the node many times);
// for distinct referencing files use GetFileDependents.
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
	return result, rows.Err()
}

// FileNeedsReindex checks if a file needs reindexing based on size and mtime.
// mtime is milliseconds since epoch (UnixMilli) stored as REAL; callers must
// pass the same precision they stored (see TouchFileMeta).
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
// mtime is milliseconds since epoch (UnixMilli) stored as REAL.
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

// GetFileNodeCount returns the stored node_count for path (0 when missing).
func (d *DB) GetFileNodeCount(path string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var n sql.NullInt64
	err := d.conn.QueryRow(`SELECT node_count FROM files WHERE path = ?`, path).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(n.Int64), nil
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
// Foreign keys are ON via DSN pragma; deleting nodes cascades to edges and
// unresolved_refs (by from_node FK). unresolved_refs.file_path has no FK so we
// delete it explicitly.
func (d *DB) ClearFile(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Drop refs anchored on this file path (file_path column has no FK).
	if _, err := tx.Exec(`DELETE FROM unresolved_refs WHERE file_path = ?`, path); err != nil {
		return fmt.Errorf("clear file unresolved_refs: %w", err)
	}

	// Delete nodes for this file. CASCADE deletes edges (source_id/target_id FK)
	// and unresolved_refs (from_node FK).
	if _, err := tx.Exec(`DELETE FROM nodes WHERE file = ?`, path); err != nil {
		return fmt.Errorf("clear file nodes: %w", err)
	}

	// Delete file record
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
		return fmt.Errorf("clear file record: %w", err)
	}

	return tx.Commit()
}

// ReplaceFileIndex atomically replaces all index rows for one file inside a
// single SQLite transaction: clears the file's old rows (unresolved_refs by
// file_path, nodes with cascaded edges/refs), inserts the new nodes/edges/
// refs, and upserts the file record. Returns the inserted node ids aligned
// with the nodes slice (A4: parse failures must never leave a half-written
// file — the caller only invokes this after extraction succeeded).
//
// Edges and refs may reference nodes of this same batch before they exist by
// using negative placeholder ids: -(i+1) refers to nodes[i]. Positive ids are
// used verbatim (e.g. module nodes created before the transaction).
//
// moduleNodes (F5): module nodes are upserted inside this same transaction
// (INSERT ... ON CONFLICT DO NOTHING, then SELECT id, reusing UpsertNode's
// conflict key), so a failed batch rolls them back and leaves no orphaned
// module nodes. Edges may reference them with placeholders
// -(len(nodes)+i+1) for moduleNodes[i]. Existing callers that pass no module
// nodes are unaffected.
func (d *DB) ReplaceFileIndex(store string, nodes []Node, edges []Edge, refs []UnresolvedRef, fileRecord *FileRecord, moduleNodes ...Node) ([]int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Module nodes first (F5): edges below may reference them, and the FK on
	// edges requires the target rows to exist within the transaction.
	moduleIDs := make([]int64, len(moduleNodes))
	for i, n := range moduleNodes {
		// S3/F5: on conflict refresh language (excluded.language), matching the
		// old UpsertNode DO UPDATE semantics the pre-batch path used — a
		// re-imported module from another language must not keep stale meta.
		if _, err := tx.Exec(`
			INSERT INTO nodes (kind, name, file, line, language)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(file, line, kind, name) DO UPDATE SET
				language = excluded.language
		`, n.Kind, n.Name, n.File, n.Line, n.Language); err != nil {
			return nil, fmt.Errorf("replace file insert module node %s: %w", n.Name, err)
		}
		var id int64
		if err := tx.QueryRow(`
			SELECT id FROM nodes WHERE file = ? AND line = ? AND kind = ? AND name = ?
		`, n.File, n.Line, n.Kind, n.Name).Scan(&id); err != nil {
			return nil, fmt.Errorf("replace file module node id lookup %s: %w", n.Name, err)
		}
		moduleIDs[i] = id
	}

	// Drop refs anchored on this file path (file_path column has no FK).
	if _, err := tx.Exec(`DELETE FROM unresolved_refs WHERE file_path = ?`, store); err != nil {
		return nil, fmt.Errorf("replace file unresolved_refs: %w", err)
	}
	// Delete nodes for this file. CASCADE deletes edges (source_id/target_id
	// FK) and unresolved_refs (from_node FK).
	if _, err := tx.Exec(`DELETE FROM nodes WHERE file = ?`, store); err != nil {
		return nil, fmt.Errorf("replace file nodes: %w", err)
	}

	ids := make([]int64, len(nodes))
	for i, n := range nodes {
		body := TruncateBody(n.Body)
		exported := 0
		if n.IsExported {
			exported = 1
		}
		if _, err := tx.Exec(`
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
			nullInt(n.StartColumn), nullInt(n.EndColumn), n.Visibility, exported, n.ReturnType); err != nil {
			return nil, fmt.Errorf("replace file insert node %s: %w", n.Name, err)
		}
		var id int64
		if err := tx.QueryRow(`
			SELECT id FROM nodes WHERE file = ? AND line = ? AND kind = ? AND name = ?
		`, n.File, n.Line, n.Kind, n.Name).Scan(&id); err != nil {
			return nil, fmt.Errorf("replace file node id lookup %s: %w", n.Name, err)
		}
		ids[i] = id
	}
	// Map batch placeholder ids (negative) to the real inserted ids. Values in
	// the batch-node range -(1..len(nodes)) map to nodes; deeper negatives map
	// to moduleNodes (F5).
	realID := func(v int64) int64 {
		if v < 0 {
			idx := -(v + 1)
			if idx < int64(len(ids)) {
				return ids[idx]
			}
			return moduleIDs[idx-int64(len(ids))]
		}
		return v
	}
	for _, e := range edges {
		if _, err := tx.Exec(`
			INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, target_id, kind, line, col) DO UPDATE SET
				col = excluded.col,
				file = excluded.file,
				provenance = excluded.provenance,
				metadata = excluded.metadata
		`, realID(e.SourceID), realID(e.TargetID), e.Kind, e.File, e.Line, e.Col, e.Provenance, e.Metadata); err != nil {
			return nil, fmt.Errorf("replace file insert edge: %w", err)
		}
	}
	for _, r := range refs {
		status := r.Status
		if status == "" {
			status = "pending"
		}
		if _, err := tx.Exec(`
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
		`, realID(r.FromNode), r.ReferenceName, r.ReferenceKind, r.Line, r.Col,
			r.FilePath, r.Language, status, r.NameTail, r.Candidates); err != nil {
			return nil, fmt.Errorf("replace file insert unresolved_ref: %w", err)
		}
	}
	if fileRecord != nil {
		if _, err := tx.Exec(`
			INSERT INTO files (path, size, mtime, indexed_at, content_hash, language, node_count)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET
				size = excluded.size,
				mtime = excluded.mtime,
				indexed_at = excluded.indexed_at,
				content_hash = excluded.content_hash,
				language = excluded.language,
				node_count = excluded.node_count
		`, fileRecord.Path, fileRecord.Size, fileRecord.Mtime, float64(time.Now().Unix()),
			fileRecord.ContentHash, fileRecord.Language, fileRecord.NodeCount); err != nil {
			return nil, fmt.Errorf("replace file record: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
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
	return d.GetStatsContext(context.Background())
}

// GetStatsContext is the context-aware variant of GetStats.
func (d *DB) GetStatsContext(ctx context.Context) (*Stats, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	s := &Stats{KindCounts: make(map[string]int)}

	if err := d.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes").Scan(&s.NodeCount); err != nil {
		return nil, fmt.Errorf("count nodes: %w", err)
	}
	if err := d.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM edges").Scan(&s.EdgeCount); err != nil {
		return nil, fmt.Errorf("count edges: %w", err)
	}
	if err := d.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&s.FileCount); err != nil {
		return nil, fmt.Errorf("count files: %w", err)
	}

	rows, err := d.conn.QueryContext(ctx, "SELECT kind, COUNT(*) FROM nodes GROUP BY kind")
	if err != nil {
		return nil, fmt.Errorf("count by kind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var cnt int
		if err := rows.Scan(&kind, &cnt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan kind count: %w", err)
		}
		s.KindCounts[kind] = cnt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows kind count: %w", err)
	}

	return s, nil
}

// ListFiles returns all indexed files (capped at 100000 to avoid unbounded
// memory usage on very large databases).
func (d *DB) ListFiles() ([]string, error) {
	return d.ListFilesContext(context.Background())
}

// ListFilesContext is the context-aware variant of ListFiles.
func (d *DB) ListFilesContext(ctx context.Context) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, "SELECT path FROM files ORDER BY path LIMIT 100000")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("list files scan: %w", err)
		}
		files = append(files, p)
	}
	return files, rows.Err()
}

// ListFilesInDir returns all indexed files whose parent directory matches dir.
func (d *DB) ListFilesInDir(dir string) ([]string, error) {
	return d.ListFilesInDirContext(context.Background(), dir)
}

// ListFilesInDirContext is the context-aware variant of ListFilesInDir.
func (d *DB) ListFilesInDirContext(ctx context.Context, dir string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	dir = filepath.ToSlash(filepath.Clean(strings.TrimSpace(dir)))
	if dir == "" || dir == "." {
		// Direct children of workdir root: no slash in relative path.
		rows, err := d.conn.QueryContext(ctx, `SELECT path FROM files ORDER BY path`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var files []string
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return nil, fmt.Errorf("list files in dir scan: %w", err)
			}
			pSlash := filepath.ToSlash(p)
			if !strings.Contains(pSlash, "/") {
				files = append(files, p)
			}
		}
		return files, rows.Err()
	}

	// Escape LIKE special chars in dir so _ and % are matched literally.
	escaped := strings.NewReplacer("\\", "\\\\", "_", "\\_", "%", "\\%").Replace(dir)
	pattern := escaped + "/%"

	rows, err := d.conn.QueryContext(ctx,
		"SELECT path FROM files WHERE path LIKE ? ESCAPE '\\' ORDER BY path", pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("list files in dir scan: %w", err)
		}
		// Only include files directly in this directory, not subdirectories.
		if filepath.ToSlash(filepath.Dir(filepath.ToSlash(p))) == dir {
			files = append(files, p)
		}
	}
	return files, rows.Err()
}

// CountFilesUnderContext returns the number of indexed files whose path
// is under prefix (same directory or descendant). Prefix may be absolute
// (legacy) or workdir-relative (current storage). Empty/"." means whole index.
func (d *DB) CountFilesUnderContext(ctx context.Context, prefix string) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	prefix = filepath.ToSlash(filepath.Clean(strings.TrimSpace(prefix)))
	if prefix == "" || prefix == "." {
		var count int
		err := d.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("count files under: %w", err)
		}
		return count, nil
	}

	// Escape LIKE wildcards so path segments with _ or % match literally.
	escaped := strings.NewReplacer("\\", "\\\\", "_", "\\_", "%", "\\%").Replace(prefix)
	var count int
	err := d.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		prefix, escaped+"/%").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count files under: %w", err)
	}
	return count, nil
}

// GetFileDependents returns distinct other files that have a structural edge
// into a symbol defined in filePath (who depends on this file).
func (d *DB) GetFileDependents(filePath string) ([]string, error) {
	return d.GetFileDependentsContext(context.Background(), filePath)
}

// GetFileDependentsContext is the context-aware variant of GetFileDependents.
func (d *DB) GetFileDependentsContext(ctx context.Context, filePath string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `
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
		if err := rows.Scan(&f); err != nil {
			return nil, fmt.Errorf("GetFileDependentsContext scan: %w", err)
		}
		if f != "" {
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
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("GetImportTargetNames scan: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// FindImporters finds files that import the given package.
// Escapes _ and % in targetPkg so they are not treated as LIKE wildcards.
func (d *DB) FindImporters(targetPkg string) ([]string, error) {
	return d.FindImportersContext(context.Background(), targetPkg)
}

// FindImportersContext is the context-aware variant of FindImporters.
func (d *DB) FindImportersContext(ctx context.Context, targetPkg string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Escape _ and % for LIKE; also escape the escape char itself.
	escaped := strings.NewReplacer("\\", "\\\\", "_", "\\_", "%", "\\%").Replace(targetPkg)
	rows, err := d.conn.QueryContext(ctx, `
		SELECT DISTINCT e.file
		FROM edges e
		JOIN nodes n ON n.id = e.target_id
		WHERE e.kind = 'imports' AND (n.name = ? OR n.name LIKE ? ESCAPE '\')
	`, targetPkg, escaped+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var file string
		if err := rows.Scan(&file); err != nil {
			return nil, fmt.Errorf("find importers scan: %w", err)
		}
		files = append(files, file)
	}
	return files, rows.Err()
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
	return d.FullTextSearchContext(context.Background(), query, limit)
}

// FullTextSearchContext is the context-aware variant of FullTextSearch.
func (d *DB) FullTextSearchContext(ctx context.Context, query string, limit int) ([]Node, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}
	safe := escapeFTS5Query(query)
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
type GraphSnapshot struct {
	Nodes []Node
	Edges []Edge
	// Truncated is set when either collection hit the safety cap and was cut
	// off; consumers must treat the snapshot as approximate (A7).
	Truncated bool
}

// graphSnapshotCap bounds the rows loaded per collection to prevent OOM on
// very large indexes. Exceeding it truncates the snapshot instead of failing.
// A var (not const) so tests can lower it to exercise the truncation path.
var graphSnapshotCap = 500_000

// GetGraphSnapshot returns all nodes and edges in one call, protected by RLock.
// When the index exceeds graphSnapshotCap rows in either collection the result
// is truncated and GraphSnapshot.Truncated is set (callers report it, they
// never crash on the partial view).
func (d *DB) GetGraphSnapshot() (*GraphSnapshot, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	snap := &GraphSnapshot{}

	nodes, err := func() ([]Node, error) {
		rows, err := d.conn.Query(`SELECT `+nodeSelectCols+` FROM nodes LIMIT ?`, graphSnapshotCap+1)
		if err != nil {
			return nil, fmt.Errorf("query nodes: %w", err)
		}
		defer rows.Close()
		return scanNodes(rows)
	}()
	if err != nil {
		return nil, err
	}
	if len(nodes) > graphSnapshotCap {
		snap.Truncated = true
		nodes = nodes[:graphSnapshotCap]
	}
	snap.Nodes = nodes

	edges, err := func() ([]Edge, error) {
		rows, err := d.conn.Query(`SELECT id, source_id, target_id, kind, file, line, col, provenance, metadata FROM edges LIMIT ?`, graphSnapshotCap+1)
		if err != nil {
			return nil, fmt.Errorf("query edges: %w", err)
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
	}()
	if err != nil {
		return nil, err
	}
	if len(edges) > graphSnapshotCap {
		snap.Truncated = true
		edges = edges[:graphSnapshotCap]
	}
	snap.Edges = edges

	return snap, nil
}
