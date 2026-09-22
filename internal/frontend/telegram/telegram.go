// Package telegram is the Telegram bot Paula is texted through: one chat with
// one person, polled for what they send and written back to.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/store"
)

// Kind is the type written in the configuration file.
const Kind = "telegram"

const (
	// defaultTokenEnv is where the bot token is read from.
	defaultTokenEnv = "TELEGRAM_TOKEN"
	// offsetFile is where the run keeps what to ask for next, so a run that
	// follows does not answer what the one before it already answered.
	offsetFile = "telegram.offset"
	// poll is how long getUpdates waits for something to happen. Telegram holds
	// the request open until it does, which is how a bot is told at once.
	poll = 30 * time.Second
	// maxUnits is the longest message Telegram takes, counted in UTF-16 code
	// units as its documentation counts them.
	maxUnits = 4096
)

type settings struct {
	TokenEnv    string `yaml:"token_env"`
	UserID      int64  `yaml:"user_id"`
	StreamEdits bool   `yaml:"stream_edits"`
}

type Frontend struct {
	client *client
	// user is the only person served: a bot is reachable by anyone who finds
	// it, and this conversation is with one person.
	user   int64
	offset string
	log    *slog.Logger
	names  api.Names
	// stream says a reply is shown as it is written, by writing it over the
	// message it started as. What she wrote then arrives as it comes rather
	// than as the texts she broke it into.
	stream bool
	// pause is how the run waits between saying she is writing, which a test
	// shortens.
	pause func(context.Context, time.Duration) error
}

// Open reads the telegram section and builds the bot it describes.
func Open(s config.Section, h api.Host) (*Frontend, error) {
	cfg := settings{TokenEnv: defaultTokenEnv}
	if err := s.Decode(&cfg); err != nil {
		return nil, err
	}
	// Every problem of the section is reported at once, as the file's are.
	var problems []error
	token := os.Getenv(cfg.TokenEnv)
	switch {
	case cfg.TokenEnv == "":
		problems = append(problems, fmt.Errorf("%s.token_env: the name of an environment variable is needed", s.Path()))
	case token == "":
		problems = append(problems, fmt.Errorf("%s.token_env: environment variable %s is not set", s.Path(), cfg.TokenEnv))
	case !tokenShape(token):
		// A token is the bot's number, a colon and the secret. One that is not
		// is a value that landed in the wrong variable, and asking Telegram
		// would say so with the token in the error.
		problems = append(problems, fmt.Errorf("%s.token_env: %s does not hold a bot token", s.Path(), cfg.TokenEnv))
	}
	if cfg.UserID <= 0 {
		problems = append(problems, fmt.Errorf("%s.user_id: the number of the one person served is needed", s.Path()))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	h.Secrets.Add(token)

	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Frontend{
		client: &client{
			url: defaultURL, token: token, http: &http.Client{},
			wait: answerWait, pause: waiting,
		},
		user:   cfg.UserID,
		offset: filepath.Join(h.DataDir, offsetFile),
		log:    log,
		names:  h.Names,
		stream: cfg.StreamEdits,
		pause:  waiting,
	}, nil
}

// tokenShape reports whether a value looks like a bot token: the bot's number,
// a colon, and the rest.
func tokenShape(token string) bool {
	id, rest, ok := strings.Cut(token, ":")
	if !ok || rest == "" || id == "" {
		return false
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return false
	}
	return len(rest) >= logs.MinSecret
}

func (f *Frontend) Kind() string { return Kind }

// Run keeps one session on the chat, fed by what the bot is sent, until the
// context ends.
func (f *Frontend) Run(ctx context.Context, session func(context.Context, api.Adapter) error) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// The token is held against the API before anything waits on it. One it
	// refuses is a bot that is never reached, which is a run that looks alive
	// and answers nothing.
	name, err := f.client.me(ctx)
	if err != nil {
		return err
	}
	f.log.Info("telegram polling", "bot", name, "user", f.user)

	a := &adapter{f: f, inputs: make(chan api.Input)}
	if f.stream {
		return session(ctx, &editing{adapter: a})
	}
	return session(ctx, a)
}

