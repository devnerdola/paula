CREATE TABLE summaries (
	id              INTEGER PRIMARY KEY,
	upto_message_id INTEGER NOT NULL REFERENCES messages(id),
	content         TEXT    NOT NULL,
	entry_id        INTEGER REFERENCES entries(id),
	created_at      INTEGER NOT NULL
) STRICT;

CREATE TABLE memories (
	id                INTEGER PRIMARY KEY,
	content           TEXT    NOT NULL,
	source_message_id INTEGER REFERENCES messages(id),
	replaced_by       INTEGER REFERENCES memories(id),
	entry_id          INTEGER REFERENCES entries(id),
	created_at        INTEGER NOT NULL
) STRICT;
