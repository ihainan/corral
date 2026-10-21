-- An account whose password someone else set (an administrator creating it
-- or resetting the password): it must choose its own before anything else.
ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0;
