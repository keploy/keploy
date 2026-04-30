package util

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// stubReader is an io.Reader whose Read call returns the configured (n, err)
// exactly once, then returns (0, io.EOF) on subsequent calls.
type stubReader struct {
	data   []byte
	err    error
	called int
}

func (s *stubReader) Read(p []byte) (int, error) {
	s.called++
	if s.called == 1 {
		n := copy(p, s.data)
		return n, s.err
	}
	return 0, io.EOF
}

// TestReadBytes_EmptyEOFRetriesWithinBudget asserts that a zero-byte EOF on
// the very first read is retried inside the bounded handshake window. The
// old Track-D fast-return surfaced this as a listmonk boot failure
// ("error connecting to DB: EOF") because the replayer's startup response
// was still being written when the app issued its first Read. Upper bound
// (HandshakeEOFRetryMax * HandshakeEOFRetrySleep) + scheduling jitter.
func TestReadBytes_EmptyEOFRetriesWithinBudget(t *testing.T) {
	r := &stubReader{data: nil, err: io.EOF}
	logger := zap.NewNop()

	start := time.Now()
	buf, err := ReadBytes(context.Background(), logger, r)
	elapsed := time.Since(start)

	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if len(buf) != 0 {
		t.Fatalf("expected empty buffer, got %d bytes", len(buf))
	}
	minExpected := time.Duration(HandshakeEOFRetryMax) * HandshakeEOFRetrySleep
	maxExpected := minExpected + 100*time.Millisecond
	if elapsed < minExpected {
		t.Fatalf("expected at least %v of retry wait, got %v — handshake retry budget missing", minExpected, elapsed)
	}
	if elapsed > maxExpected {
		t.Fatalf("empty-EOF retry took %v; budget cap is %v", elapsed, maxExpected)
	}
}

// TestReadBytes_MidStreamEOFFastReturn verifies Track-D's perf win stays
// intact: once any bytes have been read inside the call, EOF surfaces
// immediately without incurring the handshake retry window.
func TestReadBytes_MidStreamEOFFastReturn(t *testing.T) {
	r := &stubReader{data: []byte("hello"), err: io.EOF}
	logger := zap.NewNop()

	start := time.Now()
	buf, err := ReadBytes(context.Background(), logger, r)
	elapsed := time.Since(start)

	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("expected buf %q, got %q", "hello", buf)
	}
	// This case must NOT incur the handshake retry budget.
	if elapsed > 10*time.Millisecond {
		t.Fatalf("mid-stream EOF took %v; must fast-return (<10ms) to preserve Track D perf win", elapsed)
	}
}

// slowHandshakeReader simulates the listmonk Postgres v3 handshake race:
// the first N reads return (0, io.EOF) because the replayer's startup
// response hasn't landed yet. After the first write completes, subsequent
// reads deliver the payload. If ReadBytes honours its handshake retry
// budget, the test passes; if it fast-returns, the reader never gets a
// chance to deliver the data.
type slowHandshakeReader struct {
	payload       []byte
	earlyEOFs     int32
	eofsSeen      int32
	delivered     bool
	deliveredOnce int32
}

func (s *slowHandshakeReader) Read(p []byte) (int, error) {
	seen := atomic.AddInt32(&s.eofsSeen, 1)
	if seen <= s.earlyEOFs {
		return 0, io.EOF
	}
	if atomic.AddInt32(&s.deliveredOnce, 1) == 1 {
		s.delivered = true
		n := copy(p, s.payload)
		return n, nil
	}
	return 0, io.EOF
}

// TestReadBytes_HandshakeRace is the regression test for the listmonk
// boot failure. A client-side race returns (0, EOF) on the first two
// reads while the replayer finishes writing its startup response; the
// third attempt delivers the data. Under Track D fast-return this
// failed; with the bounded retry budget it succeeds.
func TestReadBytes_HandshakeRace(t *testing.T) {
	r := &slowHandshakeReader{
		payload:   []byte("startup-response"),
		earlyEOFs: 2, // two spurious EOFs, then payload
	}
	logger := zap.NewNop()

	start := time.Now()
	buf, err := ReadBytes(context.Background(), logger, r)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected nil error after retry succeeds, got %v", err)
	}
	if string(buf) != "startup-response" {
		t.Fatalf("expected handshake payload, got %q", buf)
	}
	// Must stay under the handshake budget cap plus scheduling slack.
	budget := time.Duration(HandshakeEOFRetryMax)*HandshakeEOFRetrySleep + 100*time.Millisecond
	if elapsed > budget {
		t.Fatalf("handshake-race recovery took %v; budget is %v", elapsed, budget)
	}
}

