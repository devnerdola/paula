-- A memory is searched by the words it holds. The index keeps the words and
-- reads the text from the memories, so it says nothing the memories do not.
DROP TABLE memory_embeddings;

CREATE VIRTUAL TABLE memory_words USING fts5(
	content,
	content = 'memories',
	content_rowid = 'id',
	tokenize = 'porter unicode61 remove_diacritics 2'
);

INSERT INTO memory_words(memory_words) VALUES ('rebuild');

-- A memory is written once and deleted whole, so those are the two changes
-- the index follows.
CREATE TRIGGER memory_words_insert AFTER INSERT ON memories BEGIN
	INSERT INTO memory_words(rowid, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER memory_words_delete AFTER DELETE ON memories BEGIN
	INSERT INTO memory_words(memory_words, rowid, content) VALUES ('delete', old.id, old.content);
END;

-- The log of the requests that turned memories and searches into vectors goes
-- with them: nothing sends one any more, and nothing reads one back. A batch
-- of memories was embedded under an entry of its own, which goes too; a search
-- was sent under the reply that asked, which stays.
CREATE TEMP TABLE embedding_entries AS
	SELECT DISTINCT entry_id AS id FROM requests WHERE purpose = 'embedding';
DELETE FROM requests WHERE purpose IN ('embedding', 'memory-search');
DELETE FROM entries WHERE id IN (SELECT id FROM embedding_entries);
DROP TABLE embedding_entries;
