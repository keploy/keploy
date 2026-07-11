package agent

// Tests for the streaming /storemocks ingest (StoreMocksStream).

import (
	"bytes"
	"context"
	"encoding/gob"
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// strictAgent returns a test agent whose OSS config sets the strict mock-window
// flag — the same flag config.New() seeds true by default, which gates residency.
func strictAgent(strict bool) *Agent {
	a := newTestAgent()
	a.config = &config.Config{}
	a.config.Test.StrictMockWindow = strict
	return a
}

// --- helpers ---

func newTestAgent() *Agent { return &Agent{logger: zap.NewNop()} }

func mkMock(name string, kind models.Kind, ts time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{ReqTimestampMock: ts},
	}
}

// mkBigMock carries a large HTTP body so the encoded payload is data-dominated.
func mkBigMock(name string, ts time.Time, body string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: ts,
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
		},
	}
}

// encodeStreamBody writes the wire body: gob header, then filtered then unfiltered mocks.
func encodeStreamBody(t *testing.T, filtered, unfiltered []*models.Mock) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(models.MockStreamHeader{
		FilteredCount:   len(filtered),
		UnfilteredCount: len(unfiltered),
	}); err != nil {
		t.Fatalf("encode header: %v", err)
	}
	for _, m := range filtered {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode filtered: %v", err)
		}
	}
	for _, m := range unfiltered {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode unfiltered: %v", err)
		}
	}
	return buf.Bytes()
}

