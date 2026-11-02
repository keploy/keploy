package learn

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// api-server's start-up janitors (its e2e suite recorded locally, keploy main
// 0ee73b13b, 1 run in 6):
// the notifications janitor sends "createdAt < now" (distinct on
// notifications) and the branch janitor "expires_at <= now" (find on
// branches), each reading time.Now().Unix() on its own, on two Mongo
// connections at once, in one second. Record sent the find first; record's
// full round (F) sent the distinct first, before it served the find. Before
// the fix the distinct's second was followed as a value from the find's
// recording, which F had not served before the call: integrations refused it
// ("its source recording, mock-17, was never served"), TestAppCRUD failed
// round F, and every later test moved its calls.
//
// A recording F had not served before the call is no source of an instant
// the application sent there: in F the application sent the call before it
// sent that one (or it missed), and two reads of the clock in one second
// are a coincidence, not dataflow. The value is judged as the call's own
// read, checked in its call's window at every replay, and the reason names
// the recording set aside.
func TestAStartUpReadOfTheClockIsNotFollowedFromASiblingFHadNotServed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	at := eRecAt
	recT := strconv.FormatInt(at.Unix(), 10)
	fAt := at.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	// find is the branch janitor's recording: its request holds sent (the
	// second it read), and its answer answer, recorded before before the
	// call and answered after (the two in flight together) or before.
	find := func(sent, answer string, before, answered time.Duration) Raw {
		req := at.Add(-before)
		return RawOf(&models.Mock{Name: "mock-17", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"find":"branches","filter":{"status":{"$in":["merged","closed"]},"expires_at":{"$lte":{"$numberLong":"` + sent + `"},"$exists":true}},"$db":"api-server"} }`,
			}}}},
			MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"firstBatch":[` + answer + `],"id":{"$numberLong":"0"},"ns":"api-server.branches"},"ok":{"$numberDouble":"1.0"}} }`,
			}}}},
			ReqTimestampMock: req, ResTimestampMock: at.Add(answered),
		}}, false)
	}
	type classify func(dc *DriftCache, raw []Raw, w *Window) Learned
	for name, c := range map[string]classify{
		"ClassifyFollowing": func(dc *DriftCache, raw []Raw, w *Window) Learned {
			return ClassifyFollowing(dc, []string{path}, str(liveT), str(recT), at, nil, raw, nil, w, nil)[0]
		},
		"ClassifyGiven": func(dc *DriftCache, raw []Raw, w *Window) Learned {
			return ClassifyGiven(dc, []string{path}, str(liveT), str(recT), at, nil, raw, nil, w, nil)[0]
		},
	} {
		for _, mode := range []struct {
			name string
			dc   func() *DriftCache
		}{{"client test", clientTestCache}, {"keploy test", clockedDrifts}} {
			// The row: F had not served the find before the call, whether the
			// recording gave its answer after the call's request (in flight
			// together, api-server's shape) or before.
			for _, shape := range []struct {
				what     string
				answered time.Duration
			}{{"in flight together", 400 * time.Microsecond}, {"answered before the call", -100 * time.Microsecond}} {
				got := c(mode.dc(), []Raw{find(recT, "", 600*time.Microsecond, shape.answered)}, w)
				if !plainRead(got) || got.From.Mock != "" {
					t.Errorf("%s, %s, %s: a start-up read of the clock beside a sibling F had not served: %s from %q (%q); want the call's own read of the clock", name, mode.name, shape.what, got.Kind, got.From.Mock, got.Why)
					continue
				}
				// As it is with no such recording.
				if base := c(mode.dc(), nil, w); base.Kind != got.Kind || fmt.Sprint(base.Read) != fmt.Sprint(got.Read) {
					t.Errorf("%s, %s, %s: %s read %v; with no such recording %s read %v", name, mode.name, shape.what, got.Kind, got.Read, base.Kind, base.Read)
				}
				// ClassifyGiven reads no copy rule (fol nil), so it names no
				// place it went past; ClassifyFollowing names it.
				if name == "ClassifyFollowing" && (!strings.Contains(got.Refuted, "mock-17") || !strings.Contains(got.Refuted, "had not served")) {
					t.Errorf("%s, %s, %s: the reason does not name the recording set aside and why: %q", name, mode.name, shape.what, got.Refuted)
				}
			}
		}
	}
}

