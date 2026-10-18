-- Connection version: bumped whenever an edit changes what a connection to
-- the host is (address, port, user, how it authenticates, the saved password)
-- or which machine key it trusts. Connections made under an older version
-- are not reused, borrowed or pooled.
ALTER TABLE hosts ADD COLUMN conn_version INTEGER NOT NULL DEFAULT 1;
