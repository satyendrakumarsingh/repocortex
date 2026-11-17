-- 0021_definition_text: full-text rows for each function and method.
--
-- WHY. files_fts ranks whole files, and bm25 normalises by length, so a long
-- file that holds the one function a task is about ranks below hundreds of
-- short files that mention the task's words once. On a 25,000-file repository
-- a 12,000-line service module ranked 2,315th for "audit" and 2,454th for
-- "status": far outside the fifty files a context pack reads, though two of
-- its functions are exactly what "add an audit log entry when a task status
-- changes" is about. And when a long file does match, every definition in it
-- carried the file's evidence alike, so the pack could not tell the function
-- that writes the audit row from the 300 others beside it.
--
-- One row per function or method, keyed by the symbol's id: the identifier
-- parts of its name and body, de-duplicated (search.ExpandIdentifiers), so a
-- definition's length is its vocabulary and not its line count.
--
-- CONTENTLESS, with contentless_delete. The text is derived from files_fts,
-- which already stores the source; a second copy would double the largest
-- part of the index. Rows are deleted by rowid (the symbol id) before a
-- file's or a run's symbols cascade away, as files_fts rows are.
--
-- An index built before this migration has no rows here; the next refresh
-- fills them from the stored file text (registry.BackfillDefinitionText), and
-- until then a context pack works as it did without them.
CREATE VIRTUAL TABLE defs_fts USING fts5(
    expanded,
    content = '',
    contentless_delete = 1,
    tokenize = 'porter unicode61'
);
