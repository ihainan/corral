-- The gateway made the host's Claude Code settings file itself when turning
-- the hook on (there was none): the host's connection version then (0: it did
-- not). Turning the hook off may remove the file only while the host still
-- connects as it did then (see web/askhook.go).
ALTER TABLE hosts ADD COLUMN ask_hook_created INTEGER NOT NULL DEFAULT 0;
