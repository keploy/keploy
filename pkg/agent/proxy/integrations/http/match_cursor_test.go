package http

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// cursorMock builds an HTTP mock for a stateful dependency: the same request
// (GET /counter) recorded several times with successive responses, tagged
// ConsumeCursorSaturate (as DeriveLifetime would for a data-plane kind) and
// stamped with a recorded request time so record order is well-defined.
func cursorMock(name string, ts time.Time) *models.Mock {
	m := httpMock(name, "GET", "http://api/counter")
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	m.Spec.ReqTimestampMock = ts
	return m
}

// TestMatch_StatefulCursorSaturate is the core of the stateful-dependency fix:
// repeated identical requests must be served successive recorded responses in
// record order (1,2,3), then saturate on the last (…,3,3), instead of replaying
// the first forever (the 1,1,1 false pass). Selection must follow recorded
// request time, not pool/slice order, so the recordings are supplied shuffled.
func TestMatch_StatefulCursorSaturate(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	db := &mockMemDb{
		mocks: []*models.Mock{
			cursorMock("resp-3", base.Add(2*time.Second)),
			cursorMock("resp-1", base),
			cursorMock("resp-2", base.Add(1*time.Second)),
		},
		updateUnFilteredReturn: true,
	}

	// Three recordings, read five times: advance 1,2,3 then saturate on 3.
	want := []string{"resp-1", "resp-2", "resp-3", "resp-3", "resp-3"}
	for i, w := range want {
		ok, stub, _, err := h.match(ctx, putGet("/counter"), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("call %d: ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if stub.Name != w {
			t.Fatalf("call %d served %q, want %q — the cursor must advance in record order then saturate", i+1, stub.Name, w)
		}
	}
}

// TestMatch_StatefulCursorDisabledIsLegacy pins the gate and proves the test is
// load-bearing: with stateful mocks OFF, the same three identical recordings
// serve the SAME response every time (the legacy first-match behaviour), which
// is exactly the false pass the feature closes.
func TestMatch_StatefulCursorDisabledIsLegacy(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	db := &mockMemDb{
		mocks: []*models.Mock{
			cursorMock("resp-1", base),
			cursorMock("resp-2", base.Add(1*time.Second)),
			cursorMock("resp-3", base.Add(2*time.Second)),
		},
		updateUnFilteredReturn: true,
	}

	var first string
	for i := 0; i < 4; i++ {
		// statefulMocks = false (last arg): cursor is bypassed.
		ok, stub, _, err := h.match(ctx, putGet("/counter"), db, nil, nil, nil, true, false, false, false, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("call %d: ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if i == 0 {
			first = stub.Name
			continue
		}
		if stub.Name != first {
			t.Fatalf("call %d served %q, want the same %q every time when stateful mocks are disabled", i+1, stub.Name, first)
		}
	}
}

// TestMatch_StatefulCursorSingleRecordingSaturates proves the founder's
// "handle fixture re-reads correctly" constraint: a single recorded response
// read many more times than it was recorded must keep being served (saturate),
// never a miss — and at N=1 the behaviour is identical to legacy reuse.
func TestMatch_StatefulCursorSingleRecordingSaturates(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()

	db := &mockMemDb{
		mocks:                  []*models.Mock{cursorMock("only", time.Unix(1_700_000_000, 0))},
		updateUnFilteredReturn: true,
	}

	for i := 0; i < 5; i++ {
		ok, stub, _, err := h.match(ctx, putGet("/counter"), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("read %d: a single recording must keep being served (no miss): ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if stub.Name != "only" {
			t.Fatalf("read %d served %q, want %q", i+1, stub.Name, "only")
		}
	}
}

// TestMatch_StatefulCursorKeyIsTheRequest pins what a cursor is keyed by: the
// request, not the set of recordings a call happens to schema-match. Header
// matching only checks that a recording's header keys are present, so the
// first two calls (no Authorization) match only the two recordings made
// without it, and the next two match all four. A key derived from that
// per-call set restarts the sequence (served 1,2,1,2); keyed by the request,
// the sequence carries on through it (1,2,3,4).
func TestMatch_StatefulCursorKeyIsTheRequest(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	authed := func(m *models.Mock) *models.Mock {
		m.Spec.HTTPReq.Header = map[string]string{"Authorization": "Bearer t"}
		return m
	}
	db := &mockMemDb{
		mocks: []*models.Mock{
			cursorMock("r1", base),
			cursorMock("r2", base.Add(time.Second)),
			authed(cursorMock("r3", base.Add(2*time.Second))),
			authed(cursorMock("r4", base.Add(3*time.Second))),
		},
		updateUnFilteredReturn: true,
	}
	withAuth := func() *req {
		r := putGet("/counter")
		r.header = http.Header{"Authorization": []string{"Bearer t"}}
		return r
	}
	var got []string
	for _, in := range []*req{putGet("/counter"), putGet("/counter"), withAuth(), withAuth()} {
		ok, stub, _, err := h.match(ctx, in, db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("ok=%v stub=%v err=%v", ok, stub, err)
		}
		got = append(got, stub.Name)
	}
	if g, want := strings.Join(got, ","), "r1,r2,r3,r4"; g != want {
		t.Fatalf("served %s, want %s", g, want)
	}
}

// noCursor is a store whose cursor is absent (a negative index).
type noCursor struct{}

func (noCursor) MockCursorIndex(string, int) int    { return -1 }
func (noCursor) AdvanceMockCursor(string, int, int) {}

// servedPick takes the first recording not yet served, marks it once the
// call is answered, and stays on the last once all are; a store that keeps
// no cursor serves the first, as without the capability.
func TestServedPick(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	group := []*models.Mock{cursorMock("r1", base), cursorMock("r2", base.Add(time.Second))}
	db := &mockMemDb{}
	var got []int
	for i := 0; i < 3; i++ {
		idx, commit := servedPick(group, db)
		got = append(got, idx)
		if commit != nil {
			commit()
		}
	}
	if got[0] != 0 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("picked %v, want [0 1 1]", got)
	}
	if idx, commit := servedPick(group, noCursor{}); idx != 0 || commit != nil {
		t.Fatalf("no cursor: picked %d (commit %v), want the first and no commit", idx, commit != nil)
	}
}

// servedWalk walks the recordings a test's window holds among every one the
// store stages (full), spent ones too, answering only live ones (live): a
// spent one is passed over without moving the window, the last again while
// it is live, and, once the last can answer no more, nothing, with the reason
// naming the test, how many it recorded and the last; never an earlier
// recording again, nor one outside the window. A test that recorded none is
// answered the one in force when it began while it is live.
func TestServedWalkIsFixedByEveryStagedRecording(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	s0, s1 := cursorMock("s0", base), cursorMock("s1", base.Add(time.Second))
	a0, a1 := cursorMock("a0", base.Add(3*time.Second)), cursorMock("a1", base.Add(4*time.Second))
	b0 := cursorMock("b0", base.Add(7*time.Second))
	full := []*models.Mock{s0, s1, a0, a1, b0}
	sv := integrations.SequenceView{Cursor: &mockMemDb{}, Test: "A", Start: base.Add(2 * time.Second), End: base.Add(5 * time.Second)}
	walk := func(live []*models.Mock, sv integrations.SequenceView) (string, *answeredSequence) {
		i, commit, answered := servedWalk(live, full, sv)
		if answered != nil {
			if i != -1 || commit != nil {
				t.Fatalf("a miss picked %d (commit %v)", i, commit != nil)
			}
			return "", answered
		}
		if commit != nil {
			commit()
		}
		return live[i].Name, nil
	}
	// a0 spent: a1 answers, then again while live; a1 spent too: nothing,
	// never s1 (before the window) nor b0 (another test's).
	if got, _ := walk([]*models.Mock{s0, s1, a1, b0}, sv); got != "a1" {
		t.Fatalf("with a0 spent answered %q; want a1", got)
	}
	if got, _ := walk([]*models.Mock{s0, s1, a1, b0}, sv); got != "a1" {
		t.Fatalf("a1 served, still live: answered %q; want a1 again", got)
	}
	got, answered := walk([]*models.Mock{s0, s1, b0}, sv)
	if got != "" || answered == nil {
		t.Fatalf("both spent: answered %q; want nothing", got)
	}
	if !answered.inWindow || answered.held != 2 || answered.last != "a1" || answered.test != "A" {
		t.Fatalf("the miss is %+v; want test A, 2 in its window, the last a1", *answered)
	}
	if why := answered.why(); !strings.Contains(why, "test A recorded this call 2 time(s) in its recorded window") || !strings.Contains(why, "a1") {
		t.Fatalf("the reason %q does not name the test, its window's count and the last", why)
	}
	// A test that recorded none: the one in force when it began, a1, while
	// it is live; spent, nothing (never s1 before it, nor b0 after).
	none := integrations.SequenceView{Cursor: &mockMemDb{}, Test: "C", Start: base.Add(5500 * time.Millisecond), End: base.Add(6 * time.Second)}
	if got, _ := walk([]*models.Mock{s0, s1, a1, b0}, none); got != "a1" {
		t.Fatalf("none in the window answered %q; want a1, the one in force when it began", got)
	}
	got, answered = walk([]*models.Mock{s0, s1, b0}, none)
	if got != "" || answered == nil || answered.inWindow || answered.last != "a1" {
		t.Fatalf("none in the window, the one before it spent: answered %q (%+v); want nothing, naming a1", got, answered)
	}
	if why := answered.why(); !strings.Contains(why, "test C recorded no such call in its recorded window") {
		t.Fatalf("the reason %q does not say the test recorded none", why)
	}
	// A test that recorded none, with none recorded before it either: the
	// first recorded after its window, s0, and its miss says so, not that
	// it was in force when the test began.
	first := integrations.SequenceView{Cursor: &mockMemDb{}, Test: "D", Start: base.Add(-2 * time.Second), End: base.Add(-time.Second)}
	got, answered = walk([]*models.Mock{s1, a0, a1, b0}, first)
	if got != "" || answered == nil || answered.inWindow || !answered.after || answered.last != "s0" {
		t.Fatalf("none in or before the window, the first after it spent: answered %q (%+v); want nothing, naming s0", got, answered)
	}
	if why := answered.why(); !strings.Contains(why, "test D recorded no such call in or before its recorded window; the first recorded after it, s0,") {
		t.Fatalf("the reason %q does not say s0 is the first recorded after the window", why)
	}
	// Between tests every staged recording is walked, and one spent last
	// fails closed too.
	between := integrations.SequenceView{Cursor: &mockMemDb{}}
	got, answered = walk([]*models.Mock{s0}, between)
	if got != "s0" {
		t.Fatalf("between tests answered %q; want s0", got)
	}
	if got, answered = walk([]*models.Mock{s0}, between); got != "" || answered == nil || answered.test != "" || answered.held != 5 {
		t.Fatalf("between tests, s0 served and b0 spent: answered %q (%+v); want nothing", got, answered)
	}
}