// What the rule above does not reach stays the variable of the recording,
// as before it: a recording F served before the call (with no view of it
// stated), an instant a recording's answer gave the application (followed
// whole, a founder decision), a recording whose text holds F's value too
// (read as the echo of what F gave, echoRaw, before the rule is reached), and
// every value without the proven switch.
func TestAStartUpReadOfTheClockStaysFollowedWhereTheUnservedRuleDoesNotReach(t *testing.T) {
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	at := eRecAt
	recT := strconv.FormatInt(at.Unix(), 10)
	fAt := at.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	mongo := func(name, req, res string) Raw {
		return RawOf(&models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{req}}}},
			MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
				`{ SectionSingle msg: {"cursor":{"firstBatch":[` + res + `],"id":{"$numberLong":"0"},"ns":"api-server.x"},"ok":{"$numberDouble":"1.0"}} }`,
			}}}},
			ReqTimestampMock: at.Add(-600 * time.Microsecond), ResTimestampMock: at.Add(400 * time.Microsecond),
		}}, false)
	}
	sent := func(v string) string {
		return `{ SectionSingle msg: {"find":"branches","filter":{"expires_at":{"$lte":{"$numberLong":"` + v + `"}}},"$db":"api-server"} }`
	}
	const ping = `{ SectionSingle msg: {"find":"leases","filter":{},"$db":"api-server"} }`
	elsewhere := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(time.Hour), To: fAt.Add(time.Hour + time.Millisecond)}}
	for _, g := range []struct {
		what    string
		switchV string
		raw     Raw
		w       *Window
	}{
		{"F served the find before the call, no view stated", "1", mongo("mock-17", sent(recT), "").ServedAs(1), w},
		{"the find's answer gave the application the instant", "1", mongo("mock-17", ping, `{"expires_at":{"$numberLong":"`+recT+`"}}`), w},
		{"the find's recorded text holds F's second too", "1", mongo("mock-17", sent(recT), `{"n":{"$numberLong":"`+liveT+`"}}`), w},
		{"the proven switch off", "", mongo("mock-17", sent(recT), ""), w},
	} {
		t.Run(g.what, func(t *testing.T) {
			t.Setenv(provenswitch.Env, g.switchV)
			for _, dc := range []func() *DriftCache{clientTestCache, clockedDrifts} {
				got := ClassifyFollowing(dc(), []string{path}, str(liveT), str(recT), at, nil, []Raw{g.raw}, nil, g.w, nil)[0]
				if got.Kind != KindVariable || got.From.Mock != "mock-17" {
					t.Errorf("%s from %q (%q); want the variable of mock-17, as before", got.Kind, got.From.Mock, got.Why)
				}
			}
		})
	}
	// F's window an hour from F's second. A client test's clock set reads
	// the window, which shows no read of the clock: nothing the rule reads,
	// so the variable stays. Keploy test's reads the lead the value moved
	// by, and learns it as it does with no such recording: a read of the
	// clock, which every replay checks in its call's span; never noise,
	// which would pass any value there. (Keploy main follows the value from
	// mock-17 here, a variable of a recording F had not served, which every
	// adapter refuses: a false red, never a false green. The row guards the
	// rule's own verdict.)
	t.Setenv(provenswitch.Env, "1")
	far := mongo("mock-17", sent(recT), "")
	if got := ClassifyFollowing(clientTestCache(), []string{path}, str(liveT), str(recT), at, nil, []Raw{far}, nil, elsewhere, nil)[0]; got.Kind != KindVariable || got.From.Mock != "mock-17" {
		t.Errorf("client test, the call's window an hour from F's second: %s from %q (%q); want the variable of mock-17, as before", got.Kind, got.From.Mock, got.Why)
	}
	got := ClassifyFollowing(clockedDrifts(), []string{path}, str(liveT), str(recT), at, nil, []Raw{far}, nil, elsewhere, nil)[0]
	base := ClassifyFollowing(clockedDrifts(), []string{path}, str(liveT), str(recT), at, nil, nil, nil, elsewhere, nil)[0]
	if got.Kind == KindNoise || got.Kind != base.Kind || fmt.Sprint(got.Read) != fmt.Sprint(base.Read) {
		t.Errorf("keploy test, the call's window an hour from F's second: %s read %v (%q); want a read of the clock as with no such recording (%s read %v), never noise, which passes any value", got.Kind, got.Read, got.Why, base.Kind, base.Read)
	}
}

