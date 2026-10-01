DROP INDEX IF EXISTS idx_spec_runs_spec_name_id;
DROP TABLE IF EXISTS requirement_imports;
DROP TABLE IF EXISTS requirement_links;
DROP TABLE IF EXISTS requirement_test_cases;
DROP TABLE IF EXISTS requirement_criteria;
DROP TABLE IF EXISTS requirement_specs;
ALTER TABLE spec_runs DROP COLUMN IF EXISTS metadata;
