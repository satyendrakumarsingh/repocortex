-- 0005_fk_indexes: index every foreign key that references symbols(id).
--
-- SQLite does not create an index for a foreign key. Deleting one row from
-- `symbols` therefore makes it scan every child table looking for references,
-- and seven columns reference symbols(id) across four tables.
--
-- WHY THE EXISTING INDEXES DO NOT HELP. idx_edges_src is
-- (index_run_id, src_id, kind) and idx_access_src is
-- (index_run_id, src_symbol_id): index_run_id is leftmost, so neither can serve
-- a lookup that knows only the symbol id — which is all a foreign-key check
-- has. They were built for query paths, and they serve those well; they simply
-- cannot answer this question. The other four columns had no index at all.
--
-- WHAT IT COST. Re-indexing ONE file on a 5,000-file repository took 16.9
-- seconds, because ReplaceFile deletes the file's symbols and each deletion
-- full-scans four to seven tables. Measured on a clone with exactly these
-- indexes added: 0.87 s, a 19.35x improvement for 4.35% growth on disk. The
-- one-file edit is the operation `cortex watch` performs on every save, so
-- this was the difference between a watcher that keeps up and one that does
-- not.
--
-- Four of the seven columns belong to table_accesses and config_usages, which
-- the analysis layer introduced — the cost arrived with them and nobody
-- measured the delete side.
--
-- These are single-column indexes on purpose. A foreign-key check knows the
-- symbol id and nothing else, so a leading column of anything else defeats it,
-- which is the whole bug being fixed here.

CREATE INDEX idx_edges_src_fk        ON edges (src_id);
CREATE INDEX idx_edges_dst_fk        ON edges (dst_id);
CREATE INDEX idx_refs_resolved_fk    ON symbol_references (resolved_symbol_id);
CREATE INDEX idx_access_src_fk       ON table_accesses (src_symbol_id);
CREATE INDEX idx_access_bound_fk     ON table_accesses (bound_symbol_id);
CREATE INDEX idx_cfguse_src_fk       ON config_usages (src_symbol_id);
CREATE INDEX idx_cfguse_bound_fk     ON config_usages (bound_symbol_id);
