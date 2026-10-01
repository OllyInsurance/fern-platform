-- Requirements coverage: which spec (a Linear ticket) and which of its
-- acceptance criteria each test proves, what the test does in plain English,
-- and how it is judged. A registry snapshot is imported whole (PUT
-- /api/v1/requirements); results come from the spec_runs Fern already stores.

-- Per-spec-run metadata a reporter can send (test key, criteria, what/how),
-- so a test can declare its own links instead of relying on the registry.
ALTER TABLE spec_runs ADD COLUMN IF NOT EXISTS metadata JSONB;

CREATE TABLE IF NOT EXISTS requirement_specs (
    spec_key          VARCHAR(64)  PRIMARY KEY,          -- ENG-424
    source            VARCHAR(32)  NOT NULL DEFAULT 'linear',
    title             TEXT         NOT NULL,
    url               TEXT         NOT NULL DEFAULT '',
    state             VARCHAR(64)  NOT NULL DEFAULT '',
    author            VARCHAR(255) NOT NULL DEFAULT '',
    source_created_at TIMESTAMP,
    synced_at         TIMESTAMP,
    position          INT          NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS requirement_criteria (
    spec_key     VARCHAR(64)  NOT NULL REFERENCES requirement_specs(spec_key) ON DELETE CASCADE,
    criterion_id VARCHAR(64)  NOT NULL,                  -- AC-01, NFR-02, S03
    kind         VARCHAR(16)  NOT NULL DEFAULT 'ac',     -- ac | scenario | nfr
    title        TEXT         NOT NULL DEFAULT '',
    quote        TEXT         NOT NULL DEFAULT '',       -- verbatim from the source ticket
    build_status   VARCHAR(16) NOT NULL DEFAULT '',      -- built | partial | not_built, checked in the code
    build_evidence TEXT        NOT NULL DEFAULT '',      -- file:line (or what is missing)
    position     INT          NOT NULL DEFAULT 0,
    PRIMARY KEY (spec_key, criterion_id)
);

CREATE TABLE IF NOT EXISTS requirement_test_cases (
    test_key     VARCHAR(512) PRIMARY KEY,               -- repo:file:name
    repo         VARCHAR(128) NOT NULL,
    framework    VARCHAR(32)  NOT NULL,
    file         TEXT         NOT NULL,
    line         INT          NOT NULL DEFAULT 0,
    name         TEXT         NOT NULL,
    fern_project VARCHAR(255) NOT NULL DEFAULT '',       -- where its results land
    match_name   TEXT         NOT NULL DEFAULT '',       -- spec_runs.spec_name to match
    match_mode   VARCHAR(16)  NOT NULL DEFAULT 'exact',  -- exact | suffix
    match_hint   TEXT         NOT NULL DEFAULT '',       -- must also appear in the name (suffix mode)
    what         TEXT         NOT NULL DEFAULT '',
    how          TEXT         NOT NULL DEFAULT '',
    note         TEXT         NOT NULL DEFAULT '',
    confidence   VARCHAR(16)  NOT NULL DEFAULT '',
    evidence     TEXT         NOT NULL DEFAULT '',
    url          TEXT         NOT NULL DEFAULT ''        -- source link (file:line)
);
CREATE INDEX IF NOT EXISTS idx_req_test_cases_project ON requirement_test_cases (fern_project);

CREATE TABLE IF NOT EXISTS requirement_links (
    test_key     VARCHAR(512) NOT NULL REFERENCES requirement_test_cases(test_key) ON DELETE CASCADE,
    spec_key     VARCHAR(64)  NOT NULL REFERENCES requirement_specs(spec_key) ON DELETE CASCADE,
    criterion_id VARCHAR(64)  NOT NULL DEFAULT '',       -- '' = the test is about the spec but proves no single criterion
    PRIMARY KEY (test_key, spec_key, criterion_id)
);
CREATE INDEX IF NOT EXISTS idx_req_links_spec ON requirement_links (spec_key, criterion_id);

-- Registry imports, newest last: who/what supplied the snapshot.
CREATE TABLE IF NOT EXISTS requirement_imports (
    id          BIGSERIAL PRIMARY KEY,
    source      TEXT      NOT NULL DEFAULT '',
    git_sha     VARCHAR(64) NOT NULL DEFAULT '',
    specs       INT       NOT NULL DEFAULT 0,
    criteria    INT       NOT NULL DEFAULT 0,
    tests       INT       NOT NULL DEFAULT 0,
    links       INT       NOT NULL DEFAULT 0,
    imported_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Speeds the latest-result-per-test lookup.
CREATE INDEX IF NOT EXISTS idx_spec_runs_spec_name_id ON spec_runs (spec_name, id DESC);

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app') THEN
    ALTER TABLE requirement_specs OWNER TO app;
    ALTER TABLE requirement_criteria OWNER TO app;
    ALTER TABLE requirement_test_cases OWNER TO app;
    ALTER TABLE requirement_links OWNER TO app;
    ALTER TABLE requirement_imports OWNER TO app;
  END IF;
END $$;
