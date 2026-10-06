package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Roles a message can have. A call back is a message she answers the way she
// answers one of the user's: the time she scheduled to write on her own, and
// why, stored as its text when it fires.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleCallback  = "callback"
)

// Asked reports whether a message is one she answers: one the user sent, or a
// call back that fired.
func (m *Message) Asked() bool { return m.Role == RoleUser || m.Role == RoleCallback }

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
	// ReasoningDetails are what a reply thought as its API sent it, which
	// goes back to that API with the reply.
	ReasoningDetails []json.RawMessage
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
	reasoning_details, interrupted, reply_to, entry_id, created_at`

// AddMessage stores a message and fills in its id.
func (s *Store) AddMessage(ctx context.Context, m *Message) error {
	return addMessage(ctx, s.db, m)
}

// execer runs a statement on the database or within a transaction of it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func addMessage(ctx context.Context, db execer, m *Message) error {
	parts, err := json.Marshal(m.Parts)
	if err != nil {
		return err
	}
	details, err := json.Marshal(m.ReasoningDetails)
	if err != nil {
		return err
	}
	if m.ReasoningDetails == nil {
		details = []byte("[]")
	}
	res, err := db.ExecContext(ctx, `INSERT INTO messages
		(role, channel, parts_json, reasoning, reasoning_details, interrupted,
		 reply_to, entry_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Role, m.Channel, string(parts), m.Reasoning, string(details),
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

// Messages returns up to limit of the messages the two of them said, the
// user's and hers, older than before, oldest first, and how many they said in
// all. A call back that came due is the app's, and is not among them. before 0
// means the newest ones, and limit 0 means all of them.
func (s *Store) Messages(ctx context.Context, before MessageID, limit int) (found []Message, said int, err error) {
	if limit <= 0 {
		limit = -1
	}
	err = s.snapshot(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE role IN (?, ?)`,
			RoleUser, RoleAssistant).Scan(&said); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+messageColumns+` FROM (
				SELECT `+messageColumns+` FROM messages
				 WHERE role IN (?, ?) AND (? = 0 OR id < ?)
				 ORDER BY id DESC LIMIT ?
			) ORDER BY id`, RoleUser, RoleAssistant, before, before, limit)
		if err != nil {
			return err
		}
		found, err = scanMessages(rows)
		return err
	})
	return found, said, err
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

// LastAsked returns the newest message she answers: the user's, or a call
// back that fired.
func (s *Store) LastAsked(ctx context.Context) (*Message, error) {
	row := s.ro.QueryRowContext(ctx,
		`SELECT `+messageColumns+` FROM messages
		  WHERE role IN (?, ?)
		  ORDER BY id DESC LIMIT 1`, RoleUser, RoleCallback)
	m, err := scanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// Writer is the runner and the model a reply was written by.
type Writer struct {
	Runner string
	Model  string
}

// Writers are what wrote each reply after a message: the runner and the model
// of the last request its entry sent for a reply. A reply whose entry sent no
// such request has no writer.
func (s *Store) Writers(ctx context.Context, after MessageID) (map[MessageID]Writer, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT m.id, r.runner, r.model
		  FROM messages m
		  JOIN requests r ON r.id = (SELECT max(id) FROM requests
		                              WHERE entry_id = m.entry_id AND purpose = ?)
		 WHERE m.role = ? AND m.id > ?`, PurposeReply, RoleAssistant, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[MessageID]Writer{}
	for rows.Next() {
		var id MessageID
		var w Writer
		if err := rows.Scan(&id, &w.Runner, &w.Model); err != nil {
			return nil, err
		}
		out[id] = w
	}
	return out, rows.Err()
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
		details  string
		replyTo  sql.NullInt64
		entryID  sql.NullInt64
		created  int64
		interrup int64
	)
	err := row.Scan(&m.ID, &m.Role, &m.Channel, &parts,
		&m.Reasoning, &details, &interrup, &replyTo, &entryID, &created)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(parts), &m.Parts); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(details), &m.ReasoningDetails); err != nil {
		return nil, err
	}
	if len(m.ReasoningDetails) == 0 {
		m.ReasoningDetails = nil
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
