package learn

import (
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The two readings of a call beside an exchange at its own instant
// (decideBoth) may decide one leaf at different grains. A random 256-bit
// key no place shows is one value made anew, decided whole; read as a
// recording made after the call, an exchange whose recorded answer holds
// the key's word makes the reader decide it word by word. A refusal of
// that word was looked up only at its own word, where the first reading
// had no decision, and was dropped: the call learned the key as made anew
// although F may have given the application that key there (FALSE GREEN).
// It is refused now, whether the caller reads every word or only whether
// one is refused; told how F served the exchange, as recorded or with a
// view that holds no word of the key, the key is still made anew.

const (
	bgKeyRec  = "pU3KGCUwux1tEyze1iN7LtkeP3IfyxlxF0SU1kk8nVw="
	bgKeyLive = "9fefK0k0r4f1UgtpuUsNmC6Fu1W2cqhyY3rNdGb8tg4="
)

// bgKeyCall is a call that sends bgKeyRec (bgKeyLive in round F) at
// bind.$7, beside m, an exchange of its test at the call's own instant
// whose recorded answer holds id, told to keploy as tell says.
func bgKeyCall(id string, tell func(tr *Tracker, m *models.Mock)) Input {
	m := jobsMock(id, "5")
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = eRecAt, eRecAt.Add(time.Millisecond)
	tr := NewTracker()
	tr.Begin()
	tr.ServedMock(m)
	tr.Served(tr.Placed().Test, Earlier{Mock: m.Name, At: m.Spec.ReqTimestampMock, Own: true, Payload: RawOf(m, true),
		Seen: []Seen{{Side: Response, Leaf: Leaf{Path: "row.0.n", Type: String, Value: "5"}, Live: "5"}}})
	tell(tr, m)
	pl := tr.Placed()
	const path = "bind.$7"
	return Input{Drifts: clockedDrifts(), Drift: []string{path},
		Live: Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: bgKeyLive}}},
		Rec:  Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: bgKeyRec}}},
		At:   eRecAt, Arrived: eRecAt, Earlier: InServeOrder(pl.Earlier, []*models.Mock{m}, pl.Order), Raw: eStartup(),
		Test: "test-2", Mock: "mock-300", Caps: Caps{BindAtWord: true, AnswerAtWord: true, FViewStated: true}}
}

// bgReadings is in's two readings as decideBoth reads them, each with
// every word decided: the exchanges at the call's own instant as earlier
// places of its test, then as recordings made after the call.
func bgReadings(in Input) (first, second []Decision) {
	in.Earlier, in.Raw = timedEarlier(arrivedBefore(ofOwnCall(in, in.Earlier), in.At, in.Arrived), in.At), timedRaw(in.Raw, in.At)
	in.Earlier, in.Raw = leafedAfterCall(in.Earlier, in.Raw, in.At)
	first, _ = decideWords(in, false)
	kept, late := leafedAtCall(in.Earlier, in.At)
	in.Earlier, in.Raw = kept, append(append([]Raw(nil), in.Raw...), late...)
	second, _ = decideWords(in, false)
	return first, second
}

func TestOutcomeAWordTheSecondReadingRefusesInALeafTheFirstDecidesWholeIsRefused(t *testing.T) {
	view := func(id string) func(tr *Tracker, m *models.Mock) {
		return func(tr *Tracker, m *models.Mock) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) {
				v := jobsMock(id, "9")
				v.Spec.ReqTimestampMock, v.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
				return v, nil
			})
		}
	}
	for _, c := range []struct {
		name    string
		tell    func(tr *Tracker, m *models.Mock)
		refused bool
	}{
		{"told nothing", func(*Tracker, *models.Mock) {}, true},
		{"told as recorded", func(tr *Tracker, m *models.Mock) { tr.ServedMockAsRecorded(m) }, false},
		{"told a view with no word of the key", view(chanceSrcRec), false},
		{"told a view that holds F's key", view(bgKeyLive), true},
		{"told a view keploy cannot read", func(tr *Tracker, m *models.Mock) {
			tr.ServedMockLive(m, "a", func() (*models.Mock, error) { return nil, errors.New("gone") })
		}, true},
	} {
		mk := func() Input { return bgKeyCall(bgKeyRec, c.tell) }
		// The grains this row is about: the first reading decides the key
		// whole and made anew, the second its word.
		first, second := bgReadings(mk())
		if len(first) != 1 || first[0].At.Word != -1 || first[0].Outcome != OutcomeFresh || len(second) != 1 || second[0].At.Word != 0 {
			t.Fatalf("%s: the first reading %s, the second %s; want the key whole made anew, then its word 0", c.name, oDescribe(first), oDescribe(second))
		}
		if c.refused != (second[0].Outcome == OutcomeRefused) {
			t.Fatalf("%s: the second reading %s; want refused %v", c.name, oDescribe(second), c.refused)
		}
		full, only := roPair(t, "a key made anew beside an exchange at the call's own instant that holds its word, "+c.name, mk)
		for _, ds := range [][]Decision{full, only} {
			d := oWord(t, ds, bgKeyRec, bgKeyLive)
			if !c.refused {
				if d.Outcome != OutcomeFresh || len(ds) != 1 {
					t.Errorf("FALSE RED: %s: %s; want the key made anew", c.name, oDescribe(ds))
				}
				continue
			}
			if oRefused(t, c.name, d, RefusalUnknown, "mock-6"); d.Refusal == nil {
				continue
			}
			if d.At.Word != -1 || len(ds) != 1 || !strings.Contains(d.Refusal.Why, "either reading refuses") {
				t.Errorf("%s: %s; want the key refused whole, once, saying either reading refuses it", c.name, oDescribe(ds))
			}
		}
	}
	// The exchange's answer holds another value: the second reading reads
	// the key whole too, and refused it as before.
	in := bgKeyCall(chanceSrcRec, func(*Tracker, *models.Mock) {})
	if _, second := bgReadings(in); len(second) != 1 || second[0].At.Word != -1 {
		t.Errorf("an exchange that holds another value: the second reading %s; want the key whole", oDescribe(second))
	}
	oRefused(t, "an exchange that holds another value", oWord(t, Decide(in), bgKeyRec, bgKeyLive), RefusalUnknown, "mock-6")
}

