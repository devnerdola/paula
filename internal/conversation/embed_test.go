package conversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"nerdola.dev/x/paula/internal/config"
	"nerdola.dev/x/paula/internal/runners"
	"nerdola.dev/x/paula/internal/runners/api"
	"nerdola.dev/x/paula/internal/store"
)

// meaning embeds each string by what it holds, so a test can say which
// memories are near which words without a model.
func meaning(req api.EmbedRequest) (*api.EmbedResult, error) {
	out := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
	for i, said := range req.Input {
		// Three directions, one for each thing the conversation is about.
		var v [3]float32
		for word, at := range map[string]int{"Lisbon": 0, "sister": 0, "cook": 1, "food": 1, "bicycle": 2, "bike": 2} {
			if strings.Contains(strings.ToLower(said), strings.ToLower(word)) {
				v[at]++
			}
		}
		out.Vectors[i] = v[:]
	}
	return out, nil
}

// remembering is a chat model that answers a fold with one memory of the
// oldest message it is shown.
func remembering(memory string) *fakeRunner {
	return &fakeRunner{model: chatModel(), chat: folding("hm", memory, "they talked")}
}

func TestWhatAFoldWroteIsEmbedded(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	waitFor(t, "the memories to be embedded", func() bool {
		_, by := embedding(t, r)
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
		return err == nil && len(waiting) == 0
	})

	// It went out as one request of its own, under the model that embeds.
	_, by := embedding(t, r)
	memories, err := r.store.Memories(ctx)
	if err != nil || len(memories) == 0 {
		t.Fatalf("memories = %+v, %v", memories, err)
	}
	found, _, err := r.store.NearestMemories(ctx, by, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != len(memories) {
		t.Errorf("%d memories were embedded, want the %d there are", len(found), len(memories))
	}

	// It went out in an entry of its own, recorded under what it was for, so
	// turns is a record of what was embedded and what it cost.
	entries, err := r.store.Entries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var embedded bool
	for _, entry := range entries {
		requests, err := r.store.Requests(ctx, entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range requests {
			if req.Purpose == store.PurposeEmbedding {
				embedded = true
				if len(requests) != 1 {
					t.Errorf("entry %d holds %d requests, want the embedding alone", entry.ID, len(requests))
				}
			}
		}
	}
	if !embedded {
		t.Error("nothing was recorded as having embedded anything")
	}
}

// embedding is the runner and model that serve the embed role of a test.
func embedding(t *testing.T, r *replyEngine) (*fakeRunner, store.Embedded) {
	t.Helper()
	m, by, err := r.Engine.embedded(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m.Runner.(*fakeRunner), by
}

func TestMemoriesAreSearchedByWhatTheyMean(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	vectors, by := embedding(t, r)
	vectors.mu.Lock()
	vectors.embed = meaning
	vectors.mu.Unlock()

	talkPast(t, r)
	waitFor(t, "the memories to be embedded", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
		return err == nil && len(waiting) == 0
	})

	// A question about the family finds the memory about the sister, though
	// they share not one word.
	found, err := r.Memories(ctx, "where does the family live", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("nothing was found")
	}
	if !strings.Contains(found[0].Content, "Lisbon") {
		t.Errorf("the nearest is %q, want the memory about Lisbon", found[0].Content)
	}
}

func TestMemoriesWithNoQueryAreTheNewest(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	talkPast(t, r)
	stored, err := r.store.Memories(ctx)
	if err != nil || len(stored) == 0 {
		t.Fatalf("memories = %+v, %v", stored, err)
	}

	found, err := r.Memories(ctx, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	// The newest first, and nothing was asked of a model to say so.
	if len(found) != 1 || found[0].ID != stored[len(stored)-1].ID {
		t.Errorf("found %+v, want the newest memory", found)
	}
}

// alsoEmbeds adds another model that embeds to a conversation's models, so a
// test can give the role to one that did not have it.
func alsoEmbeds(set *runners.Setup, name, id string) *runners.Configured {
	v := &fakeRunner{model: api.Model{ID: id, Context: 8192, Embeddings: true}}
	m := &runners.Configured{Name: name, ID: id, Runner: v}
	set.Runners = append(set.Runners, v)
	set.Models = append(set.Models, m)
	return m
}

func TestMemoriesAreEmbeddedAgainByTheModelThatTakesOverTheRole(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	set := sized(f, 2000)
	other := alsoEmbeds(set, "others", "some/others")
	r := openReplyWith(t, f, set)
	ctx := context.Background()

	_, was := embedding(t, r)
	talkPast(t, r)
	waitFor(t, "the memories to be embedded", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, was, 0, 10)
		return err == nil && len(waiting) == 0
	})

	// The role is given to another model, whose vectors measure nothing against
	// the ones the memories carry, so the run they were embedded in is the one
	// that embeds them again.
	if err := r.SetModel(ctx, config.RoleEmbed, other.Name); err != nil {
		t.Fatal(err)
	}
	by := store.Embedded{Runner: other.Runner.Name(), Model: other.ID}
	if waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10); err != nil || len(waiting) == 0 {
		t.Fatalf("waiting under the model that took over = %+v, %v", waiting, err)
	}

	r.say(t, "anything else?")
	waitFor(t, "the memories to be embedded by the model that took over", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
		return err == nil && len(waiting) == 0
	})
}

