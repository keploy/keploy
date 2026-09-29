package util

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

// flushCountingTransport counts Sentry flushes. With slow set it serves them
// one at a time, 200ms each, like the real transport's single worker when it
// is busy or Sentry is unreachable.
type flushCountingTransport struct {
	flushes atomic.Int64
	slow    bool
	mu      sync.Mutex
}

func (t *flushCountingTransport) Configure(sentry.ClientOptions) {}
func (t *flushCountingTransport) SendEvent(*sentry.Event)        {}
func (t *flushCountingTransport) Flush(timeout time.Duration) bool {
	t.flushes.Add(1)
	if t.slow {
		t.mu.Lock()
		defer t.mu.Unlock()
		time.Sleep(min(timeout, 200*time.Millisecond))
	}
	return true
}

// withSentry initialises the global hub as a release build does (with a DSN).
// Tests using it must not run in parallel.
func withSentry(t *testing.T, tr sentry.Transport) {
	t.Helper()
	prev := sentry.CurrentHub().Client()
	if err := sentry.Init(sentry.ClientOptions{Dsn: "https://public@sentry.invalid/1", Transport: tr}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() { sentry.CurrentHub().BindClient(prev) })
}

// stepReader hands out step bytes per Read, then err (io.EOF when nil).
type stepReader struct {
	data []byte
	step int
	err  error
}

func (r *stepReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := min(r.step, len(p), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestRecoverFlushesSentryOnlyAfterAPanic(t *testing.T) {
	tr := &flushCountingTransport{}
	withSentry(t, tr)

	func() { defer Recover(zap.NewNop(), nil, nil) }()
	if got := tr.flushes.Load(); got != 0 {
		t.Fatalf("Recover flushed Sentry %d time(s) on a clean return", got)
	}
	func() {
		defer Recover(zap.NewNop(), nil, nil)
		panic("boom")
	}()
	if tr.flushes.Load() == 0 {
		t.Fatal("Recover did not flush Sentry after recovering a panic")
	}
}

// Each parser read used to end in a Sentry flush, so under load every MySQL
// connection's parser queued behind Sentry's one transport worker (~40% of a
// CPU-capped agent's time in the production-shaped repro). 64 one-byte reads
// against a slow transport took ~13s.
func TestReadRequiredBytesDoesNotWaitOnSentry(t *testing.T) {
	tr := &flushCountingTransport{slow: true}
	withSentry(t, tr)

	want := []byte(strings.Repeat("x", 64))
	done := make(chan error, 1)
	go func() {
		b, err := ReadRequiredBytes(context.Background(), zap.NewNop(), &stepReader{data: append([]byte(nil), want...), step: 1}, len(want))
		if err == nil && string(b) != string(want) {
			err = errors.New("wrong bytes: " + string(b))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("64 one-byte reads took over 3s (%d Sentry flushes): reads are waiting on Sentry", tr.flushes.Load())
	}
	if got := tr.flushes.Load(); got != 0 {
		t.Fatalf("ReadRequiredBytes flushed Sentry %d time(s) with no panic", got)
	}
}

func TestReadRequiredBytes(t *testing.T) {
	boom := errors.New("boom")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name     string
		ctx      context.Context
		r        io.Reader
		n        int
		want     string
		wantErr  error
		minDelay time.Duration
		rest     string
	}{
		{name: "partial reads accumulate, nothing more is consumed", r: &stepReader{data: []byte("0123456789"), step: 3}, n: 7, want: "0123456", rest: "789"},
		{name: "reader error returns what was read", r: &stepReader{data: []byte("ab"), step: 1, err: boom}, n: 4, want: "ab", wantErr: boom},
		{name: "EOF is retried before it is believed", r: &stepReader{data: []byte("xy"), step: 2}, n: 5, want: "xy", wantErr: io.EOF, minDelay: 300 * time.Millisecond},
		{name: "cancelled context", ctx: cancelled, r: &stepReader{data: []byte("abcd"), step: 1}, n: 4, wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			start := time.Now()
			b, err := ReadRequiredBytes(ctx, zap.NewNop(), tc.r, tc.n)
			if !errors.Is(err, tc.wantErr) || string(b) != tc.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", b, err, tc.want, tc.wantErr)
			}
			if d := time.Since(start); d < tc.minDelay {
				t.Fatalf("returned after %v, want at least %v", d, tc.minDelay)
			}
			if tc.rest != "" {
				if rest, _ := io.ReadAll(tc.r); string(rest) != tc.rest {
					t.Fatalf("reader left %q, want %q", rest, tc.rest)
				}
			}
		})
	}
}

type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) { panic("reader bug") }

// A panic in Read ends the read with an error. The old helper goroutine
// swallowed it and never delivered a result, so the read hung.
func TestReadRequiredBytesContainsAPanickingReader(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := ReadRequiredBytes(context.Background(), zap.NewNop(), panickingReader{}, 4)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("err = %v, want the panic as an error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRequiredBytes hung on a panicking reader")
	}
}
