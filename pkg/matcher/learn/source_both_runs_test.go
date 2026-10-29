package learn

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Under the rule a copy follows its source (record's full round), an earlier
// place is a later value's source only where it held that value in both
// runs. A leaf that held the recorded value whole but that F saw hold
// another value than the one F saw at the value's place is no place it came
// from: the search goes on past it, and the value is read as any value with
// no source is (follows.refuteSeen). A read of the clock the lead moved (epoch
// seconds, one second apart from the leaf in F) is then learned as one, not
// as a variable of the leaf; a value that is no read stays refused by the
// word rule (it lost the leaf's recorded word with it); and an echo of the
// call's answer is refused, naming the place set aside, as is a moment of
// the test case's request keploy test sent another value at (requestHolds).
// A read is checked against its call's clock at every replay, so no order of
// its sources is needed; a value left noise (a read inside a longer value)
// is noise only where the order of its candidate sources is shown, and
// refused, naming the leaf set aside, where it is not (a leaf whose recorded
// time keploy cannot read, or that the recording answered at the call's own
// instant: followed). Where F saw the leaf hold the value F saw (a copy), and under the rule's
// absence (ClassifyGiven), the place stays the value's source.
func TestALeafIsAValuesSourceOnlyWhereItHeldTheValueInBothRuns(t *testing.T) {
	before := eRecAt.Add(-50 * time.Millisecond)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec, live := secs(eRecAt), secs(eRecAt.Add(FullRoundLead))
	other := secs(eRecAt.Add(FullRoundLead - time.Second))
	const setAside = "its recorded value stands whole in mock-8 at body.retries, where record's full round gave the application another value than the value's: a copy moves with its source, so the value is no copy of it"
	const read, refused = Kind("a read"), KindUnaccounted
	for _, tc := range []struct {
		name           string
		recorded, live string
		earlier        []Earlier
		resp           []Leaf
		want           Kind
		why            string // a part of the reason
	}{
		{"a read of the clock recorded whole at a leaf F saw hold another value", rec, live, []Earlier{eLeaf(before, 6, rec, other)}, nil, read, ""},
		{"a read of the clock recorded whole at a leaf F saw hold the value F saw", rec, live, []Earlier{eLeaf(before, 6, rec, live)}, nil, KindVariable, ""},
		{"a value recorded whole at a leaf F saw hold another value", "82", "5", []Earlier{eLeaf(before, 6, "82", "83")}, nil, refused, "record's full round gave the application another value there, and the value drifted with it"},
		{"a value recorded whole at a leaf F saw hold the value F saw", "82", "83", []Earlier{eLeaf(before, 6, "82", "83")}, nil, KindVariable, ""},
		{"a leaf whose recorded time keploy cannot read", rec, live, []Earlier{eLeaf(time.Time{}, 6, rec, other)}, nil, read, ""},
		{"a leaf recorded at the call's own instant", rec, live, []Earlier{eLeaf(eRecAt, 6, rec, other)}, nil, read, ""},
		// A read inside a longer value is learned as noise (its words are
		// a read's, which the word rule lets go): noise only where the
		// order of its candidate sources, the leaf set aside among them,
		// is shown, and named so.
		{"a read inside a longer value recorded whole at a leaf F saw hold another value", "at " + rec, "at " + live, []Earlier{eLeaf(before, 6, "at "+rec, "at "+other)}, nil, KindNoise, setAside + ", and keploy learns it as varying"},
		{"a read inside a longer value, at a leaf whose recorded time keploy cannot read", "at " + rec, "at " + live, []Earlier{eLeaf(time.Time{}, 6, "at "+rec, "at "+other)}, nil, refused, setAside + "; keploy cannot read when the recording gave the application mock-8"},
		{"a read inside a longer value, at a leaf recorded at the call's own instant", "at " + rec, "at " + live, []Earlier{eLeaf(eRecAt, 6, "at "+rec, "at "+other)}, nil, refused, setAside + "; the recording and record's full round gave the application the earlier answers in a different order"},
		{"a read inside a longer value, at a leaf F saw hold the value F saw", "at " + rec, "at " + live, []Earlier{eLeaf(before, 6, "at "+rec, "at "+live)}, nil, KindVariable, ""},
		{"the call's recorded answer holds the recorded value", rec, live, []Earlier{eLeaf(before, 6, rec, other)}, []Leaf{{Path: "body.n", Noise: "body.n", Type: Number, Value: rec}}, refused, setAside + "; but the call's recorded answer holds it"},
		{"the test case's own request, sent another value", rec, live, []Earlier{{Mock: givenPlace("test-1"), Given: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.retries", Type: Number, Value: rec}, Live: other},
		}}}, nil, refused, "the moment it was recorded with stands in test case test-1 at body.retries"},
		{"a request an earlier call sent another value at", rec, live, []Earlier{{Mock: "mock-8", At: before, Own: true, Payload: eLeaf(before, 6, "", "").Payload, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.retries", Type: Number, Value: rec}, Live: other},
		}}}, nil, read, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyF(t, tc.recorded, tc.live, tc.earlier, eStartup(), tc.resp)
			switch {
			case tc.want == read:
				if !noiseOrRead(got) {
					t.Fatalf("%q recorded, %q in F: %s (%q) from %+v; want a read of the clock", tc.recorded, tc.live, got.Kind, got.Why, got.From)
				}
			case got.Kind != tc.want || !strings.Contains(got.Why, tc.why):
				t.Fatalf("%q recorded, %q in F: %s (%q) from %+v; want %s, its reason holding %q", tc.recorded, tc.live, got.Kind, got.Why, got.From, tc.want, tc.why)
			case tc.want == KindVariable && got.From.Mock != "mock-8":
				t.Fatalf("%q recorded, %q in F: a variable of %+v; want one of mock-8", tc.recorded, tc.live, got.From)
			}
		})
	}
	// An instant its protocol types (a BSON datetime an earlier answer gave)
	// that the application copied into a text: one moment, which the typed
	// leaf writes to the millisecond and the application's text without
	// trailing zeros, so the two texts differ in F though the moment is
	// one. The leaf is read by its moments (dc.instantSource), never set
	// aside by its text: the value stays a variable bound whole at it (set
	// aside, the echo of a pick would bind it there instead, naming the
	// moments).
	ms := eRecAt.Truncate(time.Millisecond)
	fAt := time.Date(2026, 10, 24, 9, 56, 55, 400_000_000, time.UTC)
	typed := Earlier{Mock: "mock-8", At: before, Payload: eLeaf(before, 6, "", "").Payload, Seen: []Seen{
		{Side: Response, Leaf: InstantLeaf("body.at", "body.at", ms, time.Millisecond), Live: InstantLeaf("body.at", "body.at", fAt, time.Millisecond).Value},
	}}
	if typed.Seen[0].Leaf.Value != ms.Format(time.RFC3339Nano) || typed.Seen[0].Live == fAt.Format(time.RFC3339Nano) {
		t.Fatalf("the typed leaf writes %q, then %q: the row needs the recorded texts alike and F's apart", typed.Seen[0].Leaf.Value, typed.Seen[0].Live)
	}
	if got := classifyF(t, ms.Format(time.RFC3339Nano), fAt.Format(time.RFC3339Nano), []Earlier{typed}, eStartup(), nil); got.Kind != KindVariable || got.From.Mock != "mock-8" || !got.From.Whole || got.From.Echo {
		t.Fatalf("a copy of a typed instant, written without trailing zeros in F: %s (%q) from %+v; want a variable bound whole at mock-8, by its moments, not by the echo of a pick", got.Kind, got.Why, got.From)
	}
	// The rule's absence (ClassifyGiven): the leaf stays the value's
	// source.
	const path = "bind.$8"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	got := ClassifyGiven(clockedDrifts(), []string{path}, str(live), str(rec), eRecAt, []Earlier{eLeaf(before, 6, rec, other)}, eStartup(), nil, nil, nil)
	if len(got) != 1 || got[0].Kind != KindVariable || got[0].From.Mock != "mock-8" {
		t.Fatalf("ClassifyGiven, a read recorded whole at a leaf F saw hold another value: %+v; want a variable of mock-8 (the rule is ClassifyFollowing's)", got)
	}
}

