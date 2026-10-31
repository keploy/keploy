package http

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// leaseTick is a timer's lease call the app makes at its clock's instant at:
// it asks for the leases dirty since a minute before, claims one at, and
// lets it expire five minutes after. owner is a field no clock moves; seq,
// where set, is an id the app writes beside them.
type leaseTick struct {
	owner              string
	since, at, expires time.Time
	seq                int64
}

func tickAt(owner string, at time.Time) leaseTick {
	return leaseTick{owner: owner, since: at.Add(-time.Minute), at: at, expires: at.Add(5 * time.Minute)}
}

func (l leaseTick) path() string {
	return "/v1/leases?since=" + strconv.FormatInt(l.since.Unix(), 10)
}

func (l leaseTick) body() string {
	seq := ""
	if l.seq != 0 {
		seq = `"seq":` + strconv.FormatInt(l.seq, 10) + `,`
	}
	return `{` + seq + `"owner":"` + l.owner + `","claimedAt":` + strconv.FormatInt(l.at.UnixMilli(), 10) + `,"expiresAt":"` + strconv.FormatInt(l.expires.UnixMilli(), 10) + `"}`
}

var leaseHeader = map[string]string{"Content-Type": "application/json"}

// leaseRecording is l recorded as name, the recorded call made 5 ms after its
// read of the clock, from (models.ServedFromBackground: the pool's copy; ""
// a test's own recording; a test's name: a set-aside test's copy).
func leaseRecording(name string, l leaseTick, from string) *models.Mock {
	m := recorded(name, l.path(), leaseHeader, l.body())
	m.Spec.ReqTimestampMock = l.at.Add(5 * time.Millisecond)
	m.TestModeInfo.SetAsideFrom = from
	return m
}

// leaseCall is l as the app sends it, reaching keploy at arrived.
func leaseCall(l leaseTick, arrived time.Time) *req {
	r := sent(l.path(), leaseHeader, l.body())
	r.arrived = arrived
	return r
}

