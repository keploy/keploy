package learn

import (
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Rows of a whole time the time default checks by its form where the clock
// checks refuse it a pick or a no-clock-read (wholeSettles, wholeTime), and
// of the times inside an id or a key, which stay as learned.

// A role binding's insert as an api-server CI test made it: its createdAt,
// the second it was made, written whole as RFC 3339 text. The service
// account's find, answered a millisecond before, holds the account's
// createdAt in epoch seconds: the second before as recorded, the binding's
// own second in round F. Round F's moment stands at that answer, which does
// not show it as recorded: a pick under the clock checks, and a time the
// default checks by its form.
var (
	wRAt = time.Date(2026, 11, 12, 2, 14, 11, 3149407, time.UTC)
	wFAt = time.Date(2026, 11, 12, 2, 16, 11, 2957301, time.UTC)
)

const (
	wPath   = "body.docs.msgs.0.createdAt"
	wAnswer = "resp.0.0.cursor.firstBatch.0.createdAt"
	wSec    = "text 9999-99-99a99:99:99a"
)

func wSecs(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
func wRFC(t time.Time) string  { return t.UTC().Format(time.RFC3339) }

// wFind is the service account's find, answered at at, its createdAt rec as
// recorded and live in round F, with its payload as a real exchange has it,
// so a row reaches what learning reads of it (no unread refusal).
func wFind(at time.Time, rec, live string) Earlier {
	pay := RawOf(&models.Mock{Name: "mock-3271", Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Microsecond), ResTimestampMock: at}}, true).ServedAs(1)
	return Earlier{Mock: "mock-3271", At: at, Own: true, Payload: pay, Seen: []Seen{{Side: Response, Leaf: oText(wAnswer, rec), Live: live}}}
}

// wBinding is the insert at path, of type typ, holding rec and live, after
// the find that holds the account's createdAt (recorded a second before
// the insert's second, round F's at it).
func wBinding(path string, typ Type, rec, live string) Input {
	find := wFind(wRAt.Add(-time.Millisecond), wSecs(wRAt.Add(-time.Second)), wSecs(wFAt))
	in := iCall(path, typ, rec, live, wRAt, wFAt, iWin(wRAt, wFAt, 10*time.Millisecond), []Earlier{find}, nil)
	in.Mock = "mock-3274"
	return in
}

func TestTheTimeDefaultChecksAWholeTimeRefusedAsAPickByItsForm(t *testing.T) {
	in := wBinding(wPath, String, wRFC(wRAt), wRFC(wFAt))
	d := oWord(t, Decide(in), wRFC(wRAt), wRFC(wFAt))
	oRefused(t, "a role binding's createdAt, strict times", d, RefusalPick, "mock-3271")
	tStrictThenDefault(t, "a role binding's createdAt", in, RefusalPick, true, wSec)

	// Typed by its decoder (a BSON datetime), the same.
	typed := wBinding(wPath, Instant, iMs(wRAt.Truncate(time.Second)), iMs(wFAt.Truncate(time.Second)))
	tStrictThenDefault(t, "a typed createdAt", typed, RefusalPick, true, "typed 9999-99-99a99:99:99a")

	// An epoch number whole (a $numberLong's text, a marker's marked_at),
	// round F's second standing at the find's answer as RFC 3339.
	const marked = "body.docs.msgs.0.u.$setOnInsert.marked_at.$numberLong"
	num := iCall(marked, String, wSecs(wRAt), wSecs(wFAt), wRAt, wFAt, iWin(wRAt, wFAt, 10*time.Millisecond),
		[]Earlier{wFind(wRAt.Add(-time.Millisecond), wRFC(wRAt.Add(-time.Second)), wRFC(wFAt))}, nil)
	tStrictThenDefault(t, "an epoch marked_at", num, RefusalPick, true, "text 9999999999")
}

// A whole time no earlier place and no window of its call holds (a
// session's last_active, read from a cache: twenty minutes old as recorded,
// seven in round F) is refused no-clock-read under the clock checks, and is
// a time the default checks by its form.
func TestTheTimeDefaultChecksAWholeTimeNoReadExplainsByItsForm(t *testing.T) {
	const p = "body.docs.msgs.0.u.$set.last_active.$numberLong"
	rec, live := wSecs(wRAt.Add(-20*time.Minute)), wSecs(wFAt.Add(-7*time.Minute))
	in := iCall(p, String, rec, live, wRAt, wFAt, iWin(wRAt, wFAt, 10*time.Millisecond), nil, nil)
	tStrictThenDefault(t, "a cached last_active", in, RefusalNoClockRead, true, "text 9999999999")
	// One far from its call in a run is no epoch keploy reads as near it:
	// its refusal stands.
	far := iCall(p, String, rec, wSecs(wFAt.Add(-2*epochNear)), wRAt, wFAt, iWin(wRAt, wFAt, 10*time.Millisecond), nil, nil)
	tStrictThenDefault(t, "a last_active two years stale", far, RefusalNoClockRead, false, "")
}

