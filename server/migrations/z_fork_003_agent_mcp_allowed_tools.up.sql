-- NULL grants all tools. An empty array grants none.
-- The earlier numeric migration may already have created this column.
ALTER TABLE agent_mcp_server ADD COLUMN IF NOT EXISTS allowed_tools TEXT[];
