// Package conversation keeps the one conversation Paula has: what was said,
// when a reply is due, and what a frontend is shown of it.
package conversation

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/media"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/store"
)

// NewMessage is a message that arrived, on whichever frontend it was sent from.
// Images are the bytes as they arrived: how they are kept is not a frontend's
// business.
type NewMessage struct {
	Channel string
	// From names the frontend it was sent from. Every frontend is told, so the
	// one it came from does not show it twice.
	From   string
	Text   string
	Images [][]byte
}

// How far a reply has got. fresh and writing are ones still going: a reply
// that has written nothing can be restarted by a new message, and one that has
// written something is left to finish. The rest are what it ended as.
type state int32

const (
	fresh state = iota
	writing
	restarted
	stopped
	failed
	whole
)

// attempt is one running entry. The loop and the goroutine writing the reply
// both read how far it has got, so it is held in an atomic.
type attempt struct {
	entry   *store.Entry
	upto    store.MessageID
	channel string
	// foldDue says the messages this attempt's prompt carried took more than
	// their share of the context, and compactDue that the summary it opened
	// with took more than its room. The goroutine building the prompt is the
	// one that knows; the loop reads them once the attempt is done.
	foldDue    bool
	compactDue bool

	cancel context.CancelFunc
	state  atomic.Int32
}

func (a *attempt) at() state { return state(a.state.Load()) }

// was reports whether the attempt ended as this.
func (a *attempt) was(s state) bool { return a.at() == s }

// end cancels the attempt and records what it ends as, unless it has ended
// already: a message landing on a stop does not turn it into a restart.
func (a *attempt) end(s state) {
	for {
		at := a.at()
		if at != fresh && at != writing {
			break
		}
		if a.state.CompareAndSwap(int32(at), int32(s)) {
			break
		}
	}
	a.cancel()
}

// finished records that the model wrote the whole reply. It is written over
// whatever the attempt was being cancelled for, since the reply is there
// however it landed.
func (a *attempt) finished() { a.state.Store(int32(whole)) }

// started says a reply has written something, and reports whether it is still
// the reply that will be shown. Only a restart makes it not: a reply being
// stopped keeps what lands while it is cancelled, and one the model finishes as
// it is cancelled is kept whole.
func (a *attempt) started() bool {
	for {
		switch a.at() {
		case fresh:
			if a.state.CompareAndSwap(int32(fresh), int32(writing)) {
				return true
			}
		case restarted:
			return false
		default:
			return true
		}
	}
}

// restart says a new message takes the place of this reply, and reports whether
// it does: one that has written something is left to finish.
func (a *attempt) restart() bool {
	if !a.state.CompareAndSwap(int32(fresh), int32(restarted)) {
		return false
	}
	a.cancel()
	return true
}

// status is what the entry of this attempt ends as, once the reply is in.
func (a *attempt) status(err error) string {
	switch a.at() {
	case restarted:
		return store.StatusRestarted
	case stopped:
		return store.StatusStopped
	case failed:
		return store.StatusFailed
	case whole:
		return store.StatusDone
	}
	if err != nil {
		return store.StatusFailed
	}
	return store.StatusDone
}

type Options struct {
	Store   *store.Store
	Runners *runners.Setup
	Persona *persona.Card
	Engine  config.Engine
	// Clock is what the times of the conversation are read from, including the
	// zone they are written to a model in.
	Clock Clock
	Log   *slog.Logger
}

type Engine struct {
	store    *store.Store
	media    *media.Files
	runners  *runners.Setup
	persona  *persona.Card
	rendered string
	cfg      config.Engine
	clock    Clock
	log      *slog.Logger
	events   *events
	// ratios is what a character of a prompt costs, which every request that
	// comes back counted says more about.
	ratios ratios

	posts     chan postRequest
	stops     chan stopRequest
	waits     chan waitRequest
	standings chan standingRequest
	done      chan doneRequest
	worked    chan worked

	closing sync.Once
	closed  chan struct{}
	ended   chan struct{}

	// base is the context the loop runs under: the one the engine was opened
	// with, minus its cancellation, so work the loop is in the middle of is
	// never cut short.
	base context.Context
}

