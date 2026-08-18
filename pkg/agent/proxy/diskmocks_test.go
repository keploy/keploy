package proxy

import (
	"bytes"
	"fmt"
	"runtime"
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
