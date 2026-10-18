-- Edit version: bumped by every saved edit of the host. An edit names the
-- version its form was loaded at, and is refused when the host was saved
-- since (from another device or tab) instead of silently undoing that save.
ALTER TABLE hosts ADD COLUMN version INTEGER NOT NULL DEFAULT 1;