// TestReadBytes_TableDriven covers the non-sleep cases: data-only read,
// data-then-EOF on the same call, and a non-EOF error surfaced unchanged.
func TestReadBytes_TableDriven(t *testing.T) {
	sentinel := errors.New("some non-eof error")

	tests := []struct {
		name    string
		data    []byte
		readErr error
		wantBuf []byte
		wantErr error
	}{
		{
			name:    "data then EOF on same read returns both",
			data:    []byte("hello"),
			readErr: io.EOF,
			wantBuf: []byte("hello"),
			wantErr: io.EOF,
		},
		{
			name:    "short data no error returns data cleanly",
			data:    []byte("partial"),
			readErr: nil,
			wantBuf: []byte("partial"),
			wantErr: nil,
		},
		{
			name:    "non-EOF error surfaces unchanged",
			data:    nil,
			readErr: sentinel,
			wantBuf: nil,
			wantErr: sentinel,
		},
	}

	logger := zap.NewNop()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &stubReader{data: tc.data, err: tc.readErr}
			start := time.Now()
			buf, err := ReadBytes(context.Background(), logger, r)
			elapsed := time.Since(start)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected err %v, got %v", tc.wantErr, err)
			}
			if string(buf) != string(tc.wantBuf) {
				t.Fatalf("expected buf %q, got %q", tc.wantBuf, buf)
			}
			// No case in this table should trigger a sleep; guard against
			// accidental reintroduction of a retry delay.
			if elapsed > 10*time.Millisecond {
				t.Fatalf("case %q took %v; expected <10ms", tc.name, elapsed)
			}
		})
	}
}

// BenchmarkReadBytesEOF measures the per-call cost of ReadBytes when the
// reader immediately signals io.EOF with no data. The bounded handshake
// retry budget (3 * 50ms = 150ms worst case) dominates this number in
// the pathological empty-EOF case — still 3x better than the pre-Track-D
// 500ms sleep, and the happy path (next benchmark) stays microsecond-scale.
func BenchmarkReadBytesEOF(b *testing.B) {
	logger := zap.NewNop()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		r := &stubReader{data: nil, err: io.EOF}
		_, _ = ReadBytes(ctx, logger, r)
	}
}

// BenchmarkReadBytesHappyPath exercises the normal flow: data arrives,
// then EOF. The handshake retry must NOT trigger here because the buffer
// is non-empty on the EOF tick. Proves Track D's record-hot-path goal
// (<150ms/op total wall; in practice microseconds).
func BenchmarkReadBytesHappyPath(b *testing.B) {
	logger := zap.NewNop()
	ctx := context.Background()
	payload := []byte("hello world; this is a typical short message")
	for i := 0; i < b.N; i++ {
		r := &stubReader{data: payload, err: io.EOF}
		_, _ = ReadBytes(ctx, logger, r)
	}
}

// TestReadBytes_HappyPathPerf asserts a strict upper bound on the
// handshake-aware retry's cost in the happy path (data present on the
// first read). Serves as a CI-level perf guardrail: if someone adds a
// delay that fires before checking len(buffer), this fails.
func TestReadBytes_HappyPathPerf(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()
	payload := []byte("hello world")

	// Warm once to discount first-run scheduler cost.
	_, _ = ReadBytes(ctx, logger, &stubReader{data: payload, err: io.EOF})

	const iterations = 50
	start := time.Now()
	for i := 0; i < iterations; i++ {
		r := &stubReader{data: payload, err: io.EOF}
		buf, err := ReadBytes(ctx, logger, r)
		if err != io.EOF || string(buf) != "hello world" {
			t.Fatalf("unexpected result: buf=%q err=%v", buf, err)
		}
	}
	elapsed := time.Since(start)
	// 150ms for 50 calls = 3ms/call average. Real hardware is microseconds.
	// Fails loudly if the handshake sleep ever leaks into the happy path.
	if elapsed > 150*time.Millisecond {
		t.Fatalf("happy-path ReadBytes averaged over %v/call (total %v); handshake retry must NOT fire on data-present EOF", elapsed/iterations, elapsed)
	}
}
