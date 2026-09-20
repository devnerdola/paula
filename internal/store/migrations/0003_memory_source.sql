-- A memory is what was read out of a message, so the message it was read from
-- is part of what it is. The column it is held in took no message, and one that
-- held none would be read by nothing: every memory is read back beside the day
-- it was said, which is the message's.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE memories_with_source (
	id                INTEGER PRIMARY KEY,
	content           TEXT    NOT NULL,
	source_message_id INTEGER NOT NULL REFERENCES messages(id),
	replaced_by       INTEGER REFERENCES memories_with_source(id),
	entry_id          INTEGER REFERENCES entries(id),
	created_at        INTEGER NOT NULL
) STRICT;

INSERT INTO memories_with_source
	(id, content, source_message_id, replaced_by, entry_id, created_at)
	SELECT id, content, source_message_id, replaced_by, entry_id, created_at
	  FROM memories;

DROP TABLE memories;
ALTER TABLE memories_with_source RENAME TO memories;