// readHeaderAndDecoder consumes the header from body (as the HTTP handler
// does) and returns the header + a decoder positioned at the first mock.
func readHeaderAndDecoder(t *testing.T, body []byte) (models.MockStreamHeader, *gob.Decoder) {
	t.Helper()
	dec := gob.NewDecoder(bytes.NewReader(body))
	var header models.MockStreamHeader
	if err := dec.Decode(&header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	return header, dec
}

func loadStorage(t *testing.T, a *Agent) *ClientMockStorage {
	t.Helper()
	v, ok := a.clientMocks.Load(uint64(0))
	if !ok {
		t.Fatal("no client mock storage published")
	}
	return v.(*ClientMockStorage)
}

type mockSig struct {
	name     string
	kind     models.Kind
	lifetime models.Lifetime
	derived  bool
}

func sigs(mocks []*models.Mock) []mockSig {
	out := make([]mockSig, len(mocks))
	for i, m := range mocks {
		out[i] = mockSig{m.Name, m.Kind, m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived}
	}
	return out
}

func eqSigs(a, b []mockSig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// captureHook records the freeze anchor SetFreezeAnchor was called with.
type captureHook struct {
	AgentHook
	anchor time.Time
	called bool
}

func (c *captureHook) SetFreezeAnchor(_ context.Context, anchor time.Time) error {
	c.anchor = anchor
	c.called = true
	return nil
}

// diskEligibleMock builds a per-test mock (Mongo => LifetimePerTest) with valid
// request+response timestamps, so it is window-eligible and is kept on disk.
func diskEligibleMock(name string, req time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Mongo,
		Spec: models.MockSpec{ReqTimestampMock: req, ResTimestampMock: req.Add(time.Millisecond)},
	}
}

// TestStoreMocksStream_PerTestMocksGoToDisk verifies the residency split:
// window-eligible per-test mocks go to disk (not resident), while
// ineligible per-test mocks (missing timestamps) and config mocks stay resident.
// The on-disk store must still reconstruct the eligible mocks by window, and the freeze
// anchor must reflect the earliest on-disk timestamp.
func TestStoreMocksStream_PerTestMocksGoToDisk(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	// Residency follows the OSS config flag config.Test.StrictMockWindow — run
	// both modes and assert against the code's own gate, pkg.IsStrictMockWindow(strict),
	// which stays correct even if the ambient KEPLOY_STRICT_MOCK_WINDOW env overrides.
	for _, strict := range []bool{true, false} {
		strict := strict
		t.Run(map[bool]string{true: "strict", false: "lax"}[strict], func(t *testing.T) {
			prevHooks := ActiveHooks
			ch := &captureHook{}
			ActiveHooks = ch
			defer func() { ActiveHooks = prevHooks }()

			a := strictAgent(strict)
			wantDisk := pkg.IsStrictMockWindow(strict)

			ineligible := &models.Mock{Name: "pt-missing", Kind: models.Mongo, Spec: models.MockSpec{ReqTimestampMock: base.Add(5 * time.Second)}} // no res ts
			cfg := &models.Mock{Name: "cfg-1", Kind: models.Mongo, Spec: models.MockSpec{
				ReqTimestampMock: base.Add(3 * time.Second), ResTimestampMock: base.Add(3 * time.Second).Add(time.Millisecond),
				Metadata: map[string]string{"type": "config"},
			}}
			// filtered (per-test): 2 eligible + 1 ineligible; unfiltered: 1 config.
			filtered := []*models.Mock{
				diskEligibleMock("pt-2", base.Add(2*time.Second)),
				diskEligibleMock("pt-1", base.Add(1*time.Second)), // earliest → freeze anchor
				ineligible,
			}
			unfiltered := []*models.Mock{cfg}

			header, dec := readHeaderAndDecoder(t, encodeStreamBody(t, filtered, unfiltered))
			if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
				t.Fatalf("StoreMocksStream: %v", err)
			}
			st := loadStorage(t, a)

			if wantDisk {
				// Strict: eligible per-test mocks live on disk; only the
				// ineligible one stays resident in filtered.
				if st.diskMocks == nil {
					t.Fatal("strict mode: expected an on-disk store")
				}
				if got := st.diskMocks.Len(); got != 2 {
					t.Fatalf("expected 2 on-disk eligible per-test mocks, got %d", got)
				}
				if len(st.filtered) != 1 || st.filtered[0].Name != "pt-missing" {
					t.Fatalf("resident filtered should be just the ineligible mock, got %v", sigs(st.filtered))
				}
				got, err := st.diskMocks.LoadWindow(base, base.Add(3*time.Second))
				if err != nil {
					t.Fatalf("LoadWindow: %v", err)
				}
				if len(got) != 2 {
					t.Fatalf("LoadWindow should reconstruct 2 eligible mocks, got %d", len(got))
				}
			} else {
				// Lax: no on-disk store — all 3 per-test mocks stay resident.
				if st.diskMocks != nil {
					t.Fatal("lax mode: on-disk store must NOT engage (no RAM bound to gain)")
				}
				if len(st.filtered) != 3 {
					t.Fatalf("lax mode: all 3 per-test mocks should be resident, got %d", len(st.filtered))
				}
			}

			// config stays resident in unfiltered in BOTH modes.
			if len(st.unfiltered) != 1 || st.unfiltered[0].Name != "cfg-1" {
				t.Fatalf("config should be resident in unfiltered, got %v", sigs(st.unfiltered))
			}
			// freeze anchor = earliest request timestamp (pt-1 @ base+1s) in both
			// modes: folded from the on-disk store under strict, from the resident
			// slice under lax.
			if !ch.called {
				t.Fatal("SetFreezeAnchor was not called")
			}
			if !ch.anchor.Equal(base.Add(1 * time.Second)) {
				t.Fatalf("freeze anchor should be earliest ts %v, got %v", base.Add(1*time.Second), ch.anchor)
			}
		})
	}
}

// --- tests ---

