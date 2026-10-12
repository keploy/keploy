package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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

// windowedMemDb is mockMemDb with a test running: CurrentTestWindow reports the
// window the replayer opened for it (its recorded request..response times).
type windowedMemDb struct {
	*mockMemDb
	start, end time.Time
}

func (w *windowedMemDb) CurrentTestWindow() (time.Time, time.Time) { return w.start, w.end }

// headMock is a recorded S3 HEAD of one object made at ts: a session-reusable
// HTTP_CLIENT mock with cursor consumption, as DeriveLifetime classifies it.
func headMock(name string, ts time.Time) *models.Mock {
	m := httpMock(name, "HEAD", "http://minio:9000/bucket/set-1/mocks.yaml")
	m.Spec.ReqTimestampMock = ts
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	return m
}

func headReq() *req {
	return &req{method: "HEAD", url: &url.URL{Path: "/bucket/set-1/mocks.yaml"}, header: http.Header{}}
}

// TestMatch_CursorPrefersRunningTestsRecording: two tests made the same HEAD,
// test1 before the object existed (404) and test2 after (200). When test1 is not
// replayed (excluded, or failed before its HEAD), test2 must still get its own
// 200, not the 404 the set-wide cursor holds first.
func TestMatch_CursorPrefersRunningTestsRecording(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	db := &windowedMemDb{
		mockMemDb: &mockMemDb{
			mocks: []*models.Mock{
				headMock("head-404-test1", base.Add(500*time.Millisecond)),
				headMock("head-200-test2", base.Add(2500*time.Millisecond)),
			},
			updateUnFilteredReturn: true,
		},
		// test2 is running; test1 [base, base+1s] was skipped.
		start: base.Add(2 * time.Second),
		end:   base.Add(3 * time.Second),
	}

	ok, stub, _, err := h.match(ctx, headReq(), db, nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub == nil {
		t.Fatalf("ok=%v stub=%v err=%v", ok, stub, err)
	}
	if stub.Name != "head-200-test2" {
		t.Fatalf("test2 served %q, want its own %q — a skipped test's recording must not shift onto the next test", stub.Name, "head-200-test2")
	}
}

// TestMatch_CursorTestScopeThenSetWideFallback: the running test's own
// recordings are served in record order and saturate on its last; a test with
// no recording of its own falls back to the set-wide cursor, which has moved
// past what the earlier test was served instead of restarting at the first
// recording of the set.
func TestMatch_CursorTestScopeThenSetWideFallback(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	db := &windowedMemDb{
		mockMemDb: &mockMemDb{
			mocks: []*models.Mock{
				headMock("test1-404", base.Add(500*time.Millisecond)),
				headMock("test2-a", base.Add(2200*time.Millisecond)),
				headMock("test2-b", base.Add(2600*time.Millisecond)),
				headMock("between-tests", base.Add(4*time.Second)),
			},
			updateUnFilteredReturn: true,
		},
		start: base.Add(2 * time.Second),
		end:   base.Add(3 * time.Second),
	}

	serve := func(step string, want string) {
		t.Helper()
		ok, stub, _, err := h.match(ctx, headReq(), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("%s: ok=%v stub=%v err=%v", step, ok, stub, err)
		}
		if stub.Name != want {
			t.Fatalf("%s served %q, want %q", step, stub.Name, want)
		}
	}
	serve("test2 call 1", "test2-a")
	serve("test2 call 2", "test2-b")
	serve("test2 call 3", "test2-b") // saturates on the test's own last recording

	// test3 recorded no HEAD of this object: set-wide fallback, after test2's.
	db.start, db.end = base.Add(5*time.Second), base.Add(6*time.Second)
	serve("test3 call 1", "between-tests")
}

// TestMatch_CursorOverlappingTestWindowsDoNotReServe: tests whose recorded
// windows overlap (concurrent incoming requests) share the recordings made in
// the overlap. T1 [0s,10s] and T2 [5s,15s] each made the same HEAD, recorded at
// 6s and 8s, so both recordings are in both windows. T1 is served the 6s one;
// T2 must then get the 8s one, not the 6s one again, as it would on the
// set-wide cursor alone: a test's own recordings are taken from where the
// set-wide cursor stands.
func TestMatch_CursorOverlappingTestWindowsDoNotReServe(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)

	db := &windowedMemDb{
		mockMemDb: &mockMemDb{
			mocks: []*models.Mock{
				headMock("head-6s", base.Add(6*time.Second)),
				headMock("head-8s", base.Add(8*time.Second)),
			},
			updateUnFilteredReturn: true,
		},
	}
	for _, step := range []struct {
		test       string
		start, end time.Duration
		want       string
	}{
		{"T1", 0, 10 * time.Second, "head-6s"},
		{"T2", 5 * time.Second, 15 * time.Second, "head-8s"},
	} {
		db.start, db.end = base.Add(step.start), base.Add(step.end)
		ok, stub, _, err := h.match(ctx, headReq(), db, nil, nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("%s: ok=%v stub=%v err=%v", step.test, ok, stub, err)
		}
		if stub.Name != step.want {
			t.Fatalf("%s [%v, %v] served %q, want %q — a recording the set-wide cursor has passed must not be served again", step.test, step.start, step.end, stub.Name, step.want)
		}
	}
}
