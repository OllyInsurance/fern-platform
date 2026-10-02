ALTER TABLE requirement_criteria
    DROP COLUMN IF EXISTS gap_category,
    DROP COLUMN IF EXISTS gap_reason,
    DROP COLUMN IF EXISTS gap_pathway,
    DROP COLUMN IF EXISTS gap_ticket,
    DROP COLUMN IF EXISTS gap_source;
