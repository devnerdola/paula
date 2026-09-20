CREATE TABLE memory_embeddings (
	memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	runner    TEXT    NOT NULL,
	model     TEXT    NOT NULL,
	vector    BLOB    NOT NULL,
	PRIMARY KEY (memory_id, runner, model)
) STRICT;
