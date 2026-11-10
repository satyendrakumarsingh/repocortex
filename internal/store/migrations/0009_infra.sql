-- 0009_infra: make DEPLOYMENT facts part of the index.
--
-- internal/analyze/infra has been able to read Dockerfiles, compose files,
-- Kubernetes manifests, Helm charts and Terraform since it was written, and
-- nothing stored the result. `cortex infra` read the repository from disk on
-- every invocation and printed "NOT INDEXED" at the bottom of its own report,
-- which was honest and useless: no MCP tool, no `cortex context`, no graph
-- query and no blast radius could see a service, a port or a configuration key,
-- because none of them read the filesystem — they read this database.
--
-- The gap that closes here is not "a report is missing rows". It is that source
-- code cannot answer "which service does this code run in". No line of Go or
-- Java says so. That fact exists only in a compose file's build context and a
-- Deployment's image, so a change-safety tool that indexes only source can name
-- a function's callers and still not name what restarts when it changes.
--
-- ============================================================================
-- THERE IS NO VALUE COLUMN IN THIS MIGRATION, AND THERE MUST NEVER BE ONE.
-- ============================================================================
--
-- These files hold credentials. A Kubernetes Secret's `data` maps key names to
-- base64 credentials; a compose `environment:` block maps key names to
-- passwords. What is stored below is the LEFT-HAND SIDE: infra_keys.name is a
-- key name, infra_keys.ref names the Secret or ConfigMap a value comes from,
-- and there is no third column. That is not a filter someone could later
-- relax — the extractor's types (infra.EnvKey, infra.KeySet, infra.Unit) have
-- no field a value fits in, so there is nothing in the process that could be
-- inserted here even by mistake.
--
-- Two fields are deliberately ABSENT rather than merely empty:
--
--   * The container command line. infra.Unit.Command exists and is already
--     redacted through internal/security.Redact, and it is still the one
--     free-form string in the extractor's output that is assembled from
--     arbitrary manifest text — `/app --token=…` shaped. Redaction is a good
--     second line of defence and a poor first one, so the command does not
--     reach disk at all. It stays available in `cortex infra --disk`, which
--     prints and forgets.
--
--   * Terraform attribute values, which the extractor never reads in the first
--     place: it scans block HEADERS, so a `password = "…"` in a .tf file has no
--     path into this schema even in principle.
--
-- A third field is absent for a reason worth recording, because it was not
-- obvious and a test found it rather than a reviewer. infra.Problem carries a
-- Detail — the YAML parser's own message — and the extractor redacts it, so
-- persisting it looked safe. It is not. A parser error QUOTES THE TEXT IT
-- CHOKED ON, and internal/security.Redact matches credential SHAPES, not
-- arbitrary strings: FuzzInfraPersistence planted a sentinel value in a
-- manifest and go-yaml handed it straight back inside
-- `unknown anchor '<the value>' referenced`. Redaction is a filter and a filter
-- cannot know that an arbitrary token was somebody's password.
--
-- So the message does not reach disk. What does is the REASON — malformed_yaml,
-- too_large, templated_not_rendered — which is a stable label this codebase
-- wrote, plus the file and the line, which is what a reader acts on anyway. The
-- parser's own words remain available from `cortex infra --disk`, which
-- prints and forgets rather than storing into an index that MCP tools read.
--
-- Every remaining column in this migration therefore holds a key name, an
-- identifier, a stable label or an integer. There is no free-form file text
-- anywhere in it.
--
-- ============================================================================
-- WHY UNITS ARE SYMBOLS AND NOT ONLY ROWS
-- ============================================================================
--
-- Every table below carries symbol_id, and that column is the entire point of
-- persisting any of this. Edges reference symbols, so a deployment unit that is
-- only a row in a side table can never be an endpoint of a relationship: it
-- cannot be joined to the handler that runs inside it or to the configuration
-- key that restarts it. Endpoints and tables became symbols for exactly this
-- reason (see internal/analyze's package comment); a unit is the third case.
--
-- The side tables still exist because a symbol row cannot hold a port list, a
-- resource limit or an env source, and widening `symbols` with eight columns
-- that are NULL for 99.9% of rows to avoid four narrow tables would be the
-- wrong trade.
--
-- Env key names become symbols of kind `configuration` — the SAME kind
-- internal/analyze/config produces for a key declared in a .env or a
-- properties file, and deliberately so. registry.BindConfigUsages already binds
-- `os.Getenv("DATABASE_URL")` in source to a declared key of that kind, so the
-- moment a Deployment's env block declares DATABASE_URL, the code that reads it
-- acquires a CONFIGURED_BY edge to the manifest's key, and "what does changing
-- this config key restart" becomes a two-hop query instead of an unanswerable
-- one. Inventing a fourth kind would have made those two facts about one
-- setting into two unrelated nodes.
--
-- ============================================================================
-- WHY DEFAULTS ARE WHAT THEY ARE
-- ============================================================================
--
-- Every TEXT column defaults to '' and every INTEGER to 0, and none of them is
-- nullable, with the single exception of the foreign keys into symbols and
-- infra_units. That is a deliberate split:
--
--   * '' and 0 mean "the manifest stated nothing here", which is a fact about
--     the manifest and not a missing measurement. A compose service with no
--     image is a service built from a Dockerfile; a port with published = 0 is
--     a port the host cannot reach, and that zero is the ANSWER to "is this
--     reachable" rather than an absence of one. Making those NULL would force
--     every reader through a COALESCE and would let SQLite's NULL-before-''
--     ordering split byte-identical content across two sort positions — which
--     is precisely the hazard digest.go documents at length.
--
--   * symbol_id and unit_id are genuinely nullable, because they record a
--     RELATIONSHIP that may not exist. A key belonging to a ConfigMap has no
--     unit; a unit whose symbol failed to be written has no node. Defaulting
--     those to 0 would fabricate a reference to symbol 0, and ON DELETE SET
--     NULL exists to turn a deleted node into a visible gap rather than a
--     dangling id.
--
-- infra_problems has no symbol_id at all: a file we could not read declares
-- nothing, so there is no node to point at. It is a row about the ABSENCE of
-- rows, and it is the reason this migration exists in the shape it does — "no
-- Kubernetes manifests in this repository" and "we could not parse your
-- Kubernetes manifests" have opposite consequences, and an index that stored
-- only successes would render them identical to every consumer downstream.

CREATE TABLE infra_units (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The graph node for this unit. NULL only when the symbol could not be
    -- written, which is a defect worth seeing rather than papering over.
    symbol_id     INTEGER REFERENCES symbols(id) ON DELETE SET NULL,

    -- As declared: a compose service name, `deployment/container`, a build
    -- stage name, or `resource "aws_db_instance" "main"`.
    name          TEXT    NOT NULL,

    -- What kind of thing it is — "service", "Deployment", "stage",
    -- "resource:aws_db_instance". One column rather than four tables, for the
    -- reason infra.Unit is one type rather than four structs.
    unit_type     TEXT    NOT NULL DEFAULT '',

    -- Which manifest family declared it: dockerfile | compose | kubernetes |
    -- helm | terraform. Stored rather than derived from the path, because
    -- classification is the extractor's judgement and a reader comparing two
    -- runs must see the judgement that was actually made.
    format        TEXT    NOT NULL DEFAULT '',

    image         TEXT    NOT NULL DEFAULT '',

    -- The SOURCE DIRECTORY this unit is built from, and the Dockerfile within
    -- it. This is the one field in the entire schema that ties a running
    -- service back to code, and it is what BindInfraUnits joins on.
    build_context TEXT    NOT NULL DEFAULT '',
    dockerfile    TEXT    NOT NULL DEFAULT '',

    -- The last stage of a multi-stage build: the one that becomes the image.
    -- Earlier stages are toolchains, and reporting them as things that run
    -- would be wrong.
    final         INTEGER NOT NULL DEFAULT 0 CHECK (final IN (0,1)),

    start_line    INTEGER NOT NULL DEFAULT 0,

    -- One unit per (run, file, name, line). A compose file cannot declare the
    -- same service twice, and this makes re-staging a file idempotent instead
    -- of doubling its units.
    UNIQUE (index_run_id, file_id, name, start_line)
);

CREATE INDEX idx_infra_units_run     ON infra_units (index_run_id, name);
CREATE INDEX idx_infra_units_file    ON infra_units (file_id);
CREATE INDEX idx_infra_units_symbol  ON infra_units (symbol_id);
-- Partial: only units that name a build context participate in the join that
-- answers "which service does this code run in", and on a real repository
-- that is a small minority of the rows.
CREATE INDEX idx_infra_units_context ON infra_units (index_run_id, build_context)
    WHERE build_context <> '';

CREATE TABLE infra_ports (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    unit_id       INTEGER NOT NULL REFERENCES infra_units(id) ON DELETE CASCADE,

    -- Inside the container: EXPOSE, containerPort, the right-hand side of a
    -- compose mapping.
    container_port  INTEGER NOT NULL DEFAULT 0,
    -- Reachable from outside: a compose host port, hostPort, Service port or
    -- nodePort. Zero is the answer to "is this reachable", not a missing value.
    published_port  INTEGER NOT NULL DEFAULT 0,
    protocol        TEXT    NOT NULL DEFAULT '',
    name            TEXT    NOT NULL DEFAULT '',

    -- A declaration no number could be read from: "${PORT}:80", a range, a
    -- named targetPort. Kept because a port that resolves at deploy time is
    -- still a port, and dropping it would under-report what a service exposes.
    raw             TEXT    NOT NULL DEFAULT '',

    start_line      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_infra_ports_unit ON infra_ports (unit_id);
CREATE INDEX idx_infra_ports_run  ON infra_ports (index_run_id, container_port);

-- Configuration KEY NAMES. Read the header of this file before adding a column.
CREATE TABLE infra_keys (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,

    -- The unit that sets this key, or NULL when the key belongs to a ConfigMap,
    -- a Secret, a Helm values file or a Terraform variable block — a key set
    -- that exists independently of any one workload.
    unit_id       INTEGER REFERENCES infra_units(id) ON DELETE CASCADE,

    -- The key set's name and type when unit_id is NULL. Denormalised rather
    -- than given a table of its own: a key set has a name, a type and a line
    -- and nothing else, and a two-column table joined on every read would buy
    -- nothing.
    key_set       TEXT    NOT NULL DEFAULT '',
    key_set_type  TEXT    NOT NULL DEFAULT '',

    -- The `configuration` symbol this key name became.
    symbol_id     INTEGER REFERENCES symbols(id) ON DELETE SET NULL,

    name          TEXT    NOT NULL,

    -- Where the value comes from: literal | secret-ref | configmap-ref |
    -- downward-api | host | build-arg | image. The value itself is not read;
    -- where it comes from is often the more useful half, because "this key is
    -- supplied by Secret db-creds" says what rotating that Secret restarts.
    from_source   TEXT    NOT NULL DEFAULT '',

    -- The Secret or ConfigMap supplying it, as `name` or `name:key`. A NAME.
    ref           TEXT    NOT NULL DEFAULT '',

    -- Set when the key comes out of a Secret, or when its NAME says credential.
    -- Classifying by name requires reading nothing.
    secret        INTEGER NOT NULL DEFAULT 0 CHECK (secret IN (0,1)),

    start_line    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_infra_keys_run    ON infra_keys (index_run_id, name);
CREATE INDEX idx_infra_keys_unit   ON infra_keys (unit_id);
CREATE INDEX idx_infra_keys_file   ON infra_keys (file_id);
CREATE INDEX idx_infra_keys_symbol ON infra_keys (symbol_id);

-- A whole Secret or ConfigMap injected at once: Kubernetes `envFrom`.
--
-- Deliberately NOT expanded into infra_keys rows. The keys live in the
-- referenced object, which may be in another file or in no file in this
-- repository, and inventing key names for it would be fabrication. An
-- unresolved reference is a real finding — a deployment that depends on
-- configuration this repository does not contain — so it is stored as what it
-- is rather than dropped or guessed at.
CREATE TABLE infra_env_from (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    unit_id       INTEGER NOT NULL REFERENCES infra_units(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL DEFAULT '',   -- Secret | ConfigMap
    name          TEXT    NOT NULL DEFAULT '',
    prefix        TEXT    NOT NULL DEFAULT '',
    optional      INTEGER NOT NULL DEFAULT 0 CHECK (optional IN (0,1)),
    start_line    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_infra_env_from_unit ON infra_env_from (unit_id);
CREATE INDEX idx_infra_env_from_run  ON infra_env_from (index_run_id, name);

-- Declared resource bounds. Amounts ("512Mi", "250m") are quantities, not
-- configuration values: they are the units of a capacity question, they carry
-- no credential, and a blast-radius report that cannot say a workload is capped
-- at 512Mi cannot say why raising a cache size will kill it.
CREATE TABLE infra_limits (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    unit_id       INTEGER NOT NULL REFERENCES infra_units(id) ON DELETE CASCADE,
    resource      TEXT    NOT NULL,
    request_qty   TEXT    NOT NULL DEFAULT '',
    limit_qty     TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_infra_limits_unit ON infra_limits (unit_id);

-- A manifest we recognised and could not read.
--
-- This is the table that keeps silence honest. Without it, a repository whose
-- Kubernetes manifests all fail to parse is indistinguishable from one that has
-- none, and every consumer downstream — MCP, context, blast radius — reports
-- confidently on a deployment it never saw.
CREATE TABLE infra_problems (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    index_run_id  INTEGER NOT NULL REFERENCES index_runs(id) ON DELETE CASCADE,
    file_id       INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    format        TEXT    NOT NULL DEFAULT '',

    -- A stable label, not prose: too_large | malformed_yaml |
    -- templated_not_rendered | dockerfile_without_from | limit_reached |
    -- parser_panic, or a walker skip reason.
    reason        TEXT    NOT NULL,

    -- There is deliberately no `detail`. See the header: a parser's message
    -- quotes the text it failed on, and that text can be a credential.

    start_line    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_infra_problems_file ON infra_problems (file_id);
CREATE INDEX idx_infra_problems_run  ON infra_problems (index_run_id, reason);
