package db

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

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

// InsertFactSuperseding inserts f and, when supersedes != 0, marks that
// existing fact superseded by the new row. Both writes share one transaction
// so a failed supersede does not leave an orphan insert.

// InsertFactSuperseding inserts f and, when supersedes != 0, marks that
// existing fact superseded by the new row. Both writes share one transaction
// so a failed supersede does not leave an orphan insert.
func (d *DB) InsertFactSuperseding(f *Fact, supersedes int64) (int64, error) {
	if f == nil {
		return 0, fmt.Errorf("insert fact: nil fact")
	}
	if supersedes == 0 {
		return d.InsertFact(f)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("insert fact superseding: begin tx: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	result, err := tx.Exec(`
		INSERT INTO facts (target_file, target_symbol, target_line, content, content_hash,
			author, status, superseded_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, f.TargetFile, nullString(f.TargetSymbol), nullInt(f.TargetLine),
		f.Content, f.ContentHash, nullString(f.Author),
		statusOrDefault(f.Status), nullInt64(f.SupersededBy), now, now)
	if err != nil {
		return 0, fmt.Errorf("insert fact: %w", err)
	}
	newID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert fact id: %w", err)
	}

	var newStatus string
	err = tx.QueryRow(`SELECT status FROM facts WHERE id = ?`, newID).Scan(&newStatus)
	if err != nil {
		return 0, fmt.Errorf("insert fact superseding: lookup new fact %d: %w", newID, err)
	}
	if newStatus != "active" {
		return 0, fmt.Errorf("insert fact superseding: new fact %d is not active (status %q)", newID, newStatus)
	}

	upd, err := tx.Exec(`
		UPDATE facts SET status = 'superseded', superseded_by = ?, updated_at = ?
		WHERE id = ? AND status = 'active'
	`, newID, now, supersedes)
	if err != nil {
		return 0, fmt.Errorf("supersede fact: %w", err)
	}
	n, _ := upd.RowsAffected()
	if n == 0 {
		return 0, fmt.Errorf("fact %d not found or not active", supersedes)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("insert fact superseding: commit: %w", err)
	}
	return newID, nil
}

// GetFactByHash looks up a fact by its content hash. Returns nil when not found.

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

// GetFactByHashAndTarget looks up a fact by content hash pinned to one
// target. Same text on a different file/symbol is a different fact.

// GetFactByHashAndTarget looks up a fact by content hash pinned to one
// target. Same text on a different file/symbol is a different fact.
func (d *DB) GetFactByHashAndTarget(hash, file, symbol string) (*Fact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	row := d.conn.QueryRow(`
		SELECT id, target_file, target_symbol, target_line, content, content_hash,
			author, status, COALESCE(superseded_by,0), created_at, updated_at
		FROM facts
		WHERE content_hash = ? AND target_file = ? AND IFNULL(target_symbol, '') = IFNULL(?, '')
	`, hash, file, nullString(symbol))
	return scanFact(row)
}

// maxFactsByTarget caps GetFactsByTarget rows. A target can accumulate a
// large pile of agent facts, each with content; loading all of them just to
// show a few would balloon memory (the display layer caps separately). A var
// so tests can lower it.

// maxFactsByTarget caps GetFactsByTarget rows. A target can accumulate a
// large pile of agent facts, each with content; loading all of them just to
// show a few would balloon memory (the display layer caps separately). A var
// so tests can lower it.
var maxFactsByTarget = 500

// GetFactsByTarget returns up to maxFactsByTarget facts for a given
// target_file and optionally target_symbol, newest first, and logs when more
// exist (truncation is reported so a capped read is never mistaken for the
// full pile). Pass symbol="" to ignore symbol filter. Callers that need the
// truncation flag explicitly should use GetFactsByTargetLimited.

// GetFactsByTarget returns up to maxFactsByTarget facts for a given
// target_file and optionally target_symbol, newest first, and logs when more
// exist (truncation is reported so a capped read is never mistaken for the
// full pile). Pass symbol="" to ignore symbol filter. Callers that need the
// truncation flag explicitly should use GetFactsByTargetLimited.
func (d *DB) GetFactsByTarget(file, symbol string) ([]Fact, error) {
	facts, truncated, err := d.GetFactsByTargetLimited(file, symbol, maxFactsByTarget)
	if truncated {
		sym := ""
		if symbol != "" {
			sym = "/" + symbol
		}
		log.Printf("db: facts for target %s%s exceed the read cap (%d); returning the newest %d, more exist", file, sym, maxFactsByTarget, maxFactsByTarget)
	}
	return facts, err
}

// GetFactsByTargetLimited returns up to limit facts for a given target_file
// and optionally target_symbol, newest first, plus whether more rows exist.
// limit <= 0 falls back to maxFactsByTarget. Unlike GetFactsByTarget,
// truncation is explicit here so callers can surface it in responses.

// GetFactsByTargetLimited returns up to limit facts for a given target_file
// and optionally target_symbol, newest first, plus whether more rows exist.
// limit <= 0 falls back to maxFactsByTarget. Unlike GetFactsByTarget,
// truncation is explicit here so callers can surface it in responses.
func (d *DB) GetFactsByTargetLimited(file, symbol string, limit int) ([]Fact, bool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if limit <= 0 {
		limit = maxFactsByTarget
	}
	q := `SELECT id, target_file, target_symbol, target_line, content, content_hash,
		author, status, COALESCE(superseded_by,0), created_at, updated_at
		FROM facts WHERE target_file = ?`
	args := []interface{}{file}
	if symbol != "" {
		q += ` AND target_symbol = ?`
		args = append(args, symbol)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	facts, err := scanFacts(rows)
	if err != nil {
		return nil, false, err
	}
	truncated := len(facts) > limit
	if truncated {
		facts = facts[:limit]
	}
	return facts, truncated, nil
}

// SearchFacts searches facts by content substring (case-insensitive LIKE),
// optionally filtered by target_file, target_symbol, status, and max rows.
// status "" returns all statuses. % and _ in query are matched literally
// (escaped with ESCAPE '\') so a user search never expands into wildcards.

// SearchFacts searches facts by content substring (case-insensitive LIKE),
// optionally filtered by target_file, target_symbol, status, and max rows.
// status "" returns all statuses. % and _ in query are matched literally
// (escaped with ESCAPE '\') so a user search never expands into wildcards.
func (d *DB) SearchFacts(query, file, symbol, status string, max int) ([]Fact, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if max <= 0 {
		max = 20
	}
	var wheres []string
	var args []interface{}
	if query != "" {
		wheres = append(wheres, `content LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLikePattern(query)+"%")
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
// Both IDs must exist, oldID must be active, and newID must exist and be
// active: a supersede chain must never point at a missing or already-dead
// fact, or agents reading facts hit a broken link. Validation and the update
// run in ONE transaction so the invariant holds atomically.

// SupersedeFact marks oldID as superseded and links it to newID (the replacing fact).
// Both IDs must exist, oldID must be active, and newID must exist and be
// active: a supersede chain must never point at a missing or already-dead
// fact, or agents reading facts hit a broken link. Validation and the update
// run in ONE transaction so the invariant holds atomically.
func (d *DB) SupersedeFact(oldID, newID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return fmt.Errorf("supersede fact: begin tx: %w", err)
	}
	defer tx.Rollback()

	var newStatus string
	err = tx.QueryRow(`SELECT status FROM facts WHERE id = ?`, newID).Scan(&newStatus)
	if err == sql.ErrNoRows {
		return fmt.Errorf("supersede fact: new fact %d not found", newID)
	}
	if err != nil {
		return fmt.Errorf("supersede fact: lookup new fact %d: %w", newID, err)
	}
	if newStatus != "active" {
		return fmt.Errorf("supersede fact: new fact %d is not active (status %q)", newID, newStatus)
	}

	result, err := tx.Exec(`
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
	return tx.Commit()
}

// RetractFact marks a fact as retracted (agent later determined it was wrong).

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

// escapeLikePattern escapes LIKE wildcards (_ and %) so they match literally
// when used inside a LIKE pattern with ESCAPE '\'. Backslashes are escaped
// first so a literal backslash in the input cannot neutralize the escaping.
