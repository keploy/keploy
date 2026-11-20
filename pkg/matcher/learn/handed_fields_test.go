package learn

import (
	"encoding/base64"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// handedFieldsFixture is n texts a caller hands (an exchange's leaves): a
// request's document, an id beside a word, a percent-encoded path and a
// base64 token, in turn, and the words a call asks of each: one it holds,
// one only its decoded form holds, one all hold and one none does.
func handedFieldsFixture(n int) (texts []string, words func(i int) []string) {
	for i := range n {
		var s string
		switch i % 4 {
		case 0:
			s = fmt.Sprintf(`{"company":"w%dq","tags":["shared","x"]}`, i)
		case 1:
			s = fmt.Sprintf("w%dq shared", i)
		case 2:
			s = fmt.Sprintf("w%dq%%20shared%%2Fcart_%d", i, i)
		default:
			s = "w" + fmt.Sprint(i) + "q shared " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cart_%d", i)))
		}
		texts = append(texts, s)
	}
	return texts, func(i int) []string {
		return []string{fmt.Sprintf("w%dq", i), fmt.Sprintf("cart_%d", i), "shared", "absent"}
	}
}

// askHanded is one call of the set: fieldsWith of every word at every
// handed text, through one index on dc, released at its end. got is what
// each answered, in order; used, what the call's index kept by its own
// count at its end.
func askHanded(dc *DriftCache, texts []string, words func(int) []string) (got [][]fieldAt, used int64) {
	x := newReaderIndex(dc)
	defer x.release()
	for i, s := range texts {
		t := x.text(s)
		for _, w := range words(i) {
			at, read := t.fieldsWith(w)
			if !read {
				at = append(at, fieldAt{path: "unread"})
			}
			got = append(got, at)
		}
	}
	return got, x.used
}

// A set builds the field view of each text a caller hands, once, in the
// first call that asks it a word, as it builds a recording's: every later
// call builds none of its own, so its work per earlier leaf is flat in the
// set's size (50 and 400 texts alike), and every answer is the one a call's
// own index gives with no set. An earlier exchange's leaves are handed again
// by each later call of the set; each call built a view of every one the
// decision showed (movedField), the largest part of a Mongo replay's
// decisions.
func TestASetBuildsEachHandedTextsFieldViewOnceAtAnySize(t *testing.T) {
	defer func(r int64) { readerBudget = r }(readerBudget)
	readerBudget = 0
	for _, n := range []int{50, 400} {
		dc := NewDriftCache()
		dc.set = newSetDerived()
		fs := filtersOf(dc.set)
		texts, words := handedFieldsFixture(n)
		want, _ := askHanded(noSetCache(), texts, words)
		if !slices.ContainsFunc(want, func(at []fieldAt) bool { return len(at) == 1 }) || !slices.ContainsFunc(want, func(at []fieldAt) bool { return len(at) == 0 }) {
			t.Fatalf("%d texts: fixture answers %v; want some words held and some not", n, want[:8])
		}
		builds := callFieldBuilds.Load()
		if got, used := askHanded(dc, texts, words); !slices.EqualFunc(got, want, slices.Equal) || used != 0 {
			t.Fatalf("%d texts, first call: answers as a call with no set %v, %d bytes the call's own; want the same answers, 0", n, slices.EqualFunc(got, want, slices.Equal), used)
		}
		if fs.fieldBuilds != n || len(fs.fieldTexts) != n || keptViews(fs) != n {
			t.Fatalf("%d texts, first call: the set built %d views, has %d, keeps %d; want %d each", n, fs.fieldBuilds, len(fs.fieldTexts), keptViews(fs), n)
		}
		for c := range 2 {
			if got, used := askHanded(dc, texts, words); !slices.EqualFunc(got, want, slices.Equal) || used != 0 {
				t.Errorf("%d texts, call %d: answers as a call with no set %v, %d bytes the call's own; want the same answers, 0", n, c+2, slices.EqualFunc(got, want, slices.Equal), used)
			}
		}
		if b := callFieldBuilds.Load() - builds; b != 0 || fs.fieldBuilds != n {
			t.Errorf("%d texts, three calls: their own indexes built %d views, the set %d; want 0, and the set's %d of the first call", n, b, fs.fieldBuilds, n)
		}
	}
}

// handedDecision decides a call over n earlier exchanges of its set, each a
// request whose leaf sent the call's recorded id in the recording and F's
// id in round F (an id every request of a run sends), twice in the set's
// test. It returns what each decided, the field views the first built in
// its own index or the set built while it ran, and those the second built in
// its own index and the set built while it ran. run: the exchanges are an
// earlier test's.
func handedDecision(n int, run bool) (first, second string, firstViews, callBuilds, setBuilds int64) {
	const rec, live = "a1b2c3d4", "f0e9d8c7"
	earlier := make([]Earlier, n)
	for i := range earlier {
		name, at := fmt.Sprintf("mock-%d", i), eRecAt.Add(-time.Duration(n-i)*time.Millisecond)
		m := &models.Mock{Name: name, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(100 * time.Microsecond)}}
		earlier[i] = Earlier{Mock: name, At: at, Own: !run, Run: run, Payload: RawOf(m, true).ServedAs(i + 1), Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.company", Noise: "body.company", Type: String, Value: fmt.Sprintf(`{"company":"%s","n":%d}`, rec, i)}, Live: fmt.Sprintf(`{"company":"%s","n":%d}`, live, i)},
		}}
	}
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	defer tr.End()
	dc := tr.Drifts()
	fs := filtersOf(dc.set)
	set := func() int64 {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return int64(fs.fieldBuilds)
	}
	b1, s1 := callFieldBuilds.Load(), set()
	first = oDescribe(Decide(costCall(dc, "body.company", rec, live, eRecAt, earlier, nil, nil, nil)))
	b2, s2 := callFieldBuilds.Load(), set()
	second = oDescribe(Decide(costCall(dc, "body.company", rec, live, eRecAt, earlier, nil, nil, nil)))
	return first, second, b2 - b1 + s2 - s1, callFieldBuilds.Load() - b2, set() - s2
}

// A call's decision builds no field view of an earlier exchange's leaves
// once its set has decided a call over them: over 30 and 300 exchanges that
// each sent the call's id, of its own test or an earlier one, the second
// call builds none in its own index and the set none more, and decides as
// the first. Each such leaf the decision showed was a view built per call
// (movedField), a call's work that grew with the set.
func TestADecisionBuildsNoFieldViewOfLeavesItsSetKept(t *testing.T) {
	for _, run := range []bool{false, true} {
		for _, n := range []int{30, 300} {
			first, second, firstViews, callBuilds, setBuilds := handedDecision(n, run)
			t.Logf("%d exchanges (an earlier test's: %v): decided %s, then %s; the first call's views %d; the second built %d of its own, the set %d", n, run, first, second, firstViews, callBuilds, setBuilds)
			if firstViews < int64(n) {
				t.Fatalf("fixture, %d exchanges (an earlier test's: %v): the first call built %d field views, its own or the set's; want one of each leaf the decision showed, %d or more", n, run, firstViews, n)
			}
			if second != first {
				t.Errorf("%d exchanges (an earlier test's: %v): the second call decides %q, the first %q; want one verdict", n, run, second, first)
			}
			if callBuilds != 0 || setBuilds != 0 {
				t.Errorf("%d exchanges (an earlier test's: %v): the second call built %d field views of its own and the set %d; want none", n, run, callBuilds, setBuilds)
			}
		}
	}
}
