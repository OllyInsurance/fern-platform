-- A subtest's parent test (its registry key; empty for a top-level test), so
-- a criterion linked to both takes the subtest's result.
ALTER TABLE requirement_test_cases ADD COLUMN IF NOT EXISTS parent_key VARCHAR(512) NOT NULL DEFAULT '';