// bgDecision is a decision at word w of the steps st: refused (with a
// reason saying why) where o is OutcomeRefused.
func bgDecision(o WordOutcome, why string, w int, st ...variables.Step) Decision {
	d := Decision{At: variables.Address{Steps: st, Word: w}, Recorded: "r" + why, Full: "f" + why, Outcome: o}
	if o == OutcomeRefused {
		d.Refusal = &Refusal{Kind: RefusalUnknown, Check: "probe", Why: why, Word: w}
	}
	return d
}

// bothWays reads each decision of the second reading against the first
// reading's decision it stands under: at its own word, under the first's
// decision of its field whole (inside the text that one's steps open), or
// as the second's decision of its field whole. A refusal no decision of
// the first stands over is added, never dropped; a word the second learns
// otherwise refuses the first's; the worst of several decides; and where
// every word learns as the first did, nothing changes.
func TestBothWaysReadsEveryWordOfTheSecondReading(t *testing.T) {
	const f, g = "bind.$7", "bind.$8"
	json, b64 := variables.Step("json"), variables.Step("b64")
	whole := func(o WordOutcome, st ...variables.Step) Decision { return bgDecision(o, "whole", -1, st...) }
	for _, c := range []struct {
		name string
		ds   []Decision
		fs   []string
		as   []Decision
		afs  []string
		// want: each decision's outcome after (the first's, then any
		// added), and the check of the one refused where one is.
		want  []WordOutcome
		check string
	}{
		{"a refused word under a whole decision", []Decision{whole(OutcomeFresh)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeRefused, "w1", 1)}, []string{f, f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		{"a refused word before one learned otherwise under a whole decision", []Decision{whole(OutcomeValue)}, []string{f},
			[]Decision{bgDecision(OutcomeRefused, "w0", 0), bgDecision(OutcomeFresh, "w1", 1)}, []string{f, f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		{"a word learned otherwise before a refused one under a whole decision", []Decision{whole(OutcomeValue)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeRefused, "w1", 1)}, []string{f, f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		{"a word learned otherwise under a whole decision", []Decision{whole(OutcomeValue)}, []string{f},
			[]Decision{bgDecision(OutcomeValue, "w0", 0), bgDecision(OutcomeFresh, "w1", 1)}, []string{f, f},
			[]WordOutcome{OutcomeRefused}, "own-instant-both-ways"},
		{"every word learned as the whole", []Decision{whole(OutcomeFresh)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeFresh, "w1", 1)}, []string{f, f},
			[]WordOutcome{OutcomeFresh}, ""},
		{"a refused word inside the text the whole decision's steps open", []Decision{whole(OutcomeFresh, json)}, []string{f},
			[]Decision{bgDecision(OutcomeRefused, "w0", 0, json, b64)}, []string{f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		{"a refused word outside the text the whole decision's steps open", []Decision{whole(OutcomeFresh, json)}, []string{f},
			[]Decision{bgDecision(OutcomeRefused, "w0", 0, b64)}, []string{f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a refused word of another field", []Decision{whole(OutcomeFresh)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeRefused, "g0", 0)}, []string{f, g},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a refused word the first reading did not decide", []Decision{bgDecision(OutcomeFresh, "w0", 0)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeRefused, "w3", 3)}, []string{f, f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a refused whole under a word", []Decision{bgDecision(OutcomeFresh, "w0", 0)}, []string{f},
			[]Decision{whole(OutcomeRefused)}, []string{f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		{"a word under two whole decisions", []Decision{whole(OutcomeRefused), whole(OutcomeFresh)}, []string{f, f},
			[]Decision{bgDecision(OutcomeRefused, "w0", 0)}, []string{f},
			[]WordOutcome{OutcomeRefused, OutcomeRefused}, "probe"},
	} {
		ds, fs := bothWays(Input{}, c.ds, c.fs, c.as, c.afs)
		got := make([]WordOutcome, len(ds))
		for i, d := range ds {
			got[i] = d.Outcome
		}
		if len(fs) != len(ds) || len(got) != len(c.want) {
			t.Errorf("%s: %s (fields %v); want outcomes %v", c.name, oDescribe(ds), fs, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %s; want outcomes %v", c.name, oDescribe(ds), c.want)
				break
			}
		}
		for i, d := range ds {
			if d.Outcome != OutcomeRefused || i < len(c.ds) && c.ds[i].Outcome == OutcomeRefused {
				continue
			}
			if d.Refusal == nil || d.Refusal.Check != c.check || !strings.Contains(d.Refusal.Why, "either reading refuses") && c.check == "probe" {
				t.Errorf("%s: decision %d %s; want refused by %q, saying either reading refuses it", c.name, i, oDescribe([]Decision{d}), c.check)
			}
		}
		if len(fs) > len(c.fs) && fs[len(fs)-1] != c.afs[len(c.afs)-1] {
			t.Errorf("%s: the added refusal's field %q; want %q", c.name, fs[len(fs)-1], c.afs[len(c.afs)-1])
		}
	}
}