// Stream takes what has been added to what she is writing. A text she has
// finished is sent as she finishes it, and what she is still writing waits for
// the rest of it: a blank line is where one ends and the next begins.
func (a *adapter) Stream(ctx context.Context, text string) error {
	for _, done := range a.wrote.Add(text) {
		if err := a.f.write(ctx, api.Bubbles(done, fits), nil); err != nil {
			return err
		}
	}
	return nil
}

// EndStream sends the last of what she wrote, which is whatever she was in the
// middle of when she finished.
func (a *adapter) EndStream(ctx context.Context) error {
	return a.f.write(ctx, api.Bubbles(a.wrote.End(), fits), nil)
}

// editing is the chat when a text is shown as it is written. Each of them is
// still a message of its own; what this adds is watching one fill, by writing
// what she has so far over the message it started as.
type editing struct {
	*adapter
	// message is the one being written over, holds what it says now, and at is
	// when it last was written. All three are touched from the session's
	// goroutine alone.
	message int64
	holds   string
	at      time.Time
}

// editEvery is how often the message being written over is written over again.
// A reply arrives in fragments far faster than anyone reads, and Telegram
// holds an edit against the same limits as a message.
const editEvery = time.Second

// Stream shows what has been added to what she is writing: a text she has
// finished, whole and in the message it was filling, and what she is still
// writing as it comes.
func (s *editing) Stream(ctx context.Context, text string) error {
	for _, done := range s.wrote.Add(text) {
		if _, err := s.show(ctx, done, true); err != nil {
			return err
		}
	}
	if s.message != 0 && time.Since(s.at) < editEvery {
		return nil
	}
	// What is left is what the open message holds: anything that outgrew it
	// went into one that is now closed, and is not written again.
	left, err := s.show(ctx, s.wrote.Rest(), false)
	s.wrote.Keep(left)
	return err
}

// EndStream shows the last of what she wrote, which is whatever she was in the
// middle of when she finished.
func (s *editing) EndStream(ctx context.Context) error {
	_, err := s.show(ctx, s.wrote.End(), true)
	return err
}

// show puts a text in the chat: over the message it is filling, or in one of
// its own when there is none yet. done says she has finished it, so the next
// text starts a message of its own.
//
// What comes back is what is left of the text for the message still open, as
// she wrote it: what is shown is trimmed of the space around it, and what is
// kept is not, since what she writes next joins onto it.
func (s *editing) show(ctx context.Context, text string, done bool) (string, error) {
	s.at = time.Now()
	for {
		// A text that outgrew what a message holds carries on in another, which
		// the rest of it fills.
		at := fits(text)
		if at >= len(text) {
			break
		}
		if head := strings.TrimSpace(text[:at]); head != "" {
			if err := s.put(ctx, head); err != nil {
				return text, err
			}
		}
		s.closed()
		text = text[at:]
	}
	shown := strings.TrimSpace(text)
	if shown == "" {
		// A message of nothing is one Telegram refuses, and there is nothing of
		// it to show yet.
		if done {
			s.closed()
		}
		return text, nil
	}
	if err := s.put(ctx, shown); err != nil {
		return text, err
	}
	if done {
		s.closed()
		return "", nil
	}
	return text, nil
}

// closed lets go of the message being written over, so what she writes next
// starts one of its own.
func (s *editing) closed() { s.message, s.holds = 0, "" }

// put writes a text over the message being written, or sends it as a new one
// when there is none yet.
func (s *editing) put(ctx context.Context, text string) error {
	if s.message == 0 {
		id, err := s.f.client.send(ctx, s.f.user, text, nil)
		s.message, s.holds = id, text
		return err
	}
	if text == s.holds {
		// Writing the same thing over itself says nothing, and Telegram refuses
		// an edit that changes nothing.
		return nil
	}
	s.holds = text
	if err := s.f.client.edit(ctx, s.f.user, s.message, text); err != nil {
		// One that cannot be written over is gone as far as she is concerned —
		// deleted, or too old to change — and what she writes next starts a
		// message of its own rather than going to it for the rest of the run.
		s.closed()
		return err
	}
	return nil
}

