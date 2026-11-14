-- NULL grants all tools. An empty array grants none.
ALTER TABLE agent_mcp_server ADD COLUMN allowed_tools TEXT[];
