package learn

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// copyOrReadRun is one run of api-server's audit in keploy mock's client
// tests, on the application's clock: the test began at began; the app read
// its clock for the app's createdAt at began+created and sent it in the app's
// insert at began+insert; then it read its clock for the audit's auditedAt
// at began+audited and sent the audit at began+audit, a copy of the app's
// createdAt beside it.
type copyOrReadRun struct {
	began                           time.Time
	created, insert, audited, audit time.Duration
}

func (r copyOrReadRun) at(d time.Duration) time.Time { return r.began.Add(d) }

// window is the audit's window in r: from the insert, the previous call, to
// the audit, carrying the test's begin (Span.Began) where began is set.
func (r copyOrReadRun) window(began bool) Span {
	s := Span{From: r.at(r.insert), To: r.at(r.audit)}
	if began {
		s.Began = r.began
	}
	return s
}

// A place whose value both copies an earlier read of the clock (a value the
// test's own earlier call sent, which held it in both runs) and lies in its
// own call's window in both runs is learned as a read of the clock since the
// test began (models.SinceTestBegin), never as a variable of the earlier
// call: both explanations hold, and that read holds under both. Learned as a
// variable, its learning would hang on the full round's timing: the round
// that read the app's createdAt and the audit's auditedAt in one second
// learned auditedAt as a copy of createdAt, and a replay that read them a
// second apart missed (a false red decided by timing); the round that read
// them a second apart learned auditedAt as a read. Now both timings learn
// every place of the audit as a read of the clock, and every replay of either
// timing passes them; one whose audit holds a stamp no read since its test
// began gives (a day before) misses.
//
// One run's own window is enough, the other run's since its test began: a
// full round slower than the recording (or faster) learns the same. Where
// only the copy holds it stays the earlier call's variable: a copy neither
// run's own window holds (the app read createdAt two seconds before the
// insert in both), or a call whose window knows no test begin a replay could check
// the read from (keploy test's). Under the rule's absence (ClassifyGiven: the
// proven switch off) nothing changes. A typed instant its protocol writes to
// the second (a SQL timestamp(0)) is read the same.
func TestAClientTestLearnsACopyOfAnEarlierReadItsOwnWindowHoldsAsAReadSinceItsTestBegan(t *testing.T) {
	const (
		created = "body.cmd.documents.0.app.createdAt"
		audited = "body.cmd.documents.0.auditedAt"
	)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), created: time.Millisecond, insert: 2 * time.Millisecond, audited: 3 * time.Millisecond, audit: 4 * time.Millisecond}
	// Record's full round on the application's clock, a whole second
	// (one) or 800 ms past one (apart) at its test's begin.
	whole := time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC)
	full := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: whole.Add(base), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	apart, one := full(800*time.Millisecond), full(0)
	if secs(apart.at(apart.created)) == secs(apart.at(apart.audited)) || secs(one.at(one.created)) != secs(one.at(one.audited)) {
		t.Fatal("the row needs one full round to read createdAt and auditedAt a second apart, the other in one second")
	}
	insertAt := rec.at(rec.insert)
	// The audit's two places as recorded and as F saw them, and the insert's
	// createdAt, in text (a BSON int64 of epoch seconds, as the Mongo parser
	// hands it) or as a typed instant to the second.
	type form struct {
		name  string
		leaf  func(path string, at time.Time) Leaf
		typed bool
	}
	forms := []form{
		{"epoch seconds", func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }, false},
		{"a typed instant to the second", func(p string, at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(time.Second), time.Second) }, true},
	}
	learn := func(fm form, r, f copyOrReadRun, began, following bool) map[string]Learned {
		at := r.at(r.insert)
		ins := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
		earlier := []Earlier{{Mock: "mock-app", At: at, Own: true, Payload: ins.ServedAs(1), Seen: []Seen{
			{Side: Request, Leaf: fm.leaf("body.cmd.documents.0.createdAt", r.at(r.created)), Live: fm.leaf("body.cmd.documents.0.createdAt", f.at(f.created)).Value},
		}}}
		shape := func(r copyOrReadRun) Shape {
			return Shape{Leaves: map[string]Leaf{created: fm.leaf(created, r.at(r.created)), audited: fm.leaf(audited, r.at(r.audited))}}
		}
		w := &Window{Rec: r.window(began), F: f.window(began)}
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		got := map[string]Learned{}
		for _, ln := range classify(clientTestCache(), []string{created, audited}, shape(f), shape(r), r.at(r.audit), earlier, nil, nil, w, nil) {
			got[ln.Recorded.Path] = ln
		}
		return got
	}
	// Replays on the application's clock: both reads in one second, a
	// second apart, and an audit whose auditedAt is a day before its test.
	rp := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC).Add(base), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	replays := []struct {
		name string
		r    copyOrReadRun
		day  bool // auditedAt a day before the test began
	}{{"replay in one second", rp(0), false}, {"replay a second apart", rp(800 * time.Millisecond), false}, {"replay with auditedAt a day before", rp(0), true}}
	for _, fm := range forms {
		for _, fr := range []struct {
			name  string
			f     copyOrReadRun
			apart bool
		}{{"full round a second apart", apart, true}, {"full round in one second", one, false}} {
			t.Run(fm.name+"/"+fr.name, func(t *testing.T) {
				got := learn(fm, rec, fr.f, true, true)
				// The copy its own window holds is a read since its test
				// began in both timings. auditedAt, read a second past
				// createdAt in F, is a plain read in its window in text,
				// where F refutes the copy (sources); a typed instant's
				// source is followed by the moment it was recorded with
				// (instantServedFrom), so the copy still stands beside
				// the read there, and it is a read since its test began.
				auditedSince := models.SinceTestBegin
				if fr.apart && !fm.typed {
					auditedSince = ""
				}
				for p, since := range map[string]string{created: models.SinceTestBegin, audited: auditedSince} {
					ln := got[p]
					o, ok := ln.Clock()
					if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != since || ln.From.Mock != "" {
						t.Fatalf("%s: %s Read=%+v from %+v (%s); want a plain read of the clock checked since %q, no variable", p, ln.Kind, ln.Read, ln.From, ln.Why, since)
					}
					if fm.typed != (o.Layout == models.LayoutTyped) {
						t.Fatalf("%s: layout %q; want typed %v", p, o.Layout, fm.typed)
					}
				}
				for _, r := range replays {
					s := r.r.window(true)
					auditedAt := r.r.at(r.r.audited)
					if r.day {
						auditedAt = r.r.began.Add(-24 * time.Hour)
					}
					for p, at := range map[string]time.Time{created: r.r.at(r.r.created), audited: auditedAt} {
						o, _ := got[p].Clock()
						ok, why := CheckClockRead(p, o, fm.leaf(p, at), s, true)
						if want := !r.day || p != audited; ok != want {
							t.Fatalf("%s, %s: CheckClockRead = %v (%s); want %v", r.name, p, ok, why, want)
						}
					}
				}
			})
		}
		// The app read createdAt two seconds before the insert: the
		// audit's window holds no copy of it in that run (early), only the
		// window since its test began does.
		early, recEarly := one, rec
		early.created, early.insert, early.audited, early.audit = 100*time.Millisecond, 2150*time.Millisecond, 2300*time.Millisecond, 2400*time.Millisecond
		recEarly.insert, recEarly.audited, recEarly.audit = 2002*time.Millisecond, 2003*time.Millisecond, 2004*time.Millisecond
		t.Run(fm.name+"/one run's own window holds it", func(t *testing.T) {
			// A full round slower than the recording, or faster: the read
			// since its test began, whichever run's window holds it.
			for _, c := range []struct {
				name string
				r, f copyOrReadRun
			}{{"the recording's", rec, early}, {"the full round's", recEarly, one}} {
				ln := learn(fm, c.r, c.f, true, true)[created]
				if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || ln.From.Mock != "" {
					t.Fatalf("only %s window: createdAt %s Read=%+v from %+v (%s); want a plain read of the clock since its test began, no variable", c.name, ln.Kind, ln.Read, ln.From, ln.Why)
				}
			}
		})
		t.Run(fm.name+"/only the copy holds", func(t *testing.T) {
			for _, c := range []struct {
				name             string
				r, f             copyOrReadRun
				began, following bool
			}{
				{"a copy neither run's own window holds", recEarly, early, true, true},
				{"a call whose window knows no test begin", rec, one, false, true},
				{"the rule's absence (ClassifyGiven)", rec, one, true, false},
			} {
				got := learn(fm, c.r, c.f, c.began, c.following)
				if ln := got[created]; ln.Kind != KindVariable || ln.From.Mock != "mock-app" {
					t.Fatalf("%s: createdAt %s Read=%+v from %+v (%s); want a variable of mock-app", c.name, ln.Kind, ln.Read, ln.From, ln.Why)
				}
			}
		})
	}
	// The insert as a recording F served whole (Raw), not leaf by leaf: a
	// typed copy found in its payload is a read since its test began too.
	// Read as a value with no source, the rule's absence keeps it the
	// payload's variable.
	tf := forms[1]
	at := rec.at(rec.created).Truncate(time.Second).Format(time.RFC3339)
	holder := RawOf(&models.Mock{Name: "mock-app", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests:    []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"insert":"apps","documents":[{"createdAt":{"$date":"` + at + `"}}],"$db":"app"} }`}}}},
		ReqTimestampMock: insertAt, ResTimestampMock: insertAt.Add(time.Millisecond),
	}}, true)
	shape := func(r copyOrReadRun) Shape {
		return Shape{Leaves: map[string]Leaf{audited: tf.leaf(audited, r.at(r.audited))}}
	}
	w := &Window{Rec: rec.window(true), F: one.window(true)}
	for _, c := range []struct {
		name      string
		following bool
		want      Kind
	}{{"a typed copy found in a recording served whole", true, KindClockRead}, {"the rule's absence (ClassifyGiven)", false, KindVariable}} {
		classify := ClassifyGiven
		if c.following {
			classify = ClassifyFollowing
		}
		ln := classify(clientTestCache(), []string{audited}, shape(one), shape(rec), rec.at(rec.audit), nil, []Raw{holder}, nil, w, nil)[0]
		if o, ok := ln.Clock(); ln.Kind != c.want || c.want == KindClockRead && (!ok || o.Since != models.SinceTestBegin) || c.want == KindVariable && ln.From.Mock != "mock-app" {
			t.Fatalf("%s: %s Read=%+v from %+v (%s); want %s", c.name, ln.Kind, ln.Read, ln.From, ln.Why, c.want)
		}
	}
}

// A place whose value is built by an offset on what the application was
// given (a start-up recording's stamp, minutes before the test began: the
// recording gave the application one, record's full round another in its
// place, each the same 5m23s before the value) and lies in its own call's
// window in both runs is learned as a read of the clock since the test began
// (models.SinceTestBegin), never refused: both explanations hold in the
// rounds, and that read holds under both. api-server's app.createdAt stood
// so, and the offset rule set twelve of its tests aside. Replays whose read
// lies in their test pass; one a day before its test misses.
//
// Where only the offset holds it stays refused, naming the two recordings:
// a value its own window does not hold (read two seconds before the call
// before it), a call whose window knows no test begin a replay could check
// the read from (keploy test's), and the rule's absence (ClassifyGiven: the
// proven switch off). A typed instant to the second is read the same.
func TestAClientTestLearnsAnOffsetOfAGivenInstantItsOwnWindowHoldsAsAReadSinceItsTestBegan(t *testing.T) {
	const p = "body.cmd.documents.0.app.createdAt"
	const gap = 5*time.Minute + 23*time.Second
	const offsetRule = "a value built on what the application was given by an offset moves so"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	// The app read its clock for createdAt at audited, inside the window
	// from the call before (insert) to its own (audit).
	rec := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	f := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	// The start-up's two recordings of one call, each holding a stamp the
	// value stands gap after: the recording's (mock-3) and the one record's
	// full round gave the application in its place (mock-11), in an order
	// keploy cannot tell.
	stamp := func(name string, sent, at time.Time) *models.Mock {
		m := r1Receive(`{"SentTimestamp": "` + strconv.FormatInt(sent.UnixMilli(), 10) + `"}`)
		m.Name = name
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 03:02:51 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(200*time.Millisecond)
		return m
	}
	type fm = struct {
		name string
		leaf func(at time.Time) Leaf
	}
	forms := []fm{
		{"epoch seconds", func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }},
		{"a typed instant to the second", func(at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(time.Second), time.Second) }},
	}
	learn := func(fm fm, r, f copyOrReadRun, began, following bool) Learned {
		dc := clientTestCache()
		raw := []Raw{
			dc.RawOf(stamp("mock-3", r.at(r.audited).Add(-gap), r.began.Add(-6*time.Minute)), true),
			dc.RawOf(stamp("mock-11", f.at(f.audited).Add(-gap), r.began.Add(-5*time.Minute)), true),
		}
		w := &Window{Rec: r.window(began), F: f.window(began)}
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		shape := func(x copyOrReadRun) Shape { return Shape{Leaves: map[string]Leaf{p: fm.leaf(x.at(x.audited))}} }
		return classify(dc, []string{p}, shape(f), shape(r), r.at(r.audit), nil, raw, nil, w, nil)[0]
	}
	rp := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC).Add(base), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	for _, fm := range forms {
		t.Run(fm.name, func(t *testing.T) {
			ln := learn(fm, rec, f, true, true)
			o, ok := ln.Clock()
			if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
				t.Fatalf("%s Read=%+v from %+v (%s); want a plain read of the clock checked since the test began", ln.Kind, ln.Read, ln.From, ln.Why)
			}
			for _, r := range []struct {
				name string
				at   func(r copyOrReadRun) time.Time
				want bool
			}{
				{"replay inside its test", func(r copyOrReadRun) time.Time { return r.at(r.audited) }, true},
				{"replay at its test's begin", func(r copyOrReadRun) time.Time { return r.began }, true},
				{"replay with createdAt a day before its test", func(r copyOrReadRun) time.Time { return r.began.Add(-24 * time.Hour) }, false},
				{"replay with createdAt the given stamp's offset from the recorded one", func(copyOrReadRun) time.Time { return rec.at(rec.audited) }, false},
			} {
				for _, base := range []time.Duration{0, 800 * time.Millisecond} {
					x := rp(base)
					if ok, why := CheckClockRead(p, o, fm.leaf(r.at(x)), x.window(true), true); ok != r.want {
						t.Fatalf("%s (%v past a second): CheckClockRead = %v (%s); want %v", r.name, base, ok, why, r.want)
					}
				}
			}
			// The value read two seconds before the call before its own:
			// its own window does not hold it.
			early := func(x copyOrReadRun) copyOrReadRun {
				x.insert, x.audit = x.audited+2*time.Second, x.audited+2100*time.Millisecond
				return x
			}
			for _, c := range []struct {
				name             string
				r, f             copyOrReadRun
				began, following bool
			}{
				{"a value its own window does not hold", early(rec), early(f), true, true},
				{"a call whose window knows no test begin", rec, f, false, true},
				{"the rule's absence (ClassifyGiven)", rec, f, true, false},
			} {
				if ln := learn(fm, c.r, c.f, c.began, c.following); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, offsetRule) || !strings.Contains(ln.Why, "mock-3") || !strings.Contains(ln.Why, "mock-11") {
					t.Fatalf("%s: %s Read=%+v (%s); want refused by the offset rule, naming mock-3 and mock-11", c.name, ln.Kind, ln.Read, ln.Why)
				}
			}
		})
	}
	// Read without the offset rule alone, every other rule as it stands: a
	// read its own window holds whose recorded text stands whole in a
	// connection's start-up a swap can change (a server parameter set per
	// pool) stays refused, naming the start-up, never a read since the
	// test began.
	r2 := copyOrReadRun{began: eRecAt, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	f2 := r2
	f2.began = eRecAt.Add(69700 * time.Millisecond)
	a, b := eLaneSession("mock-hs-81", 2*time.Minute, 81), eLaneSession("mock-hs-82", 110*time.Second, 82)
	a.Spec.PostgresV3.Session.ParameterStatus["application_name"] = secs(r2.at(r2.audited))
	b.Spec.PostgresV3.Session.ParameterStatus["application_name"] = "hikari"
	raw := []Raw{RawOf(a, false).ServedAs(1).ServedAsRecorded(), RawOf(b, false).ServedAs(2).ServedAsRecorded()}
	leaf := forms[0].leaf
	ln := ClassifyFollowing(clientTestCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(f2.at(f2.audited))}}, Shape{Leaves: map[string]Leaf{p: leaf(r2.at(r2.audited))}}, r2.at(r2.audit), nil, raw, nil, &Window{Rec: r2.window(true), F: f2.window(true)}, nil)[0]
	if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "mock-hs-81 at spec.postgresV3.session.parameterStatus.application_name, a connection's recorded start-up") {
		t.Fatalf("FALSE GREEN? a read its own window holds, recorded whole in a start-up a swap can change: %s Read=%+v (%s); want refused, naming the start-up", ln.Kind, ln.Read, ln.Why)
	}
}

// A place the offset rule refuses (a start-up stamp, the recording's mock-3
// and the one record's full round gave in its place, mock-11, each the same
// gap before the value) that is a read of the clock made for its call in one
// run and earlier in its test in the other is learned as a read of the clock
// since its test began (models.SinceTestBegin), as the copy rule learns a
// copy in those windows (Window.oneRunSinceBegin): api-server's T2
// (clock-read-carried-later, 2026-10-31) read an app's createdAt, updatedAt
// and auditedAt at the start of its create handler; the recording made the
// handler's calls in 5 ms, so the audit's own window held them, while record's
// full round took half a second over them and crossed a second, so only its
// window since the test began did. The offset rule refused createdAt, and the
// test was set aside (TestTestSuiteAssertionTypes, TestTestSuitePagination,
// TestCITokensEndWithTheirServiceAccount; mv2s1's TestSavedBearerTokenIsSent-
// Trimmed and TestSlowStepFailsOnlyItself). The rescue read only the call's
// own window in both runs, so the learning hung on how fast the full round
// ran. It is read without the offset rule alone, every other rule as it
// stands, in those windows too. An earlier test's insert recorded in the same
// second (mock-12, whose own read in record's full round fell in another
// second), which refuses the value read in its own window alone (a word that
// did not drift with it), is a same-second coincidence there, not its source:
// both legs are learned so, and a replay inside its test passes while one a
// day before, or one holding the recorded value, misses.
//
// Where neither run's own window holds the read (read two seconds before the
// call before it in both), where a window knows no test begin, and under
// ClassifyGiven (the proven switch off), it stays refused, naming mock-3 and
// mock-11.
func TestAClientTestLearnsAnOffsetOfAGivenInstantOneRunsOwnWindowHoldsAsAReadSinceItsTestBegan(t *testing.T) {
	const p = "body.docs.msgs.0.app.createdAt.$numberLong"
	const gap = 11484*time.Hour + 35*time.Minute + 49*time.Second
	const offsetRule = "a value built on what the application was given by an offset moves so"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	// The recording: begin, the read at its handler's start, the call
	// before the audit (insert) and the audit, all within 6 ms.
	rec := copyOrReadRun{began: time.Date(2026, 10, 30, 23, 3, 29, 548_000_000, time.UTC), audited: time.Millisecond, insert: 5400 * time.Microsecond, audit: 5800 * time.Microsecond}
	// Record's full round: the read at its handler's start, then half a
	// second of calls, crossing a second, before the call before the audit.
	f := copyOrReadRun{began: time.Date(2026, 10, 30, 23, 4, 46, 796_000_000, time.UTC), audited: time.Millisecond, insert: 488 * time.Millisecond, audit: 489 * time.Millisecond}
	stamp := func(name string, sent, at time.Time) *models.Mock {
		m := r1Receive(`{"SentTimestamp": "` + strconv.FormatInt(sent.UnixMilli(), 10) + `"}`)
		m.Name = name
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 03:02:51 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(200*time.Millisecond)
		return m
	}
	leaf := func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }
	learn := func(r, f copyOrReadRun, began, following, earlierTest bool) Learned {
		dc := clientTestCache()
		raw := []Raw{
			dc.RawOf(stamp("mock-3", r.at(r.audited).Add(-gap), r.began.Add(-6*time.Minute)), true),
			dc.RawOf(stamp("mock-11", f.at(f.audited).Add(-gap), r.began.Add(-5*time.Minute)), true),
		}
		var earlier []Earlier
		if earlierTest {
			// An earlier test's insert of its own app, recorded in the
			// same second as this read; record's full round read its own
			// clock for it two seconds before this test began.
			earlier = []Earlier{{Mock: "mock-12", At: r.began.Add(-60 * time.Millisecond), Run: true, Seen: []Seen{{Side: Request, Leaf: leaf(r.at(r.audited)), Live: secs(f.began.Add(-2 * time.Second)), Place: p}}}}
		}
		w := &Window{Rec: r.window(began), F: f.window(began)}
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		shape := func(x copyOrReadRun) Shape { return Shape{Leaves: map[string]Leaf{p: leaf(x.at(x.audited))}} }
		return classify(dc, []string{p}, shape(f), shape(r), r.at(r.audit), earlier, raw, nil, w, nil)[0]
	}
	if secs(f.at(f.audited)) == secs(f.at(f.insert)) || secs(rec.at(rec.audited)) != secs(rec.at(rec.insert)) {
		t.Fatal("fixture: the read must lie in the recording's own window and outside record's full round's")
	}
	for _, earlierTest := range []bool{false, true} {
		t.Run(fmt.Sprintf("an earlier test's same-second insert %v", earlierTest), func(t *testing.T) {
			ln := learn(rec, f, true, true, earlierTest)
			o, ok := ln.Clock()
			if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin {
				t.Fatalf("%s Read=%+v from %+v (%s); want a plain read of the clock checked since the test began", ln.Kind, ln.Read, ln.From, ln.Why)
			}
			x := copyOrReadRun{began: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC), audited: time.Millisecond, insert: 300 * time.Millisecond, audit: 301 * time.Millisecond}
			for _, r := range []struct {
				name string
				at   time.Time
				want bool
			}{
				{"replay inside its test", x.at(x.audited), true},
				{"replay with createdAt a day before its test", x.began.Add(-24 * time.Hour), false},
				{"replay with the recorded createdAt", rec.at(rec.audited), false},
			} {
				if ok, why := CheckClockRead(p, o, leaf(r.at), x.window(true), true); ok != r.want {
					t.Fatalf("%s: CheckClockRead = %v (%s); want %v", r.name, ok, why, r.want)
				}
			}
		})
	}
	early := func(x copyOrReadRun) copyOrReadRun {
		x.insert, x.audit = x.audited+2*time.Second, x.audited+2100*time.Millisecond
		return x
	}
	for _, c := range []struct {
		name             string
		r, f             copyOrReadRun
		began, following bool
	}{
		{"neither run's own window holds it", early(rec), early(f), true, true},
		{"a call whose window knows no test begin", rec, f, false, true},
		{"the rule's absence (ClassifyGiven)", rec, f, true, false},
	} {
		if ln := learn(c.r, c.f, c.began, c.following, false); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, offsetRule) || !strings.Contains(ln.Why, "mock-3") || !strings.Contains(ln.Why, "mock-11") {
			t.Fatalf("%s: %s Read=%+v (%s); want refused by the offset rule, naming mock-3 and mock-11", c.name, ln.Kind, ln.Read, ln.Why)
		}
	}
}

// A place whose value copies an earlier read of the clock (the app's
// createdAt its insert sent), is built by an offset on what the application
// was given (a start-up recording's stamp 5m23s before it: the recording's
// mock-3, and the one record's full round gave in its place, mock-11), and
// lies in its own call's window in both runs is learned as a read of the
// clock since the test began, in both timings of the full round: all three
// explanations hold in the rounds, and that read holds under all three. Read
// again without the source search alone, the offset rule refused the copy,
// so it stayed the insert's variable, and the round that made the audit's
// two reads in one second learned auditedAt as that variable too, while the
// round that made them a second apart learned a read: a replay whose reads
// land a second apart then missed, by the full round's timing. Replays of
// either timing pass every place; an auditedAt a day before its test misses.
// One run's own window is enough, the other run's since its test began; a
// copy neither run's own window holds stays the insert's variable.
func TestAClientTestLearnsACopyThatIsAlsoAnOffsetOfAGivenInstantAsAReadSinceItsTestBegan(t *testing.T) {
	const (
		src     = "body.cmd.documents.0.createdAt"
		created = "body.cmd.documents.0.app.createdAt"
		audited = "body.cmd.documents.0.auditedAt"
		gap     = 5*time.Minute + 23*time.Second
	)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), created: time.Millisecond, insert: 2 * time.Millisecond, audited: 3 * time.Millisecond, audit: 4 * time.Millisecond}
	whole := time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC)
	full := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: whole.Add(base), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	apart, one := full(800*time.Millisecond), full(0)
	if secs(apart.at(apart.created)) == secs(apart.at(apart.audited)) || secs(one.at(one.created)) != secs(one.at(one.audited)) {
		t.Fatal("the row needs one full round to read createdAt and auditedAt a second apart, the other in one second")
	}
	stamp := func(name string, sent, at time.Time) *models.Mock {
		m := r1Receive(`{"SentTimestamp": "` + strconv.FormatInt(sent.UnixMilli(), 10) + `"}`)
		m.Name = name
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 03:02:51 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(200*time.Millisecond)
		return m
	}
	leaf := func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }
	learn := func(r, f copyOrReadRun) map[string]Learned {
		dc := clientTestCache()
		at := r.at(r.insert)
		ins := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
		earlier := []Earlier{{Mock: "mock-app", At: at, Own: true, Payload: ins.ServedAs(1), Seen: []Seen{
			{Side: Request, Leaf: leaf(src, r.at(r.created)), Live: leaf(src, f.at(f.created)).Value},
		}}}
		raw := []Raw{
			dc.RawOf(stamp("mock-3", r.at(r.created).Add(-gap), r.began.Add(-6*time.Minute)), true),
			dc.RawOf(stamp("mock-11", f.at(f.created).Add(-gap), r.began.Add(-5*time.Minute)), true),
		}
		shape := func(r copyOrReadRun) Shape {
			return Shape{Leaves: map[string]Leaf{created: leaf(created, r.at(r.created)), audited: leaf(audited, r.at(r.audited))}}
		}
		got := map[string]Learned{}
		for _, ln := range ClassifyFollowing(dc, []string{created, audited}, shape(f), shape(r), r.at(r.audit), earlier, raw, nil, &Window{Rec: r.window(true), F: f.window(true)}, nil) {
			got[ln.Recorded.Path] = ln
		}
		return got
	}
	rp := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC).Add(base), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	for _, fr := range []struct {
		name string
		f    copyOrReadRun
	}{{"full round a second apart", apart}, {"full round in one second", one}} {
		t.Run(fr.name, func(t *testing.T) {
			got := learn(rec, fr.f)
			for _, p := range []string{created, audited} {
				ln := got[p]
				if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || ln.From.Mock != "" {
					t.Fatalf("%s: %s Read=%+v from %+v (%s); want a plain read of the clock checked since the test began, no variable", p, ln.Kind, ln.Read, ln.From, ln.Why)
				}
			}
			for _, r := range []struct {
				name string
				r    copyOrReadRun
				day  bool // auditedAt a day before the test began
			}{{"replay in one second", rp(0), false}, {"replay a second apart", rp(800 * time.Millisecond), false}, {"replay with auditedAt a day before", rp(0), true}} {
				auditedAt := r.r.at(r.r.audited)
				if r.day {
					auditedAt = r.r.began.Add(-24 * time.Hour)
				}
				for p, at := range map[string]time.Time{created: r.r.at(r.r.created), audited: auditedAt} {
					o, _ := got[p].Clock()
					ok, why := CheckClockRead(p, o, leaf(p, at), r.r.window(true), true)
					if want := !r.day || p != audited; ok != want {
						t.Fatalf("%s, %s: CheckClockRead = %v (%s); want %v", r.name, p, ok, why, want)
					}
				}
			}
		})
	}
	// The app read createdAt two seconds before the insert: the audit's
	// window holds no copy of it in that run. Where one run's own window
	// holds it, it is the read since its test began; where neither does,
	// it stays the insert's variable.
	early, recEarly := one, rec
	early.created, early.insert, early.audited, early.audit = 100*time.Millisecond, 2150*time.Millisecond, 2300*time.Millisecond, 2400*time.Millisecond
	recEarly.insert, recEarly.audited, recEarly.audit = 2002*time.Millisecond, 2003*time.Millisecond, 2004*time.Millisecond
	for _, c := range []struct {
		name string
		r, f copyOrReadRun
	}{{"only the recording's window holds it", rec, early}, {"only the full round's window holds it", recEarly, one}} {
		if ln := learn(c.r, c.f)[created]; ln.Kind != KindClockRead || ln.Read == nil || ln.Read.Since != models.SinceTestBegin || ln.From.Mock != "" {
			t.Fatalf("%s: createdAt %s Read=%+v from %+v (%s); want a read of the clock since its test began", c.name, ln.Kind, ln.Read, ln.From, ln.Why)
		}
	}
	if ln := learn(recEarly, early)[created]; ln.Kind != KindVariable || ln.From.Mock != "mock-app" {
		t.Fatalf("a copy neither run's own window holds: createdAt %s Read=%+v from %+v (%s); want a variable of mock-app", ln.Kind, ln.Read, ln.From, ln.Why)
	}
}

// A value built by an offset on what the application was given (a start-up
// recording's stamp: the recording's mock-3, and the one record's full round
// gave in its place, mock-11, each the same span before it) that is a read of
// its own call's clock plus one constant offset in both runs (an expiry
// thirty days after the audit's now) is learned as that offset, checked since
// its test began: both explanations hold in the rounds, and that reading
// holds under both, as for a plain read (the row above). Replays inside their
// test and at its begin pass, for epoch seconds and a typed instant to the
// second; one a day before its test, one that keeps the recorded value, and
// one a day longer miss. A value its own window does not hold, a window with
// no test begin, and ClassifyGiven stay refused by the offset rule.
func TestAClientTestLearnsAnOffsetOfAGivenInstantThatIsItsOwnClockPlusAnOffsetAsThatOffsetSinceItsTestBegan(t *testing.T) {
	const p = "body.cmd.documents.0.app.expiresAt"
	const month = 30 * 24 * time.Hour
	const gap = month + 5*time.Minute + 23*time.Second
	const offsetRule = "a value built on what the application was given by an offset moves so"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	rec := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	f := copyOrReadRun{began: time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	stamp := func(name string, sent, at time.Time) *models.Mock {
		m := r1Receive(`{"SentTimestamp": "` + strconv.FormatInt(sent.UnixMilli(), 10) + `"}`)
		m.Name = name
		m.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 03:02:51 GMT"
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(200*time.Millisecond)
		return m
	}
	type fm = struct {
		name string
		leaf func(at time.Time) Leaf
	}
	forms := []fm{
		{"epoch seconds", func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }},
		{"a typed instant to the second", func(at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(time.Second), time.Second) }},
	}
	value := func(x copyOrReadRun) time.Time { return x.at(x.audited).Add(month) }
	learn := func(fm fm, r, f copyOrReadRun, began, following bool) Learned {
		dc := clientTestCache()
		// Each served as recorded, as its adapter says (Raw.ServedAsRecorded):
		// a serve no adapter stated refuses the value's new words.
		raw := []Raw{
			dc.RawOf(stamp("mock-3", value(r).Add(-gap), r.began.Add(-6*time.Minute)), true).ServedAsRecorded(),
			dc.RawOf(stamp("mock-11", value(f).Add(-gap), r.began.Add(-5*time.Minute)), true).ServedAsRecorded(),
		}
		w := (&Window{Rec: r.window(began), F: f.window(began)}).ComparesOffsets()
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		shape := func(x copyOrReadRun) Shape { return Shape{Leaves: map[string]Leaf{p: fm.leaf(value(x))}} }
		return classify(dc, []string{p}, shape(f), shape(r), r.at(r.audit), nil, raw, nil, w, nil)[0]
	}
	rp := func(base time.Duration) copyOrReadRun {
		return copyOrReadRun{began: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC).Add(base), insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	}
	for _, fm := range forms {
		t.Run(fm.name, func(t *testing.T) {
			ln := learn(fm, rec, f, true, true)
			o, ok := ln.Clock()
			if ln.Kind != KindClockOffset || !ok || o.Plain() || o.Min > month || o.Max <= month || o.Since != models.SinceTestBegin {
				t.Fatalf("%s Offset=%+v from %+v (%s); want the clock plus thirty days, checked since the test began", ln.Kind, ln.Offset, ln.From, ln.Why)
			}
			for _, r := range []struct {
				name string
				at   func(r copyOrReadRun) time.Time
				want bool
			}{
				{"replay inside its test", value, true},
				{"replay at its test's begin", func(r copyOrReadRun) time.Time { return r.began.Add(month) }, true},
				{"replay with the value a day before its test", func(r copyOrReadRun) time.Time { return r.began.Add(month - 24*time.Hour) }, false},
				{"replay with a time to live a day longer", func(r copyOrReadRun) time.Time { return value(r).Add(24 * time.Hour) }, false},
				{"replay that keeps the recorded value", func(copyOrReadRun) time.Time { return value(rec) }, false},
			} {
				for _, base := range []time.Duration{0, 800 * time.Millisecond} {
					x := rp(base)
					if ok, why := CheckClockRead(p, o, fm.leaf(r.at(x)), x.window(true), true); ok != r.want {
						t.Fatalf("%s (%v past a second): CheckClockRead = %v (%s); want %v", r.name, base, ok, why, r.want)
					}
				}
			}
			early := func(x copyOrReadRun) copyOrReadRun {
				x.insert, x.audit = x.audited+2*time.Second, x.audited+2100*time.Millisecond
				return x
			}
			for _, c := range []struct {
				name             string
				r, f             copyOrReadRun
				began, following bool
			}{
				{"a value its own window does not hold", early(rec), early(f), true, true},
				{"a call whose window knows no test begin", rec, f, false, true},
				{"the rule's absence (ClassifyGiven)", rec, f, true, false},
			} {
				if ln := learn(fm, c.r, c.f, c.began, c.following); ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, offsetRule) || !strings.Contains(ln.Why, "mock-3") || !strings.Contains(ln.Why, "mock-11") {
					t.Fatalf("%s: %s Offset=%+v (%s); want refused by the offset rule, naming mock-3 and mock-11", c.name, ln.Kind, ln.Offset, ln.Why)
				}
			}
		})
	}
}

// eui's role binding (eui-t2 GAPS.md G7), in a layout to the second: a
// GraphQL expiresAt the application builds as its call's clock plus thirty
// days, cut to the second (written to the second, or to the millisecond with
// the milliseconds zeroed, "2026-11-26T16:59:10.000Z", as a web app's
// GraphQL role binding writes it), beside a login answer (mock-1; record's full round
// gave mock-9 in its place) whose Set-Cookie Expires is the login's clock
// plus thirty days, six seconds earlier. A word of the value (the minute
// "59") stands in the cookie, so the value read as a variable of mock-1 at
// Set-Cookie, a part of two characters a replay's login can never give it;
// it is a read of its own call's clock plus thirty days in both runs too, so
// it is learned as that offset, checked since its test began, in both
// timings of the full round (in one minute, and across one, where no word of
// the full round's value stands in its cookie). Replays a second apart pass;
// a value that keeps the recorded one, a day longer, or thirty days after a
// read a day before its test miss. ClassifyGiven, a window with no test
// begin, and a value its own window does not hold stay the variable of
// mock-1; and a word of two characters the value really copies from an
// earlier answer (a region code) stays a variable of that answer, never an
// offset, a read, or noise.
func TestAClientTestLearnsAnExpiryALoginsCookieHoldsAWordOfAsTheClockPlusItsOffsetSinceItsTestBegan(t *testing.T) {
	const p = "body.variables.expiresAt"
	const month = 30 * 24 * time.Hour
	login := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/login", Body: `{"u":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "refresh_token_staging=abc; Path=/; Expires=" + at.Add(month).UTC().Format(time.RFC1123) + "; HttpOnly"}, Body: `{}`},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	layout := time.RFC3339
	leaf := func(at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.Truncate(time.Second).UTC().Format(layout)}
	}
	// A run: its test began at began, logged in at began+900ms, and made the
	// call at began+7s, its window from 300ms before; the app read its clock
	// for the value at began+7s+read.
	type run struct {
		began time.Time
		read  time.Duration
	}
	call := func(x run) time.Time { return x.began.Add(7 * time.Second) }
	value := func(x run) time.Time { return call(x).Add(x.read + month) }
	span := func(x run, began bool) Span {
		s := Span{From: call(x).Add(-300 * time.Millisecond), To: call(x)}
		if began {
			s.Began = x.began
		}
		return s
	}
	rec := run{began: time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	learn := func(r, f run, began, following bool, earlier ...Earlier) Learned {
		dc := clientTestCache()
		raw := []Raw{dc.RawOf(login("mock-1", r.began.Add(900*time.Millisecond)), true), dc.RawOf(login("mock-9", f.began.Add(900*time.Millisecond)), true)}
		w := (&Window{Rec: span(r, began), F: span(f, began)}).ComparesOffsets()
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		return classify(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(value(f))}}, Shape{Leaves: map[string]Leaf{p: leaf(value(r))}}, call(r), earlier, raw, nil, w, nil)[0]
	}
	for _, lay := range []struct{ name, layout string }{
		{"to the second", time.RFC3339},
		{"to the millisecond, the milliseconds zeroed", "2006-01-02T15:04:05.000Z"},
	} {
		layout = lay.layout
		for _, f := range []struct {
			name string
			run  run
		}{
			{"a full round in one minute", run{began: time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}},
			{"a full round across a minute", run{began: time.Date(2026, 10, 27, 17, 1, 56, 300_000_000, time.UTC)}},
		} {
			t.Run(lay.name+"/"+f.name, func(t *testing.T) {
				ln := learn(rec, f.run, true, true)
				o, ok := ln.Clock()
				if ln.Kind != KindClockOffset || !ok || o.Plain() || o.Min > month || o.Max <= month || o.Since != models.SinceTestBegin {
					t.Fatalf("%s Offset=%+v from %+v held %q (%s); want the clock plus thirty days, checked since the test began", ln.Kind, ln.Offset, ln.From, ln.From.Held, ln.Why)
				}
				for _, c := range []struct {
					name string
					at   func(x run) time.Time
					want bool
				}{
					{"replay inside its test", value, true},
					{"replay that keeps the recorded value", func(run) time.Time { return value(rec) }, false},
					{"replay with a time to live a day longer", func(x run) time.Time { return value(x).Add(24 * time.Hour) }, false},
					{"replay thirty days after a read a day before its test", func(x run) time.Time { return x.began.Add(month - 24*time.Hour) }, false},
				} {
					for _, base := range []time.Duration{0, 800 * time.Millisecond} {
						x := run{began: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(base)}
						if ok, why := CheckClockRead(p, o, leaf(c.at(x)), span(x, true), true); ok != c.want {
							t.Fatalf("%s (%v past a second): CheckClockRead = %v (%s); want %v", c.name, base, ok, why, c.want)
						}
					}
				}
				for _, c := range []struct {
					name             string
					r, f             run
					began, following bool
				}{
					{"the rule's absence (ClassifyGiven)", rec, f.run, true, false},
					{"a call whose window knows no test begin", rec, f.run, false, true},
					{"a value its own window does not hold", run{began: rec.began, read: -2 * time.Second}, run{began: f.run.began, read: -2 * time.Second}, true, true},
				} {
					if ln := learn(c.r, c.f, c.began, c.following); ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Held != "59" {
						t.Fatalf("%s: %s Offset=%+v from %+v held %q (%s); want the variable of mock-1 holding \"59\", as before", c.name, ln.Kind, ln.Offset, ln.From, ln.From.Held, ln.Why)
					}
				}
				// The re-read sets aside the searches for a source only: a
				// refusal of the request's checks stands there (a test case's
				// own request keploy could not read, which the value may have
				// come from), and the copy stays the variable.
				unread := Earlier{Mock: givenPlace(""), Given: true, Unread: unsentRequest, At: rec.began}
				if ln := learn(rec, f.run, true, true, unread); ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Held != "59" {
					t.Fatalf("a test case's request keploy could not read: %s Offset=%+v from %+v held %q (%s); want the variable of mock-1 holding \"59\", as before", ln.Kind, ln.Offset, ln.From, ln.From.Held, ln.Why)
				}
			})
		}
	}
	layout = time.RFC3339
	// Only the copy rule's re-read sets those searches aside. A word of the
	// value that only record's full round's login holds (the recording's
	// gave another expiry) binds no copy, so no re-read: the first reading's
	// refusal stands, naming the login, under the rule and without it,
	// never the clock plus thirty days.
	for _, following := range []bool{false, true} {
		dc := clientTestCache()
		f := run{began: time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
		other := login("mock-1", rec.began.Add(900*time.Millisecond))
		other.Spec.HTTPResp.Header["Set-Cookie"] = "refresh_token_staging=abc; Path=/; Expires=Tue, 01 Dec 2026 08:07:06 GMT; HttpOnly"
		// Each served as recorded, as its adapter says (Raw.ServedAsRecorded):
		// a serve no adapter stated refuses the value's new words.
		raw := []Raw{dc.RawOf(other, true).ServedAsRecorded(), dc.RawOf(login("mock-9", f.began.Add(900*time.Millisecond)), true).ServedAsRecorded()}
		w := (&Window{Rec: span(rec, true), F: span(f, true)}).ComparesOffsets()
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		ln := classify(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(value(f))}}, Shape{Leaves: map[string]Leaf{p: leaf(value(rec))}}, call(rec), nil, raw, nil, w, nil)[0]
		if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "appears in an earlier recording, in mock-") {
			t.Fatalf("a word only record's full round's login holds (following %v): %s Offset=%+v (%s); want refused, naming the login", following, ln.Kind, ln.Offset, ln.Why)
		}
	}
	regionCopyStaysItsVariable(t, clientTestCache(), rec.began)
}

