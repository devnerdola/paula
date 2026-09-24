-- The embed role is gone, and a model the conversation was given for it goes
-- with it: the kv table holds a model for each role there is.
DELETE FROM kv WHERE key = 'model.embed';
