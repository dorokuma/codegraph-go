package db

import (
	"database/sql"
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
var inboundParkKinds = []string{EdgeCalls, EdgeReferences, EdgeBridge, EdgeExtends, EdgeImplements}

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
//
// The whole read+write sequence runs in ONE transaction with the write lock
// held from the first read to commit (ReplaceFileIndex's single-transaction
// precedent). The previous form released d.mu between reading an inbound
// edge and inserting its ref (per-ref autocommit writes), so a concurrent
// ReplaceFileIndex of the SOURCE file could cascade-delete the parked ref's
// from_node inside that window and fail the insert with a FOREIGN KEY error
// (M7). Holding d.mu across the snapshot and the inserts makes parking
// atomic with respect to every in-process writer, the batch commits once
// instead of once per ref, and a mid-park failure rolls back the whole park
// instead of leaving a partial one behind.
func (d *DB) ParkInboundRefsForFile(file string) error {
	if file == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("park inbound refs begin: %w", err)
	}
	defer tx.Rollback()

	type parkTarget struct {
		id   int64
		kind string
		name string
	}
	// Snapshot this file's parkable targets in id-paginated chunks
	// (ForEachNodeByFileLight's bounding) so memory stays bounded on files
	// with many nodes; file/module targets never park.
	const targetBatchSize = 1000
	var lastID int64
	for {
		rows, err := tx.Query(`
			SELECT id, kind, name FROM nodes
			WHERE file = ? AND id > ? ORDER BY id LIMIT ?
		`, file, lastID, targetBatchSize)
		if err != nil {
			return fmt.Errorf("park inbound refs targets: %w", err)
		}
		var targets []parkTarget
		for rows.Next() {
			var t parkTarget
			if err := rows.Scan(&t.id, &t.kind, &t.name); err != nil {
				rows.Close()
				return fmt.Errorf("park inbound refs targets: %w", err)
			}
			targets = append(targets, t)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("park inbound refs targets: %w", err)
		}
		rows.Close()
		if len(targets) == 0 {
			break
		}
		for _, t := range targets {
			lastID = t.id
			if t.kind == KindFile || t.kind == "module" {
				continue
			}
			if err := parkInboundEdgesForNode(tx, t.id, t.name, file); err != nil {
				return err
			}
		}
		if len(targets) < targetBatchSize {
			break
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("park inbound refs commit: %w", err)
	}
	return nil
}

// parkInboundEdgesForNode inserts pending refs for one target node's inbound
// edges of parkable kinds, skipping edges whose source is gone or lives in
// the same file (same-file edges are rebuilt by the owning file's index).
// Runs on the caller's transaction; see ParkInboundRefsForFile for the
// locking and transaction contract. The insert statement is the same
// upsert InsertUnresolvedRef uses, so re-parking an existing pending/failed
// row refreshes it to pending without resetting its attempts count.
func parkInboundEdgesForNode(tx *sql.Tx, targetID int64, targetName, file string) error {
	ph := make([]string, len(inboundParkKinds))
	args := make([]interface{}, 0, len(inboundParkKinds)+1)
	args = append(args, targetID)
	for i, k := range inboundParkKinds {
		ph[i] = "?"
		args = append(args, k)
	}
	rows, err := tx.Query(`SELECT source_id, kind, file, line, col FROM edges
		WHERE target_id = ? AND kind IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return fmt.Errorf("park inbound refs edges: %w", err)
	}
	type inboundEdge struct {
		sourceID  int64
		kind      string
		file      string
		line, col int
	}
	var edges []inboundEdge
	for rows.Next() {
		var e inboundEdge
		var f sql.NullString
		var line, col sql.NullInt64
		if err := rows.Scan(&e.sourceID, &e.kind, &f, &line, &col); err != nil {
			rows.Close()
			return fmt.Errorf("park inbound refs edges: %w", err)
		}
		e.file = f.String
		e.line = int(line.Int64)
		e.col = int(col.Int64)
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("park inbound refs edges: %w", err)
	}
	rows.Close()

	for _, e := range edges {
		var srcFile, srcLang sql.NullString
		err := tx.QueryRow(`SELECT file, language FROM nodes WHERE id = ?`, e.sourceID).Scan(&srcFile, &srcLang)
		if err == sql.ErrNoRows {
			continue // source node already gone: nothing to rebuild
		}
		if err != nil {
			return fmt.Errorf("park inbound refs source: %w", err)
		}
		if !srcFile.Valid || srcFile.String == file {
			continue
		}
		refFile := e.file
		if refFile == "" {
			refFile = srcFile.String
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
		`, e.sourceID, targetName, e.kind, e.line, e.col,
			refFile, srcLang.String, "pending", refNameTail(targetName), ""); err != nil {
			return fmt.Errorf("park inbound refs insert: %w", err)
		}
	}
	return nil
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

// UnresolvedRefAttempts reads the attempts counter of one unresolved_refs
// row. attempts is not part of the UnresolvedRef list results (list callers
// never branch on it); this serves tests and audit tooling over abandoned
// rows.
func (d *DB) UnresolvedRefAttempts(id int64) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var attempts int
	err := d.conn.QueryRow(`SELECT attempts FROM unresolved_refs WHERE id = ?`, id).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("unresolved_ref attempts: %w", err)
	}
	return attempts, nil
}