// regionCopyStaysItsVariable is the control of the rows whose value a
// login's answer holds a word of by chance: a word of two characters a value
// really copies from an earlier answer (its region), in a client test whose
// window knows its test's begin (r0, record's; F two minutes later), stays a
// variable of that answer.
func regionCopyStaysItsVariable(t *testing.T, dc *DriftCache, r0 time.Time) {
	t.Helper()
	const q = "body.variables.lane"
	geo := func(name, region string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/geo", Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"region":"` + region + `","zone":"north"}`},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	f0 := r0.Add(2 * time.Minute)
	raw := []Raw{dc.RawOf(geo("mock-1", "us", r0), true), dc.RawOf(geo("mock-9", "eu", f0), true)}
	w := (&Window{Rec: Span{From: r0.Add(time.Second), To: r0.Add(2 * time.Second), Began: r0.Add(-time.Second)}, F: Span{From: f0.Add(time.Second), To: f0.Add(2 * time.Second), Began: f0.Add(-time.Second)}}).ComparesOffsets()
	ln := ClassifyFollowing(dc, []string{q}, Shape{Leaves: map[string]Leaf{q: {Path: q, Noise: q, Type: String, Value: "lane eu-7"}}}, Shape{Leaves: map[string]Leaf{q: {Path: q, Noise: q, Type: String, Value: "lane us-7"}}}, r0.Add(2*time.Second), nil, raw, nil, w, nil)[0]
	if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Held != "us" {
		t.Fatalf("FALSE GREEN? a region really copied from an earlier answer: %s from %+v held %q (%s); want the variable of mock-1 holding \"us\"", ln.Kind, ln.From, ln.From.Held, ln.Why)
	}
}

// A wall clock of no zone in the application's zone (Europe/Berlin:
// "2026-10-27 17:59:10", a Java LocalDateTime, a MySQL DATETIME as text),
// its call's own read of the clock, whose minute "59" a login's Set-Cookie
// Expires, in GMT and six seconds earlier, holds by chance, is learned as a
// read of the clock since its test began, in both timings of the full round,
// never as the variable of the login holding "59". The words' own reading of
// the clock (clockAt) reads a time of no zone in UTC, where the call's window
// does not hold it, so the word search binds "59" to the login; the window
// holds it in the application's zone (clientTestUnread, windowHolds). Both
// explanations hold in the rounds, and the read holds under both. A replay
// whose read lands in its test passes; the recorded value, and a read a day
// before its test, miss.
//
// The variable stays where only the copy holds (ClassifyGiven; a window that
// knows no test begin; a value its window does not hold) and where the
// request's checks refuse it (a test case's request keploy could not read);
// a region really copied from an earlier answer stays its variable.
func TestAClientTestLearnsAWallClockOfNoZoneALoginsCookieHoldsAWordOfAsAReadSinceItsTestBegan(t *testing.T) {
	const p = "body.variables.since"
	const month = 30 * 24 * time.Hour
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no zone database: ", err)
	}
	login := func(name string, at time.Time) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/login", Body: `{"u":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "refresh_token_staging=abc; Path=/; Expires=" + at.Add(month).UTC().Format(time.RFC1123) + "; HttpOnly"}, Body: `{}`},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	// A run: its test began at began, logged in at began+900ms, and made the
	// call at began+7s, its window from 300ms before; the app read its clock
	// for the value at began+7s+read.
	type run struct {
		began time.Time
		read  time.Duration
	}
	call := func(x run) time.Time { return x.began.Add(7 * time.Second) }
	value := func(x run) time.Time { return call(x).Add(x.read) }
	span := func(x run, began bool) Span {
		s := Span{From: call(x).Add(-300 * time.Millisecond), To: call(x)}
		if began {
			s.Began = x.began
		}
		return s
	}
	rec := run{began: time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		leaf := func(at time.Time) Leaf {
			return Leaf{Path: p, Noise: p, Type: String, Value: at.Truncate(time.Second).In(berlin).Format(layout)}
		}
		learn := func(r, f run, began, following bool, earlier ...Earlier) Learned {
			dc := clientTestCacheIn(berlin)
			raw := []Raw{dc.RawOf(login("mock-1", r.began.Add(900*time.Millisecond)), true), dc.RawOf(login("mock-9", f.began.Add(900*time.Millisecond)), true)}
			w := (&Window{Rec: span(r, began), F: span(f, began)}).ComparesOffsets()
			classify := ClassifyGiven
			if following {
				classify = ClassifyFollowing
			}
			return classify(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(value(f))}}, Shape{Leaves: map[string]Leaf{p: leaf(value(r))}}, call(r), earlier, raw, nil, w, nil)[0]
		}
		for _, f := range []struct {
			name string
			run  run
		}{
			{"a full round in one minute", run{began: time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}},
			{"a full round across a minute", run{began: time.Date(2026, 10, 27, 17, 1, 56, 300_000_000, time.UTC)}},
		} {
			t.Run(layout+"/"+f.name, func(t *testing.T) {
				ln := learn(rec, f.run, true, true)
				o, ok := ln.Clock()
				if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || o.Zone != models.ZoneApp || ln.From.Mock != "" {
					t.Fatalf("%s Read=%+v from %+v held %q (%s); want a plain read of the clock in the application's zone, checked since the test began (zone %q)", ln.Kind, ln.Read, ln.From, ln.From.Held, ln.Why, o.Zone)
				}
				for _, c := range []struct {
					name string
					at   func(x run) time.Time
					want bool
				}{
					{"replay inside its test", value, true},
					{"replay that keeps the recorded value", func(run) time.Time { return value(rec) }, false},
					{"replay with a read a day before its test", func(x run) time.Time { return x.began.Add(-24 * time.Hour) }, false},
					{"replay that writes UTC's wall clock", func(x run) time.Time {
						_, off := value(x).In(berlin).Zone()
						return value(x).Add(-time.Duration(off) * time.Second)
					}, false},
				} {
					for _, base := range []time.Duration{0, 800 * time.Millisecond} {
						x := run{began: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC).Add(base)}
						s := span(x, true)
						s.Zone = berlin
						if ok, why := CheckClockRead(p, o, leaf(c.at(x)), s, true); ok != c.want {
							t.Fatalf("%s (%v past a second): CheckClockRead = %v (%s); want %v", c.name, base, ok, why, c.want)
						}
					}
				}
				unread := Earlier{Mock: givenPlace(""), Given: true, Unread: unsentRequest, At: rec.began}
				for _, c := range []struct {
					name             string
					r, f             run
					began, following bool
					earlier          []Earlier
				}{
					{"the rule's absence (ClassifyGiven)", rec, f.run, true, false, nil},
					{"a call whose window knows no test begin", rec, f.run, false, true, nil},
					{"a value its own window does not hold", run{began: rec.began, read: -2 * time.Second}, run{began: f.run.began, read: -2 * time.Second}, true, true, nil},
					{"a test case's request keploy could not read", rec, f.run, true, true, []Earlier{unread}},
				} {
					if ln := learn(c.r, c.f, c.began, c.following, c.earlier...); ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Held != "59" {
						t.Fatalf("%s: %s Read=%+v from %+v held %q (%s); want the variable of mock-1 holding \"59\", as before", c.name, ln.Kind, ln.Read, ln.From, ln.From.Held, ln.Why)
					}
				}
			})
		}
	}
	regionCopyStaysItsVariable(t, clientTestCacheIn(berlin), rec.began)
}

