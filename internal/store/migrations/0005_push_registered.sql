-- Order in which subscriptions were last registered (re-registering moves one
-- to the end); the per-user cap drops the least recently registered.
ALTER TABLE push_subscriptions ADD COLUMN registered INTEGER NOT NULL DEFAULT 0;
UPDATE push_subscriptions SET registered = id;
