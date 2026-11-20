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
// It is refused now; told how F served the exchange, as recorded or with a
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
		ds := Decide(mk())
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
// as the second's decision of its field whole. A word of the second that
// the first decides itself is read only against that decision, never
// against a whole one it stands under. A refusal no decision of the first
// stands over is added, never dropped; a word the second learns otherwise
// refuses the first's (keploy cannot tell which the application did); the
// worst of several decides; and where every word learns as the first did,
// nothing changes.
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
		// The second reading's first decision is the refusal no decision of
		// the first reading stands over: a lookup that finds nothing reads
		// no decision of the second, so it is still added.
		{"a refused word of another field, decided first", []Decision{whole(OutcomeFresh)}, []string{f},
			[]Decision{bgDecision(OutcomeRefused, "g0", 0), bgDecision(OutcomeFresh, "w0", 0)}, []string{g, f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a refused word the first reading did not decide", []Decision{bgDecision(OutcomeFresh, "w0", 0)}, []string{f},
			[]Decision{bgDecision(OutcomeFresh, "w0", 0), bgDecision(OutcomeRefused, "w3", 3)}, []string{f, f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a refused whole under a word", []Decision{bgDecision(OutcomeFresh, "w0", 0)}, []string{f},
			[]Decision{whole(OutcomeRefused)}, []string{f},
			[]WordOutcome{OutcomeRefused}, "probe"},
		// A minted id: made anew whole, its copied piece bound beside it,
		// decided so by both readings. The piece is the first's own
		// decision, so it never refuses the whole.
		{"a whole and a piece under it, each decided alike", []Decision{whole(OutcomeFresh), bgDecision(OutcomeValue, "w0", 0)}, []string{f, f},
			[]Decision{whole(OutcomeFresh), bgDecision(OutcomeValue, "w0", 0)}, []string{f, f},
			[]WordOutcome{OutcomeFresh, OutcomeValue}, ""},
		{"a whole and a piece under it, the second refusing the piece", []Decision{whole(OutcomeFresh), bgDecision(OutcomeValue, "w0", 0)}, []string{f, f},
			[]Decision{whole(OutcomeFresh), bgDecision(OutcomeRefused, "w0", 0)}, []string{f, f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "probe"},
		{"a whole and a piece under it, the second learning the piece otherwise", []Decision{whole(OutcomeFresh), bgDecision(OutcomeValue, "w0", 0)}, []string{f, f},
			[]Decision{whole(OutcomeFresh), bgDecision(OutcomeFresh, "w0", 0)}, []string{f, f},
			[]WordOutcome{OutcomeFresh, OutcomeRefused}, "own-instant-both-ways"},
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
		// An added refusal names the field of the second reading's
		// decision it carries.
		for i := len(c.fs); i < len(fs) && i < len(ds); i++ {
			for k, a := range c.as {
				if a.Recorded == ds[i].Recorded && fs[i] != c.afs[k] {
					t.Errorf("%s: the added refusal %s names field %q; want %q", c.name, oDescribe([]Decision{ds[i]}), fs[i], c.afs[k])
				}
			}
		}
	}
}

// A minted <copied>-<random> id whose copied prefix was bound to an
// earlier place (mock-48's body.tenant) is decided whole, made anew by the
// rule, with the piece bound beside it. Beside an exchange of the test that
// holds nothing of it, at the call's own instant or untimed, the call is
// read both ways, and both readings decide the field so. The piece is a
// word the first reading decides itself, so read against the whole
// decision too it refused the whole and lost the rule's name (a false
// red); it stays made anew, as with the exchange a second before.
func TestBothWaysKeepsAMintedIdBothReadingsDecideAlike(t *testing.T) {
	const tRec, tLiv = "t4821-7f3e9b2c41d6", "t9057-c0d18e5a77b2"
	form := models.MintedForm{MinLen: 18, MaxLen: 18}
	tenant := oCarried("mock-48", "body.tenant", "t4821", "t9057", 2*time.Second, time.Second, 1)
	atCall := oCarried("mock-49", "body.other", "qq", "qq", 0, time.Millisecond, 2)
	atCall.AsRecorded = true
	untimed := atCall
	untimed.At = time.Time{}
	before := oCarried("mock-49", "body.other", "qq", "qq", time.Second, time.Millisecond, 2)
	for _, c := range []struct {
		name   string
		beside Earlier
	}{
		{"an exchange at the call's instant", atCall},
		{"an untimed exchange", untimed},
		{"control: the exchange a second before (read one way)", before},
	} {
		in := oInput(mPath, tRec, tLiv, []Earlier{tenant, c.beside}, nil)
		in.Source = mMintedAs(mPath, form)
		mFreshBeside(t, "a minted <copied>-<random> beside "+c.name, Decide(in), tRec, tLiv, form, "t4821")
		if t.Failed() {
			first, second := bgReadings(in)
			t.Logf("%s: first reading %s; second reading %s", c.name, oDescribe(first), oDescribe(second))
		}
	}
}