// api-server's e2e suite, round F on keploy main + the fix above: the branch
// janitor's find (mock-18) reached keploy at 14:58:09.9868 on the app's
// clock, and the store's consume door told its serve 22 ms later, after the
// learn step, at :10.0089. The notifications janitor's distinct (mock-23),
// sent next on the same connection with its own read of :09, had an F window
// from that consume, [:10.0089, :10.0111], which holds no read of :09, and
// integrations refused it. Record's half of the window starts at the
// previous call's request (PreviousCall), so F's starts where the previous
// call reached keploy (its request's lift), not where the store consumed it.
func TestALaterCallsWindowStartsWhereTheCallBeforeItReachedKeploy(t *testing.T) {
	find := &models.Mock{Name: "mock-18", Kind: models.Mongo}
	tr := NewTracker()
	fA := tr.Lift()
	lifted := time.Now()
	time.Sleep(2 * time.Millisecond) // the learn step, before the consume door
	tr.ServedMockIn(find, fA)
	fA.Land()
	arrived := time.Now() // the distinct reaches keploy
	// The app's second turns between the find's lift and its consume.
	turn := time.Date(2026, 10, 31, 14, 58, 10, 0, time.UTC)
	offset := turn.Sub(lifted.Add(time.Millisecond))
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r.Add(offset), true })
	s, ok := tr.LiveSpan(arrived)
	if !ok {
		t.Fatal("no span for the distinct")
	}
	// The distinct sent the second :09, read beside the find's.
	if !s.From.Before(turn) || s.To.Before(turn) {
		t.Fatalf("the distinct's span %s .. %s holds no instant of the second :09 its read sent; want it from the find's lift", s.From.UTC().Format("15:04:05.000000"), s.To.UTC().Format("15:04:05.000000"))
	}
	if s.From.After(lifted.Add(offset)) {
		t.Fatalf("the distinct's span starts at %s, after the find reached keploy (%s)", s.From.UTC().Format("15:04:05.000000"), lifted.Add(offset).UTC().Format("15:04:05.000000"))
	}
}

// The bound is record's previous call mirrored: the latest instant a call
// reached keploy among those the store served before this call reached it.
// A call aloft across this one's arrival and served after it is another
// flow's (PreviousCall: answered after the call was sent), so its lift does
// not bound this call, whichever was lifted first; of two served before, the
// later arrival bounds it, whichever was served last; and a serve told in no
// request of the application's is bounded where it was made, as before.
func TestALaterCallsWindowStartsAtTheLatestArrivalServedBeforeIt(t *testing.T) {
	m := func(n string) *models.Mock { return &models.Mock{Name: n, Kind: models.Mongo} }
	identity := func(r time.Time) (time.Time, bool) { return r, true }

	t.Run("a call served after this one arrived does not bound it", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		before := time.Now().Add(-time.Second)
		tr.ServedMockAt(m("mock-10"), before)
		fS := tr.Lift() // aloft on another connection
		time.Sleep(time.Millisecond)
		arrived := time.Now()
		time.Sleep(time.Millisecond)
		tr.ServedMockIn(m("mock-11"), fS) // served after the call arrived
		s, ok := tr.LiveSpan(arrived)
		if !ok || !s.From.Equal(before) {
			t.Fatalf("span from %s (ok %v); want from the serve made before the call arrived, %s", s.From, ok, before)
		}
	})

	t.Run("of two served before, the later arrival bounds it", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		fX := tr.Lift()
		xLifted := time.Now()
		time.Sleep(time.Millisecond)
		fY := tr.Lift()
		yLifted := time.Now()
		time.Sleep(time.Millisecond)
		tr.ServedMockIn(m("mock-21"), fY)
		time.Sleep(time.Millisecond)
		tr.ServedMockIn(m("mock-20"), fX) // served last, lifted first
		fX.Land()
		fY.Land()
		time.Sleep(time.Millisecond)
		s, ok := tr.LiveSpan(time.Now())
		if !ok || s.From.After(yLifted) || !s.From.After(xLifted) {
			t.Fatalf("span from %s (ok %v); want from the later lift, after %s and no later than %s", s.From, ok, xLifted, yLifted)
		}
	})

	t.Run("a serve told in no request is bounded where it was made", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		tr.Lift() // a request aloft that the push is not
		made := time.Now()
		tr.ServedMockIn(m("mock-30"), NoRequest())
		time.Sleep(time.Millisecond)
		s, ok := tr.LiveSpan(time.Now())
		if !ok || s.From.Before(made) {
			t.Fatalf("span from %s (ok %v); want from the push's serve, no earlier than %s", s.From, ok, made)
		}
	})
}

