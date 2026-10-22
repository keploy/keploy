package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// s12TestDb is the store as an open test of a set served by S12's background
// rules sees it: its own cursor keeps none (a test's own recordings answer once
// each, in recorded order), and its background's cursor (BackgroundCursor) is
// the one the calls between tests advance.
type s12TestDb struct {
	mockMemDb
	bg *mockMemDb
}

func (d *s12TestDb) MockCursorIndex(string, int) int    { return 0 }
func (d *s12TestDb) AdvanceMockCursor(string, int, int) {}
func (d *s12TestDb) BackgroundCursor() (integrations.MockCursor, bool) {
	return d.bg, true
}

var _ integrations.BackgroundCursor = (*s12TestDb)(nil)

// statefulAt is a stateful recording (ConsumeCursorSaturate) of one call,
// recorded at at.
func statefulAt(name string, at time.Time) *models.Mock {
	m := onHost(name, "/status", "api.example")
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	m.Spec.ReqTimestampMock = at
	return m
}

// Under S12's background rules the open test's own recordings and the set's
// background are two bands, never one sequence (S12 v16 §2.3): a test's call
// that its own recording is is answered by it, though background recordings of
// the call were recorded before it; a call only background recordings are
// advances through them on the background's cursor, the one the calls between
// tests advance, so a poller sees its recorded sequence wherever its calls
// land, then the last again. The variable pass picks the same way. Under
// today's rules (no background cursor) one sequence in record order, as before.
func TestS12StatefulPickKeepsTheTestsOwnApartFromTheBackground(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	base := time.Unix(1_700_000_000, 0)
	starting := statefulAt("bg-starting", base)
	ready := statefulAt("bg-ready", base.Add(time.Second))
	own := statefulAt("own", base.Add(2*time.Second))
	isOwn := func(m *models.Mock) bool { return m == own }
	h := newHTTP()
	db := &s12TestDb{bg: &mockMemDb{}}
	pick := func(fits ...*models.Mock) string {
		m, commit := h.strictCursorPick(fits, isOwn, db, true)
		if commit != nil {
			commit()
		}
		return m.Name
	}
	// The pool's order puts the test's own first.
	if got := pick(own, starting, ready); got != "own" {
		t.Fatalf("the test's own call was answered by %s; want its own recording", got)
	}
	for i, want := range []string{"bg-starting", "bg-ready", "bg-ready"} {
		if got := pick(ready, starting); got != want {
			t.Fatalf("background pick %d = %s; want %s (the background's sequence, then the last again)", i+1, got, want)
		}
	}
	if len(db.bg.cursors) == 0 {
		t.Fatal("the background's picks kept no mark in the background's cursor")
	}

	// The variable pass.
	hits := []*varHit{{mock: own}, {mock: starting}, {mock: ready}}
	if hit, _ := h.pickHit(hits, isOwn, &s12TestDb{bg: &mockMemDb{}}, true); hit.mock != own {
		t.Fatalf("the variable pass answered the test's own call with %s", hit.mock.Name)
	}
	vdb := &s12TestDb{bg: &mockMemDb{}}
	for i, want := range []string{"bg-starting", "bg-ready", "bg-ready"} {
		hit, commit := h.pickHit([]*varHit{{mock: ready}, {mock: starting}}, isOwn, vdb, true)
		if commit != nil {
			commit()
		}
		if hit.mock.Name != want {
			t.Fatalf("variable pass background pick %d = %s; want %s", i+1, hit.mock.Name, want)
		}
	}

	// Today's rules: one sequence across the tiers, in record order.
	plain := &mockMemDb{}
	if m, _ := h.strictCursorPick([]*models.Mock{own, starting, ready}, isOwn, plain, true); m != starting {
		t.Fatalf("today's rules picked %s; want bg-starting, the first recorded, as before", m.Name)
	}
	if hit, _ := h.pickHit([]*varHit{{mock: own}, {mock: starting}}, isOwn, plain, true); hit.mock != starting {
		t.Fatalf("today's variable pass picked %s; want bg-starting, as before", hit.mock.Name)
	}
}
