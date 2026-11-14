-- Validate the widened CHECK in a separate migration to avoid holding the
-- ACCESS EXCLUSIVE lock during the table scan.
ALTER TABLE issue VALIDATE CONSTRAINT issue_origin_type_check;
