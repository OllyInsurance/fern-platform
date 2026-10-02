-- What people add on top of the requirements registry: free JSON metadata per
-- object (spec, criterion, test, gap), and saved boards with their items.
-- Separate tables, so a registry import (PUT /api/v1/requirements, which
-- replaces the requirement_* tables) never touches them.

CREATE TABLE IF NOT EXISTS requirement_meta (
    obj_kind   VARCHAR(16)  NOT NULL,                  -- spec | criterion | test | gap
    obj_key    VARCHAR(1024) NOT NULL,                 -- ENG-465, ENG-465:AC-06, a test key
    data       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by TEXT         NOT NULL DEFAULT '',
    PRIMARY KEY (obj_kind, obj_key)
);

CREATE TABLE IF NOT EXISTS coverage_boards (
    id         BIGSERIAL    PRIMARY KEY,
    name       TEXT         NOT NULL,
    kind       VARCHAR(16)  NOT NULL,                  -- buckets | gantt
    definition JSONB        NOT NULL DEFAULT '{}'::jsonb, -- opaque to Fern
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by TEXT         NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS coverage_board_items (
    id         BIGSERIAL    PRIMARY KEY,
    board_id   BIGINT       NOT NULL REFERENCES coverage_boards(id) ON DELETE CASCADE,
    title      TEXT         NOT NULL DEFAULT '',
    start_date VARCHAR(10)  NOT NULL DEFAULT '',       -- YYYY-MM-DD or empty
    end_date   VARCHAR(10)  NOT NULL DEFAULT '',
    depends_on JSONB        NOT NULL DEFAULT '[]'::jsonb, -- item ids on the same board
    bucket     TEXT         NOT NULL DEFAULT '',
    refs       JSONB        NOT NULL DEFAULT '[]'::jsonb, -- [{kind, key}]
    metadata   JSONB        NOT NULL DEFAULT '{}'::jsonb,
    sort       DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_by TEXT         NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_coverage_board_items_board ON coverage_board_items (board_id, sort, id);

DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app') THEN
    ALTER TABLE requirement_meta OWNER TO app;
    ALTER TABLE coverage_boards OWNER TO app;
    ALTER TABLE coverage_board_items OWNER TO app;
  END IF;
END $$;
