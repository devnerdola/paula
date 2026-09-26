package conversation

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/logs"
	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
	toolkinds "nerdola.dev/x/paula/internal/tools"
)

// TestLive holds how a conversation is held to real models, at the size of the
// model's context: the runners, the persona and the default chat and vision
// models of the configuration file PAULA_LIVE points to, with the keys the
// environment holds as serve reads them. It is skipped without PAULA_LIVE, so
// the suite runs without a network.
//
//	PAULA_LIVE=live.yaml go test ./internal/conversation -run '^TestLive$' -v -timeout 3h
//
// It goes through the past in testdata, months of ordinary texting, in three
// runs of Paula:
//
//   - the first day of the past, and a first run in which she is asked to
//     remember something and sent a picture;
//   - the past that follows, as much of it as the history's reservation holds,
//     and a second run that goes on with the past. It starts counting a word
//     higher than any prompt has come to, finds the history past its
//     reservation, and compacts it before its first turn, whose messages wait
//     for it;
//   - the same again in a third run, whose compaction takes the summary so far
//     with it, and which then asks for her memories and for the picture again.
//
// What a word costs is read off the host's count of every reply, so the past
// written before a run is measured at the most a word has cost in the runs
// before it.
//
// The context is the model's as the file gives it: its context setting, or the
// largest its catalogue reports. The past covers contexts up to about 200,000
// tokens.
//
// Everything is written to a data directory of its own, which is kept, with a
// configuration file to read it by and report.md: each check with what it
// expected, what it found and the requests that show it, which paula turns
// -dump prints as they were sent and received.
func TestLive(t *testing.T) {
	path := os.Getenv("PAULA_LIVE")
	if path == "" {
		t.Skip("PAULA_LIVE names no configuration file")
	}
	lv := openLive(t, path)
	defer lv.close()

	// A picture large enough for a model to see what it shows: red on its left
	// half and blue on its right, as testdata/SOURCES.md of the media package
	// says.
	photo, err := os.ReadFile(filepath.Join("..", "media", "testdata", "red-blue-800x400.png"))
	if err != nil {
		t.Fatal(err)
	}
	p := lv.load()
	clock := &pastClock{}

	lv.printf("\n## The first run")
	day := p.exchanges[0].day
	lv.write(p.take(func(x exchange) bool { return x.day == day }), clock)
	first := lv.run("first run", clock)
	// Neither the name nor the place is anywhere in the past, so an answer
	// that holds them read them in her memories.
	first.send([]string{"Please remember this: my sister's name is Beatriz, and she lives in Recife."}, nil)
	first.send([]string{"Look at this picture I took."}, nil, photo)
	first.end()
	// The bodies of requests are kept for the newest entries only, which the
	// past written after a run takes, so each run is checked as it ends.
	lv.checkAnswered(first)
	lv.checkPictureSent(first)
	lv.checkNoMemoryTold(first)
	lv.requestsTable(first)

	second := lv.talkPast("second run", []*liveRun{first}, p, clock)
	second.end()
	lv.checkAnswered(second)
	lv.checkCompactedFirst(second)
	lv.checkParts(second)
	lv.checkResent(second)
	lv.checkNoMemoryTold(second)
	lv.requestsTable(second)

	third := lv.talkPast("third run", []*liveRun{first, second}, p, clock)
	third.send([]string{"Tell me everything you have saved in your memories about me."}, nil)
	third.send([]string{"Please open the picture I sent you a while ago and look at it again. Which colour is on its left side?"}, nil)
	third.end()
	lv.checkAnswered(third)
	lv.checkCompactedFirst(third)
	lv.checkCompressedAgain(third)
	lv.checkParts(third)
	lv.checkResent(third)
	lv.checkNoMemoryTold(third)
	lv.checkMemories(third.turns[len(third.turns)-2])
	lv.checkPictureGot(third, third.turns[len(third.turns)-1])
	lv.requestsTable(third)

	lv.printf("\n%d checks passed, %d failed.", lv.passed, lv.failed)
}

// pastClock is the time of the past: it reads what the test last set it to,
// going on from there as the machine's clock does, and never goes back.
type pastClock struct {
	Wall
	mu    sync.Mutex
	at    time.Time
	since time.Time
}

func (c *pastClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at.Add(time.Since(c.since))
}

func (c *pastClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now := c.at.Add(time.Since(c.since)); now.After(t) {
		t = now
	}
	c.at, c.since = t, time.Now()
}

// live is the live test: what it runs on, and the report it writes.
type live struct {
	t      *testing.T
	dir    string
	path   string
	cfg    *config.Config
	card   *persona.Card
	set    *runners.Setup
	tools  []toolkinds.Tool
	log    *slog.Logger
	st     *store.Store
	report *os.File

	passed, failed int
}