// A timer's tick that lands in a test's window carries its clock (a query
// bound a minute back, a claim instant, an expiry five minutes on, in seconds
// and milliseconds since the epoch, as a JSON number, a JSON string and a
// query value). A start-up or between-tests recording of the tick made at
// another instant answers it, first recorded first, when the call is that
// recording but for fields that moved with its clock: all by one amount, to
// less than one cut each, that fits the call's span on the app's clock. It
// is claimed through the store's consume door and named in an Info line by
// the fields' shapes. Any other field changed, one field moved by another
// amount (a bound two minutes back), a read made before the call's span, a
// call whose app clock keploy cannot tell, and a set-aside test's copy (its
// recording was a test's own exchange: exact only) leave the call a miss. So
// do the bounds near both clocks (T: the set's recorded range, ten minutes
// here, plus the call's span): an id in the epoch range far from either clock
// that moved by the shared amount, an expiry three days on (the stated cost:
// an offset over T is not read as the clock), and a set whose range the agent
// never gave. With the switch off no recording is read by the rule.
func TestATimersTickInATestsWindowIsServedThePoolTickItIsButForItsClock(t *testing.T) {
	base := time.Date(2026, 10, 31, 14, 37, 29, 0, time.UTC)
	recAt := time.Date(2026, 10, 31, 14, 29, 40, 0, time.UTC)
	arrived := base.Add(2 * time.Second)
	now := base.Add(time.Second)
	pool := func() []*models.Mock {
		return []*models.Mock{
			leaseRecording("tick-1", tickAt("w1", recAt), models.ServedFromBackground),
			leaseRecording("tick-2", tickAt("w1", recAt.Add(2*time.Second)), models.ServedFromBackground),
		}
	}
	twoMinutes := tickAt("w1", now)
	twoMinutes.since = now.Add(-2 * time.Minute)
	// An id in the epoch range (2017), far from either clock, that moved by
	// the shared amount, the gap between the two calls' clocks.
	idOf := func(l leaseTick, id time.Time) leaseTick {
		l.seq = id.Unix()
		return l
	}
	idAt := time.Date(2017, 7, 14, 2, 40, 0, 0, time.UTC)
	gap := now.Sub(recAt)
	idPool := func() []*models.Mock {
		return []*models.Mock{leaseRecording("id-tick", idOf(tickAt("w1", recAt), idAt), models.ServedFromBackground)}
	}
	threeDays := func(l leaseTick) leaseTick {
		l.expires = l.at.Add(72 * time.Hour)
		return l
	}
	farPool := func() []*models.Mock {
		return []*models.Mock{leaseRecording("far-tick", threeDays(tickAt("w1", recAt)), models.ServedFromBackground)}
	}
	for _, c := range []struct {
		name     string
		call     leaseTick
		aside    func() []*models.Mock
		appClock bool
		noRange  bool
		want     string
	}{
		{"a tick in the call's window is the first pool tick", tickAt("w1", now), pool, true, false, "tick-1"},
		{"another owner", tickAt("w2", now), pool, true, false, ""},
		{"a bound two minutes back", twoMinutes, pool, true, false, ""},
		{"a read made before the call's span", tickAt("w1", base.Add(-time.Minute)), pool, true, false, ""},
		{"keploy cannot tell the app's clock", tickAt("w1", now), pool, false, false, ""},
		{"a set-aside test's copy", tickAt("w1", now), func() []*models.Mock {
			return []*models.Mock{leaseRecording("aside-tick", tickAt("w1", recAt), "TestAppCRUD")}
		}, true, false, ""},
		{"an id far from either clock moved by the shared amount", idOf(tickAt("w1", now), idAt.Add(gap)), idPool, true, false, ""},
		{"an expiry three days on in a ten-minute set", threeDays(tickAt("w1", now)), farPool, true, false, ""},
		{"the agent gave no range for the set", tickAt("w1", now), pool, true, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			core, logs := observer.New(zap.InfoLevel)
			h := &HTTP{Logger: zap.New(core)}
			db := &asideMemDb{mockMemDb: &mockMemDb{updateUnFilteredReturn: true}, aside: c.aside()}
			ctx := setClockConn(c.appClock, base)
			if c.noRange {
				ctx = clockConn(c.appClock, base)
			}
			got, err := h.setAsideAnswer(ctx, leaseCall(c.call, arrived), db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false)
			if err != nil {
				t.Fatalf("setAsideAnswer: %v", err)
			}
			if c.want == "" {
				if got != nil {
					t.Fatalf("served %s; want the call left a miss", got.Name)
				}
				if db.updateUnFilteredMockOld != nil {
					t.Fatalf("claimed %s for a call it does not answer", db.updateUnFilteredMockOld.Name)
				}
				return
			}
			if got == nil || got.Name != c.want {
				t.Fatalf("served %v; want %s, the pool tick the call is but for its clock", got, c.want)
			}
			if db.updateUnFilteredMockOld == nil || db.updateUnFilteredMockOld.Name != c.want {
				t.Fatal("served without its claim through the store's consume door")
			}
			lines := logs.FilterMessageSnippet("whose clock reads moved with the call's clock").All()
			if len(lines) != 1 {
				t.Fatalf("got %d Info lines naming the clock-moved answer; want 1", len(lines))
			}
			fields, _ := lines[0].ContextMap()["readAsTheClock"].([]interface{})
			if len(fields) != 3 || fields[0] != "body.claimedAt" || fields[1] != "body.expiresAt" || fields[2] != "query.since" {
				t.Fatalf("the Info line names %v; want the three fields read as the clock, by shape", lines[0].ContextMap()["readAsTheClock"])
			}
		})
	}
	t.Setenv(provenswitch.Env, "0")
	if got := clockMovedRecordings(setClockConn(true, base), leaseCall(tickAt("w1", now), arrived), pool(), newHTTP().newStrictNoise(models.ProvenFlakyHeaderNoise(), nil, nil)); len(got) != 0 {
		t.Fatalf("with the switch off the rule read %s as the call but for its clock; want none", got[0].m.Name)
	}
}

