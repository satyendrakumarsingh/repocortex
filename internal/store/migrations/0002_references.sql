-- 0002_references: persist unresolved references so resolution can run as a
-- second pass.
--
-- Why a table rather than resolving during indexing: binding a reference to a
-- definition requires the COMPLETE symbol table, which does not exist until
-- every file has been parsed. Resolving inline would mean either parsing twice
-- or holding every file's references in memory — and a 10,920-file tree
-- produces 531,290 of them, so that is not a rounding error.
--
-- These rows are intermediate. Once resolved they produce edges; the reference
-- itself stays so that re-resolution (after a SCIP index arrives, or after the
-- resolver improves) does not require reparsing the repository.

CREATE TABLE symbol_references (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The identifier as written, e.g. "updateStatus".
    name          TEXT    NOT NULL,
    -- Tree-sitter tag kind, e.g. "reference.call".
    kind          TEXT    NOT NULL,
    -- Qualifying expression where the syntax exposes one ("svc" in
    -- svc.updateStatus()). The resolver's strongest disambiguation signal
    -- after imports.
    receiver      TEXT    NOT NULL DEFAULT '',

    start_line    INTEGER NOT NULL DEFAULT 0,
    start_col     INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    end_col       INTEGER NOT NULL DEFAULT 0,

    -- Resolution outcome, so an unresolved reference is a recorded fact rather
    -- than an absence. "How much of this repository can we actually resolve?"
    -- is a question the product must answer honestly, and it is the number
    -- published as resolution precision.
    resolved_symbol_id INTEGER REFERENCES symbols(id) ON DELETE SET NULL,
    resolution         TEXT NOT NULL DEFAULT 'pending'
                       CHECK (resolution IN ('pending','resolved','ambiguous','unresolved','ignored'))
);

CREATE INDEX idx_refs_run_name ON symbol_references (index_run_id, name);
CREATE INDEX idx_refs_file     ON symbol_references (file_id);
CREATE INDEX idx_refs_status   ON symbol_references (index_run_id, resolution);

-- Imports per file. The resolver's primary disambiguation signal: knowing a
-- file imports com.pay.repository narrows `repository.save(...)` to one package
-- instead of every `save` in the repository. Without imports, resolution
-- degrades to name matching, which the design explicitly rejects.
CREATE TABLE file_imports (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id      INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL,
    alias        TEXT    NOT NULL DEFAULT '',
    start_line   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (file_id, path, alias)
);

CREATE INDEX idx_imports_run  ON file_imports (index_run_id);
CREATE INDEX idx_imports_path ON file_imports (index_run_id, path);