type postRequest struct {
	msg   NewMessage
	parts []store.Part
	reply chan error
}

type stopRequest struct {
	reply chan stopResult
}

type stopResult struct {
	stopped bool
	err     error
}

type waitRequest struct {
	reply chan Seq
}

type standingRequest struct {
	reply chan standing
}

// standing is what Standing answers with, read in one step of the loop.
type standing struct {
	seq     Seq
	message store.MessageID
}

type doneRequest struct {
	attempt *attempt
	message *store.Message
	err     error
}

// Open picks the conversation up and starts its loop.
func Open(ctx context.Context, o Options) (*Engine, error) {
	e, l, err := newEngine(ctx, o)
	if err != nil {
		return nil, err
	}
	go l.run()
	return e, nil
}

// newEngine builds the engine and the state of its loop, without running it.
func newEngine(ctx context.Context, o Options) (*Engine, *loop, error) {
	if o.Clock == nil {
		o.Clock = Wall{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Persona == nil {
		return nil, nil, errors.New("no persona is set")
	}
	rendered, err := o.Persona.Render()
	if err != nil {
		return nil, nil, err
	}
	e := &Engine{
		store:     o.Store,
		media:     media.New(o.Store.Dir(), o.Engine.ImageMaxPx),
		runners:   o.Runners,
		persona:   o.Persona,
		rendered:  rendered,
		cfg:       o.Engine,
		clock:     o.Clock,
		log:       o.Log,
		events:    newEvents(),
		posts:     make(chan postRequest),
		stops:     make(chan stopRequest),
		waits:     make(chan waitRequest),
		standings: make(chan standingRequest),
		done:      make(chan doneRequest),
		worked:    make(chan worked),
		closed:    make(chan struct{}),
		ended:     make(chan struct{}),
	}
	e.base = context.WithoutCancel(ctx)
	if err := e.recover(ctx); err != nil {
		return nil, nil, err
	}

	e.checkRoom(ctx)

	// The loop's state is built before the loop runs, so a caller that posts
	// or waits as soon as Open returns finds it ready.
	l := &loop{e: e, debounce: e.clock.NewTimer(e.cfg.Debounce.Duration())}
	l.debounce.Stop()
	if err := l.start(ctx); err != nil {
		return nil, nil, err
	}
	return e, l, nil
}

// runEnded is what an entry ends with when the run stopped before the reply
// did.
const runEnded = "the run ended before the reply did"

// recover closes the entries a run that stopped left behind, so none of them
// still reads as running. An entry whose reply was stored ended with that
// reply, whatever the run managed to record afterwards.
func (e *Engine) recover(ctx context.Context) error {
	running, err := e.store.RunningEntries(ctx)
	if err != nil {
		return err
	}
	for i := range running {
		entry := running[i]
		entry.Status = store.StatusFailed
		entry.Error = runEnded
		entry.EndedAt = e.clock.Now()

		reply, err := e.store.ReplyOfEntry(ctx, entry.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if reply != nil {
			entry.Status = store.StatusDone
			if reply.Interrupted {
				entry.Status = store.StatusStopped
			}
			entry.Error = ""
		}
		if err := e.store.EndEntry(ctx, &entry); err != nil {
			return err
		}
		e.log.Info("entry ended by a run that stopped", "entry", entry.ID, "status", entry.Status)
	}
	return nil
}

// Close ends the loop and waits for it.
func (e *Engine) Close() {
	e.closing.Do(func() { close(e.closed) })
	<-e.ended
}

// Events yields the events after the given number.
func (e *Engine) Events(ctx context.Context, after Seq) iter.Seq2[Event, error] {
	return e.events.Events(ctx, after)
}

// Post stores a message and starts the wait before a reply.
func (e *Engine) Post(ctx context.Context, m NewMessage) error {
	// The images are kept here rather than in the loop, so decoding a photo
	// does not hold up a reply that is being written.
	parts, err := e.parts(ctx, m)
	if err != nil {
		return err
	}

	reply := make(chan error, 1)
	select {
	case e.posts <- postRequest{msg: m, parts: parts, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ended:
		return errors.New("the conversation is closed")
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop ends the reply being written, or the wait before one has started, and
// reports whether there was either.
func (e *Engine) Stop(ctx context.Context) (bool, error) {
	reply := make(chan stopResult, 1)
	select {
	case e.stops <- stopRequest{reply: reply}:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-e.ended:
		return false, errors.New("the conversation is closed")
	}
	select {
	case r := <-reply:
		return r.stopped, r.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// Wait returns once no reply is pending, with the number of the latest event,
// so a frontend can show everything up to it before going on.
func (e *Engine) Wait(ctx context.Context) (Seq, error) {
	reply := make(chan Seq, 1)
	select {
	case e.waits <- waitRequest{reply: reply}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-e.ended:
		return e.events.Seq(), nil
	}
	select {
	case seq := <-reply:
		return seq, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Standing is the number of the latest event and of the latest message stored,
// read together: a frontend follows the events from one and reads what was said
// before from the other.
func (e *Engine) Standing(ctx context.Context) (seq Seq, message store.MessageID, err error) {
	reply := make(chan standing, 1)
	select {
	case e.standings <- standingRequest{reply: reply}:
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	case <-e.ended:
		return e.events.Seq(), 0, errors.New("the conversation is closed")
	}
	select {
	case r := <-reply:
		return r.seq, r.message, nil
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
}

// History returns the messages older than a message, oldest first.
func (e *Engine) History(ctx context.Context, before store.MessageID, limit int) ([]store.Message, error) {
	return e.store.Messages(ctx, before, limit)
}

// loop is the conversation as the one goroutine that answers holds it. Every
// method here runs on that goroutine, and nothing else reads these fields.
type loop struct {
	e *Engine

	last     store.MessageID // the newest user message
	latest   store.MessageID // the newest message of any kind
	answered store.MessageID // the newest message an entry has answered
	channel  string          // where the newest user message came from
	running  *attempt
	debounce Timer
	pending  bool
	waiters  []chan Seq

	// keeping is the work that holds the prompt to the context, and embedding
	// the work that turns what a fold wrote into vectors. They are asked of
	// different models, so each runs beside the loop on its own and each waits
	// its own wait after a failure of its own: a host away for one says nothing
	// about the other, and one that is slow holds up only itself.
	keeping   work
	embedding work
}

// work is one piece of background work: whether it is out, how to cut it short,
// and when the next try may start. A piece that failed waits, doubling up to
// foldMost, since a host that refused one step refuses the next.
type work struct {
	out    bool
	cancel context.CancelFunc
	wait   time.Duration
	after  time.Time
}

// due reports whether this piece may start now: nothing of it is out, and
// whatever wait its last failure earned is over.
func (w *work) due(now time.Time) bool {
	return !w.out && !now.Before(w.after)
}

// start marks the piece as out and returns the context it runs under.
func (w *work) start(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	w.out, w.cancel = true, cancel
	return ctx
}

func (l *loop) run() {
	e := l.e
	defer close(e.ended)
	defer l.debounce.Stop()
	ctx := e.base

	for {
		select {
		case <-e.closed:
			if l.running != nil {
				// A run that ends is not a stop: what the reply had written is
				// not kept and the messages stay unanswered, the same as after
				// a run that was killed.
				a := l.running
				a.end(failed)
				req := <-e.done
				if !a.was(whole) && (req.err == nil || errors.Is(req.err, context.Canceled)) {
					req.err = errors.New(runEnded)
				}
				l.finish(ctx, req)
			}
			// Work behind a reply is cut short by the run ending, and what it
			// had written stands: the entry each piece left says how far it got.
			for _, w := range []*work{&l.keeping, &l.embedding} {
				if w.out {
					w.cancel()
				}
			}
			for l.keeping.out || l.embedding.out {
				l.take(<-e.worked)
			}
			l.pending = false
			l.wake()
			return

		case req := <-e.posts:
			req.reply <- l.post(ctx, req.msg, req.parts)

		case req := <-e.stops:
			req.reply <- l.stop(ctx)

		case req := <-e.standings:
			req.reply <- standing{seq: e.events.Seq(), message: l.latest}

		case req := <-e.waits:
			if l.idle() {
				req.reply <- e.events.Seq()
				continue
			}
			l.waiters = append(l.waiters, req.reply)

		case <-l.debounce.Chan():
			if !l.pending {
				continue
			}
			l.pending = false
			l.begin(ctx)

		case req := <-e.done:
			l.finish(ctx, req)

		case r := <-e.worked:
			l.take(r)
			if !r.embedding && l.embedding.due(e.clock.Now()) {
				// A fold is the only thing that writes memories, and what it
				// wrote has no vector until this runs. It is looked at as soon
				// as the fold is done rather than left to the next reply, which
				// may be a long time coming.
				l.embed(ctx)
			}
		}
	}
}

// take marks a piece of work as no longer out. What it failed at sets how long
// the next try of that piece waits, and says nothing about the other.
func (l *loop) take(r worked) {
	w, what := &l.keeping, "keeping the conversation inside the context"
	if r.embedding {
		w, what = &l.embedding, "embedding the memories"
	}
	w.out, w.cancel = false, nil
	w.wait, w.after = l.waitAfter(w.wait, r.err, what)
}

// waitAfter is how long the next try of one piece of work waits, and the time
// it may start at. Work that did not fail waits for nothing.
func (l *loop) waitAfter(was time.Duration, err error, what string) (time.Duration, time.Time) {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0, time.Time{}
	}
	wait := min(max(2*was, foldWait), foldMost)
	l.e.log.Warn(what, "error", err, "next try in", wait)
	return wait, l.e.clock.Now().Add(wait)
}

// behind starts the work that runs behind a reply. Each piece runs beside the
// loop on its own, so the conversation answers while they work and neither
// waits for the other.
func (l *loop) behind(ctx context.Context, a *attempt) {
	now := l.e.clock.Now()
	// Keeping the prompt inside the context is due when the prompt that just
	// went out says so: the messages took more than their share, or the summary
	// took more than its room.
	if (a.foldDue || a.compactDue) && l.keeping.due(now) {
		l.keep(ctx, a.foldDue)
	}
	// Embedding is due whenever it is not already running: what is waiting for
	// a vector is a question for the store, which answers it in one indexed
	// query, rather than something the loop keeps track of. A fold of this run
	// has just written memories, a run before this one may have left some, and
	// a model given the role since leaves every one of them without a vector of
	// the model serving now.
	if l.embedding.due(now) {
		l.embed(ctx)
	}
}

// keep starts the work that holds the prompt to the context.
func (l *loop) keep(ctx context.Context, fold bool) {
	e := l.e
	ctx = l.keeping.start(ctx)
	cancel := l.keeping.cancel
	go func() {
		defer cancel()
		// A fold is what makes the summary longer, so the summary is written
		// again after it rather than before. One the messages are not due takes
		// exchanges that are still inside their share and adds them to the very
		// summary that has outgrown its room.
		var err error
		if fold {
			err = e.fold(ctx)
		}
		if err == nil {
			err = e.compact(ctx)
		}
		e.worked <- worked{err: err}
	}()
}

// embed starts the work that turns the memories without a vector into vectors.
func (l *loop) embed(ctx context.Context) {
	e := l.e
	ctx = l.embedding.start(ctx)
	cancel := l.embedding.cancel
	go func() {
		defer cancel()
		e.worked <- worked{embedding: true, err: e.embed(ctx)}
	}()
}

// start picks the conversation up where it was left.
func (l *loop) start(ctx context.Context) error {
	e := l.e
	// What has been answered is what the entries say: the newest message an
	// entry that ended with a reply, or with a stop, was started for.
	answered, err := e.store.AnsweredUpto(ctx)
	if err != nil {
		return err
	}
	l.answered = answered

	newest, err := e.store.LastMessage(ctx, "")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if newest != nil {
		l.latest = newest.ID
	}

	last, err := e.store.LastMessage(ctx, store.RoleUser)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if last != nil {
		l.last = last.ID
		l.channel = last.Channel
		if last.ID > l.answered {
			l.pending = true
			l.debounce.Reset(e.cfg.Debounce.Duration())
		}
	}

	return nil
}

// post stores a message and starts the wait before a reply, or restarts the
// reply being written.
func (l *loop) post(ctx context.Context, m NewMessage, parts []store.Part) error {
	e := l.e
	// The conversation is stamped by one clock, which is this one: a frontend
	// says what was sent, not when.
	msg := &store.Message{
		Role:      store.RoleUser,
		Channel:   m.Channel,
		Parts:     parts,
		CreatedAt: e.clock.Now(),
	}
	if err := e.store.AddMessage(ctx, msg); err != nil {
		return err
	}
	l.last, l.latest = msg.ID, msg.ID
	l.channel = msg.Channel
	e.events.publish(Event{Kind: MessageStored, Channel: msg.Channel, From: m.From, Message: msg})

	switch {
	case l.running != nil:
		if e.cfg.PrefillCancel {
			l.running.restart()
		}
	default:
		l.pending = true
		l.debounce.Reset(e.cfg.Debounce.Duration())
	}
	return nil
}

// stop ends the reply being written, or the wait before one.
func (l *loop) stop(ctx context.Context) stopResult {
	if l.running != nil {
		l.running.end(stopped)
		return stopResult{stopped: true}
	}
	if !l.pending {
		return stopResult{}
	}

	// Nothing is being written, so the stop is an entry that answers the
	// messages waiting for a reply with nothing: what has been answered is read
	// back from the entries, so a stop that left none would be answered again by
	// the next run.
	l.pending = false
	l.debounce.Stop()
	now := l.e.clock.Now()
	entry := &store.Entry{
		Channel:        l.channel,
		AfterMessageID: l.answered,
		UptoMessageID:  l.last,
		StartedAt:      now,
	}
	if err := l.e.store.StartEntry(ctx, entry); err != nil {
		return stopResult{err: err}
	}
	entry.Status, entry.EndedAt = store.StatusStopped, now
	if err := l.e.store.EndEntry(ctx, entry); err != nil {
		return stopResult{err: err}
	}
	l.answered = l.last
	l.e.events.publish(Event{Kind: ReplyStopped, Entry: entry.ID, Channel: l.channel})
	l.wake()
	return stopResult{stopped: true}
}

// begin starts an entry for the messages that have no reply yet.
func (l *loop) begin(ctx context.Context) {
	e := l.e
	entry := &store.Entry{
		Channel:        l.channel,
		AfterMessageID: l.answered,
		UptoMessageID:  l.last,
		StartedAt:      e.clock.Now(),
	}
	if err := e.store.StartEntry(ctx, entry); err != nil {
		// Nothing was started, so the failure carries no entry. The messages
		// stay unanswered, the same as a reply that failed, and the next reply
		// covers them.
		e.log.Error("starting an entry", "error", err)
		e.events.publish(Event{Kind: ReplyFailed, Channel: l.channel, Text: err.Error()})
		l.wake()
		return
	}

	replyCtx, cancel := context.WithCancel(ctx)
	a := &attempt{entry: entry, upto: l.last, channel: l.channel, cancel: cancel}
	l.running = a
	e.events.publish(Event{Kind: ReplyStarted, Entry: entry.ID, Channel: l.channel})

	// The reply is written beside the loop, so what she is saying does not hold
	// up a message arriving or a stop.
	go func() {
		msg, err := e.reply(replyCtx, a)
		cancel()
		e.done <- doneRequest{attempt: a, message: msg, err: err}
	}()
}

// finish closes the entry a reply left and decides what happens next.
func (l *loop) finish(ctx context.Context, r doneRequest) {
	e := l.e
	a := r.attempt
	l.running = nil

	status := a.status(r.err)
	// A message is stored before the event carrying it goes out.
	if r.message != nil {
		if err := e.store.AddMessage(ctx, r.message); err != nil {
			e.log.Error("storing a reply", "entry", a.entry.ID, "error", err)
			r.message, r.err, status = nil, err, store.StatusFailed
		} else {
			l.latest = r.message.ID
		}
	}

	a.entry.Status = status
	a.entry.EndedAt = e.clock.Now()
	if r.err != nil && status == store.StatusFailed {
		a.entry.Error = r.err.Error()
	}
	e.endEntry(ctx, a.entry)
	e.log.Info("entry ended", "entry", a.entry.ID,
		"status", status, "duration", a.entry.EndedAt.Sub(a.entry.StartedAt))

	// However the reply ended, its prompt is what says whether the messages
	// have outgrown their share. One that failed for being too long is the
	// case a fold is most needed in.
	l.behind(ctx, a)

	switch status {
	case store.StatusRestarted:
		e.events.publish(Event{Kind: ReplyRestarted, Entry: a.entry.ID, Channel: a.channel})
	case store.StatusStopped:
		l.answered = max(l.answered, a.upto)
		e.events.publish(Event{Kind: ReplyStopped, Entry: a.entry.ID, Channel: a.channel, Message: r.message})
	case store.StatusFailed:
		// A reply that failed answered nothing, so the messages it was for are
		// covered by the next one, whether that comes of another message or of
		// the next run.
		e.events.publish(Event{Kind: ReplyFailed, Entry: a.entry.ID, Channel: a.channel, Text: r.err.Error()})
		l.wake()
		return
	default:
		l.answered = max(l.answered, a.upto)
		e.events.publish(Event{Kind: ReplyDone, Entry: a.entry.ID, Channel: a.channel, Message: r.message})
	}

	if l.last > l.answered {
		l.pending = true
		l.debounce.Reset(e.cfg.Debounce.Duration())
		return
	}
	l.wake()
}

// parts stores the images of a message and returns what the message is made
// of.
func (e *Engine) parts(ctx context.Context, m NewMessage) ([]store.Part, error) {
	var out []store.Part
	if text := strings.TrimSpace(m.Text); text != "" {
		out = append(out, store.Part{Type: store.PartText, Text: text})
	}
	for _, data := range m.Images {
		sha, err := e.media.Store(data)
		if err != nil {
			return nil, err
		}
		if err := e.store.AddMedia(ctx, sha); err != nil {
			return nil, err
		}
		out = append(out, store.Part{Type: store.PartImage, SHA256: sha, MIME: media.MIMEJPEG})
	}
	if len(out) == 0 {
		return nil, errors.New("there is nothing in the message")
	}
	return out, nil
}

// endEntry closes an entry and drops the bodies of the entries past
// engine.log_keep. Nothing else does either.
func (e *Engine) endEntry(ctx context.Context, entry *store.Entry) {
	ctx = context.WithoutCancel(ctx)
	if err := e.store.EndEntry(ctx, entry); err != nil {
		e.log.Error("ending an entry", "entry", entry.ID, "error", err)
		return
	}
	if err := e.store.Prune(ctx, e.cfg.LogKeep); err != nil {
		e.log.Error("dropping the oldest bodies", "error", err)
	}
}

// idle reports whether nothing is being written and nothing is waiting to be.
func (l *loop) idle() bool {
	return l.running == nil && !l.pending
}

// wake answers every Wait, once there is nothing left pending.
func (l *loop) wake() {
	if !l.idle() {
		return
	}
	seq := l.e.events.Seq()
	for _, w := range l.waiters {
		w <- seq
	}
	l.waiters = nil
}

// Since returns the messages newer than one, oldest first.
func (e *Engine) Since(ctx context.Context, after store.MessageID) ([]store.Message, error) {
	return e.store.MessagesAfter(ctx, after)
}
