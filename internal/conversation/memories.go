package conversation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"nerdola.dev/x/paula/internal/persona"
	"nerdola.dev/x/paula/internal/store"
)

// Summary is what she has been told of the conversation before the messages a
// prompt still carries, and nil while it has never been folded.
func (e *Engine) Summary(ctx context.Context) (*store.Summary, error) {
	return e.summary(ctx)
}

// Forget takes a memory away, and the ones it replaced with it. What was said
// stays: this is about what she carries, not about the conversation.
func (e *Engine) Forget(ctx context.Context, id store.MemoryID) ([]store.Memory, error) {
	return e.store.Forget(ctx, id)
}

// Memories are the memories that hold the words of a query, or the newest ones
// when there is no query. They are what she has been told, whether or not a
// prompt had room to tell her.
func (e *Engine) Memories(ctx context.Context, query string, limit int) ([]store.Memory, error) {
	return SearchMemories(ctx, e.store, e.persona, query, limit)
}

// SearchMemories are the memories of a conversation that hold the words of a
// query, the ones the words say most about first, or the newest ones when there
// is no query.
func SearchMemories(ctx context.Context, s *store.Store, card *persona.Card, query string, limit int) ([]store.Memory, error) {
	if query == "" {
		return s.LatestMemories(ctx, limit)
	}
	words := searchWords(card, query)
	if len(words) == 0 {
		return nil, fmt.Errorf("%q has no word to look for: every memory names %s or %s, so a search is for what it is about",
			query, card.User.Name, card.Name)
	}
	return s.SearchMemories(ctx, words, limit)
}

// searchWords are the words of a query that say what it looks for. Every memory
// names who it is about, which is one of the two of them, so a name finds all of
// them; and a single letter is what an apostrophe leaves of a word, as the s of
// "Caio's".
func searchWords(card *persona.Card, query string) []string {
	names := words(strings.ToLower(card.Name + " " + card.User.Name))
	var out []string
	for _, w := range words(query) {
		if utf8.RuneCountInString(w) > 1 && !slices.Contains(names, strings.ToLower(w)) {
			out = append(out, w)
		}
	}
	return out
}

// words splits text where the search splits a memory: at whatever is not a
// letter or a digit.
func words(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}
