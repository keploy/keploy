package agent

// Tests for the streaming /storemocks ingest (StoreMocksStream).

import (
	"bytes"
	"context"
	"encoding/gob"
	"runtime"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

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
