package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// twinOf is another recording of stub's call in its test, its answer's last
// byte at res, as change leaves it.
func twinOf(t *testing.T, res int, change func(m *models.Mock)) *models.Mock {
	m := singleAnswer(t, releaseAt(res))
	m.Name = "twin"
	if change != nil {
		change(m)
	}
	return m
}

// A recording is held only while no other recording of its test the call
// matches may be one its test waited for: one whose answer is due at the
// place, or one keploy cannot tell of. A twin held there too, one the call
// does not match, one of another kind, and the recording itself are none.
func TestAwaitedTwin(t *testing.T) {
	place := newPlacedDb().place
	stub := singleAnswer(t, releaseAt(45))
	yes := func([]*models.Mock) bool { return true }
	no := func([]*models.Mock) bool { return false }
	for _, tc := range []struct {
		name string
		twin *models.Mock
		same func([]*models.Mock) bool
		want bool
	}{
		{"a twin answered inside the test", twinOf(t, 6, nil), yes, true},
		{"a twin the call does not match", twinOf(t, 6, nil), no, false},
		{"a twin, and no matcher to tell", twinOf(t, 6, nil), nil, true},
		{"a twin answered after the test too", twinOf(t, 46, nil), yes, false},
		{"a twin with no marks", twinOf(t, 46, func(m *models.Mock) { m.Spec.HTTPResp.Marks = nil }), yes, true},
		{"a twin whose response the disk store keeps apart", twinOf(t, 46, func(m *models.Mock) { m.Spec.HTTPResp = nil }), yes, true},
		{"a session twin", twinOf(t, 46, func(m *models.Mock) { m.TestModeInfo.Lifetime = models.LifetimeSession }), yes, true},
		{"a twin whose call the run has not reached", twinOf(t, 46, func(m *models.Mock) { m.Spec.ReqTimestampMock = releaseAt(41) }), yes, true},
		{"a recording of another kind", twinOf(t, 6, func(m *models.Mock) { m.Kind = models.GENERIC }), yes, false},
		{"a twin with no request", twinOf(t, 6, func(m *models.Mock) { m.Spec.HTTPReq = nil }), yes, false},
		{"the recording itself", twinOf(t, 6, func(m *models.Mock) { m.Name = stub.Name }), yes, false},
		{"no recording", nil, yes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []*models.Mock
			same := tc.same
			if same != nil {
				same = func(ms []*models.Mock) bool { asked = ms; return tc.same(ms) }
			}
			var got bool
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("awaitedTwin panicked: %v", r)
					}
				}()
				got = awaitedTwin(stub, []*models.Mock{stub, tc.twin}, place, standInRelease, same)
			}()
			if got != tc.want {
				t.Fatalf("awaitedTwin = %v, want %v", got, tc.want)
			}
			for _, m := range asked {
				if m != tc.twin {
					t.Fatalf("the matcher was asked of %q, not the twin alone", m.Name)
				}
			}
		})
	}
}

// Through the parser's hold: a recording answered after its test is written
// at once while a twin answered inside the test is staged with it, served
// or not, and held when the call matches no such twin.
func TestHeldAnswer_NotHeldWhileATwinMayBeAwaited(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	stub := singleAnswer(t, releaseAt(45))
	db := newPlacedDb(stub, twinOf(t, 6, nil))
	match := func(ok bool) func([]*models.Mock) bool { return func([]*models.Mock) bool { return ok } }
	if a, err := h.heldAnswerOf(stub, releaseRequest(t, releaseCall), db, false, match(true)); err != nil || a != nil {
		t.Fatalf("a twin the call matches: held=%v err=%v; want written at once", a, err)
	}
	if a, err := h.heldAnswerOf(stub, releaseRequest(t, releaseCall), db, false, match(false)); err != nil || a == nil {
		t.Fatalf("no twin the call matches: held=%v err=%v; want held", a, err)
	}
}

// An answer served again in one set (a stateful cursor saturated on its
// recording) is written at once: its call was not on record. A new set's
// staging holds it again.
func TestHeldAnswer_ServedAgainIsWrittenAtOnce(t *testing.T) {
	registerStandIn(t)
	h := &HTTP{Logger: zap.NewNop()}
	db := newPlacedDb()
	serve := func() *heldAnswer {
		a, err := h.heldAnswerOf(singleAnswer(t, releaseAt(45)), releaseRequest(t, releaseCall), db, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if serve() == nil {
		t.Fatal("the first serve is written at once; want held")
	}
	if serve() != nil {
		t.Fatal("served again: held; want written at once")
	}
	db.move(func(p *models.RunPlace) { p.Staging++ })
	if serve() == nil {
		t.Fatal("the next set's first serve is written at once; want held")
	}
}
