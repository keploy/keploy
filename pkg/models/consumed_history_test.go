package models

import (
	"encoding/json"
	"reflect"
	"strings"
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

// A rewind is a new history of the baseline, counting one rewind more than the
// one it rewinds; the history rewound is left alone, a clone keeps the count,
// and the count crosses the wire on the filter only when there is one.
func TestConsumedHistoryRewoundToCountsTheRewinds(t *testing.T) {
	var none *ConsumedHistory
	if got := none.Rewinds(); got != 0 {
		t.Fatalf("a nil history counts %d rewinds, want 0", got)
	}
	own := ConsumedHistoryOf(map[string]MockState{"a": {Name: "a", Usage: Deleted}})
	baseline := map[string]MockState{"b": {Name: "b"}}
	first := own.RewoundTo(baseline)
	second := first.RewoundTo(baseline)
	if own.Rewinds() != 0 || first.Rewinds() != 1 || second.Rewinds() != 2 {
		t.Fatalf("rewinds %d, %d, %d; want 0, 1, 2", own.Rewinds(), first.Rewinds(), second.Rewinds())
	}
	if _, ok := own.Get("a"); !ok || own.Len() != 1 {
		t.Fatal("the rewind changed the history it rewound")
	}
	if _, ok := first.Get("a"); ok || first.Len() != 1 {
		t.Fatal("the rewound history is not the baseline")
	}
	if got := second.Clone().Rewinds(); got != 2 {
		t.Fatalf("a clone counts %d rewinds, want 2", got)
	}
	if b, _ := json.Marshal(MockFilterParams{}); strings.Contains(string(b), "consumedRewinds") {
		t.Fatalf("a filter with no rewind sends the field: %s", b)
	}
	b, _ := json.Marshal(MockFilterParams{ConsumedRewinds: 2})
	var back MockFilterParams
	if err := json.Unmarshal(b, &back); err != nil || back.ConsumedRewinds != 2 {
		t.Fatalf("the count did not cross the wire: %s -> %d (%v)", b, back.ConsumedRewinds, err)
	}
}
