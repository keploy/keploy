package util

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
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

// A declared length is only a claim. A misframed MySQL header declares up to
// 16 MiB; the read used to allocate all of it up front and hold it, outside
// every capture bound, until that much data arrived. What it allocates is now
// bounded by what arrives.
func TestReadRequiredBytesAllocatesWhatArrivesNotWhatIsDeclared(t *testing.T) {
	const declared = 16<<20 - 1 // the largest length a MySQL packet header can declare
	stop := errors.New("no more data")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	b, err := ReadRequiredBytes(context.Background(), zap.NewNop(), &stepReader{data: make([]byte, 21), step: 21, err: stop}, declared)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, stop) || len(b) != 21 {
		t.Fatalf("got (%d bytes, %v), want the 21 bytes that arrived and the reader's error", len(b), err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("reading 21 of a declared %d bytes allocated %d bytes", declared, got)
	}
}

// Every byte of a long read arrives, in order, however the reader splits it.
func TestAppendRequiredBytesGrowsAcrossManyReads(t *testing.T) {
	want := make([]byte, 3*readGrowStep+17)
	for i := range want {
		want[i] = byte(i * 7)
	}
	dst := []byte("hdr:")
	got, err := AppendRequiredBytes(context.Background(), zap.NewNop(), &stepReader{data: append([]byte(nil), want...), step: 4093}, dst, len(want))
	if err != nil {
		t.Fatal(err)
	}
	if string(got[:4]) != "hdr:" || !bytes.Equal(got[4:], want) {
		t.Fatalf("appended %d bytes that differ from the %d read", len(got)-4, len(want))
	}
	if cap(got) != len(got) {
		t.Fatalf("a completed read keeps %d bytes of slack", cap(got)-len(got))
	}
}

// RCA #2's stall: one Sentry event that cannot be delivered (egress blocked,
// the event from a recovered parser panic) made every packet read wait for a
// Flush(2s) on v3.6.78, about 6s for three reads, with no CPU burnt. The
// server here accepts and never answers, as a blackholed Sentry does.
func TestReadRequiredBytesIsNotSlowedByAnUndeliverableSentryEvent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	prev := sentry.CurrentHub().Client()
	if err := sentry.Init(sentry.ClientOptions{Dsn: "http://public@" + ln.Addr().String() + "/1"}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() {
		sentry.CurrentHub().BindClient(prev)
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})

	sentry.CaptureMessage("a parser panicked")
	time.Sleep(100 * time.Millisecond) // the transport is now blocked sending it
	r := &stepReader{data: bytes.Repeat([]byte{1, 0, 0, 1}, 3), step: 4}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := ReadRequiredBytes(context.Background(), zap.NewNop(), r, 4); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("3 packet-header reads took %v with an undeliverable Sentry event queued: reads are waiting on Sentry", d)
	}
}
