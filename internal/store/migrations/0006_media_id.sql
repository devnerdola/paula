-- A picture is looked up by its number, which was the rowid SQLite gives a
-- table keyed by text: one a VACUUM may give out again. media keeps its number
-- in a column of its own, the one each picture was given.
CREATE TABLE media_numbered (
	id            INTEGER PRIMARY KEY,
	sha256        TEXT    NOT NULL UNIQUE,
	caption       TEXT    NOT NULL DEFAULT '',
	caption_error TEXT    NOT NULL DEFAULT ''
) STRICT;

INSERT INTO media_numbered (id, sha256, caption, caption_error)
	SELECT rowid, sha256, caption, caption_error FROM media;

DROP TABLE media;
ALTER TABLE media_numbered RENAME TO media;
