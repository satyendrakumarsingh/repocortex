-- 0003_data_access: persist unresolved table accesses so they can be bound to
-- table symbols in a second pass.
--
-- Same shape and same reason as 0002_references. A repository method contains
-- `SELECT ... FROM transactions`; the `transactions` table is declared by a
-- migration file that may not have been parsed yet, and on an incremental
-- update may not be reparsed at all. Binding during indexing would mean either
-- parsing twice or ordering the walk by file type, and ordering a parallel walk
-- to make one analyzer work is a constraint that would outlive its reason.
--
-- These rows are intermediate: once bound they produce READS/WRITES/DDL edges.
-- The access itself stays so re-binding after a schema file appears does not
-- require reparsing the repository.
--
-- WHY THE ACCESS MODE LIVES HERE AND NOT ONLY ON THE EDGE. Spec §22 requires
-- READ, WRITE, READ_WRITE, DDL and UNKNOWN to be distinguishable. An access
-- that binds to no table — a query against a table declared outside this
-- repository, which is the normal case for a shared database — still carries a
-- true statement about what the code does, and dropping it would make
-- "this service writes a table we do not own" unanswerable.

CREATE TABLE table_accesses (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The table as written, lowercased and unqualified. A schema prefix is
    -- stripped at extraction: `public.transactions` and `transactions` are one
    -- table, and keeping both would split every question asked about it.
    table_name    TEXT    NOT NULL,

    -- READ | WRITE | READ_WRITE | DDL | UNKNOWN. Not a CHECK constraint with a
    -- fixed list: the analyzer owns this vocabulary, and a schema that has to
    -- be migrated to add a mode would push analyzer design into the store.
    access        TEXT    NOT NULL,

    -- The SQL verb, kept for diagnosis. "Why is this a WRITE?" is answered by
    -- "because the statement is an UPDATE" far more usefully than by a mode
    -- alone.
    verb          TEXT    NOT NULL DEFAULT '',

    -- The symbol whose body contained the statement, resolved at extraction
    -- time by line containment. NULL when the statement sits outside every
    -- definition — a migration file's top level, or a package-level constant —
    -- which is a real and common case rather than a failure.
    src_symbol_id INTEGER REFERENCES symbols(id) ON DELETE CASCADE,

    start_line    INTEGER NOT NULL DEFAULT 0,

    -- False when the statement looked like a run-time fragment. Recorded rather
    -- than used to discard, so a dynamically-assembled query still reports the
    -- table it names, with the confidence that deserves.
    certain       INTEGER NOT NULL DEFAULT 1,

    -- Binding outcome. An access that matched no declared table is a recorded
    -- fact, not an absence: it usually means the table is owned by another
    -- service, and that is worth reporting rather than losing.
    bound_symbol_id INTEGER REFERENCES symbols(id) ON DELETE SET NULL,
    binding         TEXT NOT NULL DEFAULT 'pending'
                    CHECK (binding IN ('pending','bound','unbound'))
);

CREATE INDEX idx_access_run_table ON table_accesses (index_run_id, table_name);
CREATE INDEX idx_access_file      ON table_accesses (file_id);
CREATE INDEX idx_access_src       ON table_accesses (index_run_id, src_symbol_id);
CREATE INDEX idx_access_binding   ON table_accesses (index_run_id, binding);
