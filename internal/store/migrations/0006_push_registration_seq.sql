-- Registration versions come from this AUTOINCREMENT counter, which never
-- hands out a number twice, even after the subscriptions that used it are
-- deleted. (Rows are deleted right after use; only the counter matters.)
CREATE TABLE push_registration_seq (id INTEGER PRIMARY KEY AUTOINCREMENT);
INSERT INTO push_registration_seq (id) SELECT COALESCE(MAX(registered), 0) + 1 FROM push_subscriptions;
DELETE FROM push_registration_seq;
