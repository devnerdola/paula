-- A picture she took names the call that took it. A picture sent to her names
-- none.
ALTER TABLE media ADD COLUMN tool_call_id INTEGER REFERENCES tool_calls(id);