// adapter is the chat as a session shows things on it.
type adapter struct {
	f      *Frontend
	inputs chan api.Input
	// mu guards what is showing that she is writing, and what the buttons that
	// have been offered stand for: a session turns the one on and off from the
	// goroutine it runs on, and a tap arrives on another.
	mu     sync.Mutex
	stopIt context.CancelFunc
	// picks is what a session calls each choice, by the tag its button carries,
	// and tags is the way back. They hold every choice this run has offered,
	// which is as many as there are models and roles.
	picks map[string]string
	tags  map[string]string
	// wrote is what she has written that is not a finished text yet. A session
	// hands over what she writes from the one goroutine it runs on.
	wrote api.Written
}

// Writing shows that she is writing, or stops showing it. Telegram holds the
// status for about five seconds and drops it as soon as anything is sent, so
// it is said again for as long as she is still writing.
func (a *adapter) Writing(ctx context.Context, on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopIt != nil {
		a.stopIt()
		a.stopIt = nil
	}
	if !on {
		return nil
	}
	// It follows the session: a run that ends stops saying she is writing.
	ctx, stop := context.WithCancel(ctx)
	a.stopIt = stop
	go a.f.showWriting(ctx)
	return nil
}

// showWriting says she is writing, over and over, until she has stopped.
func (f *Frontend) showWriting(ctx context.Context) {
	for {
		if err := f.client.typing(ctx, f.user); err != nil {
			if ctx.Err() == nil {
				f.log.Warn("showing that she is writing", "error", err)
			}
			return
		}
		if err := f.pause(ctx, typingAgain); err != nil {
			return
		}
	}
}

// typingAgain is how often the status is said again. Telegram shows it for
// about five seconds.
const typingAgain = 4 * time.Second

func (a *adapter) Features() api.Features {
	return api.Features{Channel: Kind}
}

// ShowCommands puts what can be typed here in the client's own list, so they
// are offered as they are typed rather than having to be known.
func (a *adapter) ShowCommands(ctx context.Context, cs []api.Command) error {
	out := make([]map[string]string, 0, len(cs))
	for _, c := range cs {
		// Telegram takes the name without its slash, and describes it in a line
		// of its own. A name it will not take is left out rather than refused:
		// what can be typed is not decided here.
		if !commandName(c.Name) {
			continue
		}
		out = append(out, map[string]string{
			"command":     c.Name,
			"description": short(c.Short),
		})
	}
	return a.f.client.setCommands(ctx, out)
}

// commandName reports whether Telegram takes a name: up to 32 of lower case,
// digits and underscores, as its documentation says.
func commandName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// short is a description as Telegram takes it: between 1 and 256 characters.
func short(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "—"
	}
	if len([]rune(text)) > 256 {
		return string([]rune(text)[:256])
	}
	return text
}

// Start begins asking Telegram what arrived, and yields it until the context
// ends.
func (a *adapter) Start(ctx context.Context) (<-chan api.Input, error) {
	offset, err := a.f.read()
	if err != nil {
		return nil, err
	}
	go a.poll(ctx, offset)
	return a.inputs, nil
}

// Send writes one thing to the chat. What she wrote as separate paragraphs is
// sent as separate messages, which is how a person texts, and each of them is
// held to what Telegram takes. What can be picked from it goes under the last
// of them as buttons.
func (a *adapter) Send(ctx context.Context, m api.Outgoing) error {
	// Only what she wrote is paced. A frontend saying something of its own is
	// answering at once, and reads as waiting on nobody.
	return a.f.write(ctx, api.Bubbles(m.Text, fits), a.buttons(m.Choices))
}

// buttons are the choices as Telegram takes them. What comes back when one is
// tapped is held to 64 bytes there, and what a session calls a choice is as
// long as it needs to be, so the adapter tags each one and remembers which is
// which.
func (a *adapter) buttons(choices []api.Choice) []button {
	if len(choices) == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.picks == nil {
		a.picks = map[string]string{}
		a.tags = map[string]string{}
	}
	out := make([]button, 0, len(choices))
	for _, c := range choices {
		tag, known := a.tags[c.Picked]
		if !known {
			tag = strconv.Itoa(len(a.picks) + 1)
			a.picks[tag], a.tags[c.Picked] = c.Picked, tag
		}
		label := c.Label
		if c.Current {
			label = "✓ " + label
		}
		out = append(out, button{Text: label, Data: tag})
	}
	return out
}

