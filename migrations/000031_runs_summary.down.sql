DROP INDEX IF EXISTS idx_spec_runs_spec_name_pattern;
DROP INDEX IF EXISTS idx_test_runs_updated_at;
DROP INDEX IF EXISTS idx_test_runs_ci_run_id;
DROP TABLE IF EXISTS ci_runs_state;
DROP TABLE IF EXISTS ci_run_nodes;
DROP TABLE IF EXISTS ci_run_test_runs;
DROP TABLE IF EXISTS ci_runs;
