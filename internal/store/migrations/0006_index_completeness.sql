-- 0006_index_completeness: record how much of what a run discovered the run
-- actually covered.
--
-- The two-phase swap proves a run was published all-or-nothing. It cannot prove
-- the published revision covers everything the walker found. A run that
-- discovered 1,200 files, processed 1,163 and published those atomically is a
-- healthy `ready` run by every check the registry applies, and is also missing
-- 37 files that every query will answer confidently and wrongly about. See
-- internal/pipeline/reconcile.go for the invariant:
--
--     files_indexed + files_skipped + parse_failures == files_discovered
--
-- WHY NOT OVERLOAD `status`. Two reasons, either sufficient. The column carries
-- a CHECK listing the lifecycle states, and SQLite cannot alter a CHECK in
-- place — changing it means rebuilding the table, which is a far larger and
-- riskier migration than three added columns. More importantly completeness is
-- ORTHOGONAL to lifecycle: a run can be `degraded` (files failed to parse) and
-- `partial` (files vanished without reaching any terminal outcome) at once, and
-- collapsing them into one column would make the pair unrepresentable and force
-- every reader to guess which of the two facts it was being told.
--
-- WHY THE DEFAULTS ARE 'complete'/0. Every run already on disk was written by a
-- build that did not measure this. Defaulting them to 'partial' would relabel
-- every existing healthy index as suspect on upgrade; defaulting to 'complete'
-- says "this run made no claim either way", which is what is actually true, and
-- files_discovered = 0 makes the absence of a measurement visible rather than
-- inventing a discovery count.

ALTER TABLE index_runs ADD COLUMN files_discovered INTEGER NOT NULL DEFAULT 0;

ALTER TABLE index_runs ADD COLUMN index_state TEXT NOT NULL DEFAULT 'complete'
    CHECK (index_state IN ('complete','partial'));

ALTER TABLE index_runs ADD COLUMN files_unaccounted INTEGER NOT NULL DEFAULT 0;
