package conversation

import (
	"context"
	"slices"
	"strings"
	"testing"

	"nerdola.dev/x/paula/internal/store"
)

// remembering is a chat model that answers a fold with one memory of the
// oldest message it is shown.
func remembering(memory string) *fakeRunner {
	return &fakeRunner{model: chatModel(), chat: folding("hm", memory, "they talked")}
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

// Every memory names Caio or her, so a query that names them finds only what
// holds its other words, and one that holds nothing else is told so rather
// than answered with every memory there is. The memories are ones a model
// kept with the remember tool on 23 September 2026.
func TestASearchLooksPastTheNames(t *testing.T) {
	f := &fakeRunner{model: chatModel(), chat: says("hello")}
	r := openReply(t, f)
	ctx := context.Background()
	r.say(t, "hey")
	first, err := r.store.MessagesAfter(ctx, 0)
	if err != nil || len(first) == 0 {
		t.Fatalf("messages = %+v, %v", first, err)
	}
	var ids []store.MemoryID
	for _, content := range []string{
		"Caio's sister Ana is arriving from Recife on Saturday and Caio is picking her up at the airport.",
		"Caio started learning the guitar this week.",
		"On 23 September 2026 Paula spent the day inking a children's book commission and burned her risotto while practicing piano.",
	} {
		m := &store.Memory{Content: content, Source: first[0].ID}
		if err := r.store.Remember(ctx, m); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}

	for _, c := range []struct {
		query string
		want  []store.MemoryID
	}{
		{"Caio's sister", ids[:1]},
		{"Paula's piano practice", ids[2:]},
		{"Caio's favourite food", nil},
	} {
		found, err := r.Memories(ctx, c.query, 10)
		if err != nil {
			t.Fatalf("searching %q: %v", c.query, err)
		}
		var got []store.MemoryID
		for _, m := range found {
			got = append(got, m.ID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("searching %q found %v, want %v", c.query, got, c.want)
		}
	}

	if _, err := r.Memories(ctx, "Caio's", 10); err == nil || !strings.Contains(err.Error(), "names Caio or Paula") {
		t.Errorf("searching for a name alone = %v, want it said that every memory names them", err)
	}
}
