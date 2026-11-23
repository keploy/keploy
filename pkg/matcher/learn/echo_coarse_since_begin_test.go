package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestOutcomeCoarseOutsideAnswerYieldsToSinceBeginInClientTests verifies that
// in keploy mock's client tests (T2), a coarse outside-scope answer
// (gaveRefusal: instant-another-scope or instant-pick-outside-scope, where two
// stamps share a second by chance) yields to step 7's plain clock read since
// the test began when step 6 leaves a declared clock send for step 7.
// Under keploy test (server tests, T1), when the value predates test begin,
// and when an outside answer holds a fine cut (< 1ms), it stays refused.
func TestOutcomeCoarseOutsideAnswerYieldsToSinceBeginInClientTests(t *testing.T) {
	rSec, fSec := int64(1795382839), int64(1795382980)
	// Test began 1s before the app audit, and this eventsV2 insert lands 2s
	// later in record vs 4s later in round F (different elapsed gaps so step 5
	// finds no constant offset, while Window.sinceBegin() holds rSec/fSec).
	rCall := time.Unix(rSec+2, 138_000_000)
	fCall := time.Unix(fSec+4, 138_000_000)
	win := &Window{
		Rec: Span{From: rCall.Add(-5 * time.Millisecond), To: rCall, Began: time.Unix(rSec-1, 0)},
		F:   Span{From: fCall.Add(-5 * time.Millisecond), To: fCall, Began: time.Unix(fSec-1, 0)},
	}
	winAfter := &Window{
		Rec: Span{From: rCall.Add(-5 * time.Millisecond), To: rCall, Began: time.Unix(rSec+1, 0)},
		F:   Span{From: fCall.Add(-5 * time.Millisecond), To: fCall, Began: time.Unix(fSec+1, 0)},
	}
	const p = "body.docs.msgs.0.eventDetails.app.createdAt.$numberLong"
	rStr, fStr := strconv.FormatInt(rSec, 10), strconv.FormatInt(fSec, 10)

	// Earlier send in the same test whose createdAt was learned/declared as a
	// clock read (Clock: true -> step 6's clockSrc is true and defers to step 7).
	auditAt := time.Unix(rSec, 500_000_000)
	auditSend := Earlier{
		Mock:    "mock-audit",
		At:      auditAt,
		Own:     true,
		Payload: echoPg("mock-audit", auditAt, "ok"),
		Seen: []Seen{{
			Side:  Request,
			Leaf:  Leaf{Path: "body.docs.msgs.0.app.createdAt.$numberLong", Type: String, Value: rStr},
			Live:  fStr,
			Place: "body.docs.msgs.0.app.createdAt.$numberLong",
			Clock: true,
		}},
	}
	// Earlier outside-scope answer holding the same coarse second in both runs
	// (e.g., an OpenBao/worker reply that fell in that second by chance).
	outsideAt := auditAt.Add(-50 * time.Millisecond)
	outsideCoarse := Earlier{
		Mock:    "mock-231",
		At:      outsideAt,
		Own:     false,
		Payload: echoPg("mock-231", outsideAt, time.Unix(rSec, 0).UTC().Format(time.RFC1123)),
		Seen: []Seen{{
			Side: Response,
			Leaf: Leaf{Path: "resp.header.Date", Type: String, Value: time.Unix(rSec, 0).UTC().Format(time.RFC1123)},
			Live: time.Unix(fSec, 0).UTC().Format(time.RFC1123),
		}},
	}
	decide := func(client bool, w *Window, earlier []Earlier, raw []Raw) Decision {
		in := iCall(p, String, rStr, fStr, rCall, fCall, w, earlier, raw)
		in.Scope = func(mock string) Scope {
			if mock == "mock-231" || mock == "mock-fine" {
				return Scope("other-test")
			}
			return Scope(in.Test)
		}
		if client {
			in.Drifts = oClientDrifts()
		}
		d := Decide(in)
		if len(d) != 1 {
			t.Fatalf("%d decisions: %s", len(d), oDescribe(d))
		}
		return d[0]
	}

	// Client test (T2): yields to step 7 (SinceTestBegin) -> Clock.
	d := decide(true, win, []Earlier{outsideCoarse, auditSend}, nil)
	iClock(t, "client test: coarse outside answer beside clock-declared audit send yields to step 7", d)
	if d.Clock == nil || d.Clock.Since != models.SinceTestBegin {
		t.Fatalf("client test: Clock=%+v; want SinceTestBegin", d.Clock)
	}

	// Fail closed 1: server test (T1, clientTests=false) keeps refusal.
	dT1 := decide(false, win, []Earlier{outsideCoarse, auditSend}, nil)
	if dT1.Refusal == nil || dT1.Refusal.Check != "instant-another-scope" {
		t.Fatalf("server test (T1): %s; want refused instant-another-scope", oDescribe([]Decision{dT1}))
	}

	// Fail closed 2: client test when value predates test begin keeps refusal.
	dPre := decide(true, winAfter, []Earlier{outsideCoarse, auditSend}, nil)
	if dPre.Refusal == nil || dPre.Refusal.Check != "instant-another-scope" {
		t.Fatalf("client test predating test begin: %s; want refused instant-another-scope", oDescribe([]Decision{dPre}))
	}

	// Fail closed 3: client test when outside answer holds a fine cut (<1ms) keeps refusal.
	rNs, fNs := int64(1795382839123456789), int64(1795382980987654321)
	rNsStr, fNsStr := strconv.FormatInt(rNs, 10), strconv.FormatInt(fNs, 10)
	outsideFine := Earlier{
		Mock:    "mock-fine",
		At:      outsideAt,
		Own:     false,
		Payload: echoPg("mock-fine", outsideAt, rNsStr),
		Seen: []Seen{{
			Side: Response,
			Leaf: Leaf{Path: "resp.ts", Type: String, Value: rNsStr},
			Live: fNsStr,
		}},
	}
	inFine := iCall(p, String, rNsStr, fNsStr, rCall, fCall, win, []Earlier{outsideFine}, nil)
	inFine.Scope = func(mock string) Scope {
		if mock == "mock-fine" {
			return Scope("other-test")
		}
		return Scope(inFine.Test)
	}
	inFine.Drifts = oClientDrifts()
	df := Decide(inFine)
	if len(df) != 1 || df[0].Refusal == nil {
		t.Fatalf("client test fine outside answer: %s; want refused", oDescribe(df))
	}
}