// The pool's exact copy of a call is served before one the call is but for
// its clock, even when the clock-moved one was recorded first.
func TestThePoolsExactCopyIsServedBeforeOneMovedWithTheClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Date(2026, 10, 31, 14, 37, 29, 0, time.UTC)
	now := base.Add(time.Second)
	moved := leaseRecording("tick-moved", tickAt("w1", now.Add(-8*time.Minute)), models.ServedFromBackground)
	exact := leaseRecording("tick-exact", tickAt("w1", now), models.ServedFromBackground)
	db := &asideMemDb{mockMemDb: &mockMemDb{updateUnFilteredReturn: true}, aside: []*models.Mock{moved, exact}}
	got, err := newHTTP().setAsideAnswer(setClockConn(true, base), leaseCall(tickAt("w1", now), base.Add(2*time.Second)), db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false)
	if err != nil {
		t.Fatalf("setAsideAnswer: %v", err)
	}
	if got == nil || got.Name != "tick-exact" {
		t.Fatalf("served %v; want tick-exact, the pool's exact copy", got)
	}
}

// A tick one more than the test's own recordings of it, which differs from
// its own recording only by its clock, is the test's extra call: the pool
// never answers it, though a pool tick is the call but for its clock
// (keploy #4745's F20). A tick of another owner than the test's own is no
// repeat of it, and the pool answers it.
func TestThePoolNeverAnswersATestsOwnExtraTickByItsClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Date(2026, 10, 31, 14, 37, 29, 0, time.UTC)
	recAt := time.Date(2026, 10, 31, 14, 29, 40, 0, time.UTC)
	now := base.Add(time.Second)
	store := func(ownOwner string) asideDb {
		own := leaseRecording("own-tick", tickAt(ownOwner, recAt.Add(30*time.Second)), "")
		poolTicks := []*models.Mock{
			leaseRecording("pool-w1", tickAt("w1", recAt), models.ServedFromBackground),
			leaseRecording("pool-w2", tickAt("w2", recAt), models.ServedFromBackground),
		}
		return asideDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{updateUnFilteredReturn: true}}, consumed: []*models.Mock{own}}, aside: poolTicks}
	}
	got, err := newHTTP().setAsideAnswer(setClockConn(true, base), leaseCall(tickAt("w1", now), base.Add(2*time.Second)), store("w1"), models.ProvenFlakyHeaderNoise(), nil, nil, true, false)
	if err != nil {
		t.Fatalf("setAsideAnswer: %v", err)
	}
	if got != nil {
		t.Fatalf("served %s to a tick one more than the test's own; want the test's extra call", got.Name)
	}
	got, err = newHTTP().setAsideAnswer(setClockConn(true, base), leaseCall(tickAt("w2", now), base.Add(2*time.Second)), store("w1"), models.ProvenFlakyHeaderNoise(), nil, nil, true, false)
	if err != nil {
		t.Fatalf("setAsideAnswer: %v", err)
	}
	if got == nil || got.Name != "pool-w2" {
		t.Fatalf("served %v to another owner's tick; want pool-w2", got)
	}
}

func TestEpochInstant(t *testing.T) {
	for _, c := range []struct {
		in  string
		ok  bool
		res time.Duration
	}{
		{"1761921449", true, time.Second},
		{"1761921449.25", true, 10 * time.Millisecond},
		{"1761921449123", true, time.Millisecond},
		{"1761921449123.5", false, 0},
		{"176192144", false, 0},
		{"0946684799", false, 0},
		{"4102444800", false, 0},
		{"17619214491234", false, 0},
		{"-761921449", false, 0},
		{"2026-10-31T14:37:29Z", false, 0},
		{"1761921449.", false, 0},
		{"", false, 0},
	} {
		_, res, ok := epochInstant(c.in)
		if ok != c.ok || res != c.res {
			t.Errorf("epochInstant(%q) = %v, %v; want %v, %v", c.in, res, ok, c.res, c.ok)
		}
	}
}

func TestMovedTogether(t *testing.T) {
	at := time.Date(2026, 10, 31, 14, 29, 40, 0, time.UTC)
	gap := 8 * time.Minute
	drift := func(moved, res time.Duration) movedPlace {
		return movedPlace{rec: at.Truncate(res), live: at.Add(moved).Truncate(res), res: res}
	}
	if !movedTogether([]movedPlace{drift(gap, time.Second), drift(gap, time.Millisecond)}, gap-time.Second) {
		t.Fatal("two fields moved by one gap, the gap past least: want together")
	}
	if movedTogether([]movedPlace{drift(gap, time.Second), drift(gap+time.Minute, time.Second)}, 0) {
		t.Fatal("two fields moved a minute apart: want not together")
	}
	if movedTogether([]movedPlace{drift(gap, time.Millisecond)}, gap+time.Second) {
		t.Fatal("a gap under least: want refused")
	}
	if movedTogether(nil, 0) {
		t.Fatal("no drift: want false")
	}
}

