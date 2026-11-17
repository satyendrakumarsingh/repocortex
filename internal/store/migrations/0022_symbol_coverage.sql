-- 0022_symbol_coverage: which lines of each definition a test run executed.
--
-- WHY. `repocortex coverage import` parsed a coverage report and printed a
-- summary; nothing was kept, so analyze_impact could say who calls a function
-- but not whether any test ever ran it, and find_tests could list the tests the
-- graph connects to a function without saying that the one run it was given
-- never executed it. A line count per definition answers both.
--
-- It is a PROFILE, not per-test data: Go coverprofile, LCOV, Cobertura and
-- JaCoCo all record how often a line ran, summed over the whole run, never
-- which test ran it. So nothing here can rank tests; it can only say "executed"
-- or "never executed in that run".
--
-- One imported profile per committed run; a new import replaces the old one
-- (coverage_imports holds one row per run). Every table is run-scoped with
-- ON DELETE CASCADE, so a retired run takes its coverage with it, as it takes
-- its edges.
--
-- THE THREE TABLES.
--
-- coverage_imports: what was imported and when. content_hash names the profile
-- bytes (the same file imported twice is the same profile); git_revision is the
-- commit HEAD named at import, which is how an answer says WHICH code the
-- numbers describe.
--
-- coverage_files: the content hash each matched file had when the profile was
-- imported. It is keyed by PATH, not by file id, on purpose. A re-indexed file
-- gets a new id and its symbols are deleted, so a table keyed by file id would
-- lose the row exactly when it is needed: the file changed, and the lines the
-- profile counted are not these lines. Keeping the path and the hash lets a
-- reader compare it with files.content_hash and say "stale" instead of showing
-- numbers for code that no longer exists.
--
-- symbol_coverage: per definition, how many of its instrumented lines ran
-- (covered_lines), how many it has (total_lines) and the highest hit count of
-- any line. Keyed by symbol id with ON DELETE CASCADE to symbols, so a changed
-- or deleted file drops its definitions' rows by itself, with no extra delete
-- in replace.go. A definition with no instrumented lines gets no row: zero of
-- zero is not a measurement.
CREATE TABLE coverage_imports (
    index_run_id     INTEGER PRIMARY KEY REFERENCES index_runs(id) ON DELETE CASCADE,
    content_hash     TEXT    NOT NULL,
    format           TEXT    NOT NULL,
    git_revision     TEXT    NOT NULL DEFAULT '',
    imported_unix    INTEGER NOT NULL,
    files_matched    INTEGER NOT NULL DEFAULT 0,
    files_unmatched  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE coverage_files (
    index_run_id INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    path         TEXT    NOT NULL,
    content_hash TEXT    NOT NULL,
    PRIMARY KEY (index_run_id, path)
) WITHOUT ROWID;

CREATE TABLE symbol_coverage (
    symbol_id     INTEGER PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    covered_lines INTEGER NOT NULL,
    total_lines   INTEGER NOT NULL,
    max_hits      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_symbol_coverage_run ON symbol_coverage (index_run_id);
