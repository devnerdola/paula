package frontend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"nerdola.dev/x/paula/internal/conversation"
	"nerdola.dev/x/paula/internal/frontend/api"
	"nerdola.dev/x/paula/internal/store"
)

// errNotAsked says a frontend that takes one line at a time was never asked
// for the next one, so nothing more will come of it.
var errNotAsked = errors.New("the next line was never asked for")

// sessions numbers the sessions of a run, so a message says which one it came
// from and the others show it.
var sessions atomic.Int64

// Session is one frontend following the conversation.
type Session struct {
	adapter  api.Adapter
	features api.Features
	conv     Conversation
	log      *slog.Logger
	// from names this session in the conversation.
	from string

	// shown is the last message this session put on the screen, and entries the
	// newest entry whose reply is on it. A session that fell behind reads them
	// to know where to pick up.
	shown   store.MessageID
	entries store.EntryID
	writing writing
	// last is the number of the latest event handled.
	last     Seq
	nextLine nextLine
	// ending says the frontend will send nothing more, so the session shows
	// what she is still writing and stops.
	ending bool
}

// writing is the reply arriving now: the entry it belongs to, the text that
// has arrived, and whether any of it went to the frontend as a stream, which is
// both a stream to close and text that cannot be taken back.
type writing struct {
	entry    store.EntryID
	text     string
	streamed bool
}

// begin starts on a reply, and clear forgets the one that was arriving.
func (w *writing) begin(entry store.EntryID) { *w = writing{entry: entry} }
func (w *writing) clear()                    { *w = writing{} }

// added is the part of a reply that is not on the screen yet. Every event
// carries the whole reply so far, so only what is new to it is written out.
func (w *writing) added(text string) string {
	if !strings.HasPrefix(text, w.text) {
		return text
	}
	return text[len(w.text):]
}

// nextLine decides when a frontend that takes one line at a time is asked for
// the next one: after the conversation has gone quiet, and after everything it
// did up to then is on the screen. A conversation that was quiet all along
// answers with no events at all, so owed is what says a line is wanted and the
// event cannot stand for it.
type nextLine struct {
	owed bool
	// waiting is a wait on its way, and promptAt the event the conversation went
	// quiet at, once one has answered.
	waiting  chan Seq
	promptAt Seq
}

// wait asks when the conversation has nothing pending, beside the showing, so
// a reply is read as it is written. A wait already on its way answers the same
// question.
func (p *nextLine) wait(ctx context.Context, conv Conversation) {
	if p.waiting != nil {
		return
	}
	waiting := make(chan Seq, 1)
	p.waiting, p.promptAt = waiting, 0
	go func() {
		seq, err := conv.Wait(ctx)
		if err != nil {
			close(waiting)
			return
		}
		waiting <- seq
	}()
}

// settled reports that the conversation is quiet and everything it did is on
// the screen, where last is the latest event shown.
func (p *nextLine) settled(last Seq) bool { return p.waiting == nil && last >= p.promptAt }

// update is one thing the conversation did, or the word that the session fell
// too far behind to be told them one by one.
type update struct {
	event conversation.Event
	err   error
}

// Options are what every session of a run shares. The adapter is not one of
// them: a session is opened for one, as its frontend yields it.
type Options struct {
	Conv Conversation
	Log  *slog.Logger
}

// New opens a session on one adapter.
func New(a api.Adapter, o Options) *Session {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	features := a.Features()
	return &Session{
		adapter:  a,
		features: features,
		conv:     o.Conv,
		log:      o.Log,
		from:     fmt.Sprintf("%s-%d", features.Channel, sessions.Add(1)),
	}
}

