package proxy

import (
	"bytes"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func perTestMockAt(name string, reqUnixSec int64) *models.Mock {
	req := time.Unix(reqUnixSec, 0).UTC()
	m := &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: req,
			ResTimestampMock: req.Add(time.Millisecond),
		},
	}
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

func names(ms []*models.Mock) map[string]struct{} {
	s := make(map[string]struct{}, len(ms))
	for _, m := range ms {
		s[m.Name] = struct{}{}
	}
	return s
}

func TestEligibleForDisk(t *testing.T) {
	valid := perTestMockAt("m1", 100)
	if !EligibleForDisk(valid) {
		t.Fatalf("valid per-test mock should be disk-eligible")
	}

	session := perTestMockAt("m2", 100)
	session.TestModeInfo.Lifetime = models.LifetimeSession
	if EligibleForDisk(session) {
		t.Fatalf("session-lifetime mock must NOT go to disk (reused across the whole session)")
	}

	missing := perTestMockAt("m3", 100)
	missing.Spec.ReqTimestampMock = time.Time{}
	if EligibleForDisk(missing) {
		t.Fatalf("missing-timestamp mock must NOT go to disk (filter routes it without a window check)")
	}

	inverted := perTestMockAt("m4", 100)
	inverted.Spec.ResTimestampMock = inverted.Spec.ReqTimestampMock.Add(-time.Second)
	if EligibleForDisk(inverted) {
		t.Fatalf("res-before-req mock must NOT go to disk (filter drops it)")
	}

	if EligibleForDisk(nil) {
		t.Fatalf("nil mock must not be disk-eligible")
	}
}

func newDiskWith(t *testing.T, ms ...*models.Mock) *DiskMocks {
	t.Helper()
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	for _, m := range ms {
		if err := s.Add(m); err != nil {
			t.Fatalf("Add(%s): %v", m.Name, err)
		}
	}
	s.Finalize()
	return s
}

func TestLoadWindow_InclusiveBounds(t *testing.T) {
	// mocks at t=10,20,30,40,50
	s := newDiskWith(t,
		perTestMockAt("m10", 10), perTestMockAt("m20", 20),
		perTestMockAt("m30", 30), perTestMockAt("m40", 40),
		perTestMockAt("m50", 50),
	)
	defer s.Close()

	// window [20,40] must return exactly m20,m30,m40 (inclusive both ends).
	got, err := s.LoadWindow(time.Unix(20, 0).UTC(), time.Unix(40, 0).UTC())
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	want := map[string]struct{}{"m20": {}, "m30": {}, "m40": {}}
	gotN := names(got)
	if len(gotN) != len(want) {
		t.Fatalf("window [20,40]: got %v, want %v", gotN, want)
	}
	for n := range want {
		if _, ok := gotN[n]; !ok {
			t.Fatalf("window [20,40]: missing %s (got %v)", n, gotN)
		}
	}
	// decoded payloads must round-trip the request timestamp.
	for _, m := range got {
		if m.Spec.ReqTimestampMock.IsZero() {
			t.Fatalf("decoded mock %s lost its request timestamp", m.Name)
		}
	}
}