// The core guarantee: streaming ingest yields the same ClientMockStorage
// (same order, same filtered/unfiltered split, same DeriveLifetime result) and
// the same freeze anchor as the legacy whole-slice path.
func TestStoreMocksStream_MatchesLegacy(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	filtered := []*models.Mock{
		mkMock("mock-1", models.HTTP, base.Add(2*time.Second)),
		mkMock("mock-2", models.Mongo, base.Add(1*time.Second)), // earliest → freeze anchor
		mkMock("mock-3", models.HTTP, base.Add(5*time.Second)),
	}
	unfiltered := []*models.Mock{
		mkMock("mock-4", models.DNS, base.Add(3*time.Second)),
		mkMock("mock-5", models.Mongo, base.Add(4*time.Second)),
	}

	// Legacy path.
	legacyHook := &captureHook{}
	RegisterHooks(legacyHook)
	legacyAgent := newTestAgent()
	if err := legacyAgent.StoreMocks(context.Background(), clone(filtered), clone(unfiltered)); err != nil {
		t.Fatalf("legacy StoreMocks: %v", err)
	}
	legacy := loadStorage(t, legacyAgent)

	// Streaming path.
	streamHook := &captureHook{}
	RegisterHooks(streamHook)
	streamAgent := newTestAgent()
	body := encodeStreamBody(t, clone(filtered), clone(unfiltered))
	header, dec := readHeaderAndDecoder(t, body)
	if err := streamAgent.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatalf("StoreMocksStream: %v", err)
	}
	streamed := loadStorage(t, streamAgent)

	if !eqSigs(sigs(legacy.filtered), sigs(streamed.filtered)) {
		t.Fatalf("filtered mismatch:\n legacy=%+v\n stream=%+v", sigs(legacy.filtered), sigs(streamed.filtered))
	}
	if !eqSigs(sigs(legacy.unfiltered), sigs(streamed.unfiltered)) {
		t.Fatalf("unfiltered mismatch:\n legacy=%+v\n stream=%+v", sigs(legacy.unfiltered), sigs(streamed.unfiltered))
	}
	if len(streamed.filtered) != 3 || len(streamed.unfiltered) != 2 {
		t.Fatalf("bad split: filtered=%d unfiltered=%d", len(streamed.filtered), len(streamed.unfiltered))
	}
	// Freeze anchor parity: both must anchor on the earliest ReqTimestampMock.
	wantAnchor := base.Add(1 * time.Second)
	if !legacyHook.called || !legacyHook.anchor.Equal(wantAnchor) {
		t.Fatalf("legacy anchor: called=%v got=%v want=%v", legacyHook.called, legacyHook.anchor, wantAnchor)
	}
	if !streamHook.called || !streamHook.anchor.Equal(wantAnchor) {
		t.Fatalf("stream anchor: called=%v got=%v want=%v", streamHook.called, streamHook.anchor, wantAnchor)
	}
}

func TestStoreMocksStream_Empty(t *testing.T) {
	RegisterHooks(&captureHook{})
	a := newTestAgent()
	body := encodeStreamBody(t, nil, nil)
	header, dec := readHeaderAndDecoder(t, body)
	if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
		t.Fatalf("empty stream: %v", err)
	}
	st := loadStorage(t, a)
	if len(st.filtered) != 0 || len(st.unfiltered) != 0 {
		t.Fatalf("expected empty storage, got f=%d u=%d", len(st.filtered), len(st.unfiltered))
	}
}

// A truncated stream must fail the whole ingest and leave the previously
// published pool intact — never a partial publish.
func TestStoreMocksStream_CorruptMidStream_NoPartialPublish(t *testing.T) {
	RegisterHooks(&captureHook{})
	a := newTestAgent()

	// Pre-existing snapshot that must survive a failed ingest.
	prior := &ClientMockStorage{filtered: []*models.Mock{mkMock("prior", models.HTTP, time.Time{})}}
	a.clientMocks.Store(uint64(0), prior)

	base := time.Now()
	filtered := []*models.Mock{
		mkMock("m1", models.HTTP, base),
		mkMock("m2", models.HTTP, base),
		mkMock("m3", models.HTTP, base),
	}
	body := encodeStreamBody(t, filtered, nil)
	// Cut off partway through the mock values so a mid-stream Decode fails.
	truncated := body[:len(body)-15]
	header, dec := readHeaderAndDecoder(t, truncated)
	err := a.StoreMocksStream(context.Background(), header, dec)
	if err == nil {
		t.Fatal("expected error on truncated stream, got nil")
	}
	// Prior snapshot must be untouched.
	got := loadStorage(t, a)
	if got != prior {
		t.Fatal("prior snapshot was replaced by a partial/failed ingest")
	}
}

