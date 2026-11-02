-- The person said no, on the card, to having the gateway put the hook that
-- makes Claude Code write its questions to its log at once into this host's
-- Claude Code settings (see web/askhook.go): the card no longer asks for it.
-- Set again to 0 once they turn it on.
ALTER TABLE hosts ADD COLUMN ask_hook_declined INTEGER NOT NULL DEFAULT 0;
