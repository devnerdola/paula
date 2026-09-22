package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultURL is the Bot API. A run points somewhere else only in a test.
const defaultURL = "https://api.telegram.org"

// downloadMax is the largest file the Bot API serves, which is what getFile is
// documented to hand over.
const downloadMax = 20 << 20

const (
	// answerWait is how long a method that answers at once may take. A
	// connection that stalls after the request is written answers never, and
	// the loop that asks what arrived has no other way out: a bot would go
	// quiet for the rest of the run with nothing to say why.
	answerWait = 30 * time.Second
	// downloadWait is the same for a file, which is worth waiting longer for:
	// the API serves up to downloadMax of it.
	downloadWait = 2 * time.Minute
)

// client talks to the Bot API. Every call is a POST of JSON to
// <url>/bot<token>/<method>, which is the whole of what Paula needs of it.
type client struct {
	url   string
	token string
	http  *http.Client
	// wait is how long a method that answers at once is given, and the margin
	// on top of the time getUpdates asks the API to hold its request open for.
	wait time.Duration
	// pause is how it waits before sending a message again, which a test does
	// not.
	pause func(context.Context, time.Duration) error
}

// answer is what every method answers with: whether it worked, and either the
// result or why it did not.
type answer struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// apiError is what the Bot API said went wrong. RetryAfter is how long it asks
// to be left alone for, and zero when it asks for nothing.
type apiError struct {
	Code       int
	Message    string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("%d %s", e.Code, e.Message)
}

// call sends one method and decodes its result into out, which may be nil for
// a method whose answer says nothing but that it worked. within is the whole
// the call may take.
func (c *client) call(ctx context.Context, method string, within time.Duration, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// The token is in the path of every request, so it is in the error of every
	// request that fails. What names the method is the method, not the URL.
	url := c.url + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	var ans answer
	if err := json.NewDecoder(io.LimitReader(resp.Body, answerMax)).Decode(&ans); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if !ans.OK {
		e := &apiError{Code: ans.ErrorCode, Message: ans.Description}
		if e.Message == "" {
			e.Message = resp.Status
		}
		if ans.Parameters != nil && ans.Parameters.RetryAfter > 0 {
			e.RetryAfter = time.Duration(ans.Parameters.RetryAfter) * time.Second
		}
		return fmt.Errorf("%s: %w", method, e)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(ans.Result, out); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

// answerMax bounds what is read of an answer. The largest of them lists
// updates, each holding a message and its metadata.
const answerMax = 32 << 20

// update is one thing that happened, as far as Paula reads it.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
	Tap      *tap     `json:"callback_query"`
}

// tap is a button that was tapped.
type tap struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From *struct {
		ID int64 `json:"id"`
	} `json:"from"`
}

// message is what arrived. A photo comes in several sizes, and a document or a
// sticker as a file of its own.
type message struct {
	Text    string `json:"text"`
	Caption string `json:"caption"`
	From    *struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Photo    []photoSize `json:"photo"`
	Document *struct {
		FileID   string `json:"file_id"`
		MIMEType string `json:"mime_type"`
	} `json:"document"`
	Sticker *struct {
		FileID     string     `json:"file_id"`
		Emoji      string     `json:"emoji"`
		IsAnimated bool       `json:"is_animated"`
		IsVideo    bool       `json:"is_video"`
		Thumbnail  *photoSize `json:"thumbnail"`
	} `json:"sticker"`
}