// A whole time the running test gave keploy (a given, TestGiven) as
// recorded, which round F gave the same there, while the call carries
// another value: the value did not follow, and the clock checks refuse it
// as a conflict. The time default settles no conflict: it stays refused, as
// under strict times, for RFC 3339 text, a typed instant and an epoch
// number, each in its call's window and stale.
func TestTheTimeDefaultLeavesAWholeTimeRefusedAsAConflict(t *testing.T) {
	win := iWin(wRAt, wFAt, 10*time.Millisecond)
	for _, c := range []struct {
		what string
		typ  Type
		v    func(time.Time) string
	}{
		{"an RFC 3339 text", String, wRFC},
		{"a typed instant", Instant, iMs},
		{"an epoch number", String, wSecs},
	} {
		for _, at := range []struct {
			what   string
			rd, fd time.Duration
		}{{"in its window", 0, 0}, {"stale", -20 * time.Minute, -7 * time.Minute}} {
			r, f := c.v(wRAt.Add(at.rd)), c.v(wFAt.Add(at.fd))
			given := Earlier{Mock: "test case post-x", Given: true, Gave: true, At: wRAt.Add(-time.Second),
				Seen: []Seen{{Side: Request, Leaf: Leaf{Path: "body.at", Noise: "body.at", Type: c.typ, Value: r}, Live: r}}}
			in := iCall(wPath, c.typ, r, f, wRAt, wFAt, win, []Earlier{given}, nil)
			wAsStrict(t, c.what+" the test gave keploy, "+at.what, in, r, f, RefusalConflict)
		}
	}
}

// A whole time its call's window holds, beside an exchange keploy holds
// no payload of (the value may have been built on its answer), is refused
// unread under the clock checks: keploy could not read the evidence. And
// one whose two runs write it in two layouts no replay passes as one form
// (RFC 3339 as recorded, RFC 1123 or a space for the T in round F) is
// refused cannot-check. The time default settles neither on a whole time:
// both stay refused, as under strict times.
func TestTheTimeDefaultLeavesAWholeTimeUnreadOrInTwoLayouts(t *testing.T) {
	win := iWin(wRAt, wFAt, 10*time.Millisecond)
	bare := Earlier{Mock: "mock-3271", At: wRAt.Add(-time.Millisecond), Own: true, Seen: []Seen{{Side: Response, Leaf: oText(wAnswer, "abc"), Live: "abd"}}}
	for _, c := range []struct {
		what string
		typ  Type
		v    func(time.Time) string
	}{
		{"an RFC 3339 text", String, wRFC},
		{"a typed instant", Instant, iMs},
		{"an epoch number", String, wSecs},
	} {
		in := iCall(wPath, c.typ, c.v(wRAt), c.v(wFAt), wRAt, wFAt, win, []Earlier{bare}, nil)
		tStrictThenDefault(t, c.what+" beside an exchange with no payload", in, RefusalUnread, false, "")
	}
	for _, f := range []string{time.RFC1123, "2006-01-02 15:04:05"} {
		in := iCall(wPath, String, wRFC(wRAt), wFAt.Format(f), wRAt, wFAt, win, nil, nil)
		tStrictThenDefault(t, "an RFC 3339 time written "+f+" in round F", in, RefusalCannotCheck, false, "")
	}
}