// TestMillisecondClockReadAfterSameMillisecondClockSendIsNotConflict verifies
// that two consecutive clock reads (e.g. a nanosecond audit stamp in mock-1386
// followed by a millisecond BSON created_at in mock-1388) that land in the
// same millisecond during record and different milliseconds in round F are
// learned as a plain clock read when the earlier send was itself a clock read
// and this call's own window holds both instants. Non-clock sends, finer-than-
// millisecond values, and out-of-window values stay refused as instant-conflict.
func TestMillisecondClockReadAfterSameMillisecondClockSendIsNotConflict(t *testing.T) {
	rT := time.Date(2026, 11, 23, 1, 40, 0, 112_000_000, time.UTC)
	fT := time.Date(2026, 11, 23, 1, 44, 3, 693_000_000, time.UTC)
	rAuditNs := strconv.FormatInt(rT.UnixNano()+30_939, 10)
	fAuditNs := strconv.FormatInt(fT.Add(-629*time.Millisecond).UnixNano()+345_230, 10)
	rMs := rT.Format("2006-01-02T15:04:05.000Z")
	fMs := fT.Format("2006-01-02T15:04:05.000Z")

	winOwn := &Window{
		Rec: Span{From: rT.Add(-5 * time.Millisecond), To: rT.Add(5 * time.Millisecond), Began: rT.Add(-time.Second)},
		F:   Span{From: fT.Add(-5 * time.Millisecond), To: fT.Add(5 * time.Millisecond), Began: fT.Add(-time.Second)},
	}
	winOut := &Window{
		Rec: Span{From: rT.Add(2 * time.Second), To: rT.Add(3 * time.Second), Began: rT.Add(-time.Second)},
		F:   Span{From: fT.Add(2 * time.Second), To: fT.Add(3 * time.Second), Began: fT.Add(-time.Second)},
	}
	mkAudit := func(isClock bool) Earlier {
		at := rT.Add(-500 * time.Microsecond)
		return Earlier{
			Mock:    "mock-1386",
			At:      at,
			Own:     true,
			Payload: oPayload("mock-1386", at),
			Seen: []Seen{{
				Side:  Request,
				Leaf:  Leaf{Path: "body.docs.msgs.0.auditedAtNanos.$numberLong", Noise: "body.docs.msgs.0.auditedAtNanos.$numberLong", Type: Number, Value: rAuditNs},
				Live:  fAuditNs,
				Place: "body.docs.msgs.0.auditedAtNanos.$numberLong",
				Clock: isClock,
			}},
		}
	}
	const p = "body.docs.msgs.0.created_at.$date.$numberLong"
	run := func(typ Type, rVal, lVal string, w *Window, isClock bool) Decision {
		in := iCall(p, typ, rVal, lVal, rT.Add(time.Millisecond), fT.Add(time.Millisecond), w, []Earlier{mkAudit(isClock)}, nil)
		in.Drifts = oClientDrifts()
		d := Decide(in)
		if len(d) != 1 {
			t.Fatalf("%d decisions: %s", len(d), oDescribe(d))
		}
		return d[0]
	}

	// Positive: earlier send is a clock read, current value is ms, own window holds it -> Clock.
	d := run(Instant, rMs, fMs, winOwn, true)
	iClock(t, "ms clock read after same-ms nanosecond clock send", d)

	// Fail closed 1: earlier send is NOT a clock read -> instant-conflict.
	dNoClk := run(Instant, rMs, fMs, winOwn, false)
	if dNoClk.Refusal == nil || dNoClk.Refusal.Check != "instant-conflict" {
		t.Fatalf("non-clock earlier send: %s; want refused instant-conflict", oDescribe([]Decision{dNoClk}))
	}

	// Fail closed 2: current value is outside its own call window -> instant-conflict.
	dOut := run(Instant, rMs, fMs, winOut, true)
	if dOut.Refusal == nil || dOut.Refusal.Check != "instant-conflict" {
		t.Fatalf("out-of-window ms value: %s; want refused instant-conflict", oDescribe([]Decision{dOut}))
	}

	// Fail closed 3: current value is finer than 1ms (nanoseconds) -> instant-conflict.
	dNs := run(Number, rAuditNs, strconv.FormatInt(fT.UnixNano()+111, 10), winOwn, true)
	if dNs.Refusal == nil || dNs.Refusal.Check != "instant-conflict" {
		t.Fatalf("nanosecond value conflicting with earlier nanosecond clock send: %s; want refused instant-conflict", oDescribe([]Decision{dNs}))
	}
}
