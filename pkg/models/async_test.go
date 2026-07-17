package models

import (
	"strings"
	"testing"
)

// A caller-supplied name is authoritative and returned verbatim.
func TestEffectiveNameUsesProvidedName(t *testing.T) {
	l := AsyncLane{Name: "config-watch", Type: "http"}
	if got := l.EffectiveName(); got != "config-watch" {
		t.Fatalf("want provided name, got %q", got)
	}
}

// An omitted name is derived deterministically: the same lane identity must
// yield the same non-empty name on every call, because the name is stamped on
// mocks at record and re-derived at replay — the two must agree.
func TestEffectiveNameGeneratedIsDeterministic(t *testing.T) {
	l := AsyncLane{
		Type:       "http",
		Match:      map[string]string{"pathRegex": "^/poll$"},
		MatchQuery: map[string]string{"watch": "true"},
	}
	a, b := l.EffectiveName(), l.EffectiveName()
	if a == "" {
		t.Fatal("generated name must be non-empty")
	}
	if a != b {
		t.Fatalf("generated name not deterministic: %q vs %q", a, b)
	}
	// A generated name should still hint at the lane (type + path slug) so it
	// reads sensibly in recorded metadata and the replay verdict.
	if !strings.HasPrefix(a, "http-") {
		t.Fatalf("generated name should carry the type prefix, got %q", a)
	}
	// Map iteration order must not affect the result: two equal lanes carrying
	// MULTIPLE match/query keys must hash the same (writeSortedKV sorts them),
	// even though Go randomizes map iteration order per map.
	multi := func() AsyncLane {
		return AsyncLane{
			Type:       "http",
			Match:      map[string]string{"host": "h.svc", "pathRegex": "^/poll$"},
			MatchQuery: map[string]string{"watch": "true", "mode": "long"},
		}
	}
	n1, n2 := multi().EffectiveName(), multi().EffectiveName()
	if n1 != n2 {
		t.Fatalf("generated name must be stable across equal multi-key lanes: %q vs %q", n1, n2)
	}
}

// Lanes that route differently (different match or matchQuery) must get
// different names, so their recorded mocks land in separate streams.
func TestEffectiveNameGeneratedDistinctForDifferentRouting(t *testing.T) {
	base := AsyncLane{Type: "http", Match: map[string]string{"pathRegex": "^/poll$"}}
	other := AsyncLane{Type: "http", Match: map[string]string{"pathRegex": "^/notify$"}}
	byQuery := AsyncLane{Type: "http", Match: map[string]string{"pathRegex": "^/poll$"}, MatchQuery: map[string]string{"watch": "true"}}
	if base.EffectiveName() == other.EffectiveName() {
		t.Fatal("different path must yield different generated name")
	}
	if base.EffectiveName() == byQuery.EffectiveName() {
		t.Fatal("different matchQuery must yield different generated name")
	}
}

// The generated name depends ONLY on routing identity (type + match +
// matchQuery). volatileParams is replay-time tuning a user may set differently
// between record and replay, so it must NOT change the name — otherwise the
// record→replay join key would break.
func TestEffectiveNameStableAcrossReplayTuning(t *testing.T) {
	rec := AsyncLane{
		Type:       "http",
		Match:      map[string]string{"pathRegex": "^/poll$"},
		MatchQuery: map[string]string{"watch": "true"},
	}
	replay := AsyncLane{
		Type:           "http",
		Match:          map[string]string{"pathRegex": "^/poll$"},
		MatchQuery:     map[string]string{"watch": "true"},
		VolatileParams: []string{"version", "cursor"},
	}
	if rec.EffectiveName() != replay.EffectiveName() {
		t.Fatalf("name must ignore volatileParams: %q vs %q",
			rec.EffectiveName(), replay.EffectiveName())
	}
}

// WithEffectiveNames fills only the empty names, leaves provided ones, and does
// not mutate the caller's slice/elements.
func TestWithEffectiveNamesFillsEmptyPreservesProvided(t *testing.T) {
	in := []AsyncLane{
		{Name: "keep-me", Type: "http", Match: map[string]string{"path": "/a"}},
		{Type: "http", Match: map[string]string{"path": "/b"}},
	}
	out := WithEffectiveNames(in)
	if len(out) != 2 {
		t.Fatalf("want 2 lanes, got %d", len(out))
	}
	if out[0].Name != "keep-me" {
		t.Fatalf("provided name changed: %q", out[0].Name)
	}
	if out[1].Name == "" {
		t.Fatal("empty name not filled")
	}
	if in[1].Name != "" {
		t.Fatal("WithEffectiveNames mutated the caller's slice")
	}
}

func TestAsyncLaneIsPollAndBaseType(t *testing.T) {
	cases := []struct {
		typ      string
		wantPoll bool
		wantBase string
	}{
		{"http", false, "http"},
		{"httpPoll", true, "http"},
		{"HTTPPOLL", true, "HTTP"}, // suffix strip is case-insensitive on the suffix only
		{"mongoPoll", true, "mongo"},
		{"", false, ""},
	}
	for _, c := range cases {
		l := AsyncLane{Type: c.typ}
		if l.IsPoll() != c.wantPoll {
			t.Fatalf("%q: IsPoll=%v want %v", c.typ, l.IsPoll(), c.wantPoll)
		}
		if l.BaseType() != c.wantBase {
			t.Fatalf("%q: BaseType=%q want %q", c.typ, l.BaseType(), c.wantBase)
		}
	}
}