// Run follows the conversation and what is typed, until the context ends or
// the frontend closes.
func (s *Session) Run(ctx context.Context) error {
	// One instant is where the session starts: the events it follows from, and
	// the last message said before it.
	after, latest, err := s.conv.Standing(ctx)
	if err != nil {
		return err
	}
	s.last, s.shown = after, latest

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	inputs, err := s.adapter.Start(ctx)
	if err != nil {
		return err
	}
	if err := s.open(ctx); err != nil {
		return err
	}

	// What the conversation is doing and how far behind the session fell travel
	// together, so they are handled in the order they happened.
	updates := make(chan update)
	go func() {
		defer close(updates)
		for e, err := range s.conv.Events(ctx, after) {
			select {
			case updates <- update{event: e, err: err}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case in, ok := <-inputs:
			if !ok {
				// Nothing more will be typed, so what she is writing is shown
				// and the session ends with it.
				inputs = nil
				s.ending = true
				s.nextLine.wait(ctx, s.conv)
				break
			}
			if err := s.input(ctx, in); err != nil && !s.goesOn(ctx, err) {
				return err
			}
		case u, ok := <-updates:
			if !ok {
				return nil
			}
			var err error
			switch {
			case u.err == nil:
				err = s.event(ctx, u.event)
			case errors.Is(u.err, conversation.ErrBehind):
				err = s.catchUp(ctx)
			default:
				err = u.err
			}
			if err != nil && !s.goesOn(ctx, err) {
				return err
			}
		case seq, ok := <-s.nextLine.waiting:
			if !ok {
				return nil
			}
			// The conversation went quiet at this event, so the next line is
			// asked for once everything up to it is on the screen.
			s.nextLine.waiting, s.nextLine.promptAt = nil, seq
		}
		if err := s.ask(ctx); err != nil && !s.goesOn(ctx, err) {
			return err
		}
		if s.ending && s.nextLine.settled(s.last) {
			return nil
		}
	}
}

// goesOn logs what came of one thing the session did, and reports whether the
// session goes on. A frontend that could not show something is still there
// for the next thing, but one that is gone, or that was never asked for a
// line, has nothing left to wait for.
func (s *Session) goesOn(ctx context.Context, err error) bool {
	switch {
	case err == nil, ctx.Err() != nil:
		return true
	case errors.Is(err, errNotAsked), errors.Is(err, api.ErrGone):
		return false
	}
	s.log.Error("showing what the conversation did", "channel", s.features.Channel, "error", err)
	return true
}

// open shows what was said before and asks for the next line. A session that
// shows none of it still starts where the conversation stands, since catching
// up later is about what it missed, not about the whole conversation.
func (s *Session) open(ctx context.Context) error {
	shower, ok := s.adapter.(api.HistoryShower)
	if !ok || shower.History() <= 0 {
		return s.prompt(ctx)
	}
	messages, err := s.conv.History(ctx, 0, shower.History())
	if err != nil {
		return err
	}
	for _, m := range messages {
		s.entries = max(s.entries, m.EntryID)
	}
	if err := shower.ShowHistory(ctx, messages); err != nil {
		return err
	}
	return s.prompt(ctx)
}

// prompt asks a frontend that shows one for the next line. One that shows none
// is simply not asked.
func (s *Session) prompt(ctx context.Context) error {
	if p, ok := s.adapter.(api.Prompter); ok {
		return p.Prompt(ctx)
	}
	return nil
}

func (s *Session) input(ctx context.Context, in api.Input) error {
	if in.Stop {
		if err := s.stop(ctx); err != nil {
			return err
		}
		return s.ready(ctx)
	}

	text := strings.TrimSpace(in.Text)
	if text != "" {
		if handled, err := s.command(ctx, text); handled {
			if err != nil {
				return err
			}
			return s.ready(ctx)
		}
	}
	if text == "" && len(in.Images) == 0 {
		return s.ready(ctx)
	}

	err := s.conv.Post(ctx, conversation.NewMessage{
		Channel: s.features.Channel,
		From:    s.from,
		Text:    text,
		Images:  in.Images,
	})
	if err != nil {
		if err := s.failed(ctx, err); err != nil {
			return err
		}
	}
	return s.ready(ctx)
}

// ready owes the next line to a frontend that takes one at a time, and starts
// waiting for the conversation to go quiet.
func (s *Session) ready(ctx context.Context) error {
	if !s.features.Sequential {
		return nil
	}
	s.nextLine.owed = true
	s.nextLine.wait(ctx, s.conv)
	return nil
}

// ask asks for the next line when one is owed and there is nothing left to
// show first.
func (s *Session) ask(ctx context.Context) error {
	if s.ending || !s.nextLine.owed || !s.nextLine.settled(s.last) {
		return nil
	}
	s.nextLine.owed = false
	if err := s.prompt(ctx); err != nil {
		return fmt.Errorf("%w: %w", errNotAsked, err)
	}
	return nil
}

func (s *Session) event(ctx context.Context, e conversation.Event) error {
	s.last = e.Seq
	if err := s.show(ctx, e); err != nil {
		return fmt.Errorf("%s: %w", e.Kind, err)
	}
	return nil
}

func (s *Session) show(ctx context.Context, e conversation.Event) error {
	switch e.Kind {
	case conversation.MessageStored:
		return s.userMessage(ctx, e)
	case conversation.ReplyStarted:
		return s.started(ctx, e)
	case conversation.ReplyText:
		return s.streaming(ctx, e)
	case conversation.ReplyDone:
		return s.ended(ctx, e, "")
	case conversation.ReplyStopped:
		return s.ended(ctx, e, " [stopped]")
	case conversation.ReplyRestarted:
		return s.dropped(ctx)
	case conversation.ReplyFailed:
		if err := s.dropped(ctx); err != nil {
			return err
		}
		return s.say(ctx, "error: "+e.Text)
	}
	return nil
}

// userMessage shows a message that was sent somewhere other than this session:
// another frontend, or another terminal of the same one.
func (s *Session) userMessage(ctx context.Context, e conversation.Event) error {
	if e.Message == nil || s.already(e.Message) {
		return nil
	}
	s.shown = e.Message.ID
	if e.From == s.from {
		return nil
	}
	if other, ok := s.adapter.(api.OtherChannels); ok {
		return other.ShowUserMessage(ctx, e.Message)
	}
	return nil
}

func (s *Session) started(_ context.Context, e conversation.Event) error {
	if s.done(e.Entry) {
		return nil
	}
	s.writing.begin(e.Entry)
	return nil
}

func (s *Session) streaming(ctx context.Context, e conversation.Event) error {
	// A reply already on the screen is not the one being written: the entry of
	// one read back while catching up never becomes the entry here, since the
	// event that would have started it was left alone.
	if e.Entry != s.writing.entry {
		return nil
	}
	added := s.writing.added(e.Text)
	s.writing.text = e.Text

	streamer, ok := s.adapter.(api.Streamer)
	if !ok {
		return nil
	}
	s.writing.streamed = true
	if added == "" {
		return nil
	}
	return streamer.Stream(ctx, added)
}

// ended shows the reply as it was stored.
func (s *Session) ended(ctx context.Context, e conversation.Event, suffix string) error {
	defer s.writing.clear()
	// A frontend that wrote this reply out has it on the screen already, so a
	// mark goes on the end of it rather than into a message of its own.
	streamer, writes := s.adapter.(api.Streamer)
	written := s.writing.streamed && writes && e.Entry == s.writing.entry
	shown := e.Message != nil && !s.already(e.Message)
	if written && shown && suffix != "" {
		if err := streamer.Stream(ctx, suffix); err != nil {
			return err
		}
	}
	if err := s.endStream(ctx); err != nil {
		return err
	}
	if !shown {
		return nil
	}
	s.shown = e.Message.ID
	s.entries = max(s.entries, e.Entry)
	if written {
		return nil
	}

	text := strings.TrimSpace(e.Message.Text() + suffix)
	if text == "" {
		return nil
	}
	return s.adapter.Send(ctx, api.Outgoing{Text: text, Hers: true})
}

// dropped clears a reply that left nothing behind.
func (s *Session) dropped(ctx context.Context) error {
	if err := s.endStream(ctx); err != nil {
		return err
	}
	s.writing.clear()
	return nil
}

// already reports whether a message is on the screen. One read back while
// catching up is, and its events arrive afterwards.
func (s *Session) already(m *store.Message) bool { return m.ID <= s.shown }

// done reports whether a reply is on the screen. The events of a reply read
// back while catching up are left alone.
func (s *Session) done(entry store.EntryID) bool { return entry > 0 && entry <= s.entries }

func (s *Session) endStream(ctx context.Context) error {
	if streamer, ok := s.adapter.(api.Streamer); ok && s.writing.streamed {
		s.writing.streamed = false
		return streamer.EndStream(ctx)
	}
	return nil
}

// catchUp shows what was missed while this session was away.
func (s *Session) catchUp(ctx context.Context) error {
	if err := s.endStream(ctx); err != nil {
		return err
	}
	s.writing.clear()

	messages, err := s.conv.Since(ctx, s.shown)
	if err != nil {
		return err
	}
	for _, m := range messages {
		s.shown = max(s.shown, m.ID)
		s.entries = max(s.entries, m.EntryID)
		if err := s.showMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// showMessage puts one message a session missed on the screen.
func (s *Session) showMessage(ctx context.Context, m store.Message) error {
	if m.Role == store.RoleUser {
		// Which frontend a message came from is not kept, so a session catching
		// up shows every one it has not shown.
		if other, ok := s.adapter.(api.OtherChannels); ok {
			return other.ShowUserMessage(ctx, &m)
		}
		return nil
	}
	if text := strings.TrimSpace(m.Text()); text != "" {
		if err := s.adapter.Send(ctx, api.Outgoing{Text: text, Hers: true}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) say(ctx context.Context, text string) error {
	return s.adapter.Send(ctx, api.Outgoing{Text: text})
}
