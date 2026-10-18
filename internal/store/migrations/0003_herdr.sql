-- herdr as a session-persistence option: which herdr session to attach ('' = the default one).
ALTER TABLE hosts ADD COLUMN herdr_session TEXT NOT NULL DEFAULT '';