// Mocks that share a time keep the order they were added in, the mock file's
// order: every command of one pipelined write is recorded with that write's
// read time, and a parser that answers a repeated command from its recordings in
// turn (Redis's INCR, one count after another) answers in recorded order
// only if the store hands them over in it. Three connections recorded at
// once are added interleaved, 60 mocks each, each connection's in chunks of
// 20 that share a time, the connections' times interleaving, so the times
// arrive out of order and every sort has ties to keep: the index (the
// window, start-up, in-flight and whole loads), each declared index
// (carry-over, staged at staging, placed at its answer), and the recorded
// and keyed walks. The keyed walk gathers its candidates key by key, and the
// recorded walk over a store with mocks placed at their answer gathers them
// from the index, where those sit after the others of their request time:
// out of the order added, so a sort that keeps the order it was given does
// not pass them. The recorded walk over a store not finalized yet guards
// against its sort being dropped.
func TestDiskMocksKeepTheRecordedOrderOfMocksThatShareATime(t *testing.T) {
	const conns, perConn, chunk = 3, 60, 20
	interleaved := func(mark string) []*models.Mock {
		var added []*models.Mock
		for j := 0; j < perConn; j++ {
			for c := 0; c < conns; c++ {
				at := int64(100 + (j/chunk)*10 + (conns - c))
				m := perTestMockAt(fmt.Sprintf("c%d-%02d", c, j), at)
				m.TestModeInfo.LifetimeDerived = true
				m.Spec.Metadata = map[string]string{mark: "true", "parity": [2]string{"even", "odd"}[j%2]}
				added = append(added, m)
			}
		}
		return added
	}
	// byTime is the order the store must hand added over in: by at, and in
	// the order added within a time.
	byTime := func(added []*models.Mock, at func(*models.Mock) time.Time) []string {
		sorted := append([]*models.Mock(nil), added...)
		sort.SliceStable(sorted, func(i, j int) bool { return at(sorted[i]).Before(at(sorted[j])) })
		out := make([]string, len(sorted))
		for i, m := range sorted {
			out[i] = m.Name
		}
		return out
	}
	req := func(m *models.Mock) time.Time { return m.Spec.ReqTimestampMock }
	res := func(m *models.Mock) time.Time { return m.Spec.ResTimestampMock }
	inOrder := func(t *testing.T, what string, got []string, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d mocks, want %d", what, len(got), len(want))
		}
		out, first := 0, -1
		for i := range got {
			if got[i] != want[i] {
				out++
				if first < 0 {
					first = i
				}
			}
		}
		if out > 0 {
			t.Fatalf("%s: %d of %d mocks out of recorded order; first at %d: %s, want %s", what, out, len(got), first, got[first], want[first])
		}
	}
	namesOf := func(t *testing.T, what string, ms []*models.Mock, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.Name
		}
		return out
	}
	from, to := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()

	t.Run("index", func(t *testing.T) {
		defer models.RegisterRequestKeys(models.HTTP, func(m *models.Mock) []string { return []string{m.Spec.Metadata["parity"]} })()
		added := interleaved("plain")
		s := newDiskWith(t, added...)
		defer s.Close()
		want := byTime(added, req)
		got, err := s.LoadWindow(from, to)
		inOrder(t, "LoadWindow", namesOf(t, "LoadWindow", got, err), want)
		got, err = s.LoadBefore(to)
		inOrder(t, "LoadBefore", namesOf(t, "LoadBefore", got, err), want)
		got, err = s.LoadAll()
		inOrder(t, "LoadAll", namesOf(t, "LoadAll", got, err), want)
		// The recorded walk sorts what it reads itself: a store not finalized
		// yet holds its index in the order added.
		raw, err := NewDiskMocks(zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		for _, m := range added {
			if err := raw.Add(m); err != nil {
				t.Fatal(err)
			}
		}
		for _, store := range []*DiskMocks{s, raw} {
			var walked []string
			if err := store.RangeRecordedBefore(to, func(m *models.Mock) bool { walked = append(walked, m.Name); return true }); err != nil {
				t.Fatal(err)
			}
			inOrder(t, "RangeRecordedBefore", walked, want)
		}
		var keyed []string
		// Every odd mock's key first: the walk gathers them before the even ones.
		if err := s.RangeKeys(models.HTTP, []string{"odd", "even"}, func(name string, _ func() (*models.Mock, error)) bool {
			keyed = append(keyed, name)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		inOrder(t, "RangeKeys", keyed, want)
	})
	t.Run("in flight", func(t *testing.T) {
		// Every mock answered after the test's start: in flight at it.
		added := interleaved("flight")
		for _, m := range added {
			m.Spec.ResTimestampMock = time.Unix(300, 0).UTC()
		}
		s := newDiskWith(t, added...)
		defer s.Close()
		got, err := s.LoadInFlight(time.Unix(99, 0).UTC(), time.Unix(250, 0).UTC())
		inOrder(t, "LoadInFlight", namesOf(t, "LoadInFlight", got, err), byTime(added, req))
	})
	t.Run("carry-over", func(t *testing.T) {
		defer models.RegisterCarryOver(models.HTTP, func(m *models.Mock) bool { return m.Spec.Metadata["carry"] == "true" })()
		added := interleaved("carry")
		s := newDiskWith(t, added...)
		defer s.Close()
		got, err := s.LoadCarryOver(from, to)
		inOrder(t, "LoadCarryOver", namesOf(t, "LoadCarryOver", got, err), byTime(added, req))
	})
	t.Run("staged at staging", func(t *testing.T) {
		defer models.RegisterStagingNeed(models.HTTP, models.StagingNeed{
			Use:   models.StagingWithStartup,
			Match: func(m *models.Mock) bool { return m.Spec.Metadata["staged"] == "true" },
		})()
		added := interleaved("staged")
		s := newDiskWith(t, added...)
		defer s.Close()
		got, err := s.LoadStaged(from, to)
		inOrder(t, "LoadStaged", namesOf(t, "LoadStaged", got, err), byTime(added, req))
	})
	t.Run("placed at its answer", func(t *testing.T) {
		// The even mocks are placed at their answer, 5s after their request:
		// the index holds each request time's odd mocks at it and its even
		// ones after it.
		defer models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool {
			return m.Spec.Metadata["placed"] == "true" && m.Spec.Metadata["parity"] == "even"
		})()
		added := interleaved("placed")
		var evens []*models.Mock
		for _, m := range added {
			if m.Spec.Metadata["parity"] == "even" {
				m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(5 * time.Second)
				evens = append(evens, m)
			}
		}
		s := newDiskWith(t, added...)
		defer s.Close()
		got, err := s.LoadAnswerPlaced(from, to)
		inOrder(t, "LoadAnswerPlaced", namesOf(t, "LoadAnswerPlaced", got, err), byTime(evens, res))
		var walked []string
		if err := s.RangeRecordedBefore(to, func(m *models.Mock) bool { walked = append(walked, m.Name); return true }); err != nil {
			t.Fatal(err)
		}
		inOrder(t, "RangeRecordedBefore", walked, byTime(added, req))
	})
}

func TestLoadWindow_EmptyAndEdges(t *testing.T) {
	s := newDiskWith(t, perTestMockAt("m10", 10), perTestMockAt("m50", 50))
	defer s.Close()

	// gap window [20,40] -> nothing
	got, err := s.LoadWindow(time.Unix(20, 0).UTC(), time.Unix(40, 0).UTC())
	if err != nil {
		t.Fatalf("LoadWindow gap: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("gap window should be empty, got %v", names(got))
	}
	// single-point window [10,10] -> exactly m10
	got, _ = s.LoadWindow(time.Unix(10, 0).UTC(), time.Unix(10, 0).UTC())
	if len(got) != 1 || got[0].Name != "m10" {
		t.Fatalf("point window [10,10] should be {m10}, got %v", names(got))
	}
}

func TestLoadBefore_StartupBand(t *testing.T) {
	s := newDiskWith(t,
		perTestMockAt("m10", 10), perTestMockAt("m20", 20), perTestMockAt("m30", 30),
	)
	defer s.Close()

	// startup band: req < 25 -> m10, m20 (strictly before)
	got, err := s.LoadBefore(time.Unix(25, 0).UTC())
	if err != nil {
		t.Fatalf("LoadBefore: %v", err)
	}
	gotN := names(got)
	if len(gotN) != 2 || func() bool { _, a := gotN["m10"]; _, b := gotN["m20"]; return !a || !b }() {
		t.Fatalf("LoadBefore(25) should be {m10,m20}, got %v", gotN)
	}
	// exact boundary is exclusive: LoadBefore(20) excludes m20
	got, _ = s.LoadBefore(time.Unix(20, 0).UTC())
	if _, has := names(got)["m20"]; has {
		t.Fatalf("LoadBefore(20) must exclude m20 (strictly before)")
	}
}

// The recordings before an instant are visited by request time, wherever a
// mock is placed (one placed at its answer past the instant was recorded
// before it), in recorded order, until the visitor stops.
func TestRangeRecordedBefore_ByRequestTime(t *testing.T) {
	defer models.RegisterAnswerPlaced(models.HTTP, func(m *models.Mock) bool { return m.Name == "m15-answered-late" })()
	late := perTestMockAt("m15-answered-late", 15)
	late.Spec.ResTimestampMock = time.Unix(40, 0).UTC()
	late.TestModeInfo.LifetimeDerived = true
	if !models.PlaceOf(late).Equal(time.Unix(40, 0).UTC()) {
		t.Fatal("precondition: the late answer places the mock at 40")
	}
	s := newDiskWith(t, perTestMockAt("m10", 10), late, perTestMockAt("m20", 20), perTestMockAt("m30", 30))
	defer s.Close()
	var order []string
	if err := s.RangeRecordedBefore(time.Unix(25, 0).UTC(), func(m *models.Mock) bool { order = append(order, m.Name); return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "m10,m15-answered-late,m20" {
		t.Fatalf("recorded before 25: %v; want m10, m15-answered-late, m20, in recorded order", order)
	}
	order = nil
	_ = s.RangeRecordedBefore(time.Unix(20, 0).UTC(), func(m *models.Mock) bool { order = append(order, m.Name); return true })
	if len(order) != 2 {
		t.Fatalf("recorded before 20: %v; want the instant itself excluded", order)
	}
	order = nil
	_ = s.RangeRecordedBefore(time.Unix(25, 0).UTC(), func(m *models.Mock) bool { order = append(order, m.Name); return false })
	if len(order) != 1 {
		t.Fatalf("a visitor that stops saw %v", order)
	}
}

// The on-disk recordings before an instant are decoded one at a time and
// kept by no one: visiting a set many times larger than one mock holds
// about one mock's worth of heap at a time, never the set.
func TestRangeRecordedBefore_HoldsOneMockAtATime(t *testing.T) {
	const n, size = 48, 1 << 20
	var ms []*models.Mock
	for i := 0; i < n; i++ {
		m := perTestMockAt(fmt.Sprintf("big-%02d", i), int64(100+i))
		m.Spec.HTTPReq = &models.HTTPReq{Method: "POST", URL: "http://h/big", Body: strings.Repeat(string(rune('a'+i%26)), size)}
		ms = append(ms, m)
	}
	s := newDiskWith(t, ms...)
	defer s.Close()
	ms = nil
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	seen := 0
	if err := s.RangeRecordedBefore(time.Unix(1000, 0).UTC(), func(m *models.Mock) bool {
		seen++
		runtime.GC()
		var st runtime.MemStats
		runtime.ReadMemStats(&st)
		if st.HeapAlloc > base.HeapAlloc && st.HeapAlloc-base.HeapAlloc > peak {
			peak = st.HeapAlloc - base.HeapAlloc
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if seen != n {
		t.Fatalf("visited %d of %d", seen, n)
	}
	if peak > 8*size {
		t.Fatalf("the heap grew %d bytes while visiting %d mocks of %d bytes: the visit holds more than a mock at a time", peak, n, size)
	}
}

// A recording that cannot be read back is an error, not one skipped: the
// learn step reads what it holds as an earlier place, and refuses the call
// when it cannot.
func TestRangeRecordedBefore_TellsAFailedRead(t *testing.T) {
	s := newDiskWith(t, perTestMockAt("m10", 10), perTestMockAt("m20", 20))
	defer s.Close()
	if err := s.f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	seen := 0
	err := s.RangeRecordedBefore(time.Unix(1000, 0).UTC(), func(*models.Mock) bool { seen++; return true })
	if err == nil {
		t.Fatalf("visited %d mocks of a store that cannot read them, and said nothing", seen)
	}
}

func TestLoadByNames_And_All(t *testing.T) {
	s := newDiskWith(t,
		perTestMockAt("m10", 10), perTestMockAt("m20", 20), perTestMockAt("m30", 30),
	)
	defer s.Close()

	got, err := s.LoadByNames([]string{"m10", "m30", "does-not-exist"})
	if err != nil {
		t.Fatalf("LoadByNames: %v", err)
	}
	gotN := names(got)
	if len(gotN) != 2 {
		t.Fatalf("LoadByNames should return the 2 present names, got %v", gotN)
	}
	if _, ok := gotN["m10"]; !ok {
		t.Fatalf("LoadByNames missing m10")
	}

	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("LoadAll should return all 3, got %d", len(all))
	}
}

func TestEarliestReqTs(t *testing.T) {
	s := newDiskWith(t, perTestMockAt("m30", 30), perTestMockAt("m10", 10), perTestMockAt("m20", 20))
	defer s.Close()
	if got := s.EarliestReqTs(); got.Unix() != 10 {
		t.Fatalf("EarliestReqTs should be t=10, got %v", got.Unix())
	}
}

// bigMockAt builds a per-test HTTP mock at req time with a DISTINCT body of
// bodyBytes (fresh allocation, not a shared string) so that, if the store were
// to retain mocks in RAM, the resident footprint would visibly track the pool.
func bigMockAt(name string, req time.Time, bodyBytes int) *models.Mock {
	body := string(bytes.Repeat([]byte{byte('a' + len(name)%26)}, bodyBytes))
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: req,
			ResTimestampMock: req.Add(time.Millisecond),
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
		},
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

// TestDiskMocks_ResidentBounded is the memory-behavior proof: a large pool of
// distinct big mocks lands on DISK (exact byte count) and does NOT stay resident
// (heap stays far below the pool), and loading one small window reconstructs
// exactly that window's mocks — with their bodies intact — without materializing
// the whole pool. This is the O(config + window) bound the fix exists to deliver.
func TestDiskMocks_ResidentBounded(t *testing.T) {
	const (
		n         = 400
		bodyBytes = 32 * 1024 // 32 KiB each
	)
	poolBytes := int64(n) * bodyBytes // ~12.8 MiB of distinct body payload
	base := time.Unix(1_700_000_000, 0).UTC()

	baseline := heapInUse()

	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer d.Close()

	for i := 0; i < n; i++ {
		m := bigMockAt(fmt.Sprintf("m%04d", i), base.Add(time.Duration(i)*time.Second), bodyBytes)
		if err := d.Add(m); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		// m goes out of scope here — the store must NOT retain it.
	}
	d.Finalize()

	// 1) Every body is on disk (exact-ish — gob framing adds a little overhead).
	if db := d.DiskBytes(); db < poolBytes {
		t.Fatalf("expected >= %d bytes on disk, got %d (bodies must be written, not held)", poolBytes, db)
	}
	if got := d.Len(); got != n {
		t.Fatalf("Len = %d, want %d", got, n)
	}

	// 2) After ingest, resident heap holds only the index (~n small entries),
	//    NOT the ~12.8 MiB of bodies. Generous margin vs. the pool.
	residentAfterIngest := int64(heapInUse()) - int64(baseline)
	if residentAfterIngest > poolBytes/8 {
		t.Fatalf("resident after ingest grew by %d B; must be << pool %d B (bodies belong on disk, only the index resident)",
			residentAfterIngest, poolBytes)
	}

	// 3) Load ONE small window (5 consecutive seconds -> 5 mocks). Bodies are
	//    ELIDED from the window (response-spill): matching never reads them, so
	//    LoadWindow returns matchers only. Resident stays far below the pool
	//    even with the window held live.
	win, err := d.LoadWindow(base.Add(10*time.Second), base.Add(14*time.Second))
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(win) != 5 {
		t.Fatalf("window [10s,14s] should hold 5 mocks, got %d", len(win))
	}
	for _, m := range win {
		if m.Spec.HTTPResp != nil {
			t.Fatalf("mock %s carried its body into the window; response must be elided until serve", m.Name)
		}
		if !m.HasSpilledResponse() {
			t.Fatalf("mock %s should carry a lazy response hydrator", m.Name)
		}
	}
	residentWithWindow := int64(heapInUse()) - int64(baseline)
	runtime.KeepAlive(win)
	runtime.KeepAlive(d)
	if residentWithWindow > poolBytes/2 {
		t.Fatalf("resident with a 5-mock window held is %d B; must be far below the whole pool %d B",
			residentWithWindow, poolBytes)
	}

	// 4) Hydrate on demand (serve time) reconstructs each body exactly.
	for _, m := range win {
		if err := m.HydrateResponse(); err != nil {
			t.Fatalf("HydrateResponse(%s): %v", m.Name, err)
		}
		if m.Spec.HTTPResp == nil || len(m.Spec.HTTPResp.Body) != bodyBytes {
			t.Fatalf("hydrated mock %s lost its %d-byte body", m.Name, bodyBytes)
		}
	}
	t.Logf("pool=%d B  residentAfterIngest=%d B  residentWithWindow(bodies elided)=%d B (window=%d mocks)",
		poolBytes, residentAfterIngest, residentWithWindow, len(win))
}

// TestLoadWindow_FanOutElidesBodies is the regression test for the auto-replay
// transient spike. A fan-out test (e.g. /loadall firing N concurrent downstream
// calls) records N per-test mocks with near-identical request timestamps, so one
// test's window spans ALL N mocks. Before response-spill, LoadWindow decoded
// whole mocks — INCLUDING their bodies — for the entire window, materialising
// ~N*bodyBytes resident even though matching never reads responses. This test
// asserts that a 150 x 1 MB fan-out window now materialises only matchers
// (resident far below the pool) and that each body still hydrates on demand at
// serve time — the transient the fix removes.
func TestLoadWindow_FanOutElidesBodies(t *testing.T) {
	const (
		n         = 150
		bodyBytes = 1024 * 1024 // 1 MiB each — mirrors bigmock2's 1 MB docs
	)
	poolBytes := int64(n) * bodyBytes // ~150 MiB
	base := time.Unix(1_700_000_000, 0).UTC()

	baseline := heapInUse()
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatalf("NewDiskMocks: %v", err)
	}
	defer d.Close()

	// Fan-out: all N mocks land within a 1 ms window (concurrent downstream calls).
	for i := 0; i < n; i++ {
		m := bigMockAt(fmt.Sprintf("cfg%04d", i), base.Add(time.Duration(i)*time.Microsecond), bodyBytes)
		if err := d.Add(m); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	d.Finalize()

	// The fan-out test's window covers all N mocks.
	win, err := d.LoadWindow(base, base.Add(time.Second))
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(win) != n {
		t.Fatalf("fan-out window should hold all %d mocks, got %d", n, len(win))
	}
	residentWithWindow := int64(heapInUse()) - int64(baseline)
	runtime.KeepAlive(win)
	runtime.KeepAlive(d)

	// The whole fan-out window must NOT materialise its bodies: resident stays
	// far below the pool (was ~poolBytes before the fix). Generous 1/8 margin.
	if residentWithWindow > poolBytes/8 {
		t.Fatalf("fan-out window materialised %d B resident; bodies must be elided, expected << pool %d B",
			residentWithWindow, poolBytes)
	}
	// Every body still hydrates on demand.
	for _, m := range win {
		if m.Spec.HTTPResp != nil {
			t.Fatalf("mock %s carried its body into the fan-out window", m.Name)
		}
		if err := m.HydrateResponse(); err != nil {
			t.Fatalf("HydrateResponse(%s): %v", m.Name, err)
		}
		if m.Spec.HTTPResp == nil || len(m.Spec.HTTPResp.Body) != bodyBytes {
			t.Fatalf("hydrated fan-out mock %s lost its %d-byte body", m.Name, bodyBytes)
		}
	}
	t.Logf("fan-out window: %d mocks, poolBytes=%d B, residentWithWindow(elided)=%d B",
		n, poolBytes, residentWithWindow)
}

func TestClose_FailsSubsequentOps(t *testing.T) {
	s := newDiskWith(t, perTestMockAt("m10", 10))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close must be idempotent, got %v", err)
	}
	if _, err := s.LoadWindow(time.Unix(0, 0), time.Unix(100, 0)); err == nil {
		t.Fatalf("LoadWindow after Close must error, not silently mis-serve")
	}
}

// TestSpilledResponsesCountsTheResponsesKeptApart: the agent's mock residency
// log reports SpilledResponses, and CI's strict mock-window replay of
// http-pokeapi requires it to be non-zero, so that phase fails, rather than
// passing without serving one, if the store stops keeping responses apart.
func TestSpilledResponsesCountsTheResponsesKeptApart(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	big := bigMockAt("big", base, responseSpillMinBytes)
	big.TestModeInfo.Lifetime = models.LifetimePerTest
	small := bigMockAt("small", base.Add(time.Second), responseSpillMinBytes-1)
	small.TestModeInfo.Lifetime = models.LifetimePerTest
	mongo := perTestMockAt("mongo", 1_700_000_002)
	mongo.Kind = models.Mongo
	mongo.Spec.MongoResponses = []models.MongoResponse{{}}
	noResp := perTestMockAt("no-response", 1_700_000_003)

	d := newDiskWith(t, big, small, mongo, noResp)
	defer d.Close()
	if got := d.Len(); got != 4 {
		t.Fatalf("precondition: %d mocks on disk, want 4", got)
	}
	if got := d.SpilledResponses(); got != 2 {
		t.Fatalf("SpilledResponses = %d, want 2 (the %d-byte HTTP body and the Mongo responses)", got, responseSpillMinBytes)
	}
	win, err := d.LoadWindow(base, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(win) != 4 {
		t.Fatalf("LoadWindow returned %d mocks, want all 4", len(win))
	}
	for _, m := range win {
		elided := m.Spec.HTTPResp == nil && len(m.Spec.MongoResponses) == 0 && m.HasSpilledResponse()
		if want := m.Name == "big" || m.Name == "mongo"; elided != want {
			t.Fatalf("mock %s: response kept apart = %v, want %v", m.Name, elided, want)
		}
	}
}

// pgStatementMock is a per-test PostgreSQL query mock of the statement
// sqlHash, recorded at second sec.
func pgStatementMock(name, sqlHash string, sec int64) *models.Mock {
	at := time.Unix(sec, 0).UTC()
	return &models.Mock{
		Name: name,
		Kind: models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type:  models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{SQLAstHash: sqlHash},
			},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true},
	}
}

// The store keeps, for each statement its mocks record, the first one
// recorded, whatever order they were added in; a mock of no statement, or one
// the server answered with an error (no shape to describe the statement by),
// is in no catalog.
func TestLoadStatements_KeepsTheFirstRecordingOfEachStatement(t *testing.T) {
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	refused := pgStatementMock("b-refused", "sha256:b", 5)
	refused.Spec.PostgresV3.Query.Response = &models.PostgresV3Response{Error: &models.PostgresV3Error{Severity: "ERROR", Code: "42P01"}}
	for _, m := range []*models.Mock{
		pgStatementMock("a-late", "sha256:a", 30),
		pgStatementMock("a-first", "sha256:a", 10),
		pgStatementMock("b", "sha256:b", 20),
		refused,
		perTestMockAt("http", 15),
	} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadStatements()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if want := []string{"a-first", "b"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("catalog %v; want the first recording of each statement, in recorded order: %v", names, want)
	}
}

// LoadStaged returns, in recorded order, the stored mocks a
// models.StagingWithStartup need declares with request time in [from, to],
// both ends included, and none other. Mocks stored before the need was
// declared are not indexed: the store indexes a mock as it is added.
func TestLoadStaged_ReadsOnlyTheDeclaredMocksOfItsRangeInRecordedOrder(t *testing.T) {
	s, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	declared := func(name string, at int64) *models.Mock {
		m := perTestMockAt(name, at)
		m.Spec.Metadata = map[string]string{"push": "true"}
		return m
	}
	if err := s.Add(declared("before-need", 15)); err != nil {
		t.Fatal(err)
	}
	defer models.RegisterStagingNeed(models.HTTP, models.StagingNeed{
		Use:   models.StagingWithStartup,
		Match: func(m *models.Mock) bool { return m.Spec.Metadata["push"] == "true" },
	})()
	for _, m := range []*models.Mock{
		declared("late", 300), perTestMockAt("plain", 20), declared("at-from", 10),
		declared("before-from", 9), declared("at-to", 400), declared("after-to", 401),
	} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LoadStaged(time.Unix(10, 0), time.Unix(400, 0))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range got {
		out = append(out, m.Name)
	}
	if want := []string{"at-from", "late", "at-to"}; !reflect.DeepEqual(out, want) {
		t.Fatalf("LoadStaged read %v; want the declared mocks of [10s, 400s] in recorded order: %v", out, want)
	}
}

// RangeKeys visits the on-disk mocks of a kind indexed under any key asked
// (models.RequestKeys), once each, in recorded order, until the visit says
// stop: none of another key, another kind, or a mock whose parser declares
// none. It decodes only what the visit loads.
func TestRangeKeysVisitsTheKeysAskedInRecordedOrder(t *testing.T) {
	defer models.RegisterRequestKeys(models.HTTP, func(m *models.Mock) []string { return []string{m.Name[:4], "any"} })()
	mongo := perTestMockAt("dele-mongo", 15)
	mongo.Kind = models.Mongo
	s := newDiskWith(t, perTestMockAt("dele-30", 30), mongo, perTestMockAt("poll-20", 20), perTestMockAt("dele-10", 10), perTestMockAt("send-40", 40))
	defer s.Close()
	visit := func(kind models.Kind, keys []string, loadName string, stopAt string) (visited, loaded []string) {
		t.Helper()
		if err := s.RangeKeys(kind, keys, func(name string, load func() (*models.Mock, error)) bool {
			visited = append(visited, name)
			if name == loadName {
				m, err := load()
				if err != nil || m.Name != name {
					t.Fatalf("load of %s = %v, %v", name, m, err)
				}
				loaded = append(loaded, m.Name)
			}
			return name != stopAt
		}); err != nil {
			t.Fatalf("RangeKeys: %v", err)
		}
		return visited, loaded
	}
	if got, loaded := visit(models.HTTP, []string{"dele"}, "dele-30", ""); !reflect.DeepEqual(got, []string{"dele-10", "dele-30"}) || !reflect.DeepEqual(loaded, []string{"dele-30"}) {
		t.Fatalf("RangeKeys(HTTP, dele) visited %v and loaded %v; want dele-10, dele-30, and only dele-30 decoded", got, loaded)
	}
	if got, _ := visit(models.HTTP, []string{"send", "dele", "any"}, "", ""); !reflect.DeepEqual(got, []string{"dele-10", "poll-20", "dele-30", "send-40"}) {
		t.Fatalf("RangeKeys(HTTP, send, dele, any) visited %v; want each HTTP mock once, in recorded order", got)
	}
	if got, _ := visit(models.HTTP, []string{"any"}, "", "poll-20"); !reflect.DeepEqual(got, []string{"dele-10", "poll-20"}) {
		t.Fatalf("a visit that stopped at poll-20 went on: %v", got)
	}
	if got, _ := visit(models.Mongo, []string{"dele"}, "", ""); len(got) != 0 {
		t.Fatalf("a kind with no declared keys visited %v", got)
	}
}