// A place the strict comparison never compares moves nothing that counts:
// a header of the static list (webhook-timestamp) holding seconds since the
// epoch that moved by an hour more than the call's clock fields does not
// refuse the pool tick, and is not named as read as the clock.
func TestAPlaceNeverComparedIsNoClockField(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Date(2026, 10, 31, 14, 37, 29, 0, time.UTC)
	recAt := time.Date(2026, 10, 31, 14, 29, 40, 0, time.UTC)
	now := base.Add(time.Second)
	rec := leaseRecording("tick-1", tickAt("w1", recAt), models.ServedFromBackground)
	rec.Spec.HTTPReq.Header["Webhook-Timestamp"] = strconv.FormatInt(recAt.Unix(), 10)
	call := leaseCall(tickAt("w1", now), base.Add(2*time.Second))
	call.header.Set("Webhook-Timestamp", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	got := clockMovedRecordings(setClockConn(true, base), call, []*models.Mock{rec}, newHTTP().newStrictNoise(models.ProvenFlakyHeaderNoise(), nil, nil))
	if len(got) != 1 || len(got[0].fields) != 3 || got[0].fields[2] != "query.since" {
		t.Fatalf("got %+v; want tick-1, read as the clock at its three compared fields alone", got)
	}
}

// setClockConn is clockConn whose set's recorded range the agent gave as ten
// minutes (learn.Tracker.SetRecordedRange): a pool recording's clock read is
// bounded by it (nearTheClocks).
func setClockConn(appClock bool, served time.Time) context.Context {
	tr := learn.NewTracker()
	if appClock {
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	}
	tr.ServedMockAt(httpMock("earlier", "GET", "/v1/account"), served)
	tr.SetLearnEnded(true)
	from := time.Date(2026, 10, 31, 14, 29, 0, 0, time.UTC)
	tr.SetRecordedRange(from, from.Add(10*time.Minute))
	return learn.WithTracker(context.Background(), tr)
}

// nearTheClocks bounds each drift near both clocks by the set's recorded
// range plus the call's span, and refuses every drift where the range is not
// known or the span runs backwards.
func TestNearTheClocks(t *testing.T) {
	call := time.Date(2026, 10, 31, 14, 37, 30, 0, time.UTC)
	rec := time.Date(2026, 10, 31, 14, 29, 40, 0, time.UTC)
	span := learn.Span{From: call, To: call.Add(2 * time.Second), Recorded: 10 * time.Minute, RecordedKnown: true}
	at := func(r, l time.Time) []movedPlace {
		return []movedPlace{{rec: r, live: l, res: time.Second, compared: true}}
	}
	tol := 10*time.Minute + 2*time.Second
	for _, c := range []struct {
		name   string
		drifts []movedPlace
		span   learn.Span
		want   bool
	}{
		{"both at their clocks", at(rec, call), span, true},
		{"both at T", at(rec.Add(-tol), call.Add(2*time.Second).Add(tol)), span, true},
		{"the recording's past T", at(rec.Add(tol+time.Second), call), span, false},
		{"the call's before its span by more than T", at(rec, call.Add(-tol-time.Second)), span, false},
		{"the call's after its span by more than T", at(rec, call.Add(2*time.Second).Add(tol+time.Second)), span, false},
		{"no range given", at(rec, call), learn.Span{From: call, To: call.Add(2 * time.Second)}, false},
		{"a span that runs backwards", at(rec, call), learn.Span{From: call, To: call.Add(-time.Second), Recorded: 10 * time.Minute, RecordedKnown: true}, false},
	} {
		if got := nearTheClocks(c.drifts, rec, c.span); got != c.want {
			t.Errorf("%s: nearTheClocks = %v; want %v", c.name, got, c.want)
		}
	}
}