// A header with an absurd count must not pre-allocate a huge slice (the cap
// bounds it) and must fail fast rather than accept it — the CodeQL untrusted
// -allocation guard.
func TestStoreMocksStream_HugeCountHeaderIsBounded(t *testing.T) {
	RegisterHooks(&captureHook{})
	a := newTestAgent()
	prior := &ClientMockStorage{filtered: []*models.Mock{mkMock("prior", models.HTTP, time.Time{})}}
	a.clientMocks.Store(uint64(0), prior)

	// Header claims ~1 billion mocks; the body carries none.
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(models.MockStreamHeader{FilteredCount: 1 << 30}); err != nil {
		t.Fatalf("encode header: %v", err)
	}
	header, dec := readHeaderAndDecoder(t, buf.Bytes())

	var err error
	alloc := measureAlloc(func() {
		err = a.StoreMocksStream(context.Background(), header, dec)
	})
	if err == nil {
		t.Fatal("expected error for a header whose count exceeds the body")
	}
	// 1<<30 *Mock pointers would be ~8 GiB; the cap keeps the upfront alloc tiny.
	if alloc > 64<<20 {
		t.Fatalf("pre-allocation not bounded: allocated %d bytes for an absurd header count", alloc)
	}
	if got := loadStorage(t, a); got != prior {
		t.Fatal("prior snapshot replaced by a failed ingest")
	}
}

// TestStoreMocksStream_LowerTransientAlloc verifies streaming decode avoids the
// whole-message gob buffer (≈ the payload) that a single whole-payload decode
// allocates. Uses exact TotalAlloc deltas, so it is deterministic — not flaky.
func TestStoreMocksStream_LowerTransientAlloc(t *testing.T) {
	const n = 3000
	const bodyLen = 8 * 1024
	const payload = n * bodyLen
	body := string(bytes.Repeat([]byte("x"), bodyLen))
	filtered := make([]*models.Mock, n)
	base := time.Now()
	for i := range filtered {
		filtered[i] = mkBigMock("mock-"+itoa(i), base, body)
	}

	var legacyBuf bytes.Buffer
	if err := gob.NewEncoder(&legacyBuf).Encode(models.StoreMocksReq{Filtered: filtered}); err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	legacyAlloc := measureAlloc(func() {
		var req models.StoreMocksReq
		if err := gob.NewDecoder(bytes.NewReader(legacyBuf.Bytes())).Decode(&req); err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		runtime.KeepAlive(req)
	})

	streamBody := encodeStreamBody(t, filtered, nil)
	streamAlloc := measureAlloc(func() {
		a := newTestAgent()
		RegisterHooks(&captureHook{})
		header, dec := readHeaderAndDecoder(t, streamBody)
		if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
			t.Fatalf("stream decode: %v", err)
		}
	})

	saved := int64(legacyAlloc) - int64(streamAlloc)
	t.Logf("transient alloc — legacy: %d KiB, stream: %d KiB, saved: %d KiB (payload %d KiB)",
		legacyAlloc/1024, streamAlloc/1024, saved/1024, payload/1024)
	if saved < payload/2 {
		t.Fatalf("streaming should save ~the whole-message buffer (~%d bytes); saved only %d", payload, saved)
	}
}

// measureAlloc returns the bytes allocated by fn (TotalAlloc delta) — exact and
// deterministic, unlike sampled peak-heap.
func measureAlloc(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func clone(in []*models.Mock) []*models.Mock {
	out := make([]*models.Mock, len(in))
	for i, m := range in {
		cp := *m
		out[i] = &cp
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
