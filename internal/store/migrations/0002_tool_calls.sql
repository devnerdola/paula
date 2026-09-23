-- What a model asked to be run in the middle of a reply, and what came of it,
-- under the entry of the reply and the request of the round that asked. What
-- was sent back to the model is the result; why a call failed is the error.
CREATE TABLE tool_calls (
	id         INTEGER PRIMARY KEY,
	entry_id   INTEGER NOT NULL REFERENCES entries(id),
	request_id INTEGER NOT NULL REFERENCES requests(id),
	call_id    TEXT    NOT NULL,
	name       TEXT    NOT NULL,
	arguments  TEXT    NOT NULL,
	result     TEXT    NOT NULL DEFAULT '',
	error      TEXT    NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	ended_at   INTEGER
) STRICT;

CREATE INDEX tool_calls_entry ON tool_calls(entry_id);
