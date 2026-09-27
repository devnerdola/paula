-- A call back is a time she scheduled to write on her own, and why. It stands
-- until it is cancelled, which deletes it, or fires, which stores the message
-- she then answers and names it here.
CREATE TABLE callbacks (
	id         INTEGER PRIMARY KEY,
	due_at     INTEGER NOT NULL,
	reason     TEXT    NOT NULL,
	entry_id   INTEGER NOT NULL REFERENCES entries(id),
	message_id INTEGER REFERENCES messages(id)
) STRICT;

CREATE INDEX callbacks_pending ON callbacks(due_at) WHERE message_id IS NULL;