func openLive(t *testing.T, path string) *live {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	card, err := persona.Load(cfg.Persona)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "paula-live-")
	if err != nil {
		t.Fatal(err)
	}
	lv := &live{t: t, dir: dir, path: path, cfg: cfg, card: card}
	lv.report, err = os.Create(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	liveConfig(t, path, dir, cfg.Persona)

	secrets := new(logs.Secrets)
	logFile, err := os.Create(filepath.Join(dir, "serve.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	lv.log = logs.New(logFile, slog.LevelInfo, logs.FormatText, secrets)
	lv.set, err = runners.Configure(cfg, runners.Host{Log: lv.log, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cfg.Tools {
		opened, err := toolkinds.Open(tc.Name, tc.Section, toolkinds.Host{
			Names:    toolkinds.Names{Character: card.Name, User: card.User.Name},
			Language: card.Language,
		})
		if err != nil {
			t.Fatal(err)
		}
		lv.tools = append(lv.tools, opened...)
	}
	lv.st, err = store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lv.set.Check(context.Background()); err != nil {
		t.Fatal(err)
	}

	lv.printf("# Live run\n")
	lv.printf("Configuration: %s", path)
	lv.printf("Data directory: %s", dir)
	lv.printf("Read it with: paula -config %s turns", filepath.Join(dir, "paula.yaml"))
	return lv
}

func (lv *live) close() {
	lv.st.Close()
	lv.printf("\nData directory: %s", lv.dir)
	lv.report.Close()
}

// open opens an engine on the store the way serve does, on a clock.
func (lv *live) open(clock Clock) *Engine {
	e, err := Open(context.Background(), Options{
		Store: lv.st, Runners: lv.set, Persona: lv.card, Engine: lv.cfg.Engine,
		Clock: clock, Log: lv.log, Tools: lv.tools,
	})
	if err != nil {
		lv.t.Fatal(err)
	}
	return e
}

// liveConfig writes the configuration file the run's data directory is read
// by: the one it ran on, with its data directory and its persona.
func liveConfig(t *testing.T, path, dir, card string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	doc := root.Content[0]
	set := func(key, value string) {
		for i := 0; i+1 < len(doc.Content); i += 2 {
			if doc.Content[i].Value == key {
				doc.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: value}
				return
			}
		}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
	}
	set("data_dir", filepath.Join(dir, "data"))
	set("persona", card)
	out, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "paula.yaml"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (lv *live) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Fprintln(lv.report, line)
	lv.t.Log(line)
}

// check writes one check down with what it expected and what it found, and
// fails the test when it does not hold.
func (lv *live) check(name string, ok bool, expected, found string, requests ...int64) {
	verdict := "PASS"
	if ok {
		lv.passed++
	} else {
		lv.failed++
		verdict = "FAIL"
	}
	lv.printf("- **%s** %s\n  - expected: %s\n  - found: %s\n  - requests: %v", verdict, name, expected, found, requests)
	if !ok {
		lv.t.Errorf("%s: expected %s, found %s (requests %v)", name, expected, found, requests)
	}
}

// pastFile is the past as testdata holds it; gen.go says how it was written.
type pastFile struct {
	Model   string `json:"model"`
	Written string `json:"written"`
	Days    [][]struct {
		At   string `json:"at"`
		From string `json:"from"`
		Text string `json:"text"`
	} `json:"days"`
}

// exchange is what he sent and her reply to it, as a run would have stored
// them, on the day of the past it was said.
type exchange struct {
	day   int
	asked []store.Message
	reply store.Message
}

// past is the conversation the test goes through, and how far it has got.
type past struct {
	exchanges []exchange
	next      int
}

// take is the exchanges from where the test has got to for as long as keep
// holds them.
func (p *past) take(keep func(exchange) bool) []exchange {
	start := p.next
	for p.next < len(p.exchanges) && keep(p.exchanges[p.next]) {
		p.next++
	}
	return p.exchanges[start:p.next]
}

// load reads the past, placed so that its first day is as many days ago as it
// holds.
func (lv *live) load() *past {
	t := lv.t
	f, err := os.Open(filepath.Join("testdata", "past.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var file pastFile
	if err := json.NewDecoder(z).Decode(&file); err != nil {
		t.Fatal(err)
	}
	lv.printf("The past: %d days written by %s on %s", len(file.Days), file.Model, file.Written)

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	p := &past{}
	var id store.MessageID
	for i, day := range file.Days {
		date := today.AddDate(0, 0, i-len(file.Days))
		var asked []store.Message
		var said []string
		var repliedAt time.Time
		flush := func() {
			if len(asked) > 0 && len(said) > 0 {
				id++
				p.exchanges = append(p.exchanges, exchange{day: i, asked: asked, reply: store.Message{
					ID: id, Role: store.RoleAssistant, Channel: "live", CreatedAt: repliedAt,
					Parts: []store.Part{{Type: store.PartText, Text: strings.Join(said, "\n\n")}},
				}})
				asked, said = nil, nil
			}
		}
		for _, m := range day {
			clock, err := time.Parse("15:04", m.At)
			if err != nil {
				t.Fatal(err)
			}
			at := date.Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute)
			if m.From == "user" {
				// What she said before he wrote again was her reply, and what
				// she says before he has written anything is nothing a run
				// could have sent: she only answers.
				if len(said) > 0 {
					flush()
				}
				id++
				asked = append(asked, store.Message{ID: id, Role: store.RoleUser, Channel: "live", CreatedAt: at,
					Parts: []store.Part{{Type: store.PartText, Text: m.Text}}})
				continue
			}
			if len(asked) > 0 {
				said = append(said, m.Text)
				repliedAt = at
			}
		}
		flush()
	}
	return p
}

// write stores exchanges the way a run stores its turns: his messages, an
// entry that answered them, and her reply in that entry. Nothing is stored
// before what the conversation already holds, and the clock is left after
// what was written.
func (lv *live) write(exchanges []exchange, clock *pastClock) {
	ctx := context.Background()
	answered, err := lv.st.AnsweredUpto(ctx)
	if err != nil {
		lv.t.Fatal(err)
	}
	var after time.Time
	if last, err := lv.st.LastMessage(ctx, ""); err == nil {
		after = last.CreatedAt
	}
	later := func(t time.Time) time.Time {
		if !t.After(after) {
			t = after.Add(time.Second)
		}
		after = t
		return t
	}
	for _, x := range exchanges {
		var last store.MessageID
		for _, msg := range x.asked {
			msg.ID, msg.CreatedAt = 0, later(msg.CreatedAt)
			if err := lv.st.AddMessage(ctx, &msg); err != nil {
				lv.t.Fatal(err)
			}
			last = msg.ID
		}
		at := later(x.reply.CreatedAt)
		entry := &store.Entry{Channel: "live", AfterMessageID: answered, UptoMessageID: last, StartedAt: at}
		if err := lv.st.StartEntry(ctx, entry); err != nil {
			lv.t.Fatal(err)
		}
		reply := x.reply
		reply.ID, reply.ReplyTo, reply.EntryID, reply.CreatedAt = 0, last, entry.ID, at
		if err := lv.st.AddMessage(ctx, &reply); err != nil {
			lv.t.Fatal(err)
		}
		entry.Status, entry.EndedAt = store.StatusDone, at
		if err := lv.st.EndEntry(ctx, entry); err != nil {
			lv.t.Fatal(err)
		}
		answered = last
	}
	clock.set(after.Add(time.Minute))
	if len(exchanges) > 0 {
		lv.printf("Written: %d exchanges of the past, %s to %s",
			len(exchanges), dateText(time.Local, exchanges[0].reply.CreatedAt), dateText(time.Local, after))
	}
}

// talkPast writes as much of the past as the history's reservation holds, and
// runs Paula on the past that follows, a turn for each burst of his messages,
// and then runs Paula on the two exchanges of the past that follow.
//
// What is written is measured at the dearest a word has cost in the runs
// before, the nearest there is to what the host will count. The run starts
// counting a word higher than that, so it finds the history past its
// reservation and has it compacted before its first turn, which his messages
// of that turn wait for.
func (lv *live) talkPast(name string, before []*liveRun, p *past, clock *pastClock) *liveRun {
	t := lv.t
	ctx := context.Background()
	lv.printf("\n## The %s", name)

	e := before[0].e
	for _, r := range before[1:] {
		if r.e.costs.rate(r.model().Name) > e.costs.rate(r.model().Name) {
			e = r.e
		}
	}
	m, err := e.roleModel(ctx, config.RoleChat)
	if err != nil {
		t.Fatal(err)
	}
	rate := e.costs.rate(m.Name)
	_, room := e.reservations(m)
	_, said, _, err := e.history(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := e.render(said, m.catalogue.Vision, 0, m.notes(), e.weighing(ctx))
	words, images := measure(msgs)
	tokens := func(words int) int {
		return int(math.Ceil(float64(words)*rate)) + images*e.costs.image(m.Name)
	}
	written := p.take(func(x exchange) bool {
		msgs, _ := e.render(append(slices.Clone(x.asked), x.reply), m.catalogue.Vision, 0, m.notes(), e.weighing(ctx))
		more, _ := measure(msgs)
		if tokens(words+more) > room {
			return false
		}
		words += more
		return true
	})
	lv.write(written, clock)
	lv.printf("The history then: %d tokens of the %d reserved, at %.3f tokens a word", tokens(words), room, rate)
	if p.next+2 > len(p.exchanges) {
		t.Fatalf("the past ran out before it reached the history's reservation: set a smaller context for the model")
	}

	r := lv.run(name, clock)
	for range 2 {
		r.exchange(p.exchanges[p.next])
		p.next++
	}
	return r
}

// exchange sends his messages of an exchange of the past, each at its time,
// and has her answer them.
func (r *liveRun) exchange(x exchange) {
	var texts []string
	var at []time.Time
	for _, msg := range x.asked {
		texts = append(texts, msg.Text())
		at = append(at, msg.CreatedAt)
	}
	r.send(texts, at)
}

// liveRun is one run of Paula: the engine as serve opens it, and what it did.
type liveRun struct {
	lv    *live
	name  string
	e     *Engine
	clock *pastClock
	after store.EntryID // the newest entry before the run
	upto  store.EntryID // the newest entry of the run
	turns []liveTurn
}

// liveTurn is what he sent in one turn and what came of it: the entry of the
// reply, the history and the reservations as the engine measured them right
// after it, and the summary that stood then.
type liveTurn struct {
	sent     []string
	message  store.MessageID
	entry    store.EntryID
	failed   string
	history  int
	reserved int
	summary  int
	standing string
}

func (t liveTurn) said() string { return strings.Join(t.sent, " / ") }

func (lv *live) newest() store.EntryID {
	entries, err := lv.st.Entries(context.Background(), 1)
	if err != nil {
		lv.t.Fatal(err)
	}
	if len(entries) == 0 {
		return 0
	}
	return entries[0].ID
}

func (lv *live) run(name string, clock *pastClock) *liveRun {
	return &liveRun{lv: lv, name: name, clock: clock, after: lv.newest(), e: lv.open(clock)}
}

func (r *liveRun) model() *model {
	m, err := r.e.roleModel(context.Background(), config.RoleChat)
	if err != nil {
		r.lv.t.Fatal(err)
	}
	return m
}

// send posts his messages of one turn, each at its time when it has one, and
// waits for the turn that answers them, and no longer: the next goes out as
// soon as she has answered, the way he would text back.
func (r *liveRun) send(texts []string, at []time.Time, images ...[]byte) {
	lv := r.lv
	ctx := context.Background()
	seq, _, err := r.e.Standing(ctx)
	if err != nil {
		lv.t.Fatal(err)
	}
	for i, text := range texts {
		if at != nil {
			r.clock.set(at[i])
		}
		msg := NewMessage{Channel: "live", Text: text}
		if i == 0 {
			msg.Images = images
		}
		if err := r.e.Post(ctx, msg); err != nil {
			lv.t.Fatal(err)
		}
	}
	turn := liveTurn{sent: texts}
	for ev, err := range r.e.Events(ctx, seq) {
		if err != nil {
			lv.t.Fatal(err)
		}
		if ev.Kind == MessageStored && ev.Message != nil && turn.message == 0 {
			turn.message = ev.Message.ID
		}
		if ev.Kind == ReplyDone || ev.Kind == ReplyStopped || ev.Kind == ReplyFailed {
			turn.entry = ev.Entry
			if ev.Kind != ReplyDone {
				turn.failed = ev.Kind.String() + ": " + ev.Text
			}
			break
		}
	}
	m := r.model()
	turn.summary, turn.reserved = r.e.reservations(m)
	turn.history = lv.historySize(r.e, m)
	if s, err := lv.st.LatestSummary(ctx); err == nil {
		turn.standing = s.Content
	}
	r.turns = append(r.turns, turn)

	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	lv.printf("- %q (%d pictures) → entry %d: %s%s\n  - history after it: %d of %d reserved",
		clip(turn.said(), 70), len(images), turn.entry, clip(oneLine(textOfMessage(reply)), 120), errorOf(turn.failed),
		turn.history, turn.reserved)
	// Every turn after one that failed is about a conversation that did not
	// happen, so the run stops at it.
	if turn.failed != "" {
		r.end()
		lv.check("every message is answered", false, "a reply to each", fmt.Sprintf("%q: %s", turn.said(), turn.failed))
		lv.t.FailNow()
	}
}

// compacted says a compaction of this run has ended and a turn has been
// answered after it.
func (r *liveRun) compacted() bool {
	for _, entry := range r.entries() {
		if entry.UptoMessageID == 0 && entry.Status != store.StatusRunning {
			return len(r.turns) > 0 && r.turns[len(r.turns)-1].entry > entry.ID
		}
	}
	return false
}

// end waits for whatever the last turn set off, and closes the run. Wait
// answers once the turn is over, and a compaction the turn set off goes on
// after it.
func (r *liveRun) end() {
	if _, err := r.e.Wait(context.Background()); err != nil {
		r.lv.t.Fatal(err)
	}
	for r.compacting() {
		time.Sleep(time.Second)
	}
	r.e.Close()
	r.upto = r.lv.newest()
}

// compacting says the last turn went through with the history past its
// reservation, and the compaction it set off has not ended.
func (r *liveRun) compacting() bool {
	if len(r.turns) == 0 {
		return false
	}
	last := r.turns[len(r.turns)-1]
	if last.failed != "" || !overflowing(r.lv.t, r.e) {
		return false
	}
	for _, entry := range r.compactions() {
		if entry.ID > last.entry && entry.Status != store.StatusRunning {
			return false
		}
	}
	return true
}

// entries are the entries of the run, oldest first.
func (r *liveRun) entries() []store.Entry {
	all, err := r.lv.st.Entries(context.Background(), 100000)
	if err != nil {
		r.lv.t.Fatal(err)
	}
	var out []store.Entry
	for _, entry := range slices.Backward(all) {
		if entry.ID > r.after && (r.upto == 0 || entry.ID <= r.upto) {
			out = append(out, entry)
		}
	}
	return out
}

// compactions are the entries of the run that answer no message.
func (r *liveRun) compactions() []store.Entry {
	return slices.DeleteFunc(r.entries(), func(e store.Entry) bool { return e.UptoMessageID != 0 })
}

func (lv *live) requests(entry store.EntryID) []store.Request {
	requests, err := lv.st.Requests(context.Background(), entry)
	if err != nil {
		lv.t.Fatal(err)
	}
	return requests
}

// replied is the first request of a reply, which a caption can go before.
func (lv *live) replied(entry store.EntryID) (store.Request, bool) {
	requests := lv.requests(entry)
	i := slices.IndexFunc(requests, func(r store.Request) bool { return r.Purpose == store.PurposeReply })
	if i < 0 {
		return store.Request{}, false
	}
	return requests[i], true
}

func (lv *live) historySize(e *Engine, m *model) int {
	ctx := context.Background()
	summary, said, _, err := e.history(ctx)
	if err != nil {
		return -1
	}
	weighing := e.weighing(ctx)
	if weighing.thought, err = e.pastThought(ctx, m, coveredUpto(summary)); err != nil {
		return -1
	}
	msgs, _ := e.render(said, m.catalogue.Vision, 0, m.notes(), weighing)
	return size(msgs, e.costs.rate(m.Name), e.costs.image(m.Name))
}

// checkAnswered holds every turn of a run to having been answered.
func (lv *live) checkAnswered(r *liveRun) {
	var found []string
	for _, turn := range r.turns {
		entry, err := lv.st.Entry(context.Background(), turn.entry)
		reply, _ := lv.st.ReplyOfEntry(context.Background(), turn.entry)
		if err != nil || entry.Status != store.StatusDone || reply == nil {
			found = append(found, fmt.Sprintf("%q not answered", clip(turn.said(), 40)))
		}
	}
	lv.check("every message is answered", len(found) == 0, "a reply ended done for each turn",
		fmt.Sprintf("%d turns; %s", len(r.turns), strings.Join(found, "; ")))
}

// checkCompactedFirst holds a run that starts on a history past its
// reservation to compacting it before its first turn, and his messages of that
// turn to waiting for it: the turn starts once the compaction has ended, and
// carries the summary and only those messages.
func (lv *live) checkCompactedFirst(r *liveRun) {
	ctx := context.Background()
	entries := r.entries()
	if len(entries) == 0 || entries[0].UptoMessageID != 0 || len(r.turns) == 0 {
		lv.check("a run whose history is past its reservation compacts it before its first turn", false,
			"a compaction as the run's first entry", fmt.Sprintf("%d entries", len(entries)))
		return
	}
	c := entries[0]
	first := r.turns[0]
	msg, err := lv.st.Message(ctx, first.message)
	entry, _ := lv.st.Entry(ctx, first.entry)
	before := err == nil && !msg.CreatedAt.After(c.EndedAt)
	later := entry != nil && !entry.StartedAt.Before(c.EndedAt)
	req, _ := lv.replied(first.entry)
	sent := sentMessages(req.RequestBody)
	var users []string
	for _, m := range sent {
		if m.role == api.RoleUser {
			users = append(users, m.text)
		}
	}
	carries := len(sent) > 0 && first.standing != "" && strings.Contains(sent[0].text, firstLine(first.standing))
	lv.check("a run whose history is past its reservation compacts it before its first turn, which waits for it and carries the summary and no message it covers",
		c.Status == store.StatusDone && before && later && carries && slices.Equal(users, first.sent),
		"the compaction first, his messages sent before it ended, the turn started after it, with the summary and only his messages of that turn",
		fmt.Sprintf("compaction entry %d %s, %s to %s; his first message sent %s; the turn started %s; summary in the prompt: %v; his messages in it: %d of the turn's %d",
			c.ID, c.Status, c.StartedAt.Format(time.TimeOnly), c.EndedAt.Format(time.TimeOnly), stamp(msg), stampEntry(entry),
			carries, len(users), len(first.sent)),
		lv.ids(c.ID, first.entry)...)
}

// checkCompressedAgain holds a compaction that follows another to compressing
// the summary so far with the history.
func (lv *live) checkCompressedAgain(r *liveRun) {
	compactions := r.compactions()
	if len(compactions) == 0 {
		return
	}
	parts := lv.requests(compactions[0].ID)
	again := len(parts) > 0 && strings.HasPrefix(userText(parts[0].RequestBody), "Summary so far:")
	lv.check("a compaction after another compresses the summary so far with the history", again,
		"its first part starting with the summary so far", fmt.Sprintf("%v", again), lv.ids(compactions[0].ID)...)
}

// checkParts holds each compaction's parts to the context and to what the
// model writes at once, and says how much of its reservation the summary took.
func (lv *live) checkParts(r *liveRun) {
	m := r.model()
	for _, c := range r.compactions() {
		parts := lv.requests(c.ID)
		lv.printf("\n### Compaction, entry %d, %s: %d parts", c.ID, c.Status, len(parts))
		var written, fits int
		whole := true
		for _, p := range parts {
			if p.Usage == nil {
				whole = false
				continue
			}
			asked := "no number of"
			if sent := sentMessages(p.RequestBody); len(sent) > 0 {
				if found := wordsAsked.FindStringSubmatch(sent[0].text); found != nil {
					asked = found[1]
				}
			}
			text := p.Usage.CompletionTokens - p.Usage.ReasoningTokens
			written += text
			lv.printf("- request #%d: carried %d tokens, asked for %s words, wrote %d tokens of text and %d of reasoning, finished %q",
				p.ID, p.Usage.PromptTokens, asked, text, p.Usage.ReasoningTokens, p.FinishReason)
			if p.FinishReason != "stop" {
				whole = false
			}
			if p.Usage.PromptTokens+p.Usage.CompletionTokens <= m.limit() && (m.output() == 0 || p.Usage.CompletionTokens <= m.output()) {
				fits++
			}
		}
		reserved := lv.reservedAt(r, c)
		lv.printf("- the summary: %d tokens of text as the host counted them, of %d reserved (%d%%)",
			written, reserved, written*100/max(1, reserved))
		ids := lv.ids(c.ID)
		lv.check("each part of a compaction is written whole", whole && len(parts) > 0,
			"every part finished with stop", fmt.Sprintf("%d parts, all whole: %v", len(parts), whole), ids...)
		lv.check("each part of a compaction fits the context and what the model writes at once", fits == len(parts),
			fmt.Sprintf("carried and written within %d, written within %d", m.limit(), m.output()),
			fmt.Sprintf("%d of %d parts", fits, len(parts)), ids...)
	}
}

// reservedAt is the summary's reservation as the engine had it for a
// compaction: the one measured after the turn before it.
func (lv *live) reservedAt(r *liveRun, c store.Entry) int {
	reserved := 0
	for _, turn := range r.turns {
		if turn.entry < c.ID {
			reserved = turn.summary
		}
	}
	return reserved
}

// checkResent holds each reply to sending the prompt before it again as it
// was, up to the time now of the message that one answered, while no
// compaction came between them, and writes down what the host kept of it.
func (lv *live) checkResent(r *liveRun) {
	lv.printf("\n### What each reply sent again, and what the host kept of it\n")
	lv.printf("| request | sent | cached | resends the one before up to its time now |")
	lv.printf("|---|---|---|---|")
	ok, pairs := true, 0
	var previous *store.Request
	for _, entry := range r.entries() {
		if entry.UptoMessageID == 0 {
			previous = nil
			continue
		}
		req, found := lv.replied(entry.ID)
		if !found || req.Usage == nil {
			continue
		}
		resent := "first after a compaction or of a run"
		if previous != nil {
			was := sentMessages(previous.RequestBody)
			sent := sentMessages(req.RequestBody)
			k := slices.IndexFunc(was, func(m sentMessage) bool { return strings.HasPrefix(m.text, "It is now ") })
			same := k > 0 && k <= len(sent) && slices.Equal(was[:k], sent[:k])
			ok = ok && same
			pairs++
			resent = fmt.Sprintf("%v, the first %d messages of #%d", same, k, previous.ID)
		}
		lv.printf("| #%d | %d | %d | %s |", req.ID, req.Usage.PromptTokens, req.Usage.CachedTokens, resent)
		previous = &req
	}
	if pairs == 0 {
		lv.printf("No two replies of the run came without a compaction between them.")
		return
	}
	lv.check("each reply resends the prompt before it up to its time now, while no compaction comes between", ok,
		"every pair the same", fmt.Sprintf("%d pairs, all the same: %v", pairs, ok))
}

// checkNoMemoryTold holds every reply of a run to telling no memory: a memory
// said in the conversation is in its messages as it was said, so one told
// would be in a system message.
func (lv *live) checkNoMemoryTold(r *liveRun) {
	memories, err := lv.st.Memories(context.Background())
	if err != nil {
		lv.t.Fatal(err)
	}
	var told []string
	var ids []int64
	for _, entry := range r.entries() {
		for _, req := range lv.requests(entry.ID) {
			if req.Purpose != store.PurposeReply {
				continue
			}
			for _, m := range sentMessages(req.RequestBody) {
				for _, mem := range memories {
					if m.role == api.RoleSystem && strings.Contains(m.text, mem.Content) {
						told = append(told, mem.Content)
						ids = append(ids, req.ID)
					}
				}
			}
		}
	}
	lv.check("no memory is in a system message of any reply", len(told) == 0,
		fmt.Sprintf("none of the %d memories", len(memories)), fmt.Sprintf("%q", told), ids...)
}

// checkMemories holds the question about her memories to having looked them
// up with her tools, and to what was remembered in the first run.
func (lv *live) checkMemories(turn liveTurn) {
	ctx := context.Background()
	memories, _ := lv.st.Memories(ctx)
	lv.check("what she was asked to remember is a memory",
		slices.ContainsFunc(memories, func(m store.Memory) bool { return strings.Contains(m.Content, "Beatriz") }),
		"a memory naming Beatriz", memoryTexts(memories))
	lv.checkCalled(turn, "a question about her memories looks them up", "list_memories", "search_memories")
	reply, _ := lv.st.ReplyOfEntry(ctx, turn.entry)
	lv.check("the answer holds what was remembered", strings.Contains(textOfMessage(reply), "Beatriz"),
		"a reply naming Beatriz", textOfMessage(reply), lv.ids(turn.entry)...)
}

// checkPictureSent holds the picture to being kept as a file and a caption,
// and sent as a picture to a model that sees and as its caption to one that
// does not.
func (lv *live) checkPictureSent(first *liveRun) {
	ctx := context.Background()
	sees := first.model().catalogue.Vision
	images, err := lv.st.Images(ctx)
	if err != nil || len(images) == 0 {
		lv.check("the picture is stored", false, "one picture", fmt.Sprintf("%v", err))
		return
	}
	img := images[0]
	e := first.e
	_, ferr := e.media.Load(img.SHA256)
	lv.check("the picture is stored as a file and as a caption", ferr == nil && img.Caption != "",
		"its file and what it showed", fmt.Sprintf("file %s (%v); caption %q", e.media.Path(img.SHA256), ferr, img.Caption))

	turn := first.turns[1]
	if req, ok := lv.replied(turn.entry); ok {
		var pictures, captions int
		for _, msg := range sentMessages(req.RequestBody) {
			pictures += msg.images
			if strings.Contains(msg.text, "[photo") {
				captions++
			}
		}
		want, wantCaptions := 1, 0
		if !sees {
			want, wantCaptions = 0, 1
		}
		lv.check("the turn with the picture sends it as one to a model that sees, and as its caption to one that does not",
			pictures == want && captions == wantCaptions,
			fmt.Sprintf("%d pictures and %d captions (the model sees: %v)", want, wantCaptions, sees),
			fmt.Sprintf("%d pictures, %d captions", pictures, captions), req.ID)
	}
}

// checkPictureGot holds the request for the picture again to getting it with
// the tool, whose answer carries the picture to a model that sees.
func (lv *live) checkPictureGot(r *liveRun, looked liveTurn) {
	ctx := context.Background()
	sees := r.model().catalogue.Vision
	lv.checkCalled(looked, "a request to look at an old picture again gets it", "get_image")
	calls, _ := lv.st.ToolCalls(ctx, looked.entry)
	if i := slices.IndexFunc(calls, func(c store.ToolCall) bool { return c.Name == "get_image" && c.Error == "" }); i >= 0 {
		requests := lv.requests(looked.entry)
		if j := slices.IndexFunc(requests, func(r store.Request) bool { return r.ID > calls[i].RequestID }); j >= 0 {
			next := requests[j]
			var inAnswers, elsewhere int
			for _, msg := range sentMessages(next.RequestBody) {
				if msg.role == api.RoleTool {
					inAnswers += msg.images
				} else {
					elsewhere += msg.images
				}
			}
			want := 0
			if sees {
				want = 1
			}
			lv.check("the picture get_image returns is sent inside its answer, to a model that sees",
				inAnswers == want && elsewhere == 0 && next.Error == "",
				fmt.Sprintf("%d pictures in tool answers and none elsewhere (the model sees: %v), taken by the host", want, sees),
				fmt.Sprintf("%d in tool answers, %d elsewhere; error %q", inAnswers, elsewhere, next.Error),
				calls[i].RequestID, next.ID)
		}
	}
	reply, _ := lv.st.ReplyOfEntry(ctx, looked.entry)
	lv.check("the answer says the left side is red", strings.Contains(strings.ToLower(textOfMessage(reply)), "red"),
		"a reply saying red", textOfMessage(reply), lv.ids(looked.entry)...)
}

// checkCalled holds a turn to having called one of the tools named.
func (lv *live) checkCalled(turn liveTurn, name string, tools ...string) {
	calls, _ := lv.st.ToolCalls(context.Background(), turn.entry)
	var called []string
	for _, c := range calls {
		called = append(called, fmt.Sprintf("%s %s", c.Name, c.Arguments))
	}
	ok := slices.ContainsFunc(calls, func(c store.ToolCall) bool { return slices.Contains(tools, c.Name) })
	lv.check(name, ok, "a call of "+strings.Join(tools, " or "), fmt.Sprintf("calls %v", called), lv.ids(turn.entry)...)
}

// requestsTable is one line for every request of a run.
func (lv *live) requestsTable(r *liveRun) {
	lv.printf("\n### Every request of the %s\n", r.name)
	lv.printf("| request | entry | for | sent | cached | written | reasoning | finish | cost |")
	lv.printf("|---|---|---|---|---|---|---|---|---|")
	var total float64
	for _, entry := range r.entries() {
		for _, req := range lv.requests(entry.ID) {
			var u store.Usage
			if req.Usage != nil {
				u = *req.Usage
			}
			total += req.Cost
			lv.printf("| #%d | %d | %s | %d | %d | %d | %d | %s | $%.6f |", req.ID, entry.ID, req.Purpose,
				u.PromptTokens, u.CachedTokens, u.CompletionTokens, u.ReasoningTokens, req.FinishReason, req.Cost)
		}
	}
	lv.printf("\nThe %s cost $%.4f.", r.name, total)
}

// ids are the requests of entries.
func (lv *live) ids(entries ...store.EntryID) []int64 {
	var out []int64
	for _, entry := range entries {
		for _, r := range lv.requests(entry) {
			out = append(out, r.ID)
		}
	}
	return out
}

// sentMessage is one message of a request as it was sent, read back from its
// body: its role, its text, and how many pictures it carried.
type sentMessage struct {
	role   string
	text   string
	images int
}

func sentMessages(body []byte) []sentMessage {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	out := make([]sentMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msg := sentMessage{role: m.Role}
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			msg.text = text
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				if p.Type == "text" {
					msg.text += p.Text
				}
				if strings.Contains(p.Type, "image") {
					msg.images++
				}
			}
		}
		out = append(out, msg)
	}
	return out
}

// userText is the text of the first message of his a request carried.
func userText(body []byte) string {
	for _, m := range sentMessages(body) {
		if m.role == api.RoleUser {
			return m.text
		}
	}
	return ""
}

func memoryTexts(memories []store.Memory) string {
	var out []string
	for _, m := range memories {
		out = append(out, fmt.Sprintf("#%d %q", m.ID, m.Content))
	}
	return strings.Join(out, "; ")
}

func textOfMessage(m *store.Message) string {
	if m == nil {
		return ""
	}
	return m.Text()
}

func stamp(m *store.Message) string {
	if m == nil {
		return "never"
	}
	return m.CreatedAt.Format(time.TimeOnly)
}

func stampEntry(e *store.Entry) string {
	if e == nil {
		return "never"
	}
	return e.StartedAt.Format(time.TimeOnly)
}

func errorOf(err string) string {
	if err == "" {
		return ""
	}
	return " — " + err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
