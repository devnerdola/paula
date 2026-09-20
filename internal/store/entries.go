package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Statuses an entry can end with.
const (
	StatusRunning   = "running"
	StatusDone      = "done"
	StatusStopped   = "stopped"
	StatusRestarted = "restarted"
	StatusFailed    = "failed"
)

// Purposes a request can be sent for.
const (
	PurposeReply    = "reply"
	PurposeCaption  = "caption"
	PurposeMemories = "memories"
	PurposeSummary  = "summary"
	// PurposeCompaction is a summary written again from itself, with no
	// messages added, because it outgrew the room it has.
	PurposeCompaction = "compaction"
)

// EntryID numbers the entries of the turn log.
type EntryID int64

type Entry struct {
	ID     EntryID
	Status string
	Error  string
	// AfterMessageID and UptoMessageID are the messages the entry was started
	// for: those after the one the conversation was answered up to, through the
	// latest one when it started. Zero is none.
	AfterMessageID MessageID
	UptoMessageID  MessageID
	Channel        string
	StartedAt      time.Time
	EndedAt        time.Time
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

type Attempt struct {
	StartedAt   time.Time     `json:"started_at"`
	FirstByteAt time.Time     `json:"first_byte_at,omitzero"`
	EndedAt     time.Time     `json:"ended_at,omitzero"`
	Status      int           `json:"status,omitempty"`
	Error       string        `json:"error,omitempty"`
	RetryAfter  time.Duration `json:"retry_after,omitempty"`
}

type Request struct {
	ID              int64
	EntryID         EntryID
	Purpose         string
	Runner          string
	Model           string
	Method          string
	URL             string
	RequestHeaders  http.Header
	RequestBody     []byte
	Attempts        []Attempt
	Status          int
	ResponseHeaders http.Header
	ResponseBody    []byte
	StartedAt       time.Time
	FirstByteAt     time.Time
	EndedAt         time.Time
	Error           string
	Provider        string
	FinishReason    string
	Usage           *Usage
	Cost            float64

	// Pruned says the bodies of this request were dropped by engine.log_keep.
	Pruned bool
}

const entryColumns = `id, status, error, channel, after_message_id,
	upto_message_id, started_at, ended_at`

// StartEntry records a running entry and fills in its id.
func (s *Store) StartEntry(ctx context.Context, e *Entry) error {
	e.Status = StatusRunning
	res, err := s.db.ExecContext(ctx, `INSERT INTO entries
		(status, error, channel, after_message_id, upto_message_id, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		e.Status, e.Error, e.Channel, nullID(e.AfterMessageID),
		nullID(e.UptoMessageID), e.StartedAt.UnixNano())
	if err != nil {
		return err
	}
	started, err := res.LastInsertId()
	e.ID = EntryID(started)
	return err
}

// EndEntry closes an entry with its status and what it answered. The reply
// itself names the entry it belongs to, so nothing here points back at it.
func (s *Store) EndEntry(ctx context.Context, e *Entry) error {
	_, err := s.db.ExecContext(ctx, `UPDATE entries
		   SET status = ?, error = ?, upto_message_id = ?, ended_at = ?
		 WHERE id = ?`,
		e.Status, e.Error, nullID(e.UptoMessageID), ns(e.EndedAt), e.ID)
	return err
}

// AnsweredUpto is the newest message an entry has answered: the largest
// upto_message_id of the entries that ended with a reply or with a stop. It is
// what a run picks the conversation up from.
func (s *Store) AnsweredUpto(ctx context.Context) (MessageID, error) {
	var upto sql.NullInt64
	err := s.ro.QueryRowContext(ctx,
		`SELECT max(upto_message_id) FROM entries WHERE status IN (?, ?)`,
		StatusDone, StatusStopped).Scan(&upto)
	if err != nil {
		return 0, err
	}
	return MessageID(id(upto)), nil
}

func (s *Store) Entry(ctx context.Context, id EntryID) (*Entry, error) {
	row := s.ro.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// Entries returns the latest entries, newest first.
func (s *Store) Entries(ctx context.Context, limit int) ([]Entry, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// RunningEntries returns every entry that reads as running, which at the start
// of a run is the ones a run that stopped left behind.
func (s *Store) RunningEntries(ctx context.Context) ([]Entry, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM entries WHERE status = ? ORDER BY id`,
		StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

func scanEntry(row scanner) (*Entry, error) {
	var (
		e       Entry
		after   sql.NullInt64
		upto    sql.NullInt64
		started int64
		ended   sql.NullInt64
	)
	err := row.Scan(&e.ID, &e.Status, &e.Error, &e.Channel,
		&after, &upto, &started, &ended)
	if err != nil {
		return nil, err
	}
	e.AfterMessageID = MessageID(id(after))
	e.UptoMessageID = MessageID(id(upto))
	e.StartedAt = time.Unix(0, started)
	e.EndedAt = at(ended)
	return &e, nil
}

const requestColumns = `id, entry_id, purpose, runner, model, method, url,
	request_headers_json, request_body_gz, attempts_json, status,
	response_headers_json, response_body_gz, started_at, first_byte_at, ended_at,
	error, provider, finish_reason, usage_json, cost, pruned`

// AddRequest records a request before it is sent, and fills in its id.
func (s *Store) AddRequest(ctx context.Context, r *Request) error {
	reqBody, err := compress(r.RequestBody)
	if err != nil {
		return err
	}
	headers, err := headersJSON(r.RequestHeaders)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO requests
		(entry_id, purpose, runner, model, method, url,
		 request_headers_json, request_body_gz, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.EntryID, r.Purpose, r.Runner, r.Model, r.Method, r.URL,
		headers, reqBody, r.StartedAt.UnixNano())
	if err != nil {
		return err
	}
	r.ID, err = res.LastInsertId()
	return err
}

// EndRequest records what came back.
func (s *Store) EndRequest(ctx context.Context, r *Request) error {
	respBody, err := compress(r.ResponseBody)
	if err != nil {
		return err
	}
	attempts, err := json.Marshal(r.Attempts)
	if err != nil {
		return err
	}
	headers, err := headersJSON(r.ResponseHeaders)
	if err != nil {
		return err
	}
	var usage string
	if r.Usage != nil {
		b, err := json.Marshal(r.Usage)
		if err != nil {
			return err
		}
		usage = string(b)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE requests
		   SET attempts_json = ?, status = ?, response_headers_json = ?,
		       response_body_gz = ?, first_byte_at = ?, ended_at = ?, error = ?,
		       provider = ?, finish_reason = ?, usage_json = ?, cost = ?
		 WHERE id = ?`,
		string(attempts), r.Status, headers, respBody,
		ns(r.FirstByteAt), ns(r.EndedAt), r.Error, r.Provider, r.FinishReason,
		usage, r.Cost, r.ID)
	return err
}

// Requests returns the requests of an entry, in the order they were sent, which
// is the order of their ids: one run writes them, one at a time.
func (s *Store) Requests(ctx context.Context, entryID EntryID) ([]Request, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+requestColumns+` FROM requests WHERE entry_id = ? ORDER BY id`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func scanRequest(row scanner) (*Request, error) {
	var (
		r         Request
		reqH      string
		reqBody   []byte
		attempts  string
		respH     string
		respBody  []byte
		started   int64
		firstByte sql.NullInt64
		ended     sql.NullInt64
		usage     string
		pruned    int64
	)
	err := row.Scan(&r.ID, &r.EntryID, &r.Purpose, &r.Runner, &r.Model,
		&r.Method, &r.URL, &reqH, &reqBody, &attempts, &r.Status, &respH,
		&respBody, &started, &firstByte, &ended, &r.Error, &r.Provider,
		&r.FinishReason, &usage, &r.Cost, &pruned)
	if err != nil {
		return nil, err
	}
	if r.RequestHeaders, err = parseHeaders(reqH); err != nil {
		return nil, err
	}
	if r.ResponseHeaders, err = parseHeaders(respH); err != nil {
		return nil, err
	}
	if r.RequestBody, err = decompress(reqBody); err != nil {
		return nil, err
	}
	if r.ResponseBody, err = decompress(respBody); err != nil {
		return nil, err
	}
	if attempts != "" {
		if err := json.Unmarshal([]byte(attempts), &r.Attempts); err != nil {
			return nil, err
		}
	}
	if usage != "" {
		r.Usage = new(Usage)
		if err := json.Unmarshal([]byte(usage), r.Usage); err != nil {
			return nil, err
		}
	}
	r.Pruned = pruned != 0
	r.StartedAt = time.Unix(0, started)
	r.FirstByteAt = at(firstByte)
	r.EndedAt = at(ended)
	return &r, nil
}

// Prune drops the request and response bodies of every entry but the latest
// keep ones. Their summary columns stay.
func (s *Store) Prune(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests
		   SET request_body_gz = NULL, response_body_gz = NULL, pruned = 1
		 WHERE (request_body_gz IS NOT NULL OR response_body_gz IS NOT NULL)
		   AND entry_id NOT IN (SELECT id FROM entries ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

func headersJSON(h http.Header) (string, error) {
	if len(h) == 0 {
		return "", nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parseHeaders(s string) (http.Header, error) {
	if s == "" {
		return nil, nil
	}
	var h http.Header
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil, err
	}
	return h, nil
}

// MessagesAnsweredBy returns the messages an entry was started for, which are
// the ones it recorded when it started.
func (s *Store) MessagesAnsweredBy(ctx context.Context, entryID EntryID) ([]Message, error) {
	// An entry with no message before it has none to start after, which is null
	// rather than the id zero.
	rows, err := s.ro.QueryContext(ctx, `SELECT `+prefixed("m", messageColumns)+`
		  FROM messages m, entries e
		 WHERE e.id = ?
		   AND m.role = ?
		   AND m.id > coalesce(e.after_message_id, 0)
		   AND m.id <= e.upto_message_id
		 ORDER BY m.id`, entryID, RoleUser)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}