// api-server's audit in keploy mock's client tests: the app read its clock
// for an app's createdAt and sent it in the app's insert, then read its
// clock again for the audit's auditedAt. The recording made both reads in
// one second, so auditedAt was recorded with the insert's createdAt whole;
// record's full round read them a second apart. auditedAt is a read of the
// clock in its call's window, learned and checked as one, not a copy of the
// insert's createdAt, which F saw hold another value. A copy of it (F's
// auditedAt the insert's createdAt), which its call's window holds as a read
// too, is a read since its test began (both explanations hold:
// TestAClientTestLearnsACopyOfAnEarlierReadItsOwnWindowHoldsAsAReadSinceItsTestBegan);
// a value no read in its call's window gives (an hour past it, the epoch) is
// refused; the rule's absence (ClassifyGiven) keeps the insert its source.
func TestAClientTestsClockReadRecordedWithAnEarlierStampIsNoCopyOfItWhereTheFullRoundReadThemApart(t *testing.T) {
	const p = "body.cmd.documents.0.auditedAt"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	r0 := time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC)
	f0 := r0.Add(8500 * time.Millisecond)
	recCreated, fCreated := secs(r0), secs(f0.Add(100*time.Millisecond))
	fAudited := secs(f0.Add(300 * time.Millisecond))
	if fCreated == fAudited {
		t.Fatalf("the full round's two reads fall in one second (%s)", fCreated)
	}
	insert := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: r0.Add(2 * time.Millisecond), ResTimestampMock: r0.Add(3 * time.Millisecond)}}, true)
	earlier := []Earlier{{Mock: "mock-app", At: r0.Add(2 * time.Millisecond), Own: true, Payload: insert.ServedAs(1), Seen: []Seen{
		{Side: Request, Leaf: Leaf{Path: "body.cmd.documents.0.createdAt", Type: Number, Value: recCreated}, Live: fCreated},
	}}}
	w := &Window{
		Rec: Span{From: r0.Add(2 * time.Millisecond), To: r0.Add(4 * time.Millisecond), Began: r0.Add(-time.Second)},
		F:   Span{From: f0.Add(150 * time.Millisecond), To: f0.Add(400 * time.Millisecond), Began: f0.Add(-time.Second)},
	}
	num := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	at := r0.Add(4 * time.Millisecond)
	for _, tc := range []struct {
		name  string
		live  string
		want  Kind
		since string
	}{
		{"auditedAt read in its call's window, a second past the insert's createdAt", fAudited, KindClockRead, ""},
		{"auditedAt the insert's createdAt in both runs (a copy its call's window holds)", fCreated, KindClockRead, models.SinceTestBegin},
		{"auditedAt an hour past its call's window", secs(f0.Add(time.Hour)), KindUnaccounted, ""},
		{"auditedAt at the epoch", "0", KindUnaccounted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln := ClassifyFollowing(clientTestCache(), []string{p}, num(tc.live), num(recCreated), at, earlier, nil, nil, w, nil)[0]
			if ln.Kind != tc.want {
				t.Fatalf("%s recorded, %s in F (the insert's createdAt %s in F): %s Read=%+v from %+v (%s); want %s", recCreated, tc.live, fCreated, ln.Kind, ln.Read, ln.From, ln.Why, tc.want)
			}
			switch tc.want {
			case KindClockRead:
				if o, ok := ln.Clock(); !ok || !o.Plain() || o.Since != tc.since {
					t.Fatalf("Read=%+v; want a plain read checked since %q", ln.Read, tc.since)
				}
			}
		})
	}
	ln := ClassifyGiven(clientTestCache(), []string{p}, num(fAudited), num(recCreated), at, earlier, nil, nil, w, nil)[0]
	if ln.Kind != KindVariable || ln.From.Mock != "mock-app" {
		t.Fatalf("ClassifyGiven: %s from %+v (%s); want a variable of mock-app (the rule is ClassifyFollowing's)", ln.Kind, ln.From, ln.Why)
	}
}

