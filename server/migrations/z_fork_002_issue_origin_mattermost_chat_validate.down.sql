-- PostgreSQL cannot mark a validated constraint NOT VALID. Recreate the state
-- immediately after the widening migration, which enforces the CHECK on new
-- writes before validating existing rows.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'slack_chat', 'agent_create', 'dingtalk_chat', 'wecom_chat', 'telegram_chat', 'mattermost_chat'))
    NOT VALID;
