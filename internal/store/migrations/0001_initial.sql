-- 0001_initial: core registry schema.
--
-- Design notes that are load-bearing:
--
--   * Every edge carries confidence + evidence + index_run_id. This cannot be
--     retrofitted later without rewriting every writer and reader, so it lands
--     in the first migration.
--
--   * index_runs.committed gates visibility. Readers only ever see rows from a
--     committed run, which is what makes a crash mid-index harmless.
--
--   * files_fts carries an `expanded` column holding identifier-split text
--     (handleCallback -> "handle callback"). FTS5's unicode61 tokenizer treats
--     an identifier as one opaque token, so without this a search for
--     "callback" cannot find handleCallback. Verified empirically; see
--     tests/integration/fts5_guarantee_test.go.

CREATE TABLE index_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    status          TEXT    NOT NULL CHECK (status IN ('indexing','ready','degraded','failed')),
    committed       INTEGER NOT NULL DEFAULT 0 CHECK (committed IN (0,1)),
    started_unix    INTEGER NOT NULL,
    finished_unix   INTEGER,
    git_revision    TEXT,
    accuracy_mode   TEXT    NOT NULL DEFAULT 'auto' CHECK (accuracy_mode IN ('fast','auto','exact')),
    files_indexed   INTEGER NOT NULL DEFAULT 0,
    symbols_found   INTEGER NOT NULL DEFAULT 0,
    edges_found     INTEGER NOT NULL DEFAULT 0,
    parse_failures  INTEGER NOT NULL DEFAULT 0,
    registry_digest TEXT,
    error           TEXT
);

-- Exactly one run may be the current committed one.
CREATE UNIQUE INDEX idx_index_runs_current
    ON index_runs (committed) WHERE committed = 1;

CREATE TABLE files (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id   INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    path           TEXT    NOT NULL,
    language       TEXT    NOT NULL DEFAULT '',
    content_hash   TEXT    NOT NULL,
    size_bytes     INTEGER NOT NULL DEFAULT 0,
    mod_time_unix  INTEGER NOT NULL DEFAULT 0,
    parse_status   TEXT    NOT NULL DEFAULT 'ok'
                   CHECK (parse_status IN ('ok','partial','failed','skipped')),
    parse_error    TEXT,
    UNIQUE (index_run_id, path)
);

CREATE INDEX idx_files_hash ON files (content_hash);
CREATE INDEX idx_files_lang ON files (index_run_id, language);

CREATE TABLE symbols (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    fqn           TEXT    NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    start_col     INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    end_col       INTEGER NOT NULL DEFAULT 0,
    signature     TEXT,
    doc           TEXT,
    exported      INTEGER NOT NULL DEFAULT 0 CHECK (exported IN (0,1)),
    confidence    TEXT    NOT NULL CHECK (confidence IN ('exact','high','medium','low')),
    source        TEXT    NOT NULL,
    UNIQUE (index_run_id, fqn, file_id, start_line)
);

CREATE INDEX idx_symbols_name ON symbols (index_run_id, name);
CREATE INDEX idx_symbols_fqn  ON symbols (index_run_id, fqn);
CREATE INDEX idx_symbols_file ON symbols (file_id);

CREATE TABLE edges (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id   INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    src_id         INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
    dst_id         INTEGER NOT NULL REFERENCES symbols(id) ON DELETE CASCADE,
    kind           TEXT    NOT NULL,
    confidence     TEXT    NOT NULL CHECK (confidence IN ('exact','high','medium','low')),
    evidence_file  TEXT    NOT NULL DEFAULT '',
    evidence_line  INTEGER NOT NULL DEFAULT 0,
    evidence_col   INTEGER NOT NULL DEFAULT 0,
    source         TEXT    NOT NULL,
    detail         TEXT,
    -- An 'exact' edge must come from a compiler. Enforced in SQL as well as in
    -- Go so a bad writer cannot corrupt the store's central promise.
    CHECK (confidence <> 'exact' OR source = 'compiler'),
    UNIQUE (index_run_id, src_id, dst_id, kind, evidence_file, evidence_line)
);

CREATE INDEX idx_edges_src  ON edges (index_run_id, src_id, kind);
CREATE INDEX idx_edges_dst  ON edges (index_run_id, dst_id, kind);
CREATE INDEX idx_edges_conf ON edges (index_run_id, confidence);

-- Full-text search. Not external-content: the `expanded` column has no backing
-- table column, so FTS5 owns this content outright.
CREATE VIRTUAL TABLE files_fts USING fts5(
    path,
    content,
    expanded,
    tokenize = 'porter unicode61'
);

-- Maps an fts rowid back to the real file row.
CREATE TABLE files_fts_map (
    fts_rowid INTEGER PRIMARY KEY,
    file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE
);

CREATE INDEX idx_files_fts_map_file ON files_fts_map (file_id);
