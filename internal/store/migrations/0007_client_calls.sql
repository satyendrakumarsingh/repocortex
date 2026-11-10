-- 0007_client_calls: stage outbound HTTP calls so they can be bound to the
-- endpoints they consume in a second pass.
--
-- Same shape and same reason as 0003_data_access, and deliberately NOT the
-- shape of an exposure. An EXPOSURE is intra-file — the route and its handler
-- are both in the file being written — so it resolves during indexing. A CALL
-- is not: `fetch("/api/orders/42")` lives in a React component, and the route
-- that answers it is declared by a controller that may be parsed later in the
-- same run, or, on an incremental update, not parsed at all. Binding at write
-- time would mean either ordering the walk by file role or parsing twice.
--
-- These rows are intermediate: once bound they produce CONSUMES edges from the
-- calling declaration to the endpoint node. The call itself stays, so a later
-- rebind (a backend file arrives, or a route is renamed) costs no reparsing.
--
-- WHY AN UNBOUND CALL IS KEPT. It is the same argument migration 0003 makes for
-- a table this repository does not declare. A call to an endpoint no route here
-- exposes is usually a call into ANOTHER service, and "this frontend depends on
-- an API we do not own" is a question worth answering rather than a row worth
-- dropping. It is also the denominator: "how many of this component's outbound
-- calls did we resolve" is unanswerable if the unresolved ones were discarded.
--
-- WHY `path` IS STORED AS WRITTEN. The matching key — scheme and host stripped,
-- query removed, parameters folded to a wildcard, lowercased — is derived at
-- binding time and never persisted in place of the literal. A displayed path
-- that has been canonicalised is a path the developer cannot find with grep.

CREATE TABLE client_calls (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The verb as the syntax states it, uppercased. Empty means the call states
    -- none this scan could read; it is treated as ANY at binding time and never
    -- guessed at.
    method        TEXT    NOT NULL DEFAULT '',

    -- The LITERAL part of the target, exactly as written. For a dynamic target
    -- this is the constant prefix and may be empty.
    path          TEXT    NOT NULL DEFAULT '',

    -- Which calling convention matched (fetch | axios | go-net-http), kept so a
    -- report can say how a call was found rather than presenting all calls as
    -- equally certain.
    client        TEXT    NOT NULL DEFAULT '',

    -- The declaration whose body contains the call, resolved at extraction time.
    -- NULL when the call sits outside every declaration — a module-level
    -- constant holding a prefetch — which is real and common rather than a
    -- failure.
    src_symbol_id INTEGER REFERENCES symbols(id) ON DELETE CASCADE,

    start_line    INTEGER NOT NULL DEFAULT 0,

    -- True when the target was assembled at run time rather than written out.
    -- Recorded rather than used to discard: a dynamic call is a real request
    -- whose target is unknown, and it still counts in coverage.
    dynamic       INTEGER NOT NULL DEFAULT 0,

    -- The extractor's confidence in the CALL, before any binding evidence is
    -- weighed. The bound edge's confidence descends from this.
    confidence    TEXT    NOT NULL DEFAULT 'low',

    -- Binding outcome. `unbound` is a recorded fact, not an absence.
    bound_symbol_id INTEGER REFERENCES symbols(id) ON DELETE SET NULL,
    binding         TEXT NOT NULL DEFAULT 'pending'
                    CHECK (binding IN ('pending','bound','unbound'))
);

CREATE INDEX idx_calls_run_path ON client_calls (index_run_id, path);
CREATE INDEX idx_calls_file     ON client_calls (file_id);
CREATE INDEX idx_calls_binding  ON client_calls (index_run_id, binding);

-- Single-column indexes on both foreign keys into symbols(id), for the reason
-- migration 0005 records at length: SQLite creates no index for a foreign key,
-- so deleting one symbol full-scans every child table without one, and
-- ReplaceFile deletes a file's symbols on every incremental update.
CREATE INDEX idx_calls_src_fk   ON client_calls (src_symbol_id);
CREATE INDEX idx_calls_bound_fk ON client_calls (bound_symbol_id);
