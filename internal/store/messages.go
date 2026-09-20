package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Roles and kinds a message can have.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// Part types a message is made of.
const (
	PartText  = "text"
	PartImage = "image"
)

type Part struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	MIME   string `json:"mime,omitempty"`
}

// MessageID numbers the messages of the conversation.
type MessageID int64

type Message struct {
	ID        MessageID
	Role      string
	Channel   string
	Parts     []Part
	Reasoning string
	// Interrupted says a reply is the part of one that was stopped.
	Interrupted bool
	ReplyTo     MessageID
	EntryID     EntryID
	CreatedAt   time.Time
}

// Text is the text of every text part, joined by blank lines.
func (m *Message) Text() string {
	var parts []string
	for _, p := range m.Parts {
		if p.Type == PartText && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Images are the image parts of the message.
func (m *Message) Images() []Part {
	var out []Part
	for _, p := range m.Parts {
		if p.Type == PartImage {
			out = append(out, p)
		}
	}
	return out
}

const messageColumns = `id, role, channel, parts_json, reasoning,
	interrupted, reply_to, entry_id, created_at`

// AddMessage stores a message and fills in its id.
func (s *Store) AddMessage(ctx context.Context, m *Message) error {
	parts, err := json.Marshal(m.Parts)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages
		(role, channel, parts_json, reasoning, interrupted, reply_to, entry_id,
		 created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Role, m.Channel, string(parts), m.Reasoning,
		m.Interrupted, nullID(m.ReplyTo), nullID(m.EntryID),
		m.CreatedAt.UnixNano())
	if err != nil {
		return err
	}
	stored, err := res.LastInsertId()
	m.ID = MessageID(stored)
	return err
}

func (s *Store) Message(ctx context.Context, id MessageID) (*Message, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE id = ?`, id)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// Messages returns up to limit messages older than before, oldest first.
// before 0 means the newest ones, and limit 0 means all of them.
func (s *Store) Messages(ctx context.Context, before MessageID, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.ro.QueryContext(ctx, `SELECT `+messageColumns+` FROM (
			SELECT `+messageColumns+` FROM messages
			 WHERE ? = 0 OR id < ?
			 ORDER BY id DESC LIMIT ?
		) ORDER BY id`, before, before, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// MessagesAfter returns the messages newer than after, oldest first.
func (s *Store) MessagesAfter(ctx context.Context, after MessageID) ([]Message, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE id > ? ORDER BY id`, after)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// LastMessage returns the newest message with that role, or the newest of any
// role when role is empty.
func (s *Store) LastMessage(ctx context.Context, role string) (*Message, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages
		  WHERE ? = '' OR role = ?
		  ORDER BY id DESC LIMIT 1`, role, role)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ReplyOfEntry returns the message an entry stored, and says when it stored
// none.
func (s *Store) ReplyOfEntry(ctx context.Context, entryID EntryID) (*Message, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages
		  WHERE entry_id = ? ORDER BY id DESC LIMIT 1`, entryID)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

type scanner interface{ Scan(dest ...any) error }

func scanMessage(row scanner) (*Message, error) {
	var (
		m        Message
		parts    string
		replyTo  sql.NullInt64
		entryID  sql.NullInt64
		created  int64
		interrup int64
	)
	err := row.Scan(&m.ID, &m.Role, &m.Channel, &parts,
		&m.Reasoning, &interrup, &replyTo, &entryID, &created)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(parts), &m.Parts); err != nil {
		return nil, err
	}
	m.Interrupted = interrup != 0
	m.ReplyTo = MessageID(id(replyTo))
	m.EntryID = EntryID(id(entryID))
	m.CreatedAt = time.Unix(0, created)
	return &m, nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func prefixed(alias, columns string) string {
	parts := strings.Split(strings.Join(strings.Fields(columns), " "), ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}
