//go:build linux

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
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// testDisk is a mocks.gob that takes what the writer writes only when the
// test lets it: a FIFO, read into a copy chunk bytes at a time. It starts
// stuck; take lets it read one more chunk, free lets it read everything from
// then on.
type testDisk struct {
	copyPath string
	chunk    int
	// pipeSize is what the FIFO holds, set once ready is closed.
	pipeSize int64
	ready    chan struct{}
	read     atomic.Int64
	mu       sync.Mutex
	freed    bool
	turn     chan struct{} // a chunk to take; closed: take everything
	done     chan error
}

func newTestDisk(t *testing.T, dir string, chunk int) *testDisk {
	t.Helper()
	setDir := filepath.Join(dir, "set-0")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(setDir, "mocks.gob")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &testDisk{copyPath: filepath.Join(dir, "written.gob"), chunk: chunk, ready: make(chan struct{}), turn: make(chan struct{}, 1), done: make(chan error, 1)}
	go func() { d.done <- d.run(fifo) }()
	t.Cleanup(d.free)
	return d
}

func (d *testDisk) run(fifo string) (err error) {
	var once sync.Once
	ready := func() { once.Do(func() { close(d.ready) }) }
	defer ready()
	in, err := os.Open(fifo)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	rc, err := in.SyscallConn()
	if err != nil {
		return err
	}
	var size int
	var serr error
	if err := rc.Control(func(fd uintptr) { size, serr = unix.FcntlInt(fd, unix.F_GETPIPE_SZ, 0) }); err != nil {
		return err
	}
	if serr != nil {
		return serr
	}
	d.pipeSize = int64(size)
	ready()
	out, err := os.Create(d.copyPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	buf := make([]byte, d.chunk)
	for {
		<-d.turn
		n, err := in.Read(buf)
		if _, werr := out.Write(buf[:n]); werr != nil {
			return werr
		}
		d.read.Add(int64(n))
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// take lets the disk read one more chunk, once it has read the last it was
// let.
func (d *testDisk) take() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.freed {
		return
	}
	select {
	case d.turn <- struct{}{}:
	default:
	}
}

// free lets the disk read everything from now on.
func (d *testDisk) free() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.freed {
		d.freed = true
		close(d.turn)
	}
}

// writerClock is the time the process could run, as Close measures the gob
// writer's stall on it (MockYaml.gobClock), moved by what the writer and the
// test's disk do instead of by the box. Each look Close arms lets the disk
// take a chunk at a time until the writer has moved since: the look comes
// with half the stall window on (a writer that keeps writing, however slow
// the disk). Or until the disk has taken more than the pipe held, a chunk
// and a whole piece of a write (progressChunk) since, with the writer not
// moved: the look comes with the whole window on (a writer that wrote a
// piece it did not count as progress).
//
// The tests measured the stall on the process's own clock against a window
// of 300 ms to 1 s, and the test's disk then had to take a chunk within the
// window: on a loaded box it did not, and Close gave up on a writer that was
// still writing.
type writerClock struct {
	ys    *MockYaml
	disk  *testDisk
	mu    sync.Mutex
	ran   time.Duration
	moved int // looks that came with the writer moved
}

func (c *writerClock) Runnable() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ran
}

func (c *writerClock) Moved() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moved
}