// A text copy of an instant the test's own earlier call sent typed (a role
// inserted with its expiresAt, then sent as text in a GraphQL call), which
// is its call's clock plus thirty days in both runs, is learned as that
// offset since its test began where another test's answer holds a word of it
// by chance (a login of an earlier test of the set, whose answer holds the
// minute of both runs): the copy rule's re-read sets aside the search of
// what F gave the application in another test (givenWhy) as it does the
// search of the test's own places, and reads the value as the clock plus its
// offset. Without that answer it learns the same; under the rule's absence
// (ClassifyGiven) it stays the copy of the insert.
func TestAClientTestLearnsATextCopyOfATypedExpiryAnotherTestsAnswerHoldsAWordOfAsTheClockPlusItsOffsetSinceItsTestBegan(t *testing.T) {
	const (
		p   = "body.variables.expiresAt"
		src = "body.cmd.documents.0.expiresAt"
	)
	const month = 30 * 24 * time.Hour
	rec := time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)
	f := time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)
	call := func(b time.Time) time.Time { return b.Add(7 * time.Second) }
	value := func(b time.Time) time.Time { return call(b).Add(month).Truncate(time.Second) }
	span := func(b time.Time) Span { return Span{From: call(b).Add(-300 * time.Millisecond), To: call(b), Began: b} }
	leaf := func(at time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: at.UTC().Format(time.RFC3339)}
	}
	for _, c := range []struct {
		name             string
		other, following bool
		want             Kind
	}{
		{"another test's answer holding its minute", true, true, KindClockOffset},
		{"no other test's answer", false, true, KindClockOffset},
		{"the rule's absence (ClassifyGiven)", true, false, KindVariable},
	} {
		dc := clientTestCache()
		insAt := call(rec).Add(-200 * time.Millisecond)
		ins := RawOf(&models.Mock{Name: "mock-2", Spec: models.MockSpec{ReqTimestampMock: insAt, ResTimestampMock: insAt.Add(time.Millisecond)}}, true)
		earlier := []Earlier{{Mock: "mock-2", At: insAt, Own: true, Payload: ins.ServedAs(1), Seen: []Seen{
			{Side: Request, Leaf: InstantLeaf(src, src, value(rec), time.Second), Live: InstantLeaf(src, src, value(f), time.Second).Value},
		}}}
		var raw []Raw
		if c.other {
			login := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "HTTP_CLIENT"},
				HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/login", Body: `{"u":"a"}`, Header: map[string]string{}},
				HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"ids":["59","01"]}`},
				ReqTimestampMock: rec.Add(900 * time.Millisecond), ResTimestampMock: rec.Add(920 * time.Millisecond),
			}}
			raw = []Raw{dc.RawOf(login, true).OfAnotherTest()}
		}
		w := (&Window{Rec: span(rec), F: span(f)}).ComparesOffsets()
		classify := ClassifyGiven
		if c.following {
			classify = ClassifyFollowing
		}
		ln := classify(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: leaf(value(f))}}, Shape{Leaves: map[string]Leaf{p: leaf(value(rec))}}, call(rec), earlier, raw, nil, w, nil)[0]
		switch o, ok := ln.Clock(); {
		case ln.Kind != c.want:
			t.Fatalf("%s: %s Offset=%+v from %+v (%s); want %s", c.name, ln.Kind, ln.Offset, ln.From, ln.Why, c.want)
		case c.want == KindVariable && ln.From.Mock != "mock-2":
			t.Fatalf("%s: a variable of %+v; want the copy of mock-2", c.name, ln.From)
		case c.want == KindClockOffset && (!ok || o.Plain() || o.Min > month || o.Max <= month || o.Since != models.SinceTestBegin):
			t.Fatalf("%s: Offset=%+v; want the clock plus thirty days, checked since the test began", c.name, ln.Offset)
		}
	}
}

// A whole copy of a stamp an earlier answer of its own test gave the
// application (a dependency's createdAt, or its expiresAt thirty days after
// its now) stays that answer's variable, in a client test whose call's own
// window holds the stamp at the second's cut in both runs too, never a read
// of the clock (or the clock plus thirty days) since the test began. A
// replay serves the answer as recorded, so a correct application copies the
// recorded stamp there, which the variable passes and a read of the clock
// refuses ("it changed"): learned as the read, the test would fail its
// confirming round and be set aside, a false red. The copy rule's "holds
// under both explanations" holds only for an earlier place the application
// makes again at replay (a value it sent: the rows above); for one it was
// given, the copy explanation predicts the recorded stamp at replay. The
// earlier answer is a recording the call's payload search finds (in text, an
// ISO instant, epoch seconds as a number or as text) or an exchange F served
// leaf by leaf, whose answer's leaf holds the stamp (in epoch seconds, or
// typed to the second); where that leaf is a value the application sent at
// that exchange, the rule above learns a read since the test began.
func TestAClientTestKeepsAWholeCopyOfAStampAnEarlierAnswerGaveItAsThatAnswersVariable(t *testing.T) {
	const month = 30 * 24 * time.Hour
	// One run: the test began at began; the dependency answered the app's
	// previous call at began+1.2 s, with its own stamp to the second; the
	// call copying it has its window from 25 ms after that answer.
	type run struct{ began time.Time }
	ans := func(r run) time.Time { return r.began.Add(1200 * time.Millisecond) }
	span := func(r run) Span {
		from := ans(r).Add(25 * time.Millisecond)
		return Span{From: from, To: from.Add(200 * time.Millisecond), Began: r.began}
	}
	stamp := func(r run) time.Time { return ans(r).Truncate(time.Second) }
	rec := run{time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	full := run{time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
	if stamp(rec).Before(span(rec).From.Truncate(time.Second)) || stamp(full).Before(span(full).From.Truncate(time.Second)) {
		t.Fatal("the row needs each run's window to hold the answer's stamp at the second's cut")
	}
	answer := func(name, url string, at time.Time, body string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: url, Body: `{"n":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: body},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	iso := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	type form struct {
		name string
		typ  Type
		body func(key string, at time.Time) string
		val  func(at time.Time) string
	}
	forms := []form{
		{"an ISO instant", String, func(k string, at time.Time) string { return `{"` + k + `":"` + iso(at) + `"}` }, iso},
		{"epoch seconds", Number, func(k string, at time.Time) string { return `{"` + k + `":` + secs(at) + `}` }, secs},
		{"epoch seconds as text", String, func(k string, at time.Time) string { return `{"` + k + `":"` + secs(at) + `"}` }, secs},
	}
	for _, c := range []struct {
		name, key, url string
		forms          []form
		value          func(r run) time.Time
	}{
		{"a dependency's createdAt", "createdAt", "http://localhost:8083/items", forms, stamp},
		{"a dependency's expiresAt", "expiresAt", "http://localhost:8083/tokens", forms[:2], func(r run) time.Time { return stamp(r).Add(month) }},
	} {
		p := "body.variables." + c.key
		for _, fm := range c.forms {
			for _, following := range []bool{true, false} {
				t.Run(c.name+"/"+fm.name+"/following="+strconv.FormatBool(following), func(t *testing.T) {
					dc := clientTestCache()
					raw := []Raw{dc.RawOf(answer("mock-1", c.url, ans(rec), fm.body(c.key, c.value(rec))), true), dc.RawOf(answer("mock-9", c.url, ans(full), fm.body(c.key, c.value(full))), true)}
					w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
					classify := ClassifyGiven
					if following {
						classify = ClassifyFollowing
					}
					lf := func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: fm.typ, Value: fm.val(at)} }
					ln := classify(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: lf(c.value(full))}}, Shape{Leaves: map[string]Leaf{p: lf(c.value(rec))}}, span(rec).To, nil, raw, nil, w, nil)[0]
					if ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
						o, _ := ln.Clock()
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-1, the answer a replay serves as recorded", ln.Kind, o, ln.From, ln.Why)
					}
				})
			}
		}
	}
	// The earlier exchange as F served it, leaf by leaf: the app's own
	// previous call, whose request (a value the app sent) or answer (a
	// dependency's findAndModify returning the document) holds the stamp the
	// call copies whole.
	const (
		src  = "body.createdAt"
		copy = "body.cmd.documents.0.app.createdAt"
	)
	type leafRun struct {
		began                           time.Time
		created, insert, audited, audit time.Duration
	}
	at := func(r leafRun, d time.Duration) time.Time { return r.began.Add(d) }
	lspan := func(r leafRun) Span { return Span{From: at(r, r.insert), To: at(r, r.audit), Began: r.began} }
	lrec := leafRun{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), created: time.Millisecond, insert: 2 * time.Millisecond, audited: 3 * time.Millisecond, audit: 4 * time.Millisecond}
	lone := leafRun{began: time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audited: 300 * time.Millisecond, audit: 400 * time.Millisecond}
	insertAt := at(lrec, lrec.insert)
	insert := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: insertAt, ResTimestampMock: insertAt.Add(time.Millisecond)}}, true)
	for _, fm := range []struct {
		name string
		leaf func(p string, at time.Time) Leaf
	}{
		{"epoch seconds", func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }},
		{"typed to the second", func(p string, at time.Time) Leaf { return InstantLeaf(p, p, at.Truncate(time.Second), time.Second) }},
	} {
		for _, side := range []Side{Response, Request} {
			t.Run("an exchange served leaf by leaf/"+fm.name+"/its "+string(side), func(t *testing.T) {
				earlier := []Earlier{{Mock: "mock-app", At: insertAt, Own: true, Payload: insert.ServedAs(1), Seen: []Seen{
					{Side: side, Leaf: fm.leaf(src, at(lrec, lrec.created)), Live: fm.leaf(src, at(lone, lone.created)).Value},
				}}}
				shape := func(r leafRun) Shape { return Shape{Leaves: map[string]Leaf{copy: fm.leaf(copy, at(r, r.created))}} }
				w := &Window{Rec: lspan(lrec), F: lspan(lone)}
				ln := ClassifyFollowing(clientTestCache(), []string{copy}, shape(lone), shape(lrec), at(lrec, lrec.audit), earlier, nil, nil, w, nil)[0]
				o, ok := ln.Clock()
				switch side {
				case Response:
					if ln.Kind != KindVariable || ln.From.Mock != "mock-app" {
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-app's answer", ln.Kind, o, ln.From, ln.Why)
					}
				default:
					if ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin {
						t.Fatalf("%s Clock=%+v from %+v (%s); want a read of the clock since the test began: the app sent the stamp", ln.Kind, o, ln.From, ln.Why)
					}
				}
			})
		}
	}
}

