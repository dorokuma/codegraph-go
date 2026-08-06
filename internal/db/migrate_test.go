package db

import (
	"testing"
)

// TestPromoteEdgesNewFailureRetry verifies S3: when the promotion transaction
// fails after the RENAME (an index-creation statement errors), the rollback
// must leave edges_new in place with the data — the state recoverEdgesRebuild
// expects on the next Open — so a later retry can complete the promotion.
func TestPromoteEdgesNewFailureRetry(t *testing.T) {
	dir := t.TempDir()
	database, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Seed one edge so the promoted table has data to lose.
	src, err := database.UpsertNode(&Node{Kind: KindFile, Name: "a.go", File: "a.go", Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := database.UpsertNode(&Node{Kind: "module", Name: "m", File: "m", Language: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertEdge(&Edge{SourceID: src, TargetID: dst, Kind: EdgeCalls, File: "a.go", Line: 1}); err != nil {
		t.Fatal(err)
	}

	// Simulate a rebuild interrupted right after DROP TABLE edges: edges is
	// gone and edges_new holds the only copy of the data. The indexes moved
	// with the RENAME; drop them to reproduce a freshly built edges_new.
	if _, err := database.conn.Exec(`ALTER TABLE edges RENAME TO edges_new`); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []string{"idx_edges_source", "idx_edges_target", "idx_edges_kind", "idx_edges_provenance"} {
		if _, err := database.conn.Exec(`DROP INDEX IF EXISTS ` + idx); err != nil {
			t.Fatal(err)
		}
	}

	// Sabotage: a table holding the index's name makes the first
	// CREATE INDEX IF NOT EXISTS inside the promotion fail (SQLite rejects
	// an index name that collides with an existing table name).
	if _, err := database.conn.Exec(`CREATE TABLE idx_edges_source (x)`); err != nil {
		t.Fatal(err)
	}
	if err := database.promoteEdgesNew(); err == nil {
		t.Fatal("expected promoteEdgesNew to fail with the sabotage table in place")
	}

	// The failed transaction must have rolled the RENAME back: edges_new is
	// still the only copy and edges does not exist.
	if ok, err := database.tableExists("edges_new"); err != nil || !ok {
		t.Fatalf("edges_new must survive the failed promotion (exists=%v err=%v)", ok, err)
	}
	if ok, err := database.tableExists("edges"); err != nil || ok {
		t.Fatalf("edges must not exist after the failed promotion (exists=%v err=%v)", ok, err)
	}
	var n int
	if err := database.conn.QueryRow(`SELECT COUNT(*) FROM edges_new`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("edges_new must keep its data, count=%d err=%v", n, err)
	}

	// Clear the sabotage: the retry (next Open's recovery) succeeds and the
	// data + lookup indexes come back.
	if _, err := database.conn.Exec(`DROP TABLE idx_edges_source`); err != nil {
		t.Fatal(err)
	}
	if err := database.promoteEdgesNew(); err != nil {
		t.Fatalf("promotion retry must succeed: %v", err)
	}
	if ok, err := database.tableExists("edges"); err != nil || !ok {
		t.Fatalf("edges must exist after the retry (exists=%v err=%v)", ok, err)
	}
	if ok, err := database.tableExists("edges_new"); err != nil || ok {
		t.Fatalf("edges_new must be gone after the retry (exists=%v err=%v)", ok, err)
	}
	e, err := database.GetEdgeByEndpoints(src, dst, EdgeCalls)
	if err != nil || e == nil {
		t.Fatalf("edge must survive the promotion, e=%+v err=%v", e, err)
	}
	for _, idx := range []string{"idx_edges_source", "idx_edges_target", "idx_edges_kind", "idx_edges_provenance"} {
		var name string
		err := database.conn.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=? AND tbl_name='edges'`, idx).Scan(&name)
		if err != nil {
			t.Fatalf("index %s must be recreated on edges after the retry: %v", idx, err)
		}
	}
}
