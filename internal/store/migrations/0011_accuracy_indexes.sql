-- 0011_accuracy_indexes: make `cortex accuracy` runnable on a large repository.
--
-- The recall report took 160 SECONDS for a single one of its queries on a
-- 91,548-symbol repository, and about twenty minutes end to end, against
-- sub-second on a 425-file one. A measurement that slow is a measurement nobody
-- runs, which makes it a measurement that stops being true without anyone
-- noticing — the same failure the whole accuracy effort exists to prevent.
--
-- The cost was two joins with nothing to stand on:
--
--   edges(evidence_file, evidence_line) had NO index at all, so adjudicating a
--   call site against the compiler's edge at the same file and line meant a
--   scan of every edge in the run, per probe. Every other access path on this
--   table is by symbol id; nothing had ever needed to look one up by location.
--
--   symbol_references(file_id, start_line) likewise. idx_refs_file covers
--   file_id alone, which narrows a 506,176-row table to one file and then scans
--   it for a line.
--
-- Both are covering indexes for the queries that need them, and neither is on a
-- write path that runs per row — they cost one B-tree each at index time and
-- turn an O(n*m) nested loop into indexed probes.
--
-- Measured on the 91,548-symbol repository: 160s -> 0.16s for the adjudication
-- query, identical row counts. See internal/cli/accuracy.go for the query shape
-- that goes with them; the index alone was worth 2x and the rewrite the rest.

CREATE INDEX IF NOT EXISTS idx_edges_evidence
    ON edges (index_run_id, evidence_file, evidence_line);

CREATE INDEX IF NOT EXISTS idx_refs_file_line
    ON symbol_references (file_id, start_line);
