package mockdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// insertGobMocks hands n mocks of about 16 KiB each to a gob store at dir.
func insertGobMocks(t *testing.T, ys *MockYaml, n int) {
	t.Helper()
	body := strings.Repeat("x", 8<<10)
	for i := 0; i < n; i++ {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: fmt.Sprintf("mock-%d", i), Spec: models.MockSpec{
			Metadata: map[string]string{"i": fmt.Sprint(i)},
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y", Body: body},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body},
		}}
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatal(err)
		}
	}
}

// MocksLeft is how many of the session's mocks the writer has yet to
// encode: none once Close has written them.
func TestGobWriter_MocksLeft(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	ys.gobMu.Lock() // the writer waits for it before it encodes any
	insertGobMocks(t, ys, 5)
	held := ys.MocksLeft()
	ys.gobMu.Unlock()
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if held != 5 {
		t.Fatalf("%d mocks left of the 5 handed to a writer that had encoded none, want 5", held)
	}
	if left := ys.MocksLeft(); left != 0 {
		t.Fatalf("%d mocks left once Close wrote them, want 0", left)
	}
}

// A mock is written once the file has taken its last byte, and not before:
// the writes the file takes count the mocks they land, in order, and a mock
// whose bytes the file took as it was encoded counts at once.
func TestGobFileWriter_CountsAMockOnceTheFileTookItWhole(t *testing.T) {
	var written atomic.Uint64
	f := &gobFileWriter{w: io.Discard, written: &written}
	step := func(what string, want uint64) {
		t.Helper()
		if got := written.Load(); got != want {
			t.Fatalf("%s: %d mocks written, want %d", what, got, want)
		}
	}
	f.encoded(10)
	f.encoded(12)
	step("two mocks in the buffer", 0)
	_, _ = f.Write(make([]byte, 9))
	step("the first short of its last byte", 0)
	_, _ = f.Write(make([]byte, 1))
	step("the first's last byte taken", 1)
	_, _ = f.Write(make([]byte, 10))
	step("the second taken, and more", 2)
	f.encoded(20)
	step("a mock the file took as it was encoded", 3)
}

// The file's writes are handed on in pieces, and each piece the file takes
// counts the mocks it lands: a write that fails part-way still counts the
// mocks it landed before it failed, and none after.
func TestGobFileWriter_CountsEachPieceTheFileTakes(t *testing.T) {
	var written atomic.Uint64
	var pieces []int
	fail := errors.New("the disk failed")
	f := &gobFileWriter{written: &written, w: writerFunc(func(b []byte) (int, error) {
		pieces = append(pieces, len(b))
		if len(pieces) == 3 {
			return 2, fail
		}
		return len(b), nil
	})}
	f.encoded(progressChunk - 1)   // in the first piece
	f.encoded(2*progressChunk + 2) // ends in the third, inside what it took
	f.encoded(2*progressChunk + 3) // ends past what the third took
	n, err := f.Write(make([]byte, 3*progressChunk))
	if !errors.Is(err, fail) || n != 2*progressChunk+2 {
		t.Fatalf("wrote %d (%v), want %d and the disk's error", n, err, 2*progressChunk+2)
	}
	if len(pieces) != 3 || pieces[0] != progressChunk || pieces[2] != progressChunk {
		t.Fatalf("pieces %v, want three of %d", pieces, progressChunk)
	}
	if got := written.Load(); got != 2 {
		t.Fatalf("%d mocks written, want the 2 the file took whole", got)
	}
}

// stuckDisk stands in for a gob file (gobFileOf): it takes the writes that
// fit in its first take bytes, then, on the first that does not, closes
// blocked and, when fail is set, fails that write and every later one; else
// it does not return from it until release closes, and then takes the rest.
// took is what the writes that returned took.
type stuckDisk struct {
	take    int
	fail    error
	took    []byte
	blocked chan struct{}
	release chan struct{}
}

