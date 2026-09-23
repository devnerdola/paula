package store

import (
	"context"
	"testing"
	"time"
)

// A call is written down as it starts, so a run that ends in the middle of one
// leaves a record that it was asked for, and what came of it is added once it
// has. An entry's calls are read back in the order they were made.
func TestAToolCallIsKeptUnderItsEntryAndTheRoundThatAsked(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	e := &Entry{StartedAt: now}
	if err := s.StartEntry(ctx, e); err != nil {
		t.Fatal(err)
	}
	r := &Request{EntryID: e.ID, Purpose: PurposeReply, Runner: "openrouter",
		Method: "POST", URL: "https://openrouter.ai/api/v1/chat/completions", StartedAt: now}
	if err := s.AddRequest(ctx, r); err != nil {
		t.Fatal(err)
	}

	first := &ToolCall{EntryID: e.ID, RequestID: r.ID, CallID: "call_1",
		Name: "search_memories", Arguments: `{"query":"Ana"}`, StartedAt: now}
	if err := s.StartToolCall(ctx, first); err != nil {
		t.Fatal(err)
	}
	// One whose run ended before it did is still there, with nothing come of it.
	second := &ToolCall{EntryID: e.ID, RequestID: r.ID, CallID: "call_2",
		Name: "search_memories", Arguments: `{"query":"Lisbon"}`, StartedAt: now}
	if err := s.StartToolCall(ctx, second); err != nil {
		t.Fatal(err)
	}
	first.Result, first.Error = "error: the index is gone", "the index is gone"
	first.EndedAt = now.Add(30 * time.Millisecond)
	if err := s.EndToolCall(ctx, first); err != nil {
		t.Fatal(err)
	}

	calls, err := s.ToolCalls(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].CallID != "call_1" || calls[1].CallID != "call_2" {
		t.Fatalf("calls = %+v, want both in the order they were made", calls)
	}
	got := calls[0]
	if got.RequestID != r.ID || got.Name != "search_memories" || got.Arguments != `{"query":"Ana"}` ||
		got.Result != "error: the index is gone" || got.Error != "the index is gone" ||
		!got.StartedAt.Equal(now) || !got.EndedAt.Equal(first.EndedAt) {
		t.Errorf("the first call = %+v", got)
	}
	if !calls[1].EndedAt.IsZero() || calls[1].Result != "" {
		t.Errorf("the call that never ended = %+v, want nothing come of it", calls[1])
	}
}