// DeleteUnresolvedRef removes one unresolved_refs row (resolved successfully).
func (d *DB) DeleteUnresolvedRef(id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(`DELETE FROM unresolved_refs WHERE id = ?`, id)
	return err
}

// ResolvedRef is one resolved unresolved_ref awaiting persistence: the edge
// to upsert plus the id of the pending ref row to delete.
type ResolvedRef struct {
	Edge         Edge
	UnresolvedID int64
}

// ApplyResolvedRefs persists a batch of resolved refs inside ONE transaction:
// per entry it upserts the edge (same statement and conflict key as
// UpsertEdge) and deletes the pending unresolved_ref (same statement as
// DeleteUnresolvedRef). The per-ref pair used to run as two implicit
// autocommit transactions, so a full-table ResolveAll over a large backlog
// committed once per write; the batched form commits once per batch. On any
// error the whole batch rolls back — the caller falls back to per-ref writes
// to isolate the offending row — so the final per-ref state is identical to
// the per-ref path.
func (d *DB) ApplyResolvedRefs(batch []ResolvedRef) error {
	if len(batch) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for i := range batch {
		e := &batch[i].Edge
		if _, err := tx.Exec(`
			INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, target_id, kind, line, col) DO UPDATE SET
				col = excluded.col,
				file = excluded.file,
				provenance = excluded.provenance,
				metadata = excluded.metadata
		`, e.SourceID, e.TargetID, e.Kind, e.File, e.Line, e.Col, e.Provenance, e.Metadata); err != nil {
			return fmt.Errorf("resolve batch upsert edge: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM unresolved_refs WHERE id = ?`, batch[i].UnresolvedID); err != nil {
			return fmt.Errorf("resolve batch delete ref %d: %w", batch[i].UnresolvedID, err)
		}
	}
	return tx.Commit()
}

// MarkUnresolvedFailed records one failed resolution attempt for a ref: it
// bumps attempts, parks the row as 'failed' so a later pass can retry, and —
// once the attempt count reaches maxAttempts — parks it as 'abandoned'
// instead: the row is kept for audit/statistics but the retry queries
// (status 'pending'/'failed') no longer select it. An empty nameTail is
// stored as-is; callers backfill it from the reference name. A missing row
// is a no-op (the ref was resolved or deleted meanwhile). Reports whether
// this call abandoned the row.
func (d *DB) MarkUnresolvedFailed(id int64, nameTail string, maxAttempts int) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var status string
	err := d.conn.QueryRow(`
		UPDATE unresolved_refs SET
			attempts = attempts + 1,
			status = CASE WHEN attempts + 1 >= ? THEN 'abandoned' ELSE 'failed' END,
			name_tail = ?
		WHERE id = ?
		RETURNING status
	`, maxAttempts, nameTail, id).Scan(&status)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mark unresolved_ref failed: %w", err)
	}
	return status == "abandoned", nil
}

// getNodesByFileCap bounds GetNodesByFile results to prevent unbounded reads.
// Test-only mutation: tests that change this value must restore it and must
// not run in parallel with other tests in this package.
