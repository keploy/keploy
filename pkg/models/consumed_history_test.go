package models

import (
	"reflect"
	"testing"
)

func TestConsumedHistorySinceIsTheLatestOfWhatWasWrittenAfter(t *testing.T) {
	h := NewConsumedHistory(0)
	h.Set(MockState{Name: "a", Usage: Updated})
	pos := h.Pos()
	h.Set(MockState{Name: "b", Usage: Updated})
	h.Set(MockState{Name: "a", Usage: Deleted})
	h.Set(MockState{Name: "b", Usage: Deleted})

	since, end, ok := h.Since(pos)
	want := map[string]MockState{"a": {Name: "a", Usage: Deleted}, "b": {Name: "b", Usage: Deleted}}
	if !ok || end != 4 || !reflect.DeepEqual(since, want) {
		t.Fatalf("Since(%d) = %v, %d, %v; want %v, 4, true", pos, since, end, ok, want)
	}
	if _, _, ok := h.Since(end + 1); ok {
		t.Fatal("Since past the history's writes reported ok: a client would build on a state that never was")
	}
	if !reflect.DeepEqual(h.Map(), want) || h.Len() != 2 {
		t.Fatalf("Map = %v; want %v", h.Map(), want)
	}
}

func TestConsumedMarkIsTheHistoryAsItWasWhenMarked(t *testing.T) {
	h := ConsumedHistoryOf(map[string]MockState{"a": {Name: "a", Usage: Updated}})
	mark := h.Mark()
	h.Set(MockState{Name: "a", Usage: Deleted})
	h.Set(MockState{Name: "b", Usage: Updated})
	if got, want := mark.State(), map[string]MockState{"a": {Name: "a", Usage: Updated}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mark.State() = %v; want %v", got, want)
	}
	c := h.Clone()
	h.Set(MockState{Name: "c", Usage: Updated})
	if c.Len() != 2 || !reflect.DeepEqual(c.Map(), map[string]MockState{"a": {Name: "a", Usage: Deleted}, "b": {Name: "b", Usage: Updated}}) {
		t.Fatalf("Clone = %v; want the two entries at the clone", c.Map())
	}
}
