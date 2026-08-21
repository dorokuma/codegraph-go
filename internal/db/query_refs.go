package db

import (
	"fmt"
	"strings"
)

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

// inboundParkKinds are symbol-level edges that CASCADE-delete when the
// callee node is replaced. Structural kinds (contains, imports) are rebuilt
// by the owning file and are not parked.

// inboundParkKinds are symbol-level edges that CASCADE-delete when the
// callee node is replaced. Structural kinds (contains, imports) are rebuilt
// by the owning file and are not parked.
var inboundParkKinds = []string{EdgeCalls, EdgeReferences, EdgeBridge, EdgeExtends, EdgeImplements}

// refNameTail matches extraction.NameTail (last segment after . / # @).

// refNameTail matches extraction.NameTail (last segment after . / # @).
func refNameTail(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if i := strings.LastIndexAny(name, "./#@"); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// ParkInboundRefsForFile writes pending unresolved_refs for edges that point
// at nodes in file from a different file. Call this before ReplaceFileIndex
// so CASCADE-deleted inbound edges can be rebuilt by ResolveForFiles.
// Do not call this on a deleted file: the callee is gone, inbound edges
// should disappear.

// ParkInboundRefsForFile writes pending unresolved_refs for edges that point
// at nodes in file from a different file. Call this before ReplaceFileIndex
// so CASCADE-deleted inbound edges can be rebuilt by ResolveForFiles.
// Do not call this on a deleted file: the callee is gone, inbound edges
// should disappear.
func (d *DB) ParkInboundRefsForFile(file string) error {
	if file == "" {
		return nil
	}
	return d.ForEachNodeByFileLight(file, func(target Node) error {
		if target.Kind == KindFile || target.Kind == "module" {
			return nil
		}
		incoming, err := d.GetIncomingEdges(target.ID, inboundParkKinds)
		if err != nil {
			return err
		}
		for _, e := range incoming {
			src, err := d.GetNodeByID(e.SourceID)
			if err != nil {
				return err
			}
			if src == nil || src.File == file {
				continue
			}
			refFile := e.File
			if refFile == "" {
				refFile = src.File
			}
			if _, err := d.InsertUnresolvedRef(&UnresolvedRef{
				FromNode:      e.SourceID,
				ReferenceName: target.Name,
				ReferenceKind: e.Kind,
				Line:          e.Line,
				Col:           e.Col,
				FilePath:      refFile,
				Language:      src.Language,
				Status:        "pending",
				NameTail:      refNameTail(target.Name),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountUnresolvedRefs returns how many unresolved_refs rows match status
// (empty status = all rows).

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
// The IN list is chunked so it stays under SQLite's variable-number ceiling
// (999), mirroring ListUnresolvedRefsByNames; each chunk carries the status
// filter. A row can only match one chunk (each path appears once), so no
// dedup is needed.

// ListUnresolvedRefsByFiles returns unresolved_refs rows for multiple file
// paths, optionally filtered by status (empty string = no filter).
// This avoids loading all unresolved refs into memory and filtering in Go.
// The IN list is chunked so it stays under SQLite's variable-number ceiling
// (999), mirroring ListUnresolvedRefsByNames; each chunk carries the status
// filter. A row can only match one chunk (each path appears once), so no
// dedup is needed.
func (d *DB) ListUnresolvedRefsByFiles(files []string, status string) ([]UnresolvedRef, error) {
	if len(files) == 0 {
		return nil, nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()

	const maxFilesPerChunk = 400 // 1 IN list x 400 + 1 status arg stays under SQLite's 999-var cap
	selectCols := `SELECT id, from_node, reference_name, reference_kind, line, col,
		file_path, language, status, name_tail, COALESCE(candidates,'')
		FROM unresolved_refs WHERE file_path IN (`

	var out []UnresolvedRef
	for start := 0; start < len(files); start += maxFilesPerChunk {
		end := start + maxFilesPerChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		ph := make([]string, len(chunk))
		args := make([]interface{}, 0, len(chunk)+1)
		for i, f := range chunk {
			ph[i] = "?"
			args = append(args, f)
		}
		q := selectCols + strings.Join(ph, ",") + `)`
		if status != "" {
			q += ` AND status = ?`
			args = append(args, status)
		}
		rows, err := d.conn.Query(q, args...)
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

// ListUnresolvedRefsByNames returns unresolved_refs rows whose reference_name
// or name_tail exactly matches one of names, optionally filtered by status
// (empty statuses = no status filter). The name filter is pushed down to SQL
// instead of loading every row and matching in Go (F1), reusing
// idx_unresolved_name and idx_unresolved_failed_tail. Names are chunked so
// the IN lists stay under SQLite's variable-number ceiling; a row matching
// both branches is returned once.

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

// DeleteUnresolvedRef removes one unresolved_refs row (resolved successfully).
func (d *DB) DeleteUnresolvedRef(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`DELETE FROM unresolved_refs WHERE id = ?`, id)
	return err
}

// MarkUnresolvedFailed parks a ref as failed so a later pass can retry.

// MarkUnresolvedFailed parks a ref as failed so a later pass can retry.
func (d *DB) MarkUnresolvedFailed(id int64, nameTail string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`
		UPDATE unresolved_refs SET status = 'failed', name_tail = ? WHERE id = ?
	`, nameTail, id)
	return err
}

// getNodesByFileCap bounds GetNodesByFile results to prevent unbounded reads.
// Test-only mutation: tests that change this value must restore it and must
// not run in parallel with other tests in this package.
