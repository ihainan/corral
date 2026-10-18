-- A push subscription belongs to the browser login that registered it and ends
-- with it (sign-out, expiry, password change, disabling the account).
ALTER TABLE push_subscriptions ADD COLUMN login_hash TEXT REFERENCES login_sessions(token_hash) ON DELETE CASCADE;
CREATE INDEX push_subscriptions_login ON push_subscriptions(login_hash);
-- Older rows have no login to end with; browsers register again on their next visit.
DELETE FROM push_subscriptions WHERE login_hash IS NULL;