// The rule reads whether F had served the sibling before the call reached
// keploy, not before the call's learn step read the tracker: a sibling the
// store served while the call waited for that read (a wait that grows with
// load) leaves the verdict as it is with the sibling unserved (Order.ArrivedAt).
// One F served before the call arrived stays the value's source, as before,
// and so does every serve where the caller says no arrival.
func TestASiblingFServedOnlyAfterTheCallArrivedIsNoSourceOfItsStartUpRead(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const path = "body.cmd.query.createdAt.$lt.$numberLong"
	at := eRecAt
	recT := strconv.FormatInt(at.Unix(), 10)
	fAt := at.Add(FullRoundLead)
	liveT := strconv.FormatInt(fAt.Unix(), 10)
	w := &Window{Rec: Span{From: at.Add(-time.Millisecond), To: at}, F: Span{From: fAt.Add(-time.Millisecond), To: fAt.Add(time.Millisecond)}}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: v}}}
	}
	find := &models.Mock{Name: "mock-17", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"find":"branches","filter":{"expires_at":{"$lte":{"$numberLong":"` + recT + `"}}},"$db":"api-server"} }`,
		}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"cursor":{"firstBatch":[],"id":{"$numberLong":"0"},"ns":"api-server.branches"},"ok":{"$numberDouble":"1.0"}} }`,
		}}}},
		ReqTimestampMock: at.Add(-600 * time.Microsecond), ResTimestampMock: at.Add(400 * time.Microsecond),
	}}
	classify := func(dc func() *DriftCache, raw []Raw) Learned {
		return ClassifyFollowing(dc(), []string{path}, str(liveT), str(recT), at, nil, raw, nil, w, nil)[0]
	}
	// raw is the find as a learn step reads it off o (the Mongo and HTTP
	// adapters' learnRaw): served where o saw it served.
	raw := func(o Order) []Raw {
		first, last := o.Of(find)
		return []Raw{o.ServedView(find, RawOf(find, false).ServedIn(first, last))}
	}
	for _, dc := range []func() *DriftCache{clientTestCache, clockedDrifts} {
		unserved := classify(dc, []Raw{RawOf(find, false)})
		if !plainRead(unserved) || unserved.From.Mock != "" {
			t.Fatalf("the find unserved: %s from %q (%q); want the call's own read of the clock", unserved.Kind, unserved.From.Mock, unserved.Why)
		}
		for _, aloft := range []bool{false, true} {
			tr := NewTracker()
			var fS *Flight
			if aloft {
				fS = tr.Lift() // the find's request, aloft across the call's arrival
			}
			time.Sleep(time.Millisecond)
			arrival := time.Now() // the distinct reaches keploy
			time.Sleep(time.Millisecond)
			tr.ServedMockIn(find, fS) // the find is served while the distinct waits for its learn step
			if fS != nil {
				fS.Land()
			}
			o := tr.Placed().Order
			got := classify(dc, raw(o.ArrivedAt(arrival)))
			if got.Kind != unserved.Kind || got.From.Mock != "" || fmt.Sprint(got.Read) != fmt.Sprint(unserved.Read) {
				t.Errorf("aloft %v: the find served after the distinct arrived: %s from %q read %v (%q); want as with the find unserved: %s read %v", aloft, got.Kind, got.From.Mock, got.Read, got.Why, unserved.Kind, unserved.Read)
			}
			if !strings.Contains(got.Refuted, "mock-17") || !strings.Contains(got.Refuted, "had not served") {
				t.Errorf("aloft %v: the reason does not name the find set aside and why: %q", aloft, got.Refuted)
			}
			// Served before the call arrived, or no arrival said: the find
			// stays the value's source, as before (fail closed).
			for what, o := range map[string]Order{"served before the call arrived": o.ArrivedAt(time.Now()), "no arrival said": o} {
				if got := classify(dc, raw(o)); got.Kind != KindVariable || got.From.Mock != "mock-17" {
					t.Errorf("aloft %v, %s: %s from %q (%q); want the variable of mock-17, as before", aloft, what, got.Kind, got.From.Mock, got.Why)
				}
			}
		}
		// A recording F served before the call arrived and again after it
		// (a pool's reusable one) was served before the call: it stays the
		// value's source, as before.
		tr := NewTracker()
		tr.ServedMock(find)
		time.Sleep(time.Millisecond)
		arrival := time.Now()
		time.Sleep(time.Millisecond)
		tr.ServedMock(find)
		if got := classify(dc, raw(tr.Placed().Order.ArrivedAt(arrival))); got.Kind != KindVariable || got.From.Mock != "mock-17" {
			t.Errorf("served before the call arrived and again after: %s from %q (%q); want the variable of mock-17, as before", got.Kind, got.From.Mock, got.Why)
		}
		// A later set numbers its serves from 1 again, and each serve's
		// instant is that set's own: the earlier set's serve, made before
		// this call arrived, never stands for the find served after it.
		// Kept, the earlier instant would read as the find's serve, and the
		// rule would switch off in every set after the first.
		tr = NewTracker()
		tr.ServedMock(&models.Mock{Name: "mock-3", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: at.Add(-time.Second)}})
		time.Sleep(time.Millisecond)
		tr.NewSetAt(time.Now())
		time.Sleep(time.Millisecond)
		arrival = time.Now()
		time.Sleep(time.Millisecond)
		tr.ServedMock(find)
		if got := classify(dc, raw(tr.Placed().Order.ArrivedAt(arrival))); got.Kind != unserved.Kind || got.From.Mock != "" || !strings.Contains(got.Refuted, "mock-17") {
			t.Errorf("the second set's find served after the call arrived: %s from %q (%q); want as with the find unserved: %s, mock-17 set aside", got.Kind, got.From.Mock, got.Why, unserved.Kind)
		}
	}
}

