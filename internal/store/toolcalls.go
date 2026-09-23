package store

import (
	"context"
	"database/sql"
	"time"
)

// ToolCall is one tool a model asked to be run, and what came of it. Result is
// what was sent back to the model, and Error is why the call failed, empty
// when it did not.
type ToolCall struct {
	ID        int64
	EntryID   EntryID
	RequestID int64
	CallID    string
	Name      string
	Arguments string
	Result    string
	Error     string
	StartedAt time.Time
	EndedAt   time.Time
}

// StartToolCall records a call as it starts and fills in its id, so a run that
// stops in the middle of one leaves a record that it was asked for.
func (s *Store) StartToolCall(ctx context.Context, c *ToolCall) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO tool_calls
		(entry_id, request_id, call_id, name, arguments, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.EntryID, c.RequestID, c.CallID, c.Name, c.Arguments, c.StartedAt.UnixNano())
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

// EndToolCall records what came of a call.
func (s *Store) EndToolCall(ctx context.Context, c *ToolCall) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tool_calls
		   SET result = ?, error = ?, ended_at = ?
		 WHERE id = ?`,
		c.Result, c.Error, c.EndedAt.UnixNano(), c.ID)
	return err
}

// ToolCalls are the calls an entry made, in the order they were made.
func (s *Store) ToolCalls(ctx context.Context, entry EntryID) ([]ToolCall, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT id, entry_id, request_id, call_id,
		       name, arguments, result, error, started_at, ended_at
		  FROM tool_calls WHERE entry_id = ? ORDER BY id`, entry)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var c ToolCall
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&c.ID, &c.EntryID, &c.RequestID, &c.CallID, &c.Name,
			&c.Arguments, &c.Result, &c.Error, &started, &ended); err != nil {
			return nil, err
		}
		c.StartedAt = time.Unix(0, started)
		c.EndedAt = at(ended)
		out = append(out, c)
	}
	return out, rows.Err()
}
