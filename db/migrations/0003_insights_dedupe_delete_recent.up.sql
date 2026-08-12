-- Evidence-backed insight records (P4-B02): one row per user, type and
-- period, so regenerated summaries upsert instead of duplicating.
CREATE UNIQUE INDEX idx_insights_user_type_period
    ON insights(user_id, insight_type, period_start);

-- Chat state for individual transaction deletion (P4-D02).
ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check
    CHECK (kind IN (
        'confirm_extraction','edit_total','edit_merchant','edit_date',
        'edit_category','edit_type','delete_account','delete_recent'));