func useStuckDisk(t *testing.T, take int, fail error) *stuckDisk {
	t.Helper()
	d := &stuckDisk{take: take, fail: fail, blocked: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	saved := gobFileOf
	gobFileOf = func(f *os.File) io.Writer {
		return writerFunc(func(b []byte) (int, error) {
			if len(d.took)+len(b) > d.take {
				once.Do(func() { close(d.blocked) })
				if d.fail != nil {
					return 0, d.fail
				}
				<-d.release
				return f.Write(b)
			}
			d.took = append(d.took, b...)
			return f.Write(b)
		})
	}
	t.Cleanup(func() { gobFileOf = saved })
	return d
}

// wholeMocksIn is how many mocks the gob stream's first bytes hold whole.
func wholeMocksIn(t *testing.T, b []byte) int {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prefix.gob")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, _, err := readGobMocksCut(p)
	if err != nil {
		t.Fatalf("reading the %d bytes the disk took: %v", len(b), err)
	}
	return len(got)
}

// Close waits for a write the disk does not return from, for as long as it
// does not return: no clock gives up on it and drops the mocks it holds.
// Once the disk takes the rest, every mock is on it.
func TestClose_WaitsForAWriteThatDoesNotReturnUntilItDoes(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := useStuckDisk(t, 100<<10, nil)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 20 // about 320 KiB: the buffer writes once it holds 256 KiB
	insertGobMocks(t, ys, n)
	closed := make(chan error, 1)
	go func() { closed <- ys.Close() }()
	<-disk.blocked
	// Long past any no-progress bound a test would have set: Close waits.
	hold := time.NewTimer(2 * time.Second)
	defer hold.Stop()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the disk's write had not", err)
	case <-hold.C:
	}
	close(disk.release)
	if err := <-closed; err != nil {
		t.Fatalf("Close, once the disk took the rest: %v", err)
	}
	got, err := readGobMocks(filepath.Join(dir, "set-0", "mocks.gob"))
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}

// A disk that is stuck ends at the caller's stop, which says exactly how many
// mocks the file had not taken whole, and a later Close lands them all once
// the disk takes the rest.
func TestCloseUnless_EndsAStuckDiskAtTheStopAndCountsWhatItDidNotWrite(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := useStuckDisk(t, 100<<10, nil)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 20
	insertGobMocks(t, ys, n)
	stop := make(chan struct{})
	closed := make(chan error, 1)
	go func() { closed <- ys.CloseUnless(stop) }()
	<-disk.blocked
	close(stop)
	err := <-closed
	left := n - wholeMocksIn(t, disk.took)
	want := fmt.Sprintf("%d of the %d mocks handed to the gob mock writer were not written", left, n)
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), want) {
		t.Fatalf("CloseUnless at a stuck disk's stop returned %v, want models.ErrMocksNotWritten saying %q", err, want)
	}
	if left == 0 || left == n {
		t.Fatalf("the disk took %d bytes holding %d of %d mocks whole: the test wants some, not all, written", len(disk.took), n-left, n)
	}
	close(disk.release)
	if err := ys.Close(); err != nil {
		t.Fatalf("Close, once the disk took the rest: %v", err)
	}
	got, err := readGobMocks(filepath.Join(dir, "set-0", "mocks.gob"))
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}

// A disk that fails a write says so: Close returns its error, and exactly how
// many mocks the file had not taken whole, as models.ErrMocksNotWritten.
func TestClose_SaysHowManyMocksAFailingDiskDidNotWrite(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := useStuckDisk(t, 100<<10, syscall.EIO)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 20
	insertGobMocks(t, ys, n)
	err := ys.Close()
	left := n - wholeMocksIn(t, disk.took)
	want := fmt.Sprintf("%d of the %d mocks handed to the gob mock writer were not written", left, n)
	if !errors.Is(err, models.ErrMocksNotWritten) || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Close on a failing disk returned %v, want models.ErrMocksNotWritten with the disk's error, saying %q", err, want)
	}
}

// The file the writer leaves for another test set's is finished as at Close:
// a tail it could not take is said, with its mocks counted as not written.
// It used to be flushed with its error dropped, and Close said nothing.
func TestClose_SaysWhatTheFileLeftForAnotherTestSetCouldNotTake(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	saved := gobFileOf
	gobFileOf = func(f *os.File) io.Writer {
		if filepath.Base(filepath.Dir(f.Name())) == "set-0" {
			return writerFunc(func([]byte) (int, error) { return 0, syscall.ENOSPC })
		}
		return f
	}
	t.Cleanup(func() { gobFileOf = saved })
	ys := New(zap.NewNop(), dir, "mocks")
	insertGobMocks(t, ys, 3) // well within the buffer: set-0's file takes none of it yet
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: "other", Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
		HTTPResp: &models.HTTPResp{StatusCode: 200},
	}}
	if err := ys.InsertMock(context.Background(), m, "set-1"); err != nil {
		t.Fatal(err)
	}
	err := ys.Close()
	want := "3 of the 4 mocks handed to the gob mock writer were not written"
	if !errors.Is(err, models.ErrMocksNotWritten) || !errors.Is(err, syscall.ENOSPC) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Close returned %v, want models.ErrMocksNotWritten with set-0's error, saying %q", err, want)
	}
	if got, err := readGobMocks(filepath.Join(dir, "set-1", "mocks.gob")); err != nil || len(got) != 1 {
		t.Fatalf("set-1 holds %d mocks (%v), want its 1", len(got), err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
