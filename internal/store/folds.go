package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// MemoryID numbers the memories a conversation left behind.
type MemoryID int64

// Summary is the conversation up to a message, in prose. A fold writes a new
// one, so the newest is the one that counts and the older ones say how it got
// there.
type Summary struct {
	ID int64
	// UptoMessageID is the newest message the summary covers, and CoversUpto is
	// when that message was sent, which is read back with the summary rather
	// than kept beside it. The messages after it are the ones a prompt still
	// carries one by one.
	UptoMessageID MessageID
	CoversUpto    time.Time
	Content       string
}

// Memory is a lasting fact of the conversation: one a fold read out of it, or
// one she kept herself in the middle of a reply.
type Memory struct {
	ID      MemoryID
	Content string
	// Source is the message it was said in, and SaidAt is when that message
	// was sent, which is read back with the memory rather than kept beside it.
	Source MessageID
	SaidAt time.Time
	// ReplacedBy is the memory that took its place, or zero while it stands.
	ReplacedBy MemoryID
	// Replaces are the memories this one takes the place of, which Fold and
	// Remember read and write as their ReplacedBy. A memory read back carries
	// ReplacedBy rather than this.
	Replaces []MemoryID
}

const summaryColumns = `summaries.id, upto_message_id, content, messages.created_at`

// summariesFrom is where a summary and the time it covers up to are read from:
// the time is the message's, rather than a copy kept beside the summary.
const summariesFrom = `FROM summaries
	JOIN messages ON messages.id = summaries.upto_message_id`

const memoryColumns = `memories.id, memories.content, source_message_id,
	replaced_by, messages.created_at`

// memoriesFrom is where a memory and the day it was said are read from: the
// day is the message's, rather than a copy kept beside the memory.
const memoriesFrom = `FROM memories
	JOIN messages ON messages.id = memories.source_message_id`

// LatestSummary is the summary that counts, and reports ErrNotFound while the
// conversation has never been folded.
func (s *Store) LatestSummary(ctx context.Context) (*Summary, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+summaryColumns+` `+summariesFrom+` ORDER BY summaries.id DESC LIMIT 1`)
	out, err := scanSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// Memories are the memories that still stand, oldest first, which is the order
// a prompt tells them in.
func (s *Store) Memories(ctx context.Context) ([]Memory, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+` `+memoriesFrom+`
		 WHERE replaced_by IS NULL ORDER BY memories.id`)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// LatestMemories are the memories that still stand, newest first, at most
// limit of them: what she has been told most recently.
func (s *Store) LatestMemories(ctx context.Context, limit int) ([]Memory, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+` `+memoriesFrom+`
		 WHERE replaced_by IS NULL ORDER BY memories.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// SearchMemories are the memories that still stand and hold any of the words,
// the ones the words say most about first, at most limit of them. A word finds
// its other forms, as "sisters" finds a memory of a sister, and a word most
// memories hold says little of which one is meant.
func (s *Store) SearchMemories(ctx context.Context, words []string, limit int) ([]Memory, error) {
	if len(words) == 0 {
		return nil, nil
	}
	// Each word is looked for as a word, whatever it spells: AND or NOT in a
	// query are what was asked about, not how.
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	rows, err := s.ro.QueryContext(ctx, `SELECT `+memoryColumns+` `+memoriesFrom+`
		  JOIN memory_words ON memory_words.rowid = memories.id
		 WHERE memory_words MATCH ? AND replaced_by IS NULL
		 ORDER BY bm25(memory_words) LIMIT ?`, strings.Join(quoted, " OR "), limit)
	if err != nil {
		return nil, err
	}
	return scanMemories(rows)
}

// Forget deletes a memory and the ones it took the place of, which stand for
// nothing once what replaced them is gone. The summary and the messages stay:
// forgetting is about what she carries, not about what was said. It returns
// what went, oldest first.
func (s *Store) Forget(ctx context.Context, id MemoryID) ([]Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// What replaced a memory is newer than it, so the ones going are the one
	// named and everything that walks back to it. The set is read in one step,
	// which is what ends the walk: a memory already in it is not followed again.
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE going(id) AS (
			SELECT ?
			UNION
			SELECT memories.id FROM memories JOIN going ON memories.replaced_by = going.id
		)
		SELECT `+memoryColumns+` `+memoriesFrom+`
		 WHERE memories.id IN (SELECT id FROM going)
		 ORDER BY memories.id`, int64(id))
	if err != nil {
		return nil, err
	}
	out, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}

	// The oldest goes first, since a memory points at the one that replaced it
	// and nothing may point at a row that is gone.
	for _, m := range out {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, int64(m.ID)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// Remember keeps a memory she wrote down herself in the middle of a reply,
// rather than one a fold read out of the conversation, and fills in its ID.
//
// What it replaces are numbers she chose, so one that does not name a memory
// that stands keeps nothing, where a fold would leave it be: she is told, and
// asks again with what she meant.
func (s *Store) Remember(ctx context.Context, m *Memory) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `INSERT INTO memories
		(content, source_message_id) VALUES (?, ?)`,
		m.Content, int64(m.Source))
	if err != nil {
		return err
	}
	stored, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for i, replaced := range m.Replaces {
		// A number named twice is one memory, replaced once.
		if slices.Contains(m.Replaces[:i], replaced) {
			continue
		}
		// A number that named nothing before this memory was kept may be the
		// one it was given, so only an older memory is taken.
		res, err := tx.ExecContext(ctx, `UPDATE memories SET replaced_by = ?
			 WHERE id = ? AND id < ? AND replaced_by IS NULL`,
			stored, int64(replaced), stored)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("%w: memory %d does not stand", ErrNotFound, replaced)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.ID = MemoryID(stored)
	return nil
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
		(upto_message_id, content) VALUES (?, ?)`,
		int64(summary.UptoMessageID), summary.Content)
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
			(content, source_message_id) VALUES (?, ?)`,
			m.Content, int64(m.Source))
		if err != nil {
			return err
		}
		stored, err := res.LastInsertId()
		if err != nil {
			return err
		}
		m.ID = MemoryID(stored)
		for _, replaced := range m.Replaces {
			// A memory takes the place of one older than itself. The fold read
			// the memories it replaces before it asked its model, and a number
			// is given again once the row that held it is forgotten, so a step
			// that took long enough can name what is now its own row.
			if _, err := tx.ExecContext(ctx,
				`UPDATE memories SET replaced_by = ? WHERE id = ? AND id < ?`,
				int64(m.ID), int64(replaced), int64(m.ID)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func scanSummary(row scanner) (*Summary, error) {
	var out Summary
	var upto sql.NullInt64
	var covers int64
	if err := row.Scan(&out.ID, &upto, &out.Content, &covers); err != nil {
		return nil, err
	}
	out.UptoMessageID = MessageID(id(upto))
	out.CoversUpto = time.Unix(0, covers)
	return &out, nil
}

func scanMemories(rows *sql.Rows) ([]Memory, error) {
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

func scanMemory(row scanner) (*Memory, error) {
	var out Memory
	var source, replaced sql.NullInt64
	var said int64
	if err := row.Scan(&out.ID, &out.Content, &source, &replaced, &said); err != nil {
		return nil, err
	}
	out.Source = MessageID(id(source))
	out.ReplacedBy = MemoryID(id(replaced))
	out.SaidAt = time.Unix(0, said)
	return &out, nil
}