// picked is what a session called the choice a tag stands for, and false for a
// tag from a run that is no longer this one.
func (a *adapter) picked(tag string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	what, ok := a.picks[tag]
	return what, ok
}

// ShowUserMessage shows what was said on another frontend, since the bot is
// the only thing that writes in this chat and what she answers would otherwise
// read as an answer to nothing. It goes the way everything else in the chat
// does: the paragraphs of it, each cut to what Telegram takes.
func (a *adapter) ShowUserMessage(ctx context.Context, m *store.Message) error {
	text := strings.TrimSpace(m.Text())
	if text == "" {
		return nil
	}
	return a.f.write(ctx, api.Bubbles(a.f.names.User+": "+text, fits), nil)
}

// write sends messages one after the other. Nothing waits between them: a text
// goes as she finishes writing it, so the chat fills at the pace she writes
// rather than all at once at the end.
func (f *Frontend) write(ctx context.Context, messages []string, buttons []button) error {
	if len(messages) == 0 && len(buttons) > 0 {
		// A choice with nothing said before it is still a choice to offer.
		messages = []string{"\u2014"}
	}
	for i, text := range messages {
		// What can be picked goes under the last of what was said, so the
		// buttons sit at the bottom of the chat where they are tapped.
		var under []button
		if i == len(messages)-1 {
			under = buttons
		}
		if _, err := f.client.send(ctx, f.user, text, under); err != nil {
			return err
		}
	}
	return nil
}

// waiting is how a run waits between messages. A test does not.
func waiting(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// fits is how much of a text goes in one message: as many characters as leave
// the message inside what Telegram takes, cut at the last line break there is
// room for, so a reply breaks where she wrote a break.
func fits(text string) int {
	if units(text) <= maxUnits {
		return len(text)
	}
	// The index of the first rune that does not fit is the end of what does.
	at, taken := len(text), 0
	for i, r := range text {
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if taken+n > maxUnits {
			at = i
			break
		}
		taken += n
	}
	if cut := strings.LastIndexByte(text[:at], '\n'); cut > 0 {
		return cut
	}
	return at
}

// units is the length of a text as Telegram counts it.
func units(text string) int { return len(utf16.Encode([]rune(text))) }

// read is the update this run starts after: the one the run before it took
// last. A data directory with none starts from whatever Telegram still holds.
func (f *Frontend) read() (int64, error) {
	b, err := os.ReadFile(f.offset)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", f.offset, err)
	}
	return offset, nil
}

// took writes down that an update was handed over. What is kept is the one to
// ask for next: the API hands back everything from the offset it is given, so
// keeping the one that was taken would have it handed over again.
func (f *Frontend) took(id int64) {
	err := os.WriteFile(f.offset, []byte(strconv.FormatInt(id+1, 10)+"\n"), 0o600)
	if err != nil {
		f.log.Error("keeping where telegram is read from", "error", err)
	}
}

// poll asks what arrived, over and over, and hands each of them to the session
// as an input. An update is written down once the session has taken it: a run
// that ends before that leaves it for the next run to answer.
func (a *adapter) poll(ctx context.Context, offset int64) {
	f, out := a.f, a.inputs
	defer close(out)
	var wait time.Duration
	for {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		updates, err := f.client.updates(ctx, offset, poll)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait = after(err, wait)
			f.log.Warn("asking telegram what arrived", "error", err, "next try in", wait)
			continue
		}
		wait = 0

		for _, u := range updates {
			offset = u.UpdateID + 1
			in, ok := a.arrived(ctx, u)
			if ctx.Err() != nil {
				// An update read no further than the run reaches is not one
				// that was answered, so it is left for the next run.
				return
			}
			if !ok {
				f.took(u.UpdateID)
				continue
			}
			select {
			case out <- in:
				f.took(u.UpdateID)
			case <-ctx.Done():
				return
			}
		}
	}
}

// after is how long to wait before asking again. Telegram says how long it
// wants to be left alone when it is the one refusing; anything else is a
// second, doubling up to a minute.
func after(err error, was time.Duration) time.Duration {
	var e *apiError
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	if was <= 0 {
		return time.Second
	}
	return min(2*was, time.Minute)
}

