-- codegraph-go schema (logic version 7)
-- Step 1: official-aligned fields + unresolved_refs foundation.

-- Key/value bag for index logic version, flags, etc.
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,        -- function, class, method, variable, constant, type, struct, interface
    name TEXT NOT NULL,
    file TEXT NOT NULL,
    line INTEGER NOT NULL,
    end_line INTEGER,
    body TEXT,
    language TEXT,
    -- official-aligned optional fields (may be empty until extractors fill them)
    qualified_name TEXT,
    signature TEXT,
    docstring TEXT,
    start_column INTEGER,
    end_column INTEGER,
    visibility TEXT,
    is_exported INTEGER DEFAULT 0,
    return_type TEXT,
    UNIQUE(file, line, kind, name)
);

CREATE TABLE IF NOT EXISTS edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    target_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,        -- calls, imports, extends, implements, references, bridge
    file TEXT,
    line INTEGER NOT NULL DEFAULT 0,   -- call-site line (0 = unknown); part of uniqueness
    col INTEGER NOT NULL DEFAULT 0,    -- call-site column; part of uniqueness
    provenance TEXT,           -- exact / import / proximity / heuristic
    metadata TEXT,             -- JSON object
    -- A3: per call-site uniqueness. One source can call the same target from
    -- many lines; the old UNIQUE(source_id,target_id,kind) collapsed them.
    -- NOT NULL DEFAULT 0 keeps NULLs from bypassing the constraint.
    UNIQUE(source_id, target_id, kind, line, col)
);

CREATE TABLE IF NOT EXISTS files (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    size INTEGER,
    mtime REAL,
    indexed_at REAL,
    content_hash TEXT,
    language TEXT,
    node_count INTEGER DEFAULT 0
);

-- Pending / failed references for the resolution pass (step 2–3).
-- status: pending → resolved(deleted) | failed (kept for retry)
CREATE TABLE IF NOT EXISTS unresolved_refs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    from_node INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    reference_name TEXT NOT NULL,
    reference_kind TEXT NOT NULL,
    line INTEGER NOT NULL,
    col INTEGER NOT NULL DEFAULT 0,
    file_path TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL DEFAULT 'unknown',
    status TEXT NOT NULL DEFAULT 'pending',
    name_tail TEXT NOT NULL DEFAULT '',
    candidates TEXT,           -- JSON array
    UNIQUE(from_node, reference_name, reference_kind, line, col)
);

-- FTS5 full-text search index
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
    name,
    body,
    language,
    content='nodes',
    content_rowid='id',
    tokenize='unicode61 tokenchars _'
);

-- Triggers to keep FTS index in sync
CREATE TRIGGER IF NOT EXISTS nodes_ai AFTER INSERT ON nodes BEGIN
    INSERT INTO nodes_fts(rowid, name, body, language) VALUES (new.id, new.name, new.body, new.language);
END;

CREATE TRIGGER IF NOT EXISTS nodes_ad AFTER DELETE ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name, body, language) VALUES('delete', old.id, old.name, old.body, old.language);
END;

CREATE TRIGGER IF NOT EXISTS nodes_au AFTER UPDATE ON nodes BEGIN
    INSERT INTO nodes_fts(nodes_fts, rowid, name, body, language) VALUES('delete', old.id, old.name, old.body, old.language);
    INSERT INTO nodes_fts(rowid, name, body, language) VALUES (new.id, new.name, new.body, new.language);
END;

-- Indexes for fast lookup (core columns only).
-- Indexes on columns added in later logic versions are created in ensureSchema()
-- after ADD COLUMN, so old on-disk DBs can open without "no such column".
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_nodes_file ON nodes(file);
CREATE INDEX IF NOT EXISTS idx_nodes_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(source_id);
CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_id);
CREATE INDEX IF NOT EXISTS idx_edges_kind ON edges(kind);

-- Agent-accessible fact storage: cross-session findings attached to code symbols.
CREATE TABLE IF NOT EXISTS facts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    target_file TEXT NOT NULL,
    target_symbol TEXT,
    target_line INTEGER,
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    author TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    superseded_by INTEGER REFERENCES facts(id),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK(status IN ('active', 'superseded', 'retracted'))
);
CREATE INDEX IF NOT EXISTS idx_facts_target ON facts(target_file, target_symbol);
-- Same note may hang on more than one symbol; uniqueness is per target.
CREATE UNIQUE INDEX IF NOT EXISTS idx_facts_hash_target
    ON facts(content_hash, target_file, IFNULL(target_symbol, ''));
