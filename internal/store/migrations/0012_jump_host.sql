-- The host a host is reached through (a jump host): another host of the
-- same owner (checked by the application). A host that others are reached
-- through cannot be deleted on its own; deleting the owner's hosts all at
-- once (with the account) still works, as the check runs at the end of the
-- statement.
ALTER TABLE hosts ADD COLUMN jump_host_id INTEGER REFERENCES hosts(id);
CREATE INDEX hosts_jump ON hosts(jump_host_id) WHERE jump_host_id IS NOT NULL;
