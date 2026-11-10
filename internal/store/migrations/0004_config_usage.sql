-- 0004_config_usage: persist unresolved configuration reads so they can be
-- bound to declared keys in a second pass.
--
-- Third instance of the pattern 0002 and 0003 describe, and for the third time
-- the same reason: the key `os.Getenv("DATABASE_URL")` reads may be declared by
-- a .env or application.yml parsed after the code that reads it.
--
-- NO VALUE COLUMN, AND THERE MUST NEVER BE ONE. Spec §24 requires that secret
-- values are never exposed, and internal/analyze/config implements that as an
-- absence rather than a filter: nothing upstream reads a value, so nothing can
-- be written here. A schema with nowhere to put a value cannot leak one, which
-- is a stronger guarantee than any redaction list — a list can be wrong about a
-- key nobody anticipated.

CREATE TABLE config_usages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The key as written at the call site.
    key_name      TEXT    NOT NULL,
    -- How it was read: env | spring-value | viper | config. Distinguishes a
    -- process environment variable from an application setting, which matters
    -- when deciding where a missing key has to be added.
    mechanism     TEXT    NOT NULL DEFAULT '',

    -- The definition whose body performed the read, by line containment. NULL
    -- for a read at file scope, which is ordinary in scripts and module-level
    -- configuration.
    src_symbol_id INTEGER REFERENCES symbols(id) ON DELETE CASCADE,
    start_line    INTEGER NOT NULL DEFAULT 0,

    bound_symbol_id INTEGER REFERENCES symbols(id) ON DELETE SET NULL,
    binding         TEXT NOT NULL DEFAULT 'pending'
                    CHECK (binding IN ('pending','bound','unbound'))
);

CREATE INDEX idx_cfguse_run_key ON config_usages (index_run_id, key_name);
CREATE INDEX idx_cfguse_file    ON config_usages (file_id);
CREATE INDEX idx_cfguse_binding ON config_usages (index_run_id, binding);
