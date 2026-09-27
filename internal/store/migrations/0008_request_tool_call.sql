-- A request a tool call made of a runner, such as a web search, names the call
-- that made it. A request of a reply's own rounds names none.
ALTER TABLE requests ADD COLUMN tool_call_id INTEGER REFERENCES tool_calls(id);