// The rule passes over only a place the application sent (sentOnly), and a
// connection's recorded start-up (isStartUp, the one place a swap of
// start-ups can change a word: swapHolds) holds no field of a request: so
// the rule's "!swap" can never decide alone, and a word such a swap can
// change stays refused naming the start-up (startUpWhy). Should a start-up
// ever carry a request field, this fails, and the rule's own guard against
// a swap's word needs a row of its own.
func TestAConnectionsStartUpHoldsNoFieldTheUnservedRuleReads(t *testing.T) {
	x := RawOf(eLaneSession("mock-hs-81", 2*time.Minute, 81), false)
	if !x.startUp {
		t.Fatal("a Postgres v3 session is not read as a connection's start-up")
	}
	if len(x.payload().fields) == 0 {
		t.Fatal("the start-up has no fields")
	}
	for _, f := range x.payload().fields {
		if requestPath(f.path) {
			t.Errorf("a start-up's field %s is read as a request's: the unserved rule (follows.notServed) may then pass over a word a swap of start-ups can change", f.path)
		}
	}
}

// A serve's call reached keploy where its request lifted (Flight.arrivedBy),
// told through each door a parser tells a serve by: a serve a push caused
// (LiftCaused) and a data-plane mock told through ServedHandshakeIn. And a
// serve filed at an earlier instant than serves already kept (ServedMockAt)
// raises the bound of every later serve's call, not only its own.
func TestALaterCallsWindowStartsWhereEachDoorSaysItsCallArrived(t *testing.T) {
	m := func(n string) *models.Mock { return &models.Mock{Name: n, Kind: models.Mongo} }
	identity := func(r time.Time) (time.Time, bool) { return r, true }

	t.Run("a serve a push caused is bounded where it was made", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		before := time.Now().Add(-time.Second)
		tr.ServedMockAt(m("mock-40"), before)
		pub := tr.Lift() // the PUBLISH
		time.Sleep(time.Millisecond)
		made := time.Now()
		tr.ServedMockIn(m("mock-41"), pub.LiftCaused()) // the push it let go
		time.Sleep(time.Millisecond)
		s, ok := tr.LiveSpan(time.Now())
		if !ok || s.From.Before(made) {
			t.Fatalf("span from %s (ok %v); want from the push's serve, no earlier than %s", s.From, ok, made)
		}
	})

	t.Run("a data-plane mock told as a handshake is bounded where its request lifted", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		before := time.Now().Add(-time.Second)
		tr.ServedMockAt(m("mock-50"), before)
		f := tr.Lift()
		lifted := time.Now()
		time.Sleep(2 * time.Millisecond) // the learn step
		tr.ServedHandshakeIn(m("mock-51"), f, f)
		f.Land()
		time.Sleep(time.Millisecond)
		s, ok := tr.LiveSpan(time.Now())
		if !ok || s.From.After(lifted) || !s.From.After(before) {
			t.Fatalf("span from %s (ok %v); want from its lift, after %s and no later than %s", s.From, ok, before, lifted)
		}
	})

	t.Run("a serve filed earlier than one kept raises the later one's bound", func(t *testing.T) {
		tr := NewTracker()
		tr.SetAppClock(identity)
		f := tr.Lift()
		time.Sleep(time.Millisecond)
		filed := time.Now() // a call that reached keploy after f's request
		time.Sleep(time.Millisecond)
		tr.ServedMockIn(m("mock-60"), f) // kept first, its call from f's lift
		f.Land()
		tr.ServedMockAt(m("mock-61"), filed) // filed after, at an earlier instant
		time.Sleep(time.Millisecond)
		s, ok := tr.LiveSpan(time.Now())
		if !ok || !s.From.Equal(filed) {
			t.Fatalf("span from %s (ok %v); want from the later arrival of the two, %s", s.From, ok, filed)
		}
	})
}