func TestAClientTestKeepsAWholeCopyOfAnAnswersStampInAnyLayoutOrFormAsThatAnswersVariable(t *testing.T) {
	const month = 30 * 24 * time.Hour
	// One run: the test began at began; the earlier exchange was made at
	// began+1.2 s and holds a stamp to the second; the call copying it has
	// its window from 25 ms after it.
	type run struct{ began time.Time }
	ans := func(r run) time.Time { return r.began.Add(1200 * time.Millisecond) }
	span := func(r run) Span {
		from := ans(r).Add(25 * time.Millisecond)
		return Span{From: from, To: from.Add(200 * time.Millisecond), Began: r.began}
	}
	stamp := func(r run) time.Time { return ans(r).Truncate(time.Second) }
	rec := run{time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	full := run{time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
	secs := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	iso := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	mock := func(name string, at time.Time, reqBody, respBody string) *models.Mock {
		return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/items", Body: reqBody, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: respBody},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	type form struct {
		name string
		js   func(at time.Time) string // the stamp as a JSON value
		leaf func(p string, at time.Time) Leaf
	}
	isoF := form{"an ISO instant", func(at time.Time) string { return `"` + iso(at) + `"` }, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: iso(at)} }}
	epochF := form{"epoch seconds", secs, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }}
	textF := form{"epoch seconds as text", func(at time.Time) string { return `"` + secs(at) + `"` }, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: secs(at)} }}
	typedF := form{"typed to the second", isoF.js, func(p string, at time.Time) Leaf { return InstantLeaf(p, p, at, time.Second) }}
	const p = "body.variables.createdAt"
	encoded := func(at time.Time) string { return `{"q":"` + url.QueryEscape("c="+secs(at)+"&x=1") + `"}` }
	// A recorded payload holds the stamp: in its answer (a dependency's
	// createdAt), or in its request (what the app sent in its previous
	// call); in a field as the copy's form, or as epoch seconds inside a
	// URL-encoded field (a declared form of its text). The payload record's
	// full round made is given too (F's), or not.
	for _, c := range []struct {
		name   string
		answer bool
		body   func(fm form, at time.Time) string
		forms  []form
		both   []bool
	}{
		{"its answer's field", true, func(fm form, at time.Time) string { return `{"createdAt":` + fm.js(at) + `}` }, []form{isoF, epochF, textF, typedF}, []bool{true, false}},
		{"its request's field", false, func(fm form, at time.Time) string { return `{"createdAt":` + fm.js(at) + `}` }, []form{isoF, epochF, textF}, []bool{true, false}},
		{"its request's field", false, func(fm form, at time.Time) string { return `{"createdAt":` + fm.js(at) + `}` }, []form{typedF}, []bool{false}},
		{"its answer's URL-encoded field", true, func(_ form, at time.Time) string { return encoded(at) }, []form{epochF, textF, typedF}, []bool{true, false}},
		// An ISO copy of the epoch only F's payload beside it leads keploy to.
		{"its answer's URL-encoded field", true, func(_ form, at time.Time) string { return encoded(at) }, []form{isoF}, []bool{true}},
		{"its request's URL-encoded field", false, func(_ form, at time.Time) string { return encoded(at) }, []form{epochF, textF}, []bool{true, false}},
	} {
		for _, fm := range c.forms {
			for _, both := range c.both {
				t.Run("a recorded payload/"+c.name+"/"+fm.name+"/F's too="+strconv.FormatBool(both), func(t *testing.T) {
					dc := clientTestCache()
					payload := func(name string, r run) *models.Mock {
						if c.answer {
							return mock(name, ans(r), `{"n":"a"}`, c.body(fm, stamp(r)))
						}
						return mock(name, ans(r), c.body(fm, stamp(r)), `{"ok":1}`)
					}
					raw := []Raw{dc.RawOf(payload("mock-1", rec), true)}
					if both {
						raw = append(raw, dc.RawOf(payload("mock-9", full), true))
					}
					w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
					ln := ClassifyFollowing(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: fm.leaf(p, stamp(full))}}, Shape{Leaves: map[string]Leaf{p: fm.leaf(p, stamp(rec))}}, span(rec).To, nil, raw, nil, w, nil)[0]
					o, _ := ln.Clock()
					if c.answer && (ln.Kind != KindVariable || ln.From.Mock != "mock-1") {
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-1, the answer a replay serves as recorded", ln.Kind, o, ln.From, ln.Why)
					}
					if !c.answer && ln.Kind != KindClockRead {
						t.Fatalf("%s Clock=%+v from %+v (%s); want a read of the clock: the app sent the stamp, and reads its clock again at replay", ln.Kind, o, ln.From, ln.Why)
					}
				})
			}
		}
	}
	// An exchange served leaf by leaf, the stamp at a leaf of its answer or
	// its request, in any layout, copied in any layout.
	for _, side := range []Side{Response, Request} {
		for _, sf := range []form{isoF, epochF, textF, typedF} {
			for _, cf := range []form{isoF, epochF, textF, typedF} {
				t.Run("an exchange served leaf by leaf/its "+string(side)+"/"+sf.name+" copied as "+cf.name, func(t *testing.T) {
					earlier := []Earlier{{Mock: "mock-app", At: ans(rec), Payload: RawOf(mock("mock-app", ans(rec), `{}`, `{}`), true).ServedAs(1), Seen: []Seen{
						{Side: side, Leaf: sf.leaf("body.createdAt", stamp(rec)), Live: sf.leaf("body.createdAt", stamp(full)).Value},
					}}}
					w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
					ln := ClassifyFollowing(clientTestCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: cf.leaf(p, stamp(full))}}, Shape{Leaves: map[string]Leaf{p: cf.leaf(p, stamp(rec))}}, span(rec).To, earlier, nil, nil, w, nil)[0]
					o, _ := ln.Clock()
					if side == Response && (ln.Kind != KindVariable || ln.From.Mock != "mock-app") {
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-app's answer", ln.Kind, o, ln.From, ln.Why)
					}
					if side == Request && ln.Kind != KindClockRead {
						t.Fatalf("%s Clock=%+v from %+v (%s); want a read of the clock: the app sent the stamp", ln.Kind, o, ln.From, ln.Why)
					}
				})
			}
		}
	}
	// A login served leaf by leaf, its Set-Cookie's Expires holding words
	// of the value by chance (a part of the answer, never the value whole):
	// the expiry (the clock plus thirty days) or the clock alone is learned
	// as that read since the test began, as from a recorded payload.
	cookie := func(at time.Time) string {
		return "refresh_token_staging=abc; Path=/; Expires=" + at.Add(month).UTC().Format(time.RFC1123) + "; HttpOnly"
	}
	call := func(r run) time.Time { return r.began.Add(7 * time.Second) }
	cspan := func(r run) Span { return Span{From: call(r).Add(-300 * time.Millisecond), To: call(r), Began: r.began} }
	login := func(r run) time.Time { return r.began.Add(900 * time.Millisecond) }
	for _, off := range []time.Duration{month, 0} {
		t.Run("a login served leaf by leaf/the clock plus "+off.String(), func(t *testing.T) {
			const hp = "header.Set-Cookie"
			m := mock("mock-1", login(rec), `{"u":"a"}`, `{}`)
			m.Spec.HTTPResp.Header["Set-Cookie"] = cookie(login(rec))
			earlier := []Earlier{{Mock: "mock-1", At: login(rec), Payload: RawOf(m, true).ServedAs(1), Seen: []Seen{
				{Side: Response, Leaf: Leaf{Path: hp, Noise: hp, Type: String, Value: cookie(login(rec))}, Live: cookie(login(full))},
			}}}
			lf := func(r run) Leaf {
				return Leaf{Path: p, Noise: p, Type: String, Value: call(r).Add(off).Truncate(time.Second).UTC().Format(time.RFC3339)}
			}
			w := (&Window{Rec: cspan(rec), F: cspan(full)}).ComparesOffsets()
			ln := ClassifyFollowing(clientTestCache(), []string{p}, Shape{Leaves: map[string]Leaf{p: lf(full)}}, Shape{Leaves: map[string]Leaf{p: lf(rec)}}, call(rec), earlier, nil, nil, w, nil)[0]
			o, ok := ln.Clock()
			want := KindClockOffset
			if off == 0 {
				want = KindClockRead
			}
			if ln.Kind != want || !ok || o.Since != models.SinceTestBegin {
				t.Fatalf("%s Clock=%+v from %+v held %q (%s); want %s since the test began: the cookie holds words of it by chance", ln.Kind, o, ln.From, ln.From.Held, ln.Why, want)
			}
		})
	}
}

