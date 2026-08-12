ALTER TABLE pending_actions DROP CONSTRAINT pending_actions_kind_check;
ALTER TABLE pending_actions ADD CONSTRAINT pending_actions_kind_check
    CHECK (kind IN (
        'confirm_extraction','edit_total','edit_merchant','edit_date',
        'edit_category','edit_type','delete_account'));

DROP INDEX IF EXISTS idx_insights_user_type_period;