// wAsStrict checks the decisions of in under the time default are the
// clock checks' (strict times): the same outcome and refusal of each word,
// none a time checked by its form; and that strict refuses the word of r
// and f in want, so the row asks the time default about such a refusal.
func wAsStrict(t *testing.T, what string, in Input, r, f string, want ...RefusalKind) {
	t.Helper()
	in.TimeForm = false
	strict := Decide(in)
	for _, d := range strict {
		if d.Refusal != nil && d.Refusal.Kind == RefusalUnread {
			t.Fatalf("%s, strict times: %s; want a row that reaches the time logic, not one refused unread", what, oDescribe([]Decision{d}))
		}
	}
	if len(want) > 0 {
		d := oWord(t, strict, r, f)
		if d.Refusal == nil || !slices.Contains(want, d.Refusal.Kind) {
			t.Fatalf("%s, strict times: %s; want the word refused %v", what, oDescribe([]Decision{d}), want)
		}
	}
	in.TimeForm = true
	def := Decide(in)
	if len(def) != len(strict) {
		t.Errorf("FALSE GREEN? %s: the time default %s; want strict's %s", what, oDescribe(def), oDescribe(strict))
		return
	}
	for i, d := range def {
		s := strict[i]
		if d.Outcome == OutcomeClock || d.ByForm != nil || d.Outcome != s.Outcome || (d.Refusal == nil) != (s.Refusal == nil) ||
			d.Refusal != nil && d.Refusal.Kind != s.Refusal.Kind {
			t.Errorf("FALSE GREEN? %s: the time default %s; want strict's %s, no time checked by its form", what, oDescribe([]Decision{d}), oDescribe([]Decision{s}))
		}
	}
}

// wOid is an ObjectId made at at: its first four bytes the second, in hex,
// then rest.
func wOid(at time.Time, rest string) string {
	b := []byte{byte(at.Unix() >> 24), byte(at.Unix() >> 16), byte(at.Unix() >> 8), byte(at.Unix())}
	return hex.EncodeToString(b) + rest
}

// wUUID7 is a time-ordered UUID (version 7) made at at.
func wUUID7(at time.Time, rest string) string {
	ms := at.UnixMilli()
	h := strconv.FormatInt(ms, 16)
	for len(h) < 12 {
		h = "0" + h
	}
	return h[:8] + "-" + h[8:] + "-7" + rest
}

