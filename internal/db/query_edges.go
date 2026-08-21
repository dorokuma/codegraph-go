package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

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

// GetIncomingEdges returns edges targeting nodeID, optionally filtered by kinds.
func (d *DB) GetIncomingEdges(nodeID int64, kinds []string) ([]Edge, error) {
	return d.listEdges(`target_id = ?`, nodeID, kinds)
}

// GetOutgoingEdges returns edges originating at nodeID, optionally filtered by kinds.

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

// structuralEdgeKinds are relationship kinds that count for callers/callees/impact.
// Official CodeGraph walks calls + references (routes→handlers) + bridges.
const structuralEdgeSQL = `('calls','references','bridge')`

// graphQueryRowLimit bounds graph-query results (callers/callees/impact). Hot
// symbols can have tens of thousands of edges; without a cap a single query
// would load them all (with bodies) under RLock. Rows beyond the cap are
// silently dropped (no flag available on these signatures) — the cap is far
// above practical hotspot sizes, it bounds memory, not results. A var so
// tests can lower it.

// graphQueryRowLimit bounds graph-query results (callers/callees/impact). Hot
// symbols can have tens of thousands of edges; without a cap a single query
// would load them all (with bodies) under RLock. Rows beyond the cap are
// silently dropped (no flag available on these signatures) — the cap is far
// above practical hotspot sizes, it bounds memory, not results. A var so
// tests can lower it.
var graphQueryRowLimit = 50_000

// GetCallers returns nodes that call/reference the given node ID.
// Includes: call sites, route→handler references (reversed), bridge sources.
// A3: multiple call-site edges to the same node exist now; callers are the
// DISTINCT source nodes (use GetIncomingEdges for per-call-site rows).

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
		ORDER BY n.id LIMIT ?
	`, nodeID, graphQueryRowLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetCallees returns nodes that the given node ID calls/references.
// For a route node this surfaces the handler via references edges.
// A3: distinct callee nodes (per-call-site rows via GetOutgoingEdges).

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
		ORDER BY n.id LIMIT ?
	`, nodeID, graphQueryRowLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetCallersWithKind is like GetCallers but also returns the edge kind per hit.

// GetCallersWithKind is like GetCallers but also returns the edge kind per hit.
func (d *DB) GetCallersWithKind(nodeID int64) ([]NodeRef, error) {
	return d.GetCallersWithKindContext(context.Background(), nodeID)
}

// GetCallersWithKindContext is the context-aware variant of GetCallersWithKind.

// GetCallersWithKindContext is the context-aware variant of GetCallersWithKind.
func (d *DB) GetCallersWithKindContext(ctx context.Context, nodeID int64) ([]NodeRef, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	rows, err := d.conn.QueryContext(ctx, `
		SELECT n.id, n.kind, n.name, n.file,
			CASE WHEN e.line > 0 THEN e.line ELSE n.line END,
			n.end_line, n.body, n.language,
			n.qualified_name, n.signature, n.docstring, n.start_column, n.end_column,
			n.visibility, n.is_exported, n.return_type, e.kind
		FROM edges e
		JOIN nodes n ON n.id = e.source_id
		WHERE e.target_id = ? AND e.kind IN `+structuralEdgeSQL+`
		ORDER BY n.id, e.line LIMIT ?
	`, nodeID, graphQueryRowLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRefs(rows)
}

// GetCalleesWithKind is like GetCallees but also returns the edge kind per hit.

// GetCalleesWithKind is like GetCallees but also returns the edge kind per hit.
func (d *DB) GetCalleesWithKind(nodeID int64) ([]NodeRef, error) {
	return d.GetCalleesWithKindContext(context.Background(), nodeID)
}

// GetCalleesWithKindContext is the context-aware variant of GetCalleesWithKind.

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
		ORDER BY n.id LIMIT ?
	`, nodeID, graphQueryRowLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodeRefs(rows)
}

// NodeRef is a node plus the edge kind that connected it.

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
		LIMIT ?
	`, nodeID, graphQueryRowLimit)
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
