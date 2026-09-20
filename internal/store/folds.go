package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MemoryID numbers the memories a conversation left behind.
type MemoryID int64

// Summary is the conversation up to a message, in prose. A fold writes a new
// one, so the newest is the one that counts and the older ones say how it got
// there.
type Summary struct {
	ID int64
	// UptoMessageID is the newest message the summary covers. The messages
	// after it are the ones a prompt still carries one by one.
	UptoMessageID MessageID
	Content       string
	EntryID       EntryID
	CreatedAt     time.Time
}

// Memory is a lasting fact a fold read out of the conversation.
type Memory struct {
	ID      MemoryID
	Content string
	// Source is the message it was said in, and SaidAt is when that message
	// was sent, which is read back with the memory rather than kept beside it.
	Source MessageID
	SaidAt time.Time
	// ReplacedBy is the memory that took its place, or zero while it stands.
	ReplacedBy MemoryID
	EntryID    EntryID
	CreatedAt  time.Time
	// Replaces are the memories this one takes the place of, which Fold reads
	// and writes as their ReplacedBy. A memory read back carries ReplacedBy
	// rather than this.
	Replaces []MemoryID
}

const summaryColumns = `id, upto_message_id, content, entry_id, created_at`

const memoryColumns = `memories.id, content, source_message_id, replaced_by,
	memories.entry_id, memories.created_at, messages.created_at`

// LatestSummary is the summary that counts, and reports ErrNotFound while the
// conversation has never been folded.
func (s *Store) LatestSummary(ctx context.Context) (*Summary, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+summaryColumns+` FROM summaries ORDER BY id DESC LIMIT 1`)
	out, err := scanSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// Memories are the memories that still stand, oldest first, which is the order
// a prompt tells them in.
func (s *Store) Memories(ctx context.Context) ([]Memory, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+`
		  FROM memories JOIN messages ON messages.id = memories.source_message_id
		 WHERE replaced_by IS NULL ORDER BY memories.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Fold stores what one step of a fold learned: the summary as it now reads,
// and the memories it read out of the messages that went into it. Both land
// together, since a summary that covers messages whose memories were lost
// would lose them for good.
func (s *Store) Fold(ctx context.Context, summary *Summary, memories []Memory) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO summaries
		(upto_message_id, content, entry_id, created_at)
		VALUES (?, ?, ?, ?)`,
		int64(summary.UptoMessageID), summary.Content,
		nullID(summary.EntryID), summary.CreatedAt.UnixNano())
	if err != nil {
		return err
	}
	stored, err := res.LastInsertId()
	if err != nil {
		return err
	}
	summary.ID = stored

	for i := range memories {
		m := &memories[i]
		res, err := tx.ExecContext(ctx, `INSERT INTO memories
			(content, source_message_id, entry_id, created_at)
			VALUES (?, ?, ?, ?)`,
			m.Content, nullID(m.Source), nullID(m.EntryID), m.CreatedAt.UnixNano())
		if err != nil {
			return err
		}
		stored, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.ID = MemoryID(stored)
		for _, replaced := range m.Replaces {
			if _, err := tx.ExecContext(ctx,
				`UPDATE memories SET replaced_by = ? WHERE id = ?`,
				int64(m.ID), int64(replaced)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func scanSummary(row scanner) (*Summary, error) {
	var out Summary
	var upto, entry sql.NullInt64
	var created int64
	if err := row.Scan(&out.ID, &upto, &out.Content, &entry, &created); err != nil {
		return nil, err
	}
	out.UptoMessageID = MessageID(id(upto))
	out.EntryID = EntryID(id(entry))
	out.CreatedAt = time.Unix(0, created)
	return &out, nil
}

func scanMemory(row scanner) (*Memory, error) {
	var out Memory
	var source, replaced, entry sql.NullInt64
	var created, said int64
	if err := row.Scan(&out.ID, &out.Content, &source, &replaced, &entry, &created, &said); err != nil {
		return nil, err
	}
	out.Source = MessageID(id(source))
	out.ReplacedBy = MemoryID(id(replaced))
	out.EntryID = EntryID(id(entry))
	out.CreatedAt = time.Unix(0, created)
	out.SaidAt = time.Unix(0, said)
	return &out, nil
}
