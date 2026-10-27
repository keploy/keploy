package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
// Where only the copy holds it stays the earlier call's variable: a copy its
// own window does not hold (the app read createdAt two seconds before the
// insert), or a call whose window knows no test begin a replay could check
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
	insert := RawOf(&models.Mock{Name: "mock-app", Spec: models.MockSpec{ReqTimestampMock: insertAt, ResTimestampMock: insertAt.Add(time.Millisecond)}}, true)
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
	learn := func(fm form, f copyOrReadRun, began, following bool) map[string]Learned {
		earlier := []Earlier{{Mock: "mock-app", At: insertAt, Own: true, Payload: insert.ServedAs(1), Seen: []Seen{
			{Side: Request, Leaf: fm.leaf("body.cmd.documents.0.createdAt", rec.at(rec.created)), Live: fm.leaf("body.cmd.documents.0.createdAt", f.at(f.created)).Value},
		}}}
		shape := func(r copyOrReadRun) Shape {
			return Shape{Leaves: map[string]Leaf{created: fm.leaf(created, r.at(r.created)), audited: fm.leaf(audited, r.at(r.audited))}}
		}
		w := &Window{Rec: rec.window(began), F: f.window(began)}
		classify := ClassifyGiven
		if following {
			classify = ClassifyFollowing
		}
		got := map[string]Learned{}
		for _, ln := range classify(clientTestCache(), []string{created, audited}, shape(f), shape(rec), rec.at(rec.audit), earlier, nil, nil, w, nil) {
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
				got := learn(fm, fr.f, true, true)
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
		t.Run(fm.name+"/only the copy holds", func(t *testing.T) {
			// The app read createdAt two seconds before the insert: the
			// audit's window holds no copy of it.
			early := one
			early.created, early.insert, early.audited, early.audit = 100*time.Millisecond, 2150*time.Millisecond, 2300*time.Millisecond, 2400*time.Millisecond
			for _, c := range []struct {
				name             string
				f                copyOrReadRun
				began, following bool
			}{
				{"a copy its own window does not hold", early, true, true},
				{"a call whose window knows no test begin", one, false, true},
				{"the rule's absence (ClassifyGiven)", one, true, false},
			} {
				got := learn(fm, c.f, c.began, c.following)
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
