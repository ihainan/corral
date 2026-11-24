-- A user's own picture (doc UI item 6, 11-24): squared and made small in the
-- browser, checked here; kept apart from users so its bytes are never read
-- with the account on every request.
CREATE TABLE user_avatars (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    mime       TEXT    NOT NULL,
    image      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
);