func TestAClientTestLearnsACopyOfAnAnswersTypedStampTheFullRoundRefutesAsAReadSinceItsTestBegan(t *testing.T) {
	// One run of the app's own previous call, an exchange F served leaf by
	// leaf, and of the call copying its stamp: the stamp read at
	// began+created, the call's window from began+insert to began+audit.
	type run struct {
		began                  time.Time
		created, insert, audit time.Duration
	}
	at := func(r run, d time.Duration) time.Time { return r.began.Add(d) }
	span := func(r run) Span { return Span{From: at(r, r.insert), To: at(r, r.audit), Began: r.began} }
	rec := run{began: time.Date(2026, 10, 26, 1, 41, 6, 300_000_000, time.UTC), created: time.Millisecond, insert: 2 * time.Millisecond, audit: 4 * time.Millisecond}
	full := run{began: time.Date(2026, 10, 26, 1, 42, 15, 0, time.UTC), created: 100 * time.Millisecond, insert: 150 * time.Millisecond, audit: 400 * time.Millisecond}
	const src, copy = "body.createdAt", "body.cmd.documents.0.app.createdAt"
	insertAt := at(rec, rec.insert)
	insert := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: insertAt, ResTimestampMock: insertAt.Add(time.Millisecond)}}, true)
	recAt, fullAt := at(rec, rec.created), at(full, full.created)
	seconds := func(p string, a time.Time) Leaf { return InstantLeaf(p, p, a, time.Second) }
	millis := func(p string, a time.Time) Leaf { return InstantLeaf(p, p, a, time.Millisecond) }
	wall := func(p string, a time.Time) Leaf { return WallClockLeaf(p, p, a.UTC(), time.Second) }
	iso := func(p string, a time.Time) Leaf {
		return Leaf{Path: p, Noise: p, Type: String, Value: a.UTC().Truncate(time.Second).Format(time.RFC3339)}
	}
	for _, c := range []struct {
		name string
		leaf func(p string, a time.Time) Leaf
		seen func(mk func(p string, a time.Time) Leaf) []Seen
		read bool
	}{
		// The full round refutes the copy: F's answer held another moment,
		// or the recorded one again, while the call sent its own clock.
		{"the answer held it as recorded and F's another moment", seconds, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, fullAt.Add(5*time.Second)).Value}}
		}, true},
		{"the answer held it in both runs", seconds, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, recAt).Value}}
		}, true},
		{"a wall clock the answer held as recorded and F's another moment", wall, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, fullAt.Add(5*time.Second)).Value}}
		}, true},
		{"a wall clock the answer held in both runs", wall, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, recAt).Value}}
		}, true},
		{"the request held it in both runs", seconds, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Request, Leaf: mk(src, recAt), Live: mk(src, recAt).Value}}
		}, true},
		// The echo's place is what the app sent (a constant), F's moment
		// held by an answer that recorded another: no answer's whole copy.
		{"a text whose echo stands in what the app sent", iso, func(func(string, time.Time) Leaf) []Seen {
			return []Seen{
				{Side: Request, Leaf: seconds("body.sent", recAt), Live: seconds("body.sent", recAt).Value},
				{Side: Response, Leaf: seconds("body.other", recAt.Add(-7*time.Second)), Live: seconds("body.other", fullAt).Value},
			}
		}, true},
		// Controls: F's answer held the instant F saw in the value too, a
		// whole copy in both runs, which stays the answer's variable.
		{"the answer held it in both runs' values", seconds, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, fullAt).Value}}
		}, false},
		{"a wall clock the answer held in both runs' values", wall, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, fullAt).Value}}
		}, false},
		{"a millisecond instant the answer held in both runs' values", millis, func(mk func(string, time.Time) Leaf) []Seen {
			return []Seen{{Side: Response, Leaf: mk(src, recAt), Live: mk(src, fullAt).Value}}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			earlier := []Earlier{{Mock: "mock-app", At: insertAt, Own: true, Payload: insert.ServedAs(1), Seen: c.seen(c.leaf)}}
			shape := func(r run) Shape { return Shape{Leaves: map[string]Leaf{copy: c.leaf(copy, at(r, r.created))}} }
			w := &Window{Rec: span(rec), F: span(full)}
			ln := ClassifyFollowing(clientTestCache(), []string{copy}, shape(full), shape(rec), at(rec, rec.audit), earlier, nil, nil, w, nil)[0]
			o, ok := ln.Clock()
			if !c.read {
				if ln.Kind != KindVariable || ln.From.Mock != "mock-app" || ln.From.Side != Response {
					t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-app's answer, which held the value in both runs", ln.Kind, o, ln.From, ln.Why)
				}
				return
			}
			if ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin {
				t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want a read of the clock since the test began: the full round refutes the copy, and the read holds in both runs", ln.Kind, o, ln.From, ln.Why)
			}
		})
	}
}

