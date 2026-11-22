package util

import (
	"context"
	"errors"
	"io"
	"testing"

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

// bytesAfterEOFReader returns (0, io.EOF) on its first Read and its bytes on
// the next: the reader an empty-EOF retry was kept for, whose EOF is not its
// end. No reader keploy reads through does this (see ReadBytes).
type bytesAfterEOFReader struct {
	late  []byte
	reads int
}

func (r *bytesAfterEOFReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return 0, io.EOF
	}
	return copy(p, r.late), nil
}

// TestAnEmptyEOFIsBelievedAtTheReadThatReturnsIt: ReadBytes and
// ReadBytesWaiting return an EOF with nothing read from the Read that returns
// it, and read no more, even from a reader that would hand bytes over next.
// They used to read an empty EOF again three times, 50 ms apart, which a
// loaded host ran late (a row that bounded that wait's time failed at about 5
// load per CPU), and which only ever delayed a close: no reader keploy reads
// through returns bytes after an EOF.
func TestAnEmptyEOFIsBelievedAtTheReadThatReturnsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(context.Context, *zap.Logger, io.Reader) ([]byte, error)
	}{
		{"ReadBytes", ReadBytes},
		{"ReadBytesWaiting", ReadBytesWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &bytesAfterEOFReader{late: []byte("late")}
			buf, err := tc.read(context.Background(), zap.NewNop(), r)
			if !errors.Is(err, io.EOF) || buf != nil {
				t.Fatalf("%s = (%q, %v), want (nil, io.EOF): the EOF was not believed", tc.name, buf, err)
			}
			if r.reads != 1 {
				t.Fatalf("%s read %d times, want 1: it read on past an EOF", tc.name, r.reads)
			}
		})
	}
}

// TestReadBytesDoesNotTakeAReadThatEndsAfterItsContext: a Read that returns
// bytes after the read's context ended is not taken; the context's error is
// returned instead, as it always was.
func TestReadBytesDoesNotTakeAReadThatEndsAfterItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := readerFunc(func(p []byte) (int, error) {
		cancel()
		return copy(p, "late"), nil
	})
	buf, err := ReadBytes(ctx, zap.NewNop(), r)
	if !errors.Is(err, context.Canceled) || buf != nil {
		t.Fatalf("ReadBytes = (%q, %v), want (nil, context.Canceled): it took a read that ended after its context", buf, err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestReadBytes_MidStreamEOFFastReturn: an EOF that comes with bytes returns
// them with the EOF, from its one Read.
func TestReadBytes_MidStreamEOFFastReturn(t *testing.T) {
	r := &stubReader{data: []byte("hello"), err: io.EOF}
	buf, err := ReadBytes(context.Background(), zap.NewNop(), r)

	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("expected buf %q, got %q", "hello", buf)
	}
	if r.called != 1 {
		t.Fatalf("mid-stream EOF read %d times; must return on its first read", r.called)
	}
}

// TestReadBytes_TableDriven: what one Read gives is returned with its error,
// from that one Read; a Read that gave nothing returns nil.
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
		{
			name:    "empty EOF returns nil and EOF",
			data:    nil,
			readErr: io.EOF,
			wantBuf: nil,
			wantErr: io.EOF,
		},
		{
			name:    "empty read no error returns nil",
			data:    nil,
			readErr: nil,
			wantBuf: nil,
			wantErr: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &stubReader{data: tc.data, err: tc.readErr}
			buf, err := ReadBytes(context.Background(), zap.NewNop(), r)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected err %v, got %v", tc.wantErr, err)
			}
			if string(buf) != string(tc.wantBuf) || (tc.wantBuf == nil) != (buf == nil) {
				t.Fatalf("expected buf %q, got %q", tc.wantBuf, buf)
			}
			if r.called != 1 {
				t.Fatalf("case %q read %d times; expected one read", tc.name, r.called)
			}
		})
	}
}

// BenchmarkReadBytesEOF measures the per-call cost of ReadBytes when the
// reader immediately signals io.EOF with no data: one Read, no wait.
func BenchmarkReadBytesEOF(b *testing.B) {
	logger := zap.NewNop()
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		r := &stubReader{data: nil, err: io.EOF}
		_, _ = ReadBytes(ctx, logger, r)
	}
}

// BenchmarkReadBytesHappyPath exercises the normal flow: data arrives,
// then EOF, in one Read.
func BenchmarkReadBytesHappyPath(b *testing.B) {
	logger := zap.NewNop()
	ctx := context.Background()
	payload := []byte("hello world; this is a typical short message")
	for i := 0; i < b.N; i++ {
		r := &stubReader{data: payload, err: io.EOF}
		_, _ = ReadBytes(ctx, logger, r)
	}
}