// A model given the role while the work is out embeds nothing of what that
// work came back with: it finished under the model before it, and the memories
// are without a vector from this one.
func TestAModelTakesOverTheRoleWhileTheMemoriesAreBeingEmbedded(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	set := sized(f, 2000)
	other := alsoEmbeds(set, "others", "some/others")
	r := openReplyWith(t, f, set)
	ctx := context.Background()

	vectors, _ := embedding(t, r)
	out, held := make(chan struct{}), make(chan struct{})
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		close(out)
		<-held
		res := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
		for i := range res.Vectors {
			res.Vectors[i] = []float32{1, 0, 0}
		}
		return res, nil
	}
	vectors.mu.Unlock()

	talkPast(t, r)
	waitFor(t, "the embedding to go out", func() bool {
		select {
		case <-out:
			return true
		default:
			return false
		}
	})
	if err := r.SetModel(ctx, config.RoleEmbed, other.Name); err != nil {
		t.Fatal(err)
	}
	close(held)

	by := store.Embedded{Runner: other.Runner.Name(), Model: other.ID}
	r.say(t, "anything else?")
	waitFor(t, "the memories to be embedded by the model that took over", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
		return err == nil && len(waiting) == 0
	})
}

func TestEmbeddingABatchThatComesBackShort(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))

	vectors, _ := embedding(t, r)
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		// One vector short: storing the rest would leave a memory that is
		// searched for and never found.
		return &api.EmbedResult{Vectors: make([][]float32, len(req.Input)-1)}, nil
	}
	vectors.mu.Unlock()

	talkPast(t, r)
	waitFor(t, "the embedding to be taken as a failure", func() bool {
		return strings.Contains(r.log.String(), "embedding the memories")
	})
	got := r.log.String()
	if !strings.Contains(got, "embedded as") {
		t.Errorf("the log holds %q, want what came back short", got)
	}
	// It is the embedding that failed, not the fold that wrote them.
	if strings.Contains(got, "keeping the conversation inside the context") {
		t.Errorf("the log holds %q, want nothing said of the fold", got)
	}
}