// A value found in a recording made before the call is followed as a value
// from there: a word of it, the value whole (an id, a moment's epoch
// milliseconds or its text, a read of the clock the lead moved, as it stands
// or as its escaped text decodes), a moment its protocol types, or a moment
// the echo takes for a pick from what the application was given (another
// recording holds the moment F saw), in a field or in what an encoded
// payload decodes to. A word the value holds only as F saw it, found in such
// a recording, is refused naming it. Where record's full round never served
// that recording before the call (its own call missed, was answered from
// another recording, or was not made), the reason says so, never that keploy
// cannot follow the value there yet; where it served it, the reason stands
// as before. Both the classifier keploy test uses and the one keploy mock's
// client tests use read so.
func TestAReasonSaysTheFullRoundNeverServedTheValuesSource(t *testing.T) {
	const p = "body.v"
	const id = "bb7ed9ee-670e-48d1-982f-b54b2b73982f"
	const never = "otherwise it flowed from mock-31, its source recording, which record's full round never served before this call (its own call missed earlier, was answered from another recording, or was not made)"
	const yet = "otherwise it flowed from mock-31, where keploy cannot follow it yet"
	ms := time.Millisecond
	day := r1Insert.Add(-24 * time.Hour).Truncate(ms)
	// A moment the application picks from what it was given: recorded
	// with day, F saw got, which another recording (mock-11) holds.
	got := day.Add(2*time.Minute + 311*ms)
	millis := func(at time.Time) string { return strconv.FormatInt(at.UnixMilli(), 10) }
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	escaped := func(at time.Time) string { return url.QueryEscape(at.Format(time.RFC3339)) }
	const echo = "the moment it was recorded with stands in mock-31%s, and the moment record's full round saw in it stands earlier in the test too; "
	for _, tc := range []struct {
		name      string
		body      string // what the recording's message holds
		also      string // what mock-11, served before the call too, holds ("": no mock-11)
		rec, live Shape
		found     string // how the reason begins
		kind      Kind   // what the value is learned as ("": a variable of mock-31)
	}{
		{"an id whole", "S12 message", "", strLeaf(p, id), strLeaf(p, "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5"), "its recorded value stands whole in mock-31 at spec.resp.body; ", ""},
		{"a word of a longer value", "S12 message", "", strLeaf(p, "order 670e of"), strLeaf(p, "order 9a9a of"), "a part of its recorded value (its characters 7 to 10) stands in mock-31 at spec.resp.body; ", ""},
		{"a moment's epoch milliseconds whole", "at " + millis(day), "", strLeaf(p, millis(day)), strLeaf(p, millis(day.Add(time.Hour))), "its recorded value stands whole in mock-31 at spec.resp.body; ", ""},
		{"a moment's text whole", "at " + day.Format(time.RFC3339Nano), "", strLeaf(p, day.Format(time.RFC3339Nano)), strLeaf(p, day.Add(time.Hour).Format(time.RFC3339Nano)), "its recorded value stands whole in mock-31 at spec.resp.body; ", ""},
		{"a typed moment", "at " + millis(day), "", typedLeaf(p, day, ms), typedLeaf(p, day.Add(time.Hour), ms), "the moment it was recorded with stands in mock-31 at spec.resp.body, ", ""},
		{"a read of the clock the lead moved, whole", "at " + secs(day), "", strLeaf(p, secs(day)), strLeaf(p, secs(day.Add(FullRoundLead))), "its recorded value stands whole in mock-31 at spec.resp.body; ", ""},
		{"a read of the clock the lead moved, whole as its escaped text decodes", "at " + day.Format(time.RFC3339), "", strLeaf(p, escaped(day)), strLeaf(p, escaped(day.Add(FullRoundLead))), "its recorded value stands whole in mock-31 at spec.resp.body; ", ""},
		{"a moment picked from what the application was given, in another layout", "at " + millis(day), "at " + millis(got), strLeaf(p, day.Format(time.RFC3339Nano)), strLeaf(p, got.Format(time.RFC3339Nano)), fmt.Sprintf(echo, " at spec.resp.body"), ""},
		{"a typed moment picked from what the application was given", "at " + millis(day), "at " + millis(got), typedLeaf(p, day, ms), typedLeaf(p, got, ms), fmt.Sprintf(echo, " at spec.resp.body"), ""},
		{"a typed moment picked from what the application was given, in an encoded payload", base64.StdEncoding.EncodeToString([]byte("sent at " + millis(day) + "!!")), "at " + millis(got), typedLeaf(p, day, ms), typedLeaf(p, got, ms), fmt.Sprintf(echo, "'s payload"), ""},
		{"a word the value holds only as F saw it", "S12 message", "", strLeaf(p, "order 9a9a of"), strLeaf(p, "order 670e of"), "a word of what record's full round saw in it (its characters 7 to 10) appears in an earlier recording, in mock-31 at spec.resp.body: ", KindUnaccounted},
	} {
		kind := tc.kind
		if kind == "" {
			kind = KindVariable
		}
		for _, served := range []bool{false, true} {
			for _, follow := range []bool{false, true} {
				dc := clockedDrifts()
				m := receiveOf("mock-31", id, r1Insert.Add(-time.Minute))
				m.Spec.HTTPResp.Body = strings.Replace(m.Spec.HTTPResp.Body, "S12 message", tc.body, 1)
				raw := []Raw{dc.RawOf(m, served)}
				if tc.also != "" {
					o := receiveOf("mock-11", "aaaaaaaa-1111-4222-8333-bbbbbbbbbbbb", r1Insert.Add(-30*time.Second))
					o.Spec.HTTPResp.Body = strings.Replace(o.Spec.HTTPResp.Body, "S12 message", tc.also, 1)
					raw = append(raw, dc.RawOf(o, true))
				}
				var ln Learned
				if follow {
					ln = ClassifyFollowing(dc, []string{p}, tc.live, tc.rec, r1Insert, nil, raw, nil, nil, nil)[0]
				} else {
					ln = ClassifyIn(dc, []string{p}, tc.live, tc.rec, r1Insert, nil, raw, nil, nil)[0]
				}
				want, not := never, yet
				if served {
					want, not = yet, never
				}
				// A refusal's reason may go on to say what the chance-word
				// rule found (follows.excused); a variable's ends there.
				ends := strings.HasSuffix(ln.Why, want)
				if kind != KindVariable {
					ends = strings.Contains(ln.Why, want+";") || ends
				}
				if ln.Kind != kind || kind == KindVariable && ln.From.Mock != "mock-31" || !strings.HasPrefix(ln.Why, tc.found) || !ends || strings.Contains(ln.Why, not) {
					t.Errorf("%s, served %v, client test %v: %s from %+v (%s); want %s (of mock-31 where a variable) whose reason begins %q and ends: %s", tc.name, served, follow, ln.Kind, ln.From, ln.Why, kind, tc.found, want)
				}
			}
		}
	}
}
