package models

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// read is a clock read recorded at at (RFC 3339) to resolution res.
func read(t *testing.T, at string, res time.Duration) ClockRead {
	t.Helper()
	tm, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatal(err)
	}
	return ClockRead{At: tm, Res: res}
}

// A clock read goes with the bind it was read at: a's bind keeps a's reads,
// a bind taken from b keeps b's, and a variable both bind is a clock read only
// when both say so.
func TestMergeVariablesKeepsAClockReadWithItsBind(t *testing.T) {
	at := []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Second)}
	for _, c := range []struct {
		name string
		a, b MockVariable
		want []ClockRead
	}{
		{"a's bind, marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, at},
		{"a bind taken from b, marked", MockVariable{Recorded: "r", Use: []string{"url.query.since"}}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, at},
		{"both bind, both marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, at},
		{"both bind, one marked", MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, MockVariable{Recorded: "r", Bind: "body.at"}, nil},
		{"both bind, the other marked", MockVariable{Recorded: "r", Bind: "body.at"}, MockVariable{Recorded: "r", Bind: "body.at", Clock: at}, nil},
		{"a use marked is no clock read", MockVariable{Recorded: "r", Bind: "body.at"}, MockVariable{Recorded: "r", Use: []string{"url.query.since"}, Clock: at}, nil},
	} {
		in := MockVariables{"at": c.a}
		got := MergeVariables(in, MockVariables{"at": c.b})["at"]
		if !reflect.DeepEqual(got.Clock, c.want) && !(len(got.Clock) == 0 && len(c.want) == 0) {
			t.Errorf("%s: merged Clock %v, want %v", c.name, got.Clock, c.want)
		}
		if len(got.Clock) > 0 {
			got.Clock[0].Res = time.Hour
			if in["at"].Clock != nil && in["at"].Clock[0].Res == time.Hour {
				t.Errorf("%s: the merged block shares its reads with a", c.name)
			}
		}
	}
}

// FD-3's case is two reads of one clock made in one second: the earlier
// read's resolution ends less than a second before the later one begins, as
// the recordings say. A creation time and an expiry an hour later are both
// clock reads, and are no such pair; nor is a variable with no reads, nor two
// that hold a different number of reads, nor a time of day and a date.
func TestReadInOneSecond(t *testing.T) {
	sec := func(at string) []ClockRead { return []ClockRead{read(t, at, time.Second)} }
	for _, c := range []struct {
		name string
		a, b []ClockRead
		want bool
	}{
		{"one second, straddling a tick", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:01Z"), true},
		{"the other way round", sec("2026-10-14T10:00:01Z"), sec("2026-10-14T10:00:00Z"), true},
		{"the same instant", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z"), true},
		{"two seconds apart", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:02Z"), false},
		{"an expiry an hour later", sec("2026-10-14T10:00:00Z"), sec("2026-10-14T11:00:00Z"), false},
		{"milliseconds less than a second apart", []ClockRead{read(t, "2026-10-14T10:00:00.100Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:01.099Z", time.Millisecond)}, true},
		{"milliseconds a second apart", []ClockRead{read(t, "2026-10-14T10:00:00.100Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:01.101Z", time.Millisecond)}, false},
		{"minutes straddling a tick", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:01:00Z", time.Minute)}, true},
		{"minutes two apart", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:02:00Z", time.Minute)}, false},
		// The earlier read's resolution, whichever of the two it is.
		{"a minute, then a millisecond read at the next minute", []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, []ClockRead{read(t, "2026-10-14T10:01:00.500Z", time.Millisecond)}, true},
		{"a millisecond read, then the minute before it", []ClockRead{read(t, "2026-10-14T10:01:00.500Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:00:00Z", time.Minute)}, true},
		{"a millisecond read, then a minute read after it", []ClockRead{read(t, "2026-10-14T10:00:58.500Z", time.Millisecond)}, []ClockRead{read(t, "2026-10-14T10:01:00Z", time.Minute)}, false},
		{"no reads", nil, nil, false},
		{"one with no reads", sec("2026-10-14T10:00:00Z"), nil, false},
		{"the other with no reads", nil, sec("2026-10-14T10:00:00Z"), false},
		{"another number of reads", sec("2026-10-14T10:00:00Z"), append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z")...), false},
		{"two reads each, the second an hour apart", append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T10:00:00Z")...), append(sec("2026-10-14T10:00:00Z"), sec("2026-10-14T11:00:00Z")...), false},
		{"times of day across midnight", []ClockRead{{At: time.Date(0, 1, 1, 23, 59, 59, 0, time.UTC), Res: time.Second, Day: true}}, []ClockRead{{At: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, true},
		{"times of day an hour apart", []ClockRead{{At: time.Date(0, 1, 1, 10, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, []ClockRead{{At: time.Date(0, 1, 1, 11, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, false},
		{"a time of day and a date", []ClockRead{{At: time.Date(0, 1, 1, 10, 0, 0, 0, time.UTC), Res: time.Second, Day: true}}, sec("0000-01-01T10:00:00Z"), false},
	} {
		if got := ReadInOneSecond(c.a, c.b); got != c.want {
			t.Errorf("%s: ReadInOneSecond %v, want %v", c.name, got, c.want)
		}
	}
}

// A variable's clock reads are stored with its mock, in YAML and JSON, and
// read back as they were.
func TestAVariablesClockReadsRoundTrip(t *testing.T) {
	in := MockVariables{"at": {Recorded: "r", Bind: "body.at", Clock: []ClockRead{
		read(t, "2026-10-14T10:00:00.250Z", time.Millisecond),
		{At: time.Date(0, 1, 1, 10, 0, 41, 0, time.UTC), Res: time.Second, Day: true},
	}}}
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML MockVariables
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("%v in\n%s", err, y)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON MockVariables
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for format, got := range map[string]MockVariables{"yaml": fromYAML, "json": fromJSON} {
		g, w := got["at"].Clock, in["at"].Clock
		if len(g) != len(w) {
			t.Fatalf("%s: read back %v, want %v", format, g, w)
		}
		for i := range w {
			if !g[i].At.Equal(w[i].At) || g[i].Res != w[i].Res || g[i].Day != w[i].Day {
				t.Errorf("%s: read %d back as %+v, want %+v", format, i, g[i], w[i])
			}
		}
	}
}

// Words go with the bind they were learned at, as a clock read does: a's bind
// keeps a's, a bind taken from b takes b's, a variable both bind at one place
// holds both's slots (void when they disagree), and a side without words
// never strips them.
func TestMergeVariablesKeepsWordsWithTheirBind(t *testing.T) {
	w := func(slots ...WordSlot) *WordsEntry {
		return &WordsEntry{V: WordsVersion, Digest: "9f2c41d07a3be815", Layout: "4be1c09a5d3e7f21", Slots: slots}
	}
	other := &WordsEntry{V: WordsVersion, Digest: "9f2c41d07a3be815", Layout: "0000000000000000", Slots: []WordSlot{{Seg: 0}}}
	for _, c := range []struct {
		name string
		a, b MockVariable
		want *WordsEntry
	}{
		{"a's bind keeps a's", MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 1})}, MockVariable{Recorded: "r", Use: []string{"url.query.id"}}, w(WordSlot{Seg: 1})},
		{"a bind taken from b takes b's", MockVariable{Recorded: "r", Use: []string{"url.query.id"}}, MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 2})}, w(WordSlot{Seg: 2})},
		{"both bind: the union", MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 1})}, MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 2})}, w(WordSlot{Seg: 1}, WordSlot{Seg: 2})},
		{"b has none: a's stay", MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 1})}, MockVariable{Recorded: "r", Bind: "body.id"}, w(WordSlot{Seg: 1})},
		{"a has none: b's are taken", MockVariable{Recorded: "r", Bind: "body.id"}, MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 2})}, w(WordSlot{Seg: 2})},
		{"both bind, two layouts: void", MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 1})}, MockVariable{Recorded: "r", Bind: "body.id", Words: other}, &WordsEntry{V: WordsVersion, Digest: "9f2c41d07a3be815", Layout: WordsVoidLayout, Slots: []WordSlot{}}},
		{"another bind of b's is not a's", MockVariable{Recorded: "r", Bind: "body.id", Words: w(WordSlot{Seg: 1})}, MockVariable{Recorded: "r", Bind: "header.x-id", Words: w(WordSlot{Seg: 2})}, w(WordSlot{Seg: 1})},
	} {
		in := MockVariables{"id": c.a}
		got := MergeVariables(in, MockVariables{"id": c.b})["id"]
		if !reflect.DeepEqual(got.Words, c.want) {
			t.Errorf("%s: merged words %+v, want %+v", c.name, got.Words, c.want)
			continue
		}
		if got.Words != nil && len(got.Words.Slots) > 0 {
			got.Words.Slots[0].Seg = 99
			for _, side := range []MockVariable{c.a, c.b} {
				if side.Words != nil && len(side.Words.Slots) > 0 && side.Words.Slots[0].Seg == 99 {
					t.Errorf("%s: the merged block shares its words with an input", c.name)
				}
			}
		}
		// MergeBinds merges as MergeVariables does.
		bound := MergeBinds(map[string]MockVariables{"mock-1": {"id": c.a}}, map[string]MockVariables{"mock-1": {"id": c.b}})
		if c.want != nil && c.want.Void() != bound["mock-1"]["id"].Words.Void() {
			t.Errorf("%s: MergeBinds merged words %+v", c.name, bound["mock-1"]["id"].Words)
		}
	}
}
