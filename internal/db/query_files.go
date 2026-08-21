package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// UpsertFile records a file's indexing status (legacy signature; extra fields empty).
func (d *DB) UpsertFile(path string, size int64, mtime float64) error {
	return d.UpsertFileRecord(&FileRecord{Path: path, Size: size, Mtime: mtime})
}

// UpsertFileRecord writes a full files row including content_hash / language / node_count.

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

// ListFiles returns all indexed files (capped at 100000 to avoid unbounded
// memory usage on very large databases).
func (d *DB) ListFiles() ([]string, error) {
	return d.ListFilesContext(context.Background())
}

// ListFilesContext is the context-aware variant of ListFiles.

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

// pattern-match candidate cap. Generous on purpose: basename collisions in
// the thousands are possible on big indexes, and candidates are re-filtered
// by fileHintMatches afterwards. A var (not const) so tests can lower it.

// pattern-match candidate cap. Generous on purpose: basename collisions in
// the thousands are possible on big indexes, and candidates are re-filtered
// by fileHintMatches afterwards. A var (not const) so tests can lower it.
var findFileCandidatesPatternLimit = 10_000

// FindFileCandidatesContext finds candidate file paths in the files table
// matching a path or basename hint without scanning the full table.

// FindFileCandidatesContext finds candidate file paths in the files table
// matching a path or basename hint without scanning the full table.
func (d *DB) FindFileCandidatesContext(ctx context.Context, hint string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	hint = strings.TrimSpace(hint)
	if hint == "" {
		return nil, nil
	}

	hClean := filepath.ToSlash(filepath.Clean(hint))
	hTrim := strings.TrimPrefix(hClean, "./")
	hBase := filepath.Base(hClean)

	exacts := map[string]bool{}
	var exactList []string
	for _, p := range []string{hint, hClean, hTrim} {
		if p != "" && !exacts[p] {
			exacts[p] = true
			exactList = append(exactList, p)
		}
	}

	seen := make(map[string]bool)
	var paths []string

	// Exact matches must take priority and cannot be evicted by broad LIKE matches.
	if len(exactList) > 0 {
		var exactWheres []string
		var exactArgs []interface{}
		for _, p := range exactList {
			exactWheres = append(exactWheres, "path = ?")
			exactArgs = append(exactArgs, p)
		}
		qExact := `SELECT DISTINCT path FROM files WHERE (` + strings.Join(exactWheres, " OR ") + `) ORDER BY path`
		rows, err := d.conn.QueryContext(ctx, qExact, exactArgs...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("find file candidates scan exact: %w", err)
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	patterns := map[string]bool{}
	if hTrim != "" && hTrim != "." && hTrim != ".." {
		patterns["%/"+escapeLikePattern(hTrim)] = true
	}
	if hBase != "" && hBase != "." && hBase != ".." {
		patterns[escapeLikePattern(hBase)] = true
		patterns["%/"+escapeLikePattern(hBase)] = true
	}

	var patWheres []string
	var patArgs []interface{}
	for pat := range patterns {
		patWheres = append(patWheres, "path LIKE ? ESCAPE '\\'")
		patArgs = append(patArgs, pat)
	}

	if len(patWheres) > 0 {
		qPat := `SELECT DISTINCT path FROM files WHERE (` + strings.Join(patWheres, " OR ") + `) ORDER BY path LIMIT ?`
		rows, err := d.conn.QueryContext(ctx, qPat, append(patArgs, findFileCandidatesPatternLimit)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("find file candidates scan pattern: %w", err)
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return paths, nil
}

// FindFileCandidates is the background context variant of FindFileCandidatesContext.

// FindFileCandidates is the background context variant of FindFileCandidatesContext.
func (d *DB) FindFileCandidates(hint string) ([]string, error) {
	return d.FindFileCandidatesContext(context.Background(), hint)
}

// ListFilesInDir returns all indexed files whose parent directory matches dir.

// ListFilesInDir returns all indexed files whose parent directory matches dir.
func (d *DB) ListFilesInDir(dir string) ([]string, error) {
	return d.ListFilesInDirContext(context.Background(), dir)
}

// ListFilesInDirContext is the context-aware variant of ListFilesInDir.

// ListFilesInDirContext is the context-aware variant of ListFilesInDir.
func (d *DB) ListFilesInDirContext(ctx context.Context, dir string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	dir = filepath.ToSlash(filepath.Clean(strings.TrimSpace(dir)))
	if dir == "" || dir == "." {
		// Direct children of workdir root: no slash in relative path.
		// Capped like ListFilesContext to avoid unbounded memory use on very
		// large databases (M4).
		rows, err := d.conn.QueryContext(ctx, `SELECT path FROM files ORDER BY path LIMIT 100000`)
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
	escaped := escapeLikePattern(dir)
	pattern := escaped + "/%"
	// Constrain to direct children in SQL (exactly one segment below dir) so
	// the LIMIT below counts only what this call can return. A bare `dir/%`
	// LIKE would pull the whole subtree — tens of thousands of nested paths
	// — and could truncate before the Go-level direct-child filter, silently
	// dropping direct children (M4-aligned cap as in ListFilesContext).
	subPattern := escaped + "/%/%"

	rows, err := d.conn.QueryContext(ctx,
		"SELECT path FROM files WHERE path LIKE ? ESCAPE '\\' AND path NOT LIKE ? ESCAPE '\\' ORDER BY path LIMIT 100000",
		pattern, subPattern)
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
		// Only include files directly in this directory, not subdirectories
		// (backstop for the SQL-level constraint above).
		if filepath.ToSlash(filepath.Dir(filepath.ToSlash(p))) == dir {
			files = append(files, p)
		}
	}
	return files, rows.Err()
}

// CountFilesUnderContext returns the number of indexed files whose path
// is under prefix (same directory or descendant). Prefix may be absolute
// (legacy) or workdir-relative (current storage). Empty/"." means whole index.

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
	escaped := escapeLikePattern(prefix)
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

// GetFileDependents returns distinct other files that have a structural edge
// into a symbol defined in filePath (who depends on this file).
func (d *DB) GetFileDependents(filePath string) ([]string, error) {
	return d.GetFileDependentsContext(context.Background(), filePath)
}

// GetFileDependentsContext is the context-aware variant of GetFileDependents.

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

// DeleteFile removes a file and its nodes/edges from the index.
func (d *DB) DeleteFile(path string) error {
	return d.ClearFile(path)
}

// GetImportTargetNames returns module/symbol names imported by a source file.

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

// FindImporters finds files that import the given package.
// Escapes _ and % in targetPkg so they are not treated as LIKE wildcards.
func (d *DB) FindImporters(targetPkg string) ([]string, error) {
	return d.FindImportersContext(context.Background(), targetPkg)
}

// FindImportersContext is the context-aware variant of FindImporters.

// FindImportersContext is the context-aware variant of FindImporters.
func (d *DB) FindImportersContext(ctx context.Context, targetPkg string) ([]string, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Escape _ and % for LIKE; also escape the escape char itself.
	escaped := escapeLikePattern(targetPkg)
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
