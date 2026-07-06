-- Create share links table (presigned + shortened share URLs for test results)
CREATE TABLE IF NOT EXISTS share_links (
    id         BIGSERIAL    PRIMARY KEY,
    code       VARCHAR(64)  NOT NULL,
    path       TEXT         NOT NULL,
    created_by VARCHAR(255) NOT NULL DEFAULT '',
    expires_at TIMESTAMP    NOT NULL,
    created_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

CREATE UNIQUE INDEX idx_share_links_code ON share_links (code);
CREATE INDEX idx_share_links_expires_at ON share_links (expires_at);
CREATE INDEX idx_share_links_deleted_at ON share_links (deleted_at);

-- Grant permissions to app user
ALTER TABLE share_links OWNER TO app;
GRANT ALL PRIVILEGES ON TABLE share_links TO app;