// started reports whether a message is the one a client sends when the chat is
// opened, which carries what it was opened from after it.
func started(text string) bool {
	name, _, _ := strings.Cut(strings.TrimSpace(text), " ")
	return name == "/start"
}

// arrived is what one update becomes for the conversation, and false for one
// there is nothing to answer.
func (a *adapter) arrived(ctx context.Context, u update) (api.Input, bool) {
	if u.Tap != nil {
		return a.tapped(ctx, u.Tap)
	}
	return a.f.input(ctx, u.Message)
}

// tapped is the choice a button stands for. A client shows a tap as pending
// until it is answered, so every one of them is, whoever tapped.
func (a *adapter) tapped(ctx context.Context, t *tap) (api.Input, bool) {
	if err := a.f.client.answerTap(ctx, t.ID); err != nil && ctx.Err() == nil {
		a.f.log.Warn("answering a tap", "error", err)
	}
	if t.From == nil || t.From.ID != a.f.user {
		a.f.log.Info("a tap from someone else", "user", tapper(t))
		return api.Input{}, false
	}
	// A tag this run never offered goes on as it is: what can be picked is not
	// decided here, and the session says what it makes of it.
	what, ok := a.picked(t.Data)
	if !ok {
		what = t.Data
	}
	return api.Input{Picked: what}, true
}

// tapper is who tapped, and zero for a tap that says nothing about it.
func tapper(t *tap) int64 {
	if t.From == nil {
		return 0
	}
	return t.From.ID
}

// input is what a message becomes for the conversation, and false for one that
// is not for Paula or holds nothing she can read.
func (f *Frontend) input(ctx context.Context, m *message) (api.Input, bool) {
	if m == nil || m.From == nil {
		return api.Input{}, false
	}
	if m.From.ID != f.user {
		// A bot is reachable by anyone who finds it. Nobody else is answered,
		// and what they sent is not stored.
		f.log.Info("a message from someone else", "user", m.From.ID)
		return api.Input{}, false
	}

	if started(m.Text) {
		// A client sends this of its own accord when the chat is opened, so it
		// is not something that was said and she is not asked to answer it.
		return api.Input{}, false
	}

	text := m.Text + m.Caption
	if text == "" && m.Sticker != nil {
		// A sticker says what its emoji says, and that is what she is told it
		// said. The picture goes with it.
		text = m.Sticker.Emoji
	}
	in := api.Input{Text: strings.TrimSpace(text)}
	file, ok := f.file(m)
	switch {
	case ok:
		image, err := f.client.download(ctx, file)
		if err != nil {
			f.log.Error("reading what was sent", "error", err)
			f.tell(ctx, "that picture did not arrive, try sending it again")
			return api.Input{}, false
		}
		if media.Detect(image) == "" {
			f.tell(ctx, "that one is not "+strings.Join(media.Types(), ", "))
			return api.Input{}, false
		}
		in.Images = [][]byte{image}
	case in.Text == "":
		f.tell(ctx, "text, photos, stickers and image files are what she reads")
		return api.Input{}, false
	}
	return in, true
}

// file is the image of a message, and false for one that carries none: the
// largest size of a photo, an image sent as a file, or a sticker, which is an
// image of its own unless it moves, and then its cover is.
func (f *Frontend) file(m *message) (string, bool) {
	switch {
	case len(m.Photo) > 0:
		largest := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.Width*p.Height > largest.Width*largest.Height {
				largest = p
			}
		}
		return largest.FileID, true
	case m.Document != nil && strings.HasPrefix(m.Document.MIMEType, "image/"):
		return m.Document.FileID, true
	case m.Sticker != nil:
		if m.Sticker.IsAnimated || m.Sticker.IsVideo {
			if m.Sticker.Thumbnail == nil {
				return "", false
			}
			return m.Sticker.Thumbnail.FileID, true
		}
		return m.Sticker.FileID, true
	}
	return "", false
}

// tell says something to the chat that is not part of the conversation, such
// as why what was sent could not be read.
func (f *Frontend) tell(ctx context.Context, text string) {
	if _, err := f.client.send(ctx, f.user, text, nil); err != nil && ctx.Err() == nil {
		f.log.Error("telling telegram", "error", err)
	}
}
