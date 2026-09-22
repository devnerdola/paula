CREATE TABLE entries (
	id               INTEGER PRIMARY KEY,
	status           TEXT    NOT NULL,
	error            TEXT    NOT NULL DEFAULT '',
	channel          TEXT    NOT NULL DEFAULT '',
	after_message_id INTEGER REFERENCES messages(id),
	upto_message_id  INTEGER REFERENCES messages(id),
	started_at       INTEGER NOT NULL,
	ended_at         INTEGER
) STRICT;

CREATE TABLE messages (
	id          INTEGER PRIMARY KEY,
	role        TEXT    NOT NULL,
	channel     TEXT    NOT NULL DEFAULT '',
	parts_json  TEXT    NOT NULL,
	reasoning   TEXT    NOT NULL DEFAULT '',
	interrupted INTEGER NOT NULL DEFAULT 0,
	reply_to    INTEGER REFERENCES messages(id),
	entry_id    INTEGER REFERENCES entries(id),
	created_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE media (
	sha256        TEXT PRIMARY KEY,
	caption       TEXT NOT NULL DEFAULT '',
	caption_error TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;

CREATE TABLE requests (
	id                    INTEGER PRIMARY KEY,
	entry_id              INTEGER NOT NULL REFERENCES entries(id),
	purpose               TEXT    NOT NULL,
	runner                TEXT    NOT NULL,
	model                 TEXT    NOT NULL DEFAULT '',
	method                TEXT    NOT NULL,
	url                   TEXT    NOT NULL,
	request_headers_json  TEXT    NOT NULL DEFAULT '',
	request_body_gz       BLOB,
	attempts_json         TEXT    NOT NULL DEFAULT '',
	status                INTEGER NOT NULL DEFAULT 0,
	response_headers_json TEXT    NOT NULL DEFAULT '',
	response_body_gz      BLOB,
	started_at            INTEGER NOT NULL,
	first_byte_at         INTEGER,
	ended_at              INTEGER,
	error                 TEXT    NOT NULL DEFAULT '',
	provider              TEXT    NOT NULL DEFAULT '',
	finish_reason         TEXT    NOT NULL DEFAULT '',
	usage_json            TEXT    NOT NULL DEFAULT '',
	cost                  REAL    NOT NULL DEFAULT 0,
	pruned                INTEGER NOT NULL DEFAULT 0
) STRICT;

-- What a fold wrote. Each stands for a message: a summary for the newest one
-- it covers, a memory for the one it was read out of, and the day each is read
-- back beside is that message's rather than a copy kept here.
CREATE TABLE summaries (
	id              INTEGER PRIMARY KEY,
	upto_message_id INTEGER NOT NULL REFERENCES messages(id),
	content         TEXT    NOT NULL
) STRICT;

CREATE TABLE memories (
	id                INTEGER PRIMARY KEY,
	content           TEXT    NOT NULL,
	source_message_id INTEGER NOT NULL REFERENCES messages(id),
	replaced_by       INTEGER REFERENCES memories(id)
) STRICT;

-- A memory is searched by what it means, which is a vector of the model that
-- read it: a model given the role later writes its own, and a memory that is
-- forgotten takes them with it.
CREATE TABLE memory_embeddings (
	memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
	runner    TEXT    NOT NULL,
	model     TEXT    NOT NULL,
	vector    BLOB    NOT NULL,
	PRIMARY KEY (memory_id, runner, model)
) STRICT;

CREATE INDEX requests_entry ON requests(entry_id);
CREATE INDEX messages_entry ON messages(entry_id);
CREATE INDEX entries_status ON entries(status);