func (c *writerClock) After(time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	stop := make(chan struct{})
	// Close read the progress it compares with before it armed this look.
	from, took := c.ys.gobProgress.Load(), c.disk.read.Load()
	go func() {
		select {
		case <-c.disk.ready:
		case <-stop:
			return
		}
		bound := c.disk.pipeSize + int64(c.disk.chunk+progressChunk)
		for {
			// The disk's count first: the writer counts a piece before it
			// writes the next, so once the disk has read past bound, the
			// progress read after it has moved.
			read := c.disk.read.Load()
			if c.ys.gobProgress.Load() != from {
				c.fire(ch, gobStallAfter/2, true)
				return
			}
			if read-took > bound {
				c.fire(ch, gobStallAfter, false)
				return
			}
			c.disk.take()
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	var once sync.Once
	return ch, func() bool { once.Do(func() { close(stop) }); return true }
}

func (c *writerClock) fire(ch chan<- time.Time, by time.Duration, moved bool) {
	c.mu.Lock()
	c.ran += by
	if moved {
		c.moved++
	}
	c.mu.Unlock()
	ch <- time.Time{}
}

// stuckClock is the clock of a process that runs on while the writer does
// not move: each look comes at once, a tenth of the stall window on.
func stuckClock() stall.Clock { return &stall.ManualClock{Step: stall.OnTime} }

// waitsPastTheWindow fails t unless Close looked at a moving writer for more
// than the stall window of run time: a Close that gave up after one window
// whatever the writer did would have passed otherwise.
func waitsPastTheWindow(t *testing.T, clk *writerClock) {
	t.Helper()
	if ran := clk.Runnable(); ran <= gobStallAfter {
		t.Fatalf("Close waited %s of run time (%d looks at a moving writer), not past the stall window %s: the test showed nothing", ran, clk.Moved(), gobStallAfter)
	}
}

// Close waits for a writer that keeps writing, however long a slow disk
// takes: it used to give up after a fixed 5 s, and a recording that ended
// then lost every mock not yet written (on a disk taking about 640 KiB/s,
// 3.3 MB of 9.9 MB were on it). Here the disk takes a chunk for each of
// Close's looks, about 70 of them over the file, half the stall window each.
func TestClose_WaitsForASlowDiskThatKeepsWriting(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := newTestDisk(t, dir, 64<<10)
	ys := New(zap.NewNop(), dir, "mocks")
	clk := &writerClock{ys: ys, disk: disk}
	ys.gobClock = clk
	const n = 280
	insertGobMocks(t, ys, n)
	if err := ys.Close(); err != nil {
		t.Fatalf("Close gave up after %s of run time with %d bytes on the disk: %v", clk.Runnable(), disk.read.Load(), err)
	}
	waitsPastTheWindow(t, clk)
	disk.free()
	if err := <-disk.done; err != nil {
		t.Fatal(err)
	}
	got, err := readGobMocks(disk.copyPath)
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}

// A write the disk takes longer over than the stall window is waited for
// while it goes: the file's buffer writes 256 KiB at once, which a disk
// taking 32 KiB for each of Close's looks takes eight looks, four windows,
// over.
func TestClose_WaitsForAWriteTheDiskTakesLongOver(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := newTestDisk(t, dir, 32<<10)
	ys := New(zap.NewNop(), dir, "mocks")
	clk := &writerClock{ys: ys, disk: disk}
	ys.gobClock = clk
	const n = 60
	insertGobMocks(t, ys, n)
	if err := ys.Close(); err != nil {
		t.Fatalf("Close gave up with %d bytes on the disk: %v", disk.read.Load(), err)
	}
	waitsPastTheWindow(t, clk)
	disk.free()
	if err := <-disk.done; err != nil {
		t.Fatal(err)
	}
	got, err := readGobMocks(disk.copyPath)
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}

// A writer that makes no progress (a stuck disk) is not waited for forever:
// once it has made none for gobStallAfter, Close says how many mocks were not
// written, as models.ErrMocksNotWritten. A later Close waits for it again,
// and once the disk takes the rest, every mock is on it.
func TestClose_SaysWhatAStuckWriterDidNotWrite(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := newTestDisk(t, dir, 64<<10)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 100
	insertGobMocks(t, ys, n)
	ys.gobClock = stuckClock()
	err := ys.Close()
	if !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close returned %v on a stuck disk, want models.ErrMocksNotWritten", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("of the %d mocks handed to it were not encoded", n)) {
		t.Fatalf("Close said %q; want how many of the %d mocks were not written", err, n)
	}

	// The disk takes the rest as Close looks: a Close that did not wait
	// would return with the writer still holding mocks.
	ys.gobClock = &writerClock{ys: ys, disk: disk}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close, once the disk took the rest: %v", err)
	}
	select {
	case <-ys.gobDone:
	default:
		t.Fatalf("Close returned with the writer still writing (%d mocks not encoded)", ys.MocksLeft())
	}
	disk.free()
	if err := <-disk.done; err != nil {
		t.Fatal(err)
	}
	got, err := readGobMocks(disk.copyPath)
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}

// Close does not wait behind an insert that holds the writer's lifecycle
// lock while the disk is stuck: one the full queue sent to the synchronous
// fallback holds it while it waits for the stuck writer, and Close used to
// wait for it for as long as the disk stayed stuck. It says what was not
// written; once the disk takes the rest, a later Close lands every mock.
func TestClose_DoesNotWaitBehindAnInsertOnAStuckDisk(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := newTestDisk(t, dir, 256<<10)
	ys := New(zap.NewNop(), dir, "mocks")
	body := strings.Repeat("x", 512)
	var inserted atomic.Int64
	insertDone := make(chan error, 1)
	var stop atomic.Bool
	go func() {
		for i := 0; !stop.Load(); i++ {
			m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: fmt.Sprintf("mock-%d", i), Spec: models.MockSpec{
				HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y", Body: body},
				HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body},
			}}
			if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
				insertDone <- err
				return
			}
			inserted.Add(1)
		}
		insertDone <- nil
	}()
	for ys.gobOverflows.Load() == 0 {
		select {
		case err := <-insertDone:
			t.Fatalf("the inserts ended (%v) before the queue overflowed", err)
		default:
		}
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	ys.gobClock = stuckClock()
	if err := ys.Close(); !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close returned %v behind an insert held on a stuck disk, want models.ErrMocksNotWritten", err)
	}

	disk.free()
	if err := <-insertDone; err != nil {
		t.Fatal(err)
	}
	ys.gobClock = &writerClock{ys: ys, disk: disk}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close, once the disk took the rest: %v", err)
	}
	if err := <-disk.done; err != nil {
		t.Fatal(err)
	}
	got, err := readGobMocks(disk.copyPath)
	if err != nil || int64(len(got)) != inserted.Load() {
		t.Fatalf("the disk holds %d mocks (%v), want all %d inserted", len(got), err, inserted.Load())
	}
}

// CloseUnless stops waiting for a writer that is still writing once it is
// told to (a user's second interrupt), and says what it had not written; a
// later Close waits for the rest.
func TestCloseUnless_StopsWaitingWhenToldTo(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := newTestDisk(t, dir, 64<<10)
	ys := New(zap.NewNop(), dir, "mocks")
	ys.gobClock = &writerClock{ys: ys, disk: disk}
	const n = 120
	insertGobMocks(t, ys, n)
	stop := make(chan struct{})
	close(stop)
	err := ys.CloseUnless(stop)
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), "the stop was asked to end now") {
		t.Fatalf("CloseUnless returned %v once told to stop, want models.ErrMocksNotWritten saying why", err)
	}
	disk.free()
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-disk.done; err != nil {
		t.Fatal(err)
	}
	got, err := readGobMocks(disk.copyPath)
	if err != nil || len(got) != n {
		t.Fatalf("the disk holds %d mocks (%v), want all %d", len(got), err, n)
	}
}
