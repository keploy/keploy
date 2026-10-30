package learn

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// stampedAnswers are n HTTP answers recorded a second apart before at, each
// with a Date stamp, a word, a number and an instant of its own.
func stampedAnswers(n int, at time.Time) []*models.Mock {
	out := make([]*models.Mock, n)
	for i := range out {
		m := getAnswer(fmt.Sprintf("mock-%d", i), "http://api/items/"+fmt.Sprint(i), fmt.Sprintf(`{"note":"w%d","n":%d,"made":"2026-10-%02dT08:%02d:00Z"}`, i, 100+i, 10+i, i), at.Add(-time.Duration(n-i)*time.Second))
		m.Spec.HTTPResp.Header["Date"] = fmt.Sprintf("Thu, 22 Oct 2026 08:0%d:00 GMT", i)
		if i == 4 {
			// An instant its field holds only once unescaped (textForms).
			m.Spec.HTTPResp.Body = `{"note":"w4","seen":"2026%2D10%2D14T09%3A04%3A00Z"}`
		}
		out[i] = m
	}
	return out
}

// momentOf is the instant text names, as a payload's keyed instant.
func momentOf(t *testing.T, text string) moment {
	t.Helper()
	for _, sp := range instantSpans(text) {
		if sp.keyed {
			return sp.moment()
		}
	}
	t.Fatalf("%q names no instant", text)
	return moment{}
}

// The readers of an earlier recording's payload that every call runs over
// every earlier place (the stamp rule's stamps, stampsEchoed; the offset
// and word rules' words and instants of another test's answer,
// answerWords and answerMoments; the echo's field instants, echoRaw, and
// what F saw of them, heldAt) read the payload itself on each call, so
// with the set's store of payloads over its budget each call loaded every
// earlier payload again (with none kept, 73 loads in the second test of
// this row). They now read what they derive once for the set, by the
// recording (setDerived): with no payload kept, a later test of the set
// loads none, and loads one only to name the field where the echo finds
// its source; each learns what a cache of no tracker learns.
func TestEveryReaderOfAnEarlierPayloadReadsItOnceForTheSet(t *testing.T) {
	defer func(b int64) { bodyBudget = b }(bodyBudget)
	bodyBudget = 0
	at := eRecAt
	const n = 6
	mocks := stampedAnswers(n, at)
	const word, num = "Req.query.bind[0]", "Req.query.bind[1]"
	rec := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "a1b2c3d4"}, num: {Path: num, Type: Number, Value: "7041"}}}
	live := Shape{Leaves: map[string]Leaf{word: {Path: word, Type: String, Value: "z9y8x7w6"}, num: {Path: num, Type: Number, Value: "7093"}}}
	// Half served in the test, half another recorded test's answers.
	place := func(i int, x Raw) Raw {
		if i%2 == 0 {
			return x.ServedAs(i + 1)
		}
		return x.OfAnotherTest()
	}
	far := momentOf(t, "2030-01-01T00:00:00Z")
	made := func(i int) moment { return momentOf(t, fmt.Sprintf("2026-10-%02dT08:%02d:00Z", 10+i, i)) }
	// The echo's source in mock-2's field, as recorded (echoRaw) and as F
	// saw it (heldAt), and a moment no place holds.
	recIn2 := []echoMatch{{rec: made(2), live: far}}
	liveIn3 := []echoMatch{{rec: far, live: made(3)}}
	liveIn4 := []echoMatch{{rec: far, live: momentOf(t, "2026-10-14T09:04:00Z")}}
	none := []echoMatch{{rec: far, live: momentOf(t, "2031-01-01T00:00:00Z")}}
	type result struct {
		follow, given   []Learned
		echo, echoNone  Place
		held, heldNone  string
		heldUnescaped   string
		late            any
		words, moments  any
		echoOk, echoNOk bool
	}
	run := func(dc *DriftCache, raw []Raw) result {
		var r result
		r.follow = ClassifyFollowing(dc, []string{word, num}, live, rec, at, nil, raw, nil, nil, nil)
		r.given = ClassifyGiven(dc, []string{word, num}, live, rec, at, nil, raw, nil, nil, nil)
		r.echoNone, r.echoNOk = dc.echoRaw(raw[1], none)
		r.heldNone = dc.heldAt(none, at, nil, raw)
		return r
	}
	hits := func(dc *DriftCache, raw []Raw, r *result) {
		r.echo, r.echoOk = dc.echoRaw(raw[2], recIn2)
		r.held = dc.heldAt(liveIn3, at, nil, raw)
		r.heldUnescaped = dc.heldAt(liveIn4, at, nil, raw)
		r.words, r.moments = dc.wordsOf(raw[3]), dc.answerMoments(raw[3])
		// What the call says of when another test's answer was recorded
		// is the call's, not the set's (timedRaw, GivenEarlier's late).
		y := raw[3]
		y.at, y.late, y.noTime = at, true, true
		r.late = dc.answerMoments(y)
	}
	eager := make([]Raw, n)
	for i, m := range mocks {
		eager[i] = place(i, RawOf(m, i%2 == 0))
	}
	want := run(noSet(clockedDrifts()), eager)
	hits(noSet(clockedDrifts()), eager, &want)
	if !want.echoOk || want.held == "" || want.echoNOk || want.heldNone != "" || !strings.Contains(want.heldUnescaped, "mock-4 at ") {
		t.Fatalf("the row's echo finds %v (%v), %q and %q, none %v and %q: it pins nothing", want.echoOk, want.echo, want.held, want.heldUnescaped, want.echoNOk, want.heldNone)
	}

	tr := NewTracker()
	tr.NewSet()
	for test := 1; test <= 2; test++ {
		tr.Begin()
		dc := tr.Drifts()
		dc.appClock = true
		raw := make([]Raw, n)
		for i, m := range mocks {
			raw[i] = place(i, dc.LazyRawOf(m, i%2 == 0, nil))
		}
		before := dc.Loads()
		got := run(dc, raw)
		loads := dc.Loads() - before
		hits(dc, raw, &got)
		hitLoads := dc.Loads() - before - loads
		if !reflect.DeepEqual(got, want) {
			t.Errorf("test %d learned %+v, want %+v (a cache of no tracker's)", test, got, want)
		}
		switch {
		case test == 1 && loads < n:
			t.Fatalf("the first test loaded %d of the %d earlier payloads: the row reads too few to tell", loads, n)
		case test == 2 && loads != 0:
			t.Errorf("FALSE COST: with no payload kept, the second test's calls loaded %d earlier payloads again, want 0", loads)
		case test == 2 && hitLoads != 3:
			t.Errorf("naming the echo's three places loaded %d payloads, want 3 (one each)", hitLoads)
		}
		tr.End()
	}
}