// A host that will not take one memory says nothing about the ones behind it.
// The batch is asked for in halves until the one it refuses is alone, and that
// one is left aside for the run rather than left in front of every memory
// after it, which would leave them all unsearchable for good.
func TestAMemoryTheHostWillNotTakeIsLeftAside(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	vectors, by := embedding(t, r)
	var asked atomic.Int64
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		asked.Add(1)
		for _, said := range req.Input {
			if strings.Contains(said, "bicycle") {
				return nil, &api.APIError{Status: 400, Message: "this one cannot be embedded"}
			}
		}
		return meaning(req)
	}
	vectors.mu.Unlock()

	// Four memories, one of which the host will not take.
	var memories []store.Memory
	for _, said := range []string{
		"Ana lives in Lisbon", "Caio can cook", "Caio rides a bicycle", "they like food",
	} {
		memories = append(memories, store.Memory{
			Content: said, Source: add(t, r, store.RoleUser, said),
		})
	}
	err := r.store.Fold(ctx, &store.Summary{
		UptoMessageID: memories[0].Source, Content: "they talked",
	}, memories)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.embed(ctx); err != nil {
		t.Fatalf("embedding = %v, want the run to carry on without it", err)
	}
	if asked.Load() < 2 {
		t.Errorf("the host was asked %d times, want the batch asked for in halves", asked.Load())
	}

	// The one it would not take is the one without a vector, and it is said so.
	waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || !strings.Contains(waiting[0].Content, "bicycle") {
		t.Errorf("waiting = %+v, want the one the host would not take", waiting)
	}
	if got := r.log.String(); !strings.Contains(got, "a memory was not turned into a vector") {
		t.Errorf("the log holds %q, want the memory that was left aside", got)
	}

	// The run does not ask about it again: the same model would say the same
	// thing, and the memories behind it are embedded already.
	was := asked.Load()
	if err := r.embed(ctx); err != nil {
		t.Fatal(err)
	}
	if asked.Load() != was {
		t.Errorf("the host was asked %d more times about what it refused", asked.Load()-was)
	}
}

// What was set aside is asked for and dropped every time, so a batch of them
// is a window that reaches no further than they do. The window is widened by
// as many rather than read as nothing left to do, which would leave every
// memory behind them without a vector for the whole run.
func TestAWindowOfWhatWasSetAsideDoesNotHideWhatIsBehindIt(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	vectors, by := embedding(t, r)
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		for _, said := range req.Input {
			if strings.Contains(said, "bicycle") {
				return nil, &api.APIError{Status: 400, Message: "this one cannot be embedded"}
			}
		}
		return meaning(req)
	}
	vectors.mu.Unlock()

	// A whole batch of memories the host will not take, and one behind them
	// that it would.
	var memories []store.Memory
	for i := range embedBatch + 1 {
		said := fmt.Sprintf("Caio rides a bicycle %d", i)
		if i == embedBatch {
			said = "Ana lives in Lisbon"
		}
		memories = append(memories, store.Memory{
			Content: said, Source: add(t, r, store.RoleUser, said),
		})
	}
	err := r.store.Fold(ctx, &store.Summary{
		UptoMessageID: memories[0].Source, Content: "they talked",
	}, memories)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.embed(ctx); err != nil {
		t.Fatalf("embedding = %v, want the run to carry on without it", err)
	}
	waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 2*embedBatch)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != embedBatch {
		t.Fatalf("%d memories are waiting, want only the %d the host would not take",
			len(waiting), embedBatch)
	}
	for _, mem := range waiting {
		if !strings.Contains(mem.Content, "bicycle") {
			t.Errorf("%q is waiting, want it embedded behind the ones set aside", mem.Content)
		}
	}
}

// How wide a model's vectors are is something only an answer says, and a
// search is an answer: one that finds nothing because everything she
// remembers was written at another width is what tells the run the width
// moved, and what she remembers is written again behind it.
func TestASearchSaysHowWideTheModelAnswersNow(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	vectors, by := embedding(t, r)
	wide := &atomic.Int64{}
	wide.Store(3)
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		out := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
		for i := range out.Vectors {
			out.Vectors[i] = make([]float32, wide.Load())
			out.Vectors[i][0] = 1
		}
		return out, nil
	}
	vectors.mu.Unlock()

	talkPast(t, r)
	waitFor(t, "the memories to be embedded", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
		return err == nil && len(waiting) == 0
	})

	// The model now answers at another width. Every memory was written at the
	// one it answered at before, so the search reaches none of them.
	wide.Store(4)
	found, err := r.Memories(ctx, "where does the family live", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %+v, want nothing measured against a width of its own", found)
	}
	if got := r.log.String(); !strings.Contains(got, "another width") {
		t.Errorf("the log holds %q, want what was left out of the search", got)
	}

	// The search said how wide it answers now, so what she remembers is
	// waiting to be written again, and the next reply writes it.
	if waiting, err := r.store.MemoriesToEmbed(ctx, by, 4, 10); err != nil || len(waiting) == 0 {
		t.Fatalf("waiting = %+v, %v, want what was written at the width before", waiting, err)
	}
	r.say(t, "anything else?")
	waitFor(t, "the memories to be written again", func() bool {
		waiting, err := r.store.MemoriesToEmbed(ctx, by, 4, 10)
		return err == nil && len(waiting) == 0
	})
	found, err = r.Memories(ctx, "where does the family live", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Error("nothing was found once they were written again")
	}
}