func TestAClientTestLearnsACopyOfARecordingsAnswerStampTheFullRoundRefutesAsAReadSinceItsTestBegan(t *testing.T) {
	// One run of a recording whose answer holds a stamp, read at ans, and of
	// the call that sends a value recorded equal to it: the call's window
	// holds the value at the second's cut in both runs.
	type run struct{ began time.Time }
	ans := func(r run) time.Time { return r.began.Add(1200 * time.Millisecond) }
	span := func(r run) Span {
		from := ans(r).Add(25 * time.Millisecond)
		return Span{From: from, To: from.Add(200 * time.Millisecond), Began: r.began}
	}
	stamp := func(r run) time.Time { return ans(r).Truncate(time.Second) }
	rec := run{time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	full := run{time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
	// answer is the recording's answer: its body and its headers.
	type answer struct {
		body   string
		header map[string]string
	}
	mock := func(at time.Time, a answer) *models.Mock {
		if a.header == nil {
			a.header = map[string]string{}
		}
		return &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/items", Body: `{"n":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: a.header, Body: a.body},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	type form struct {
		name string
		// text is the stamp's text; quoted: a JSON string.
		text   func(at time.Time) string
		quoted bool
		leaf   func(p string, at time.Time) Leaf
	}
	isoText := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	epochText := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	iso := form{"an ISO instant", isoText, true, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: isoText(at)} }}
	epoch := form{"epoch seconds", epochText, false, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: epochText(at)} }}
	typed := form{"an instant typed to the second", isoText, true, func(p string, at time.Time) Leaf { return InstantLeaf(p, p, at, time.Second) }}
	// in is where the answer holds the stamp: its body as a field of its
	// own, or inside a URL-encoded field (a declared form of its text), or a
	// header of its own.
	type in struct {
		name string
		hold func(f form, at time.Time) answer
	}
	plain := in{"", func(f form, at time.Time) answer {
		if f.quoted {
			return answer{body: `{"createdAt":"` + f.text(at) + `"}`}
		}
		return answer{body: `{"createdAt":` + f.text(at) + `}`}
	}}
	encoded := in{" inside a URL-encoded field", func(f form, at time.Time) answer {
		return answer{body: `{"next":"` + url.QueryEscape("at="+f.text(at)) + `"}`}
	}}
	header := in{" in a header", func(f form, at time.Time) answer {
		return answer{body: `{"id":"a"}`, header: map[string]string{"X-Created-At": f.text(at)}}
	}}
	const p = "body.variables.createdAt"
	for _, c := range []struct {
		name string
		// view is what record's full round gave the application at the
		// recording, x as recorded; nil where it gave no view.
		view func(x Raw, hold func(at time.Time) answer) Raw
		read bool
	}{
		// The full round refutes the copy: its view of the answer held
		// another moment, or the recorded one again, while the call sent its
		// own clock, or no stamp (in a header: it lacked the field).
		{"the full round's answer held another moment", func(x Raw, hold func(time.Time) answer) Raw {
			return x.ServedAs(1).ServedLive(RawOf(mock(ans(full), hold(stamp(full).Add(-7*time.Second))), true))
		}, true},
		{"the full round served the answer as recorded", func(x Raw, _ func(time.Time) answer) Raw {
			return x.ServedAs(1).ServedAsRecorded()
		}, true},
		{"the full round's answer held no stamp", func(x Raw, _ func(time.Time) answer) Raw {
			return x.ServedAs(1).ServedLive(RawOf(mock(ans(full), answer{body: `{"id":"a"}`}), true))
		}, true},
		// Controls: the full round's answer held the value it sent too (a
		// whole copy in both runs), or keploy has no view of it: the
		// answer's variable.
		{"the full round's answer held the value", func(x Raw, hold func(time.Time) answer) Raw {
			return x.ServedAs(1).ServedLive(RawOf(mock(ans(full), hold(stamp(full))), true))
		}, false},
		{"the full round gave no view of the answer", nil, false},
	} {
		for _, pair := range [][2]form{{iso, iso}, {epoch, epoch}, {iso, typed}, {epoch, typed}} {
			for _, lay := range []in{plain, encoded, header} {
				src, cp := pair[0], pair[1]
				t.Run(c.name+"/"+src.name+lay.name+" copied as "+cp.name, func(t *testing.T) {
					dc := clientTestCache()
					hold := func(a time.Time) answer { return lay.hold(src, a) }
					x := dc.RawOf(mock(ans(rec), hold(stamp(rec))), true)
					if c.view != nil {
						x = c.view(x, hold)
					}
					w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
					ln := ClassifyFollowing(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: cp.leaf(p, stamp(full))}}, Shape{Leaves: map[string]Leaf{p: cp.leaf(p, stamp(rec))}}, span(rec).To, nil, []Raw{x}, nil, w, nil)[0]
					o, ok := ln.Clock()
					if !c.read {
						if ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
							t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-1's answer: no run refutes the copy", ln.Kind, o, ln.From, ln.Why)
						}
						return
					}
					if ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin {
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want a read of the clock since the test began: the full round refutes the copy, and the read holds in both runs", ln.Kind, o, ln.From, ln.Why)
					}
				})
			}
		}
	}
}

