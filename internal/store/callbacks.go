package store

import (
	"context"
	"database/sql"
	"time"
)

// CallbackID numbers the call backs she scheduled.
type CallbackID int64

// Callback is a time she scheduled to write on her own, and why. It is
// pending until it fires, which stores the message she then answers.
type Callback struct {
	ID     CallbackID
	DueAt  time.Time
	Reason string
	// Entry is the reply that scheduled it.
	Entry EntryID
	// Message is the message it fired as, and zero while it is pending.
	Message MessageID
}

const callbackColumns = `id, due_at, reason, entry_id, message_id`

// Schedule keeps a call back and fills in its number.
func (s *Store) Schedule(ctx context.Context, c *Callback) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO callbacks
		(due_at, reason, entry_id) VALUES (?, ?, ?)`,
		c.DueAt.UnixNano(), c.Reason, int64(c.Entry))
	if err != nil {
		return err
	}
	stored, err := res.LastInsertId()
	c.ID = CallbackID(stored)
	return err
}

// Callbacks are the pending call backs, soonest first.
func (s *Store) Callbacks(ctx context.Context) ([]Callback, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+callbackColumns+` FROM callbacks
		 WHERE message_id IS NULL ORDER BY due_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Callback
	for rows.Next() {
		c, err := scanCallback(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// MoveCallback gives a pending call back another time.
func (s *Store) MoveCallback(ctx context.Context, id CallbackID, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE callbacks SET due_at = ?
		 WHERE id = ? AND message_id IS NULL`, at.UnixNano(), id)
	if err != nil {
		return err
	}
	return changedOne(res)
}

// CancelCallback takes a pending call back away.
func (s *Store) CancelCallback(ctx context.Context, id CallbackID) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM callbacks
		 WHERE id = ? AND message_id IS NULL`, id)
	if err != nil {
		return err
	}
	return changedOne(res)
}

// Fire stores the message a pending call back fires as, and names it on the
// call back, in one transaction: a run that ends between the two would fire
// it twice.
func (s *Store) Fire(ctx context.Context, id CallbackID, m *Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := addMessage(ctx, tx, m); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE callbacks SET message_id = ?
		 WHERE id = ? AND message_id IS NULL`, int64(m.ID), id)
	if err != nil {
		return err
	}
	if err := changedOne(res); err != nil {
		return err
	}
	return tx.Commit()
}

func changedOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanCallback(row scanner) (*Callback, error) {
	var (
		c       Callback
		due     int64
		message sql.NullInt64
	)
	if err := row.Scan(&c.ID, &due, &c.Reason, &c.Entry, &message); err != nil {
		return nil, err
	}
	c.DueAt = time.Unix(0, due)
	c.Message = MessageID(id(message))
	return &c, nil
}
