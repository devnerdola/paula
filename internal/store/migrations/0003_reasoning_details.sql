-- What a reply thought, as the API it came from sent it, for the API to be
-- handed back with the reply: a model that reasons reads what it thought
-- before, and some of it is signed, so it goes back exactly as it came. A JSON
-- array, empty for a reply whose API sends reasoning as text alone.
ALTER TABLE messages ADD COLUMN reasoning_details TEXT NOT NULL DEFAULT '[]';