func TestAClientTestKeepsAnEscapedCopyOfARecordingsAnswerStampAsThatAnswersVariable(t *testing.T) {
	// The call sends, URL-encoded, the stamp a recording's answer gave the
	// application, and the full round's answer held the value it sent too,
	// or keploy has no view of it: a whole copy in both runs, which stays
	// the answer's variable. The decoded value's source site marks no copy
	// (Place.givenWhole): the copy rule's second classification reads no
	// escaped text as a read, so the variable stands without the mark. A
	// change that reads one as a read must mark this copy there again.
	type run struct{ began time.Time }
	ans := func(r run) time.Time { return r.began.Add(1200 * time.Millisecond) }
	span := func(r run) Span {
		from := ans(r).Add(25 * time.Millisecond)
		return Span{From: from, To: from.Add(200 * time.Millisecond), Began: r.began}
	}
	stamp := func(r run) time.Time { return ans(r).Truncate(time.Second) }
	rec := run{time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	full := run{time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
	iso := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	mock := func(at time.Time, body string) *models.Mock {
		return &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/items", Body: `{"n":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: body},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	hold := func(at time.Time) string { return `{"createdAt":"` + iso(at) + `"}` }
	const p = "body.variables.since"
	sent := func(at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: url.QueryEscape(iso(at))} }
	for _, c := range []struct {
		name string
		view func(x Raw) Raw
	}{
		{"the full round's answer held the value", func(x Raw) Raw {
			return x.ServedAs(1).ServedLive(RawOf(mock(ans(full), hold(stamp(full))), true))
		}},
		{"the full round gave no view of the answer", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			dc := clientTestCache()
			x := dc.RawOf(mock(ans(rec), hold(stamp(rec))), true)
			if c.view != nil {
				x = c.view(x)
			}
			if v := sent(stamp(rec)).Value; !strings.Contains(v, "%3A") {
				t.Fatalf("the call's value %q holds no escape", v)
			}
			w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
			ln := ClassifyFollowing(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: sent(stamp(full))}}, Shape{Leaves: map[string]Leaf{p: sent(stamp(rec))}}, span(rec).To, nil, []Raw{x}, nil, w, nil)[0]
			if ln.Kind != KindVariable || ln.From.Mock != "mock-1" || ln.From.Held != iso(stamp(rec)) {
				o, _ := ln.Clock()
				t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-1's answer, holding %s: the call copies it whole in both runs", ln.Kind, o, ln.From, ln.Why, iso(stamp(rec)))
			}
		})
	}
}

