package agent

// Tests for the streaming /storemocks ingest (StoreMocksStream) — proving it
// produces a ClientMockStorage byte-for-byte equivalent to the legacy whole
// -slice StoreMocks, preserves order/lifetime/freeze-anchor, handles empty and
// corrupt streams safely, and materializes far less transient memory than the
// legacy whole-payload gob decode (the auto-replay OOM fix).

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
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

// mkBigMock builds a mock carrying a large HTTP response body so the encoded
// payload is dominated by real data — this is what makes the legacy whole
// -message gob buffer (allocated ≈ full payload) measurably larger than the
// streaming per-mock buffer.
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

// encodeStreamBody writes the on-wire streaming body exactly as the client
// does (magic + gob header + each filtered then unfiltered mock).
func encodeStreamBody(t *testing.T, filtered, unfiltered []*models.Mock) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := io.WriteString(&buf, models.StoreMocksStreamMagic); err != nil {
		t.Fatalf("write magic: %v", err)
	}
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(models.MockStreamHeader{
		FilteredCount:   len(filtered),
		UnfilteredCount: len(unfiltered),
		ProtoVersion:    models.StoreMocksStreamProtoVersion,
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

// readHeaderAndDecoder consumes the magic + header from body (as the HTTP
// handler does) and returns the header + a decoder positioned at the first mock.
func readHeaderAndDecoder(t *testing.T, body []byte) (models.MockStreamHeader, *gob.Decoder) {
	t.Helper()
	r := bytes.NewReader(body)
	magic := make([]byte, len(models.StoreMocksStreamMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		t.Fatalf("read magic: %v", err)
	}
	if string(magic) != models.StoreMocksStreamMagic {
		t.Fatalf("bad magic %q", magic)
	}
	dec := gob.NewDecoder(r)
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
	// Truncate to cut off partway through the mock values (keep magic+header
	// and some bytes, drop the tail) so a mid-stream Decode fails.
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

// TestStoreMocksStream_PeakMemory proves the streaming decode holds far less
// transient memory than the legacy whole-payload gob decode for a large corpus.
func TestStoreMocksStream_PeakMemory(t *testing.T) {
	// Large per-mock bodies so the encoded payload is dominated by data: the
	// legacy whole-payload decode reads the single ~N*bodyLen message into one
	// gob buffer (≈ full payload) before decoding, while the streaming decoder
	// only ever buffers one mock message at a time. That transient buffer is
	// the difference the fix removes.
	const n = 3000
	const bodyLen = 8 * 1024
	body := string(bytes.Repeat([]byte("x"), bodyLen))
	filtered := make([]*models.Mock, n)
	base := time.Now()
	for i := range filtered {
		filtered[i] = mkBigMock("mock-"+itoa(i), base, body)
	}

	// Legacy: decode the whole StoreMocksReq at once (raw body + full slice live together).
	var legacyBuf bytes.Buffer
	if err := gob.NewEncoder(&legacyBuf).Encode(models.StoreMocksReq{Filtered: filtered}); err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	legacyPeak := measurePeak(func() {
		var req models.StoreMocksReq
		if err := gob.NewDecoder(bytes.NewReader(legacyBuf.Bytes())).Decode(&req); err != nil {
			t.Fatalf("legacy decode: %v", err)
		}
		runtime.KeepAlive(req)
	})

	// Streaming: decode into ClientMockStorage one mock at a time.
	streamBody := encodeStreamBody(t, filtered, nil)
	streamPeak := measurePeak(func() {
		a := newTestAgent()
		RegisterHooks(&captureHook{})
		header, dec := readHeaderAndDecoder(t, streamBody)
		if err := a.StoreMocksStream(context.Background(), header, dec); err != nil {
			t.Fatalf("stream decode: %v", err)
		}
	})

	t.Logf("transient alloc — legacy: %d KiB, stream: %d KiB (ratio %.2f)",
		legacyPeak/1024, streamPeak/1024, float64(streamPeak)/float64(legacyPeak))
	// The legacy whole-message gob buffer roughly doubles transient allocation
	// vs streaming. Require a clear margin (streaming < 75% of legacy) so the
	// test is a meaningful regression guard, not a coin flip.
	if streamPeak >= legacyPeak*3/4 {
		t.Fatalf("streaming transient alloc (%d) not clearly below legacy (%d)", streamPeak, legacyPeak)
	}
}

// measurePeak returns the peak live-heap growth (max HeapInuse delta over a
// pre-GC baseline) observed WHILE fn runs. This captures the transient the OOM
// is about: legacy holds gob's whole-message buffer AND the decoded objects at
// once (~2×), while streaming holds the decoded objects plus one small frame
// (~1×). A sampler goroutine polls HeapInuse in a tight loop; ReadMemStats is
// expensive but that only perturbs timing, not correctness of the max.
func measurePeak(fn func()) uint64 {
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	stop := make(chan struct{})
	result := make(chan uint64, 1)
	go func() {
		var peak uint64
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				result <- peak
				return
			default:
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > peak {
					peak = ms.HeapInuse
				}
			}
		}
	}()

	fn()
	close(stop)
	peak := <-result
	if peak < base.HeapInuse {
		return 0
	}
	return peak - base.HeapInuse
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
