-- Why a criterion is not proven today and what would prove it. Imported with
-- the registry (PUT /api/v1/requirements); empty when there is no gap.
ALTER TABLE requirement_criteria
    ADD COLUMN IF NOT EXISTS gap_category VARCHAR(32) NOT NULL DEFAULT '', -- not_built | awaiting_decision | external_dependency | test_infra | defect
    ADD COLUMN IF NOT EXISTS gap_reason   TEXT        NOT NULL DEFAULT '', -- why it is not covered today
    ADD COLUMN IF NOT EXISTS gap_pathway  TEXT        NOT NULL DEFAULT '', -- the step that would cover it
    ADD COLUMN IF NOT EXISTS gap_ticket   TEXT        NOT NULL DEFAULT '', -- OllyInsurance/olly#N, Linear ENG-N
    ADD COLUMN IF NOT EXISTS gap_source   TEXT        NOT NULL DEFAULT ''; -- file:line of the skip / fixme / defect