func TestAClientTestKeepsAWholeCopyOfAnAnswersSecondFieldHoldingTheStampAsThatAnswersVariable(t *testing.T) {
	// A recording's answer holds the recorded stamp at two fields, an
	// upsert's pair (createdAt and updatedAt, equal where record made the
	// entity), and the call sends a value recorded equal to it: the call's
	// window holds the value at the second's cut in both runs. Where record's
	// full round found the entity, its view holds an older moment at the
	// first field and the value the call sent at the second: the call copies
	// the second field whole in both runs, which stays the answer's variable
	// (a replay serves the answer as recorded, and the copy holds it), named
	// at the second field (in headers; an HTTP body is one field, named the
	// body). Where no field of the answer that held the stamp held, in the
	// full round, the value it sent, the full round refutes the copy, and
	// the read of the clock since the test began holds in both runs.
	type run struct{ began time.Time }
	ans := func(r run) time.Time { return r.began.Add(1200 * time.Millisecond) }
	span := func(r run) Span {
		from := ans(r).Add(25 * time.Millisecond)
		return Span{From: from, To: from.Add(200 * time.Millisecond), Began: r.began}
	}
	stamp := func(r run) time.Time { return ans(r).Truncate(time.Second) }
	rec := run{time.Date(2026, 10, 27, 16, 59, 3, 100_000_000, time.UTC)}
	full := run{time.Date(2026, 10, 27, 17, 1, 19, 300_000_000, time.UTC)}
	old := time.Date(2026, 10, 27, 16, 40, 0, 0, time.UTC)
	type answer struct {
		body   string
		header map[string]string
	}
	mock := func(at time.Time, a answer) *models.Mock {
		if a.header == nil {
			a.header = map[string]string{}
		}
		return &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/items", Body: `{"n":"a"}`, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: a.header, Body: a.body},
			ReqTimestampMock: at, ResTimestampMock: at.Add(20 * time.Millisecond),
		}}
	}
	type form struct {
		name   string
		text   func(at time.Time) string
		quoted bool
		leaf   func(p string, at time.Time) Leaf
	}
	isoText := func(at time.Time) string { return at.UTC().Format(time.RFC3339) }
	epochText := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	iso := form{"an ISO instant", isoText, true, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: isoText(at)} }}
	epoch := form{"epoch seconds", epochText, false, func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: epochText(at)} }}
	typed := form{"an instant typed to the second", isoText, true, func(p string, at time.Time) Leaf { return InstantLeaf(p, p, at, time.Second) }}
	// in is where the answer holds the pair (first, second); second is the
	// end of the name of the field that holds the second. Two headers are
	// two fields of a recording. An HTTP body is one field, its text whole:
	// a body that holds the pair holds both in that field.
	type in struct {
		name   string
		second string
		hold   func(f form, first, second time.Time) answer
	}
	headers := in{"two headers", "X-Updated-At", func(f form, a, b time.Time) answer {
		return answer{body: `{"id":"a"}`, header: map[string]string{"X-Created-At": f.text(a), "X-Updated-At": f.text(b)}}
	}}
	body := in{"a body's two fields", "spec.resp.body", func(f form, a, b time.Time) answer {
		q := func(at time.Time) string {
			if f.quoted {
				return `"` + f.text(at) + `"`
			}
			return f.text(at)
		}
		return answer{body: `{"createdAt":` + q(a) + `,"updatedAt":` + q(b) + `}`}
	}}
	const p = "body.variables.lastModified"
	for _, c := range []struct {
		name string
		// recorded is the pair record's answer held; view the pair record's
		// full round gave the application, nil where it gave no view.
		recorded func() (time.Time, time.Time)
		view     func() (time.Time, time.Time)
		read     bool
		// fields: only where the pair stands in two fields of the
		// recording (headers).
		fields bool
	}{
		{"the full round's answer held an older moment first and the value second",
			func() (time.Time, time.Time) { return stamp(rec), stamp(rec) },
			func() (time.Time, time.Time) { return old, stamp(full) }, false, false},
		// The full round refutes the copy: no field of its answer held the
		// value it sent at a field that held it at record.
		{"the full round's answer held older moments at both",
			func() (time.Time, time.Time) { return stamp(rec), stamp(rec) },
			func() (time.Time, time.Time) { return old, old }, true, false},
		// The full round's view is read field by field, never whole: its
		// value at another field than the one that held the stamp at record
		// is no copy in both runs. (In a body, one field, it is read at that
		// field: a stated limit.)
		{"the recording held the stamp first only and the full round's answer the value second only",
			func() (time.Time, time.Time) { return stamp(rec), old },
			func() (time.Time, time.Time) { return old, stamp(full) }, true, true},
		// Controls: the full round's answer held the value at both, or
		// keploy has no view of it: the answer's variable.
		{"the full round's answer held the value at both",
			func() (time.Time, time.Time) { return stamp(rec), stamp(rec) },
			func() (time.Time, time.Time) { return stamp(full), stamp(full) }, false, false},
		{"the full round gave no view of the answer",
			func() (time.Time, time.Time) { return stamp(rec), stamp(rec) }, nil, false, false},
	} {
		for _, pair := range [][2]form{{iso, iso}, {epoch, epoch}, {iso, typed}, {epoch, typed}} {
			for _, lay := range []in{headers, body} {
				if c.fields && lay.name != headers.name {
					continue
				}
				src, cp := pair[0], pair[1]
				t.Run(c.name+"/"+lay.name+" of "+src.name+" copied as "+cp.name, func(t *testing.T) {
					dc := clientTestCache()
					a, b := c.recorded()
					x := dc.RawOf(mock(ans(rec), lay.hold(src, a, b)), true)
					if c.view != nil {
						a, b := c.view()
						x = x.ServedAs(1).ServedLive(RawOf(mock(ans(full), lay.hold(src, a, b)), true))
					}
					w := (&Window{Rec: span(rec), F: span(full)}).ComparesOffsets()
					ln := ClassifyFollowing(dc, []string{p}, Shape{Leaves: map[string]Leaf{p: cp.leaf(p, stamp(full))}}, Shape{Leaves: map[string]Leaf{p: cp.leaf(p, stamp(rec))}}, span(rec).To, nil, []Raw{x}, nil, w, nil)[0]
					o, ok := ln.Clock()
					if c.read {
						if ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin {
							t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want a read of the clock since the test began: the full round refutes the copy, and the read holds in both runs", ln.Kind, o, ln.From, ln.Why)
						}
						return
					}
					if ln.Kind != KindVariable || ln.From.Mock != "mock-1" {
						t.Fatalf("FALSE RED: %s Clock=%+v from %+v (%s); want the variable of mock-1's answer: no run refutes the copy", ln.Kind, o, ln.From, ln.Why)
					}
					if c.view == nil {
						return
					}
					if va, vb := c.view(); va != vb && !strings.HasSuffix(ln.From.Field, lay.second) {
						t.Fatalf("the variable of %s; want it named at the field that held the value in both runs, %s", ln.From.Field, lay.second)
					}
				})
			}
		}
	}
}

// A slow full round must not turn a read of the clock into a copy (founder,
// 2026-10-30: two calls that read one clock in one second are a
// coincidence, not dataflow). api-server's app create, as the T2 recording
// shows it: the vault PUT that stamps the access token used reads the clock
// right before its call (lastUsedAt, epoch seconds); the handler read its
// clock at its start for the app's createdAt, and the audit insert sends that
// several calls later, beside the PUT's value in the same second. In the
// recording every call is a millisecond apart, so the audit's own window
// (from the previous call) holds createdAt. Record's full round ran slow:
// the calls between the handler's start and the audit took most of a
// second, so the audit's window opens in the next second, and only the
// window since the test began holds createdAt, as it does the audit's
// auditedAt, which keploy reads there. Learned as a copy of the PUT's value,
// the audit bound the PUT's place (a variable and a clock read at one
// place), and a later test whose own copy fell in that second bound the
// run's variable there, so the PUT, served aside to a later test, was a
// miss. The same holds where the audit is a later test's call, and where a
// later test's HTTP call carries it (the run's variable re-read in the same
// windows). Each is learned as a read of the clock since its test began, so a replay that
// reads the two a second apart passes, and the PUT's place keeps its clock
// read alone.
func TestASlowFullRoundLearnsASameSecondCopyAsAReadSinceItsTestBegan(t *testing.T) {
	const (
		lastUsed = "body.data.lastUsedAt"
		created  = "body.docs.msgs.0.app.createdAt.$numberLong"
	)
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	num := func(p string, at time.Time) Leaf { return Leaf{Path: p, Noise: p, Type: Number, Value: secs(at)} }
	// put: the PUT's read and call; read: the handler's read of createdAt;
	// prev and call: the audit's previous call and its own.
	type run struct {
		began, put, read, prev, call time.Time
	}
	recB := time.Date(2026, 10, 30, 11, 19, 15, 340_000_000, time.UTC)
	rec := run{recB, recB.Add(14 * time.Millisecond), recB.Add(15 * time.Millisecond), recB.Add(21 * time.Millisecond), recB.Add(22 * time.Millisecond)}
	fB := recB.Add(FullRoundLead).Truncate(time.Second).Add(185 * time.Millisecond)
	slow := run{fB, fB.Add(100 * time.Millisecond), fB.Add(150 * time.Millisecond), fB.Add(900 * time.Millisecond), fB.Add(950 * time.Millisecond)}
	if secs(slow.put) != secs(slow.read) || secs(slow.read) == secs(slow.prev) || secs(rec.read) != secs(rec.prev) {
		t.Fatal("the row needs the slow round to read both in one second and open the audit's window in the next, and the recording to hold the read in that window")
	}
	// A full round as fast as the recording: the call's own window holds the
	// read in both runs, and the HTTP call's read is checked in that window.
	fast := run{fB, fB.Add(14 * time.Millisecond), fB.Add(15 * time.Millisecond), fB.Add(21 * time.Millisecond), fB.Add(22 * time.Millisecond)}
	for _, c := range []struct {
		name    string
		ownTest bool
		http    bool // an HTTP call: the source not followed (ClassifyIn), the run's variable re-read alone
		f       run
		since   string
	}{
		{"the audit in the PUT's test", true, false, slow, models.SinceTestBegin},
		{"the audit in a later test", false, false, slow, models.SinceTestBegin},
		{"an HTTP call in a later test", false, true, slow, models.SinceTestBegin},
		{"an HTTP call in a later test, a fast full round", false, true, fast, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !c.ownTest {
				// An earlier test's call is a place of the run, read
				// under the run's rule, only under the proven switch.
				t.Setenv(provenswitch.Env, "1")
			}
			put := RawOf(&models.Mock{Name: "mock-198", Spec: models.MockSpec{ReqTimestampMock: rec.put, ResTimestampMock: rec.put.Add(time.Millisecond)}}, true)
			earlier := []Earlier{{Mock: "mock-198", At: rec.put, Own: c.ownTest, Run: !c.ownTest, Payload: put.ServedAs(1), Seen: []Seen{
				{Side: Request, Leaf: num(lastUsed, rec.put), Live: secs(c.f.put), Place: lastUsed},
			}}}
			r, f := rec, c.f
			if !c.ownTest {
				// The later test began after the PUT, in its second.
				r.began, f.began = rec.put.Add(2*time.Millisecond), c.f.put.Add(10*time.Millisecond)
			}
			w := &Window{Rec: Span{From: r.prev, To: r.call, Began: r.began}, F: Span{From: f.prev, To: f.call, Began: f.began}}
			shape := func(at time.Time) Shape { return Shape{Leaves: map[string]Leaf{created: num(created, at)}} }
			classify := func(dc *DriftCache, drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf, w *Window) []Learned {
				return ClassifyFollowing(dc, drift, live, rec, at, earlier, raw, resp, w, nil)
			}
			if c.http {
				classify = ClassifyIn
			}
			ln := classify(clientTestCache(), []string{created}, shape(f.read), shape(r.read), r.call, earlier, nil, nil, w)[0]
			if o, ok := ln.Clock(); ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != c.since || ln.From.Mock != "" {
				t.Fatalf("createdAt, read the PUT's second, is %s Read=%+v from %+v (%s); want a read of the clock checked since %q, no copy of mock-198", ln.Kind, ln.Read, ln.From, ln.Why, c.since)
			}
		})
	}
}