type photoSize struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// updates asks for what happened after the given one, waiting up to timeout
// for something to happen.
func (c *client) updates(ctx context.Context, offset int64, timeout time.Duration) ([]update, error) {
	body := map[string]any{
		"timeout":         int(timeout / time.Second),
		"allowed_updates": []string{"message", "callback_query"},
	}
	if offset > 0 {
		body["offset"] = offset
	}
	var out []update
	// The API holds the request open until something happens, so what it is
	// given is that and the margin a method that answers at once has.
	if err := c.call(ctx, "getUpdates", timeout+c.wait, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// me is the bot the token belongs to, which is what says the token is one the
// API takes.
func (c *client) me(ctx context.Context) (string, error) {
	var out struct {
		Username string `json:"username"`
	}
	if err := c.call(ctx, "getMe", c.wait, map[string]any{}, &out); err != nil {
		return "", err
	}
	return out.Username, nil
}

// sendTries is how often one message is sent before it is given up on. What
// she says is often several messages, one after the other, and one of them
// refused would leave her mid-sentence.
const sendTries = 3

// button is one thing to tap: what it says, and what comes back when it is
// tapped, which Telegram holds to 64 bytes.
type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

// edit writes over a message that was already sent. What it now says is what
// the caller knows, so nothing here asks Telegram to take an edit that changes
// nothing.
func (c *client) edit(ctx context.Context, chat, message int64, text string) error {
	return c.call(ctx, "editMessageText", c.wait, map[string]any{
		"chat_id":    chat,
		"message_id": message,
		"text":       text,
	}, nil)
}

// answerTap tells the client a tap was taken. Telegram shows the tap as
// pending until it is, so every one of them is answered.
func (c *client) answerTap(ctx context.Context, id string) error {
	return c.call(ctx, "answerCallbackQuery", c.wait, map[string]any{
		"callback_query_id": id,
	}, nil)
}

// send writes one message to a chat. A message the API asked to be given a
// moment, or could not take just then, is sent again: a message that arrived
// twice is read as one she sent twice, and a reply cut off in the middle is
// read as the whole of what she had to say.
func (c *client) send(ctx context.Context, chat int64, text string, buttons []button) (int64, error) {
	body := map[string]any{"chat_id": chat, "text": text}
	if len(buttons) > 0 {
		// One button to a row, since what they say is a line rather than a word.
		rows := make([][]button, 0, len(buttons))
		for _, b := range buttons {
			rows = append(rows, []button{b})
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	var wait time.Duration
	for try := 1; ; try++ {
		var out struct {
			MessageID int64 `json:"message_id"`
		}
		err := c.call(ctx, "sendMessage", c.wait, body, &out)
		if err == nil || try >= sendTries || !worthAgain(err) {
			return out.MessageID, err
		}
		wait = after(err, wait)
		if werr := c.pause(ctx, wait); werr != nil {
			return 0, err
		}
	}
}

// worthAgain reports whether a request is worth sending again: one the API
// asked to be given a moment, one it could not answer just then, and one that
// never got an answer at all. What it refused outright it refuses again.
func worthAgain(err error) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return true
	}
	return e.Code == http.StatusTooManyRequests || e.Code >= 500
}

// setCommands is what the client offers when a message starts with a slash.
// Telegram keeps the list for the bot, so it is sent whole each time.
func (c *client) setCommands(ctx context.Context, commands []map[string]string) error {
	return c.call(ctx, "setMyCommands", c.wait, map[string]any{
		"commands": commands,
	}, nil)
}

// typing says she is writing, which Telegram shows for about five seconds or
// until something is sent to the chat.
func (c *client) typing(ctx context.Context, chat int64) error {
	return c.call(ctx, "sendChatAction", c.wait, map[string]any{
		"chat_id": chat,
		"action":  "typing",
	}, nil)
}

// download reads a file the Bot API holds, which it serves from a path of its
// own once getFile has been asked for it.
func (c *client) download(ctx context.Context, fileID string) ([]byte, error) {
	var out struct {
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", c.wait, map[string]any{"file_id": fileID}, &out); err != nil {
		return nil, err
	}
	if out.FilePath == "" {
		return nil, fmt.Errorf("getFile: the answer holds no path for %s", fileID)
	}

	// The bound covers reading the file as well as asking for it: a body that
	// arrives a byte at a time never ends on its own.
	ctx, cancel := context.WithTimeout(ctx, downloadWait)
	defer cancel()

	url := c.url + "/file/bot" + c.token + "/" + out.FilePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", fileName(out.FilePath), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", fileName(out.FilePath), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, downloadMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > downloadMax {
		return nil, fmt.Errorf("%s is larger than the %d bytes the API serves", fileName(out.FilePath), downloadMax)
	}
	return b, nil
}

// fileName is the name of a file as it is worth reporting: what the API called
// it, without the directories it keeps it under.
func fileName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
