-- What Claude Code's model picker listed, as the gateway last read it off a
-- terminal — kept so that the list is told at once the next time, a
-- gateway started again included, instead of opening the picker in the
-- person's terminal to read it (seconds, and seen there). By the account
-- on its host (as settings_pending's), the host its requests go to and how
-- it signs in: processes alike in those are offered the same models. rows
-- is the list as the page is given it (JSON). Nothing here is a secret;
-- gone by for a limited time (the gateway's lineListFor), and let go when
-- a switch did not go as the list said.
CREATE TABLE model_lists (
  key  TEXT PRIMARY KEY,
  at   INTEGER NOT NULL,
  rows BLOB NOT NULL
);
