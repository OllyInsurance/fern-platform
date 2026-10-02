-- Coverage over time. A criterion's verdict is decided on request from the
-- latest results (GET /requirements/coverage), so yesterday's answer is gone
-- once new results land. A snapshot keeps one row per criterion per day: the
-- verdict and gap the coverage page showed (branch main, last 30 days).
-- Written by a background job (at startup and every few hours, replacing the
-- day's rows) and after each registry import. Derived data: safe to truncate.

CREATE TABLE IF NOT EXISTS coverage_snapshots (
    snapshot_date DATE        NOT NULL,
    spec_key      VARCHAR(64) NOT NULL,
    criterion_id  VARCHAR(64) NOT NULL,
    kind          VARCHAR(16) NOT NULL DEFAULT '',
    spec_title    TEXT        NOT NULL DEFAULT '',
    verdict       VARCHAR(16) NOT NULL,
    gap_category  VARCHAR(32) NOT NULL DEFAULT '',
    build_status  VARCHAR(16) NOT NULL DEFAULT '',
    taken_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (snapshot_date, spec_key, criterion_id)
);
CREATE INDEX IF NOT EXISTS idx_coverage_snapshots_date ON coverage_snapshots (snapshot_date);

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app') THEN
    ALTER TABLE coverage_snapshots OWNER TO app;
  END IF;
END $$;

-- The read-only role the lenspack dashboards use, when it exists.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lenspack_ro') THEN
    GRANT SELECT ON coverage_snapshots TO lenspack_ro;
  END IF;
END $$;