// A time inside an id or a key is no whole time: the time default leaves
// it as the clock checks learn it, whatever its refusal. An epoch second
// inside a lease's _id (a sweeper's lease, its key the second it was made),
// refused a pick or no-clock-read; a time-ordered UUID whole (its time a
// part of the id), and a value one run wrote as a time and the other as
// such a UUID; the minute inside the leases the runs made (a number no
// layout reads as an instant); and an ObjectId whose first four bytes are
// round F's second at the find's answer (a cluster's filter._id, a role
// binding's principal and its key) stand as strict learns them.
func TestATimeInsideAnIdOrAKeyStaysAsLearned(t *testing.T) {
	const id = "body.documents[0]._id"
	win := iWin(wRAt, wFAt, 10*time.Millisecond)
	stale := func(v func(time.Time) string) (string, string) {
		return v(wRAt.Add(-20 * time.Minute)), v(wFAt.Add(-7 * time.Minute))
	}
	r, f := stale(wSecs)
	wAsStrict(t, "an epoch second inside a lease's _id, read from a cache", iCall(id, String, "ui-app-data-sweep:"+r, "ui-app-data-sweep:"+f, wRAt, wFAt, win, nil, nil), r, f, RefusalNoClockRead)
	picked := iCall(id, String, "pat-janitor:"+wSecs(wRAt), "pat-janitor:"+wSecs(wFAt), wRAt, wFAt, win,
		[]Earlier{wFind(wRAt.Add(-time.Millisecond), wRFC(wRAt.Add(-time.Second)), wRFC(wFAt))}, nil)
	wAsStrict(t, "an epoch second inside a lease's _id, picked", picked, wSecs(wRAt), wSecs(wFAt), RefusalPick)

	ur, uf := wUUID7(wRAt.Add(-20*time.Minute), "a1b-8c2d-0123456789ab"), wUUID7(wFAt.Add(-7*time.Minute), "f9e-9d3c-ba9876543210")
	tr, tf := stale(wRFC)
	for _, u := range [][2]string{{ur, uf}, {ur, wUUID7(wFAt.Add(-7*time.Minute), "a1b-8c2d-0123456789ab")}, {tr, uf}, {ur, tf}} {
		wAsStrict(t, "a time-ordered UUID "+u[0]+" to "+u[1], iCall(id, String, u[0], u[1], wRAt, wFAt, win, nil, nil), "", "")
		picked := iCall(id, String, u[0], u[1], wRAt, wFAt, win, []Earlier{wFind(wRAt.Add(-time.Millisecond), wRFC(wRAt.Add(-time.Second)), wRFC(wFAt))}, nil)
		wAsStrict(t, "a time-ordered UUID "+u[0]+" to "+u[1]+" beside the find", picked, "", "")
	}

	// The instant at the start of a longer text (an epoch second, then a
	// suffix; an RFC 3339 time, then one), picked from the find's answer
	// or read from a cache: a word of the text, as at its end.
	cache := func(v func(time.Time) string, suffix string) Input {
		r, f := stale(v)
		return iCall(id, String, r+suffix, f+suffix, wRAt, wFAt, win, nil, nil)
	}
	pick := func(v func(time.Time) string, suffix string, ans func(time.Time) string) Input {
		return iCall(id, String, v(wRAt)+suffix, v(wFAt)+suffix, wRAt, wFAt, win,
			[]Earlier{wFind(wRAt.Add(-time.Millisecond), ans(wRAt.Add(-time.Second)), ans(wFAt))}, nil)
	}
	r, f = stale(wSecs)
	wAsStrict(t, "an epoch second at the start of a lease's _id, read from a cache", cache(wSecs, "-pat-janitor"), r, f, RefusalNoClockRead)
	wAsStrict(t, "an epoch second at the start of a lease's _id, picked", pick(wSecs, "-pat-janitor", wRFC), wSecs(wRAt), wSecs(wFAt), RefusalPick)
	r, f = stale(wRFC)
	wAsStrict(t, "an RFC 3339 time at the start of a text, read from a cache", cache(wRFC, "-x"), r, f, RefusalNoClockRead)
	wAsStrict(t, "an RFC 3339 time at the start of a text, picked", pick(wRFC, "-x", wSecs), wRFC(wRAt), wRFC(wFAt), RefusalPick)

	for _, l := range [][2]string{{"ui-app-data-sweep:29907035", "ui-app-data-sweep:29907037"}, {"pat-janitor:996901", "pat-janitor:996902"}} {
		wAsStrict(t, "a lease's _id "+l[0], iCall(id, String, l[0], l[1], wRAt, wFAt, win, nil, nil), "", "")
	}

	for _, c := range []struct{ what, path, prefix string }{
		{"a cluster's filter._id", "body.cmd.filter._id", ""},
		{"a role binding's principal", "body.docs.msgs.0.principal.id", ""},
		{"a role binding's principal key", "body.docs.msgs.0.principalKey", "USER:"},
	} {
		or, of := c.prefix+wOid(wRAt.Add(-time.Second), "32e505659b477299"), c.prefix+wOid(wFAt, "32e505659b4772a3")
		in := iCall(c.path, String, or, of, wRAt, wFAt, win, []Earlier{wFind(wRAt.Add(-time.Millisecond), wSecs(wRAt.Add(-time.Second)), wSecs(wFAt))}, nil)
		// After the find, which holds its first four bytes' second as an
		// epoch number: a value made anew each run, in both.
		if ds := Decide(in); len(ds) == 0 || !allOutcome(ds, OutcomeFresh) {
			t.Errorf("%s, strict times: %s; want a value made anew each run", c.what, oDescribe(ds))
		}
		wAsStrict(t, c.what, in, "", "")
		// Beside the test case's request that holds the recorded id whole,
		// which round F sent as recorded: refused, as a conflict, in both.
		given := Earlier{Mock: "test case post-query-82", Given: true, Seen: []Seen{{Side: Request, Leaf: oText("body", strings.TrimPrefix(or, c.prefix)), Live: strings.TrimPrefix(or, c.prefix)}}}
		in.Earlier = append([]Earlier{given}, in.Earlier...)
		ds := Decide(in)
		if len(ds) == 0 || !oneRefusedAs(ds, RefusalConflict) {
			t.Errorf("%s beside the test case's request, strict times: %s; want refused as a conflict", c.what, oDescribe(ds))
		}
		wAsStrict(t, c.what+" beside the test case's request", in, "", "")
	}
}

// oneRefusedAs reports whether a decision of ds is a refusal of kind k.
func oneRefusedAs(ds []Decision, k RefusalKind) bool {
	return slices.ContainsFunc(ds, func(d Decision) bool { return d.Refusal != nil && d.Refusal.Kind == k })
}

// allOutcome reports whether every decision of ds has outcome o.
func allOutcome(ds []Decision, o WordOutcome) bool {
	return !slices.ContainsFunc(ds, func(d Decision) bool { return d.Outcome != o })
}
