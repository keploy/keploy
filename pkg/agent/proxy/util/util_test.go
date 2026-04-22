package util

import (
	"context"
	"errors"
	"io"
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

// TestReadBytes_EOFReturnsImmediately verifies that an empty read with io.EOF
// returns without the historical 100ms x 5 retry sleep. The old code could
// block up to ~500ms here; we require well under 10ms to prove the sleep
// is gone.
func TestReadBytes_EOFReturnsImmediately(t *testing.T) {
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
	// 10ms allows ample headroom for goroutine scheduling jitter while still
	// failing loudly if the 100ms sleep ever comes back.
	if elapsed > 10*time.Millisecond {
		t.Fatalf("ReadBytes on empty EOF took %v; must return immediately (<10ms)", elapsed)
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
// reader immediately signals io.EOF with no data. Before the sleep removal
// this reported ~400-500 ms/op (up to 5 * 100ms sleep). After removal it
// should be well under 1ms/op (typically ~1µs on modern hardware).
func BenchmarkReadBytesEOF(b *testing.B) {
	logger := zap.NewNop()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		r := &stubReader{data: nil, err: io.EOF}
		_, _ = ReadBytes(ctx, logger, r)
	}
}
