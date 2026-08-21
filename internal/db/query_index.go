package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

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
		// RETURNING id replaces the old INSERT-then-SELECT round trip (one
		// fewer query per row on a single-connection pool; SQLite 3.35+).
		var id int64
		if err := tx.QueryRow(`
			INSERT INTO nodes (kind, name, file, line, language)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(file, line, kind, name) DO UPDATE SET
				language = excluded.language
			RETURNING id
		`, n.Kind, n.Name, n.File, n.Line, n.Language).Scan(&id); err != nil {
			return nil, fmt.Errorf("replace file insert module node %s: %w", n.Name, err)
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
		if err := tx.QueryRow(`
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
			RETURNING id
		`, n.Kind, n.Name, n.File, n.Line, n.EndLine, body, n.Language,
			n.QualifiedName, n.Signature, n.Docstring,
			nullInt(n.StartColumn), nullInt(n.EndColumn), n.Visibility, exported, n.ReturnType).Scan(&ids[i]); err != nil {
			return nil, fmt.Errorf("replace file insert node %s: %w", n.Name, err)
		}
	}
	// Map batch placeholder ids (negative) to the real inserted ids. Values in
	// the batch-node range -(1..len(nodes)) map to nodes; deeper negatives map
	// to moduleNodes (F5). Any placeholder outside both ranges is a malformed
	// batch: it must fail with a diagnostic error (transaction rolls back)
	// instead of panicking on an out-of-range slice index.
	realID := func(v int64) (int64, error) {
		if v < 0 {
			idx := -(v + 1)
			if idx < int64(len(ids)) {
				return ids[idx], nil
			}
			mIdx := idx - int64(len(ids))
			if mIdx >= int64(len(moduleIDs)) {
				return 0, fmt.Errorf("placeholder id %d out of range: %d batch nodes, %d module nodes", v, len(ids), len(moduleIDs))
			}
			return moduleIDs[mIdx], nil
		}
		return v, nil
	}
	for _, e := range edges {
		sid, err := realID(e.SourceID)
		if err != nil {
			return nil, fmt.Errorf("replace file edge source: %w", err)
		}
		tid, err := realID(e.TargetID)
		if err != nil {
			return nil, fmt.Errorf("replace file edge target: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO edges (source_id, target_id, kind, file, line, col, provenance, metadata)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_id, target_id, kind, line, col) DO UPDATE SET
				col = excluded.col,
				file = excluded.file,
				provenance = excluded.provenance,
				metadata = excluded.metadata
		`, sid, tid, e.Kind, e.File, e.Line, e.Col, e.Provenance, e.Metadata); err != nil {
			return nil, fmt.Errorf("replace file insert edge: %w", err)
		}
	}
	for _, r := range refs {
		status := r.Status
		if status == "" {
			status = "pending"
		}
		fid, err := realID(r.FromNode)
		if err != nil {
			return nil, fmt.Errorf("replace file unresolved_ref from_node: %w", err)
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
		`, fid, r.ReferenceName, r.ReferenceKind, r.Line, r.Col,
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

// CountNodes returns the total number of nodes in the index.

// CountNodes returns the total number of nodes in the index.
func (d *DB) CountNodes() (int, error) {
	return d.CountNodesContext(context.Background())
}

// CountNodesContext is the context-aware variant of CountNodes.

// CountNodesContext is the context-aware variant of CountNodes.
func (d *DB) CountNodesContext(ctx context.Context) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var n int
	err := d.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes").Scan(&n)
	return n, err
}

// Stats returns index statistics.

// Stats returns index statistics.
type Stats struct {
	NodeCount  int
	EdgeCount  int
	FileCount  int
	KindCounts map[string]int
}

// GetStats returns index statistics.

// GetStats returns index statistics.
func (d *DB) GetStats() (*Stats, error) {
	return d.GetStatsContext(context.Background())
}

// GetStatsContext is the context-aware variant of GetStats.

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
// Test-only mutation: tests that change this value must restore it and must
// not run in parallel with other tests in this package.

// graphSnapshotCap bounds the rows loaded per collection to prevent OOM on
// very large indexes. Exceeding it truncates the snapshot instead of failing.
// Test-only mutation: tests that change this value must restore it and must
// not run in parallel with other tests in this package.
var graphSnapshotCap = 500_000

// SetGraphSnapshotCapForTest overrides graphSnapshotCap for testing and returns the previous value.

// SetGraphSnapshotCapForTest overrides graphSnapshotCap for testing and returns the previous value.
func SetGraphSnapshotCapForTest(newCap int) int {
	old := graphSnapshotCap
	graphSnapshotCap = newCap
	return old
}

// GetGraphSnapshot returns all nodes and edges in one call, protected by RLock.
// When the index exceeds graphSnapshotCap rows in either collection the result
// is truncated and GraphSnapshot.Truncated is set (callers report it, they
// never crash on the partial view).

// GetGraphSnapshot returns all nodes and edges in one call, protected by RLock.
// When the index exceeds graphSnapshotCap rows in either collection the result
// is truncated and GraphSnapshot.Truncated is set (callers report it, they
// never crash on the partial view).
func (d *DB) GetGraphSnapshot() (*GraphSnapshot, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	snap := &GraphSnapshot{}

	nodes, err := func() ([]Node, error) {
		rows, err := d.conn.Query(`SELECT `+nodeLightSelectCols+` FROM nodes LIMIT ?`, graphSnapshotCap+1)
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
