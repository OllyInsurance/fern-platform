-- Runs API (GET /runs ...): a CI run is every test run sharing metadata
-- ci_run_id. These tables summarise each CI run so the list, facets and
-- lane/bucket trends answer without reading spec_runs. They are derived
-- data: refreshed on ingest, rebuilt by the backfill, safe to truncate.

CREATE TABLE IF NOT EXISTS ci_runs (
    ci_run_id    VARCHAR(255) PRIMARY KEY,
    sha          VARCHAR(255) NOT NULL DEFAULT '',
    branch       VARCHAR(255) NOT NULL DEFAULT '',
    runner       VARCHAR(32)  NOT NULL DEFAULT 'github',
    host         VARCHAR(255) NOT NULL DEFAULT '',
    workflow     VARCHAR(255) NOT NULL DEFAULT '',
    url          TEXT         NOT NULL DEFAULT '',
    projects     TEXT[]       NOT NULL DEFAULT '{}',
    lane_keys    TEXT[]       NOT NULL DEFAULT '{}',
    lane_names   TEXT[]       NOT NULL DEFAULT '{}',
    started_at   TIMESTAMPTZ  NOT NULL,
    ended_at     TIMESTAMPTZ,
    duration_ms  BIGINT       NOT NULL DEFAULT 0,
    status       VARCHAR(16)  NOT NULL,
    total        INT NOT NULL DEFAULT 0,
    passed       INT NOT NULL DEFAULT 0,
    failed       INT NOT NULL DEFAULT 0,
    skipped      INT NOT NULL DEFAULT 0,
    known_gap    INT NOT NULL DEFAULT 0,
    defect       INT NOT NULL DEFAULT 0,
    lanes        JSONB        NOT NULL DEFAULT '[]',
    search       TEXT         NOT NULL DEFAULT '',
    refreshed_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ci_runs_started ON ci_runs (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_ci_runs_branch_started ON ci_runs (branch, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_ci_runs_sha ON ci_runs (sha varchar_pattern_ops);
CREATE INDEX IF NOT EXISTS idx_ci_runs_projects ON ci_runs USING GIN (projects);
CREATE INDEX IF NOT EXISTS idx_ci_runs_lanes ON ci_runs USING GIN (lane_names);

-- Which CI run each test run belongs to.
CREATE TABLE IF NOT EXISTS ci_run_test_runs (
    test_run_id BIGINT PRIMARY KEY REFERENCES test_runs(id) ON DELETE CASCADE,
    ci_run_id   VARCHAR(255) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ci_run_test_runs_ci ON ci_run_test_runs (ci_run_id);

-- One row per lane and per bucket of each CI run, for trends.
CREATE TABLE IF NOT EXISTS ci_run_nodes (
    ci_run_id   VARCHAR(255) NOT NULL,
    level       VARCHAR(8)   NOT NULL, -- lane | bucket
    lane_key    TEXT         NOT NULL,
    key         TEXT         NOT NULL, -- the lane key, or the bucket (suite) name
    project     VARCHAR(255) NOT NULL DEFAULT '',
    branch      VARCHAR(255) NOT NULL DEFAULT '',
    sha         VARCHAR(255) NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ  NOT NULL,
    duration_ms BIGINT       NOT NULL DEFAULT 0,
    status      VARCHAR(16)  NOT NULL,
    total       INT NOT NULL DEFAULT 0,
    passed      INT NOT NULL DEFAULT 0,
    failed      INT NOT NULL DEFAULT 0,
    skipped     INT NOT NULL DEFAULT 0,
    known_gap   INT NOT NULL DEFAULT 0,
    defect      INT NOT NULL DEFAULT 0,
    PRIMARY KEY (ci_run_id, level, lane_key, key)
);
CREATE INDEX IF NOT EXISTS idx_ci_run_nodes_key ON ci_run_nodes (level, key text_pattern_ops, started_at);
CREATE INDEX IF NOT EXISTS idx_ci_run_nodes_started ON ci_run_nodes (level, started_at);

-- Summary bookkeeping (the version the backfill built).
CREATE TABLE IF NOT EXISTS ci_runs_state (
    key   VARCHAR(64) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Finding a CI run's test runs and a test's history.
CREATE INDEX IF NOT EXISTS idx_test_runs_ci_run_id ON test_runs ((metadata->>'ci_run_id'));
CREATE INDEX IF NOT EXISTS idx_test_runs_updated_at ON test_runs (updated_at);
CREATE INDEX IF NOT EXISTS idx_spec_runs_spec_name_pattern ON spec_runs (spec_name varchar_pattern_ops);