// A key the host will not take is the host, not the memory. Reading it as one
// memory after another the host refuses would leave every one of them aside
// for the run, each in a failed entry of its own, and the work would report
// that it had nothing left to do.
func TestAKeyTheHostWillNotTakeIsNotTheMemory(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))
	ctx := context.Background()

	vectors, by := embedding(t, r)
	var asked atomic.Int64
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		asked.Add(1)
		return nil, &api.APIError{Status: 401, Message: "the key was not taken"}
	}
	vectors.mu.Unlock()

	var memories []store.Memory
	for _, said := range []string{"Ana lives in Lisbon", "Caio can cook", "they like food"} {
		memories = append(memories, store.Memory{
			Content: said, Source: add(t, r, store.RoleUser, said),
		})
	}
	err := r.store.Fold(ctx, &store.Summary{
		UptoMessageID: memories[0].Source, Content: "they talked",
	}, memories)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.embed(ctx); err == nil {
		t.Fatal("embedding said nothing went wrong, want the key the host would not take")
	}
	if asked.Load() != 1 {
		t.Errorf("the host was asked %d times, want the batch left whole", asked.Load())
	}

	// They are all still waiting: the next try asks for the same batch.
	waiting, err := r.store.MemoriesToEmbed(ctx, by, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != len(memories) {
		t.Errorf("%d memories are waiting, want the %d the host said nothing about",
			len(waiting), len(memories))
	}
}

// Embedding is asked of another model, so a host away for it says nothing
// about the fold: waiting for one would let the prompt grow past its share
// while the messages it is made of pile up.
func TestEmbeddingThatFailsDoesNotHoldUpTheFold(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))

	vectors, _ := embedding(t, r)
	vectors.mu.Lock()
	vectors.embed = func(api.EmbedRequest) (*api.EmbedResult, error) {
		return nil, errors.New("the host is away")
	}
	vectors.mu.Unlock()

	talkPast(t, r)
	waitFor(t, "the embedding to fail", func() bool {
		return strings.Contains(r.log.String(), "embedding the memories")
	})
	folded := len(f.sentFor(store.PurposeMemories))

	// The clock moves by the debounce of each message, which is far less than
	// the wait a failure earns: a fold held to the embedding's wait would not
	// come round again here.
	long := strings.Repeat("a long thing to say ", 20)
	for i := range 8 {
		r.say(t, fmt.Sprintf("more %d: %s", i, long))
	}
	waitFor(t, "the conversation to be folded again", func() bool {
		return len(f.sentFor(store.PurposeMemories)) > folded
	})
}

// A host that is slow rather than away holds up nothing either: the two pieces
// of work run beside each other, and a backlog of memories to embed would
// otherwise let the prompt grow for as long as the backlog takes.
func TestEmbeddingThatIsSlowDoesNotHoldUpTheFold(t *testing.T) {
	f := remembering("Caio's sister Ana lives in Lisbon.")
	r := openReplyWith(t, f, sized(f, 2000))

	vectors, _ := embedding(t, r)
	held := make(chan struct{})
	vectors.mu.Lock()
	vectors.embed = func(req api.EmbedRequest) (*api.EmbedResult, error) {
		<-held
		out := &api.EmbedResult{Vectors: make([][]float32, len(req.Input))}
		for i := range out.Vectors {
			out.Vectors[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
	vectors.mu.Unlock()
	defer close(held)

	// The first fold writes memories, and embedding them goes out and stays
	// out. The conversation keeps being folded while it hangs there.
	talkPast(t, r)
	folded := len(f.sentFor(store.PurposeMemories))

	long := strings.Repeat("a long thing to say ", 20)
	for i := range 8 {
		r.say(t, fmt.Sprintf("more %d: %s", i, long))
	}
	waitFor(t, "the conversation to be folded again", func() bool {
		return len(f.sentFor(store.PurposeMemories)) > folded
	})
}
