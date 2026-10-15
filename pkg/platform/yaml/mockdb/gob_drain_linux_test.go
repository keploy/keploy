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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// slowDisk is a mocks.gob that takes what the writer writes only as fast as
// read lets it: a FIFO, read into a copy chunk bytes at a time, every pause,
// while it is not paused.
type slowDisk struct {
	copyPath string
	read     atomic.Int64
	paused   atomic.Bool
	done     chan error
}

func newSlowDisk(t *testing.T, dir string, chunk int, pause time.Duration) *slowDisk {
	t.Helper()
	setDir := filepath.Join(dir, "set-0")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(setDir, "mocks.gob")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	d := &slowDisk{copyPath: filepath.Join(dir, "written.gob"), done: make(chan error, 1)}
	go func() {
		d.done <- func() error {
			in, err := os.Open(fifo)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.Create(d.copyPath)
			if err != nil {
				return err
			}
			defer out.Close()
			buf := make([]byte, chunk)
			for {
				if d.paused.Load() {
					time.Sleep(pause)
					continue
				}
				n, err := in.Read(buf)
				d.read.Add(int64(n))
				if _, werr := out.Write(buf[:n]); werr != nil {
					return werr
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				time.Sleep(pause)
			}
		}()
	}()
	return d
}

// Close waits for a writer that keeps writing, however long a slow disk
// takes: it used to give up after a fixed 5 s, and a recording that ended
// then lost every mock not yet written (on a disk taking about 640 KiB/s,
// 3.3 MB of 9.9 MB were on it). Here the disk takes about 7 s, with progress
// every 100 ms, against a stall window of 1 s.
func TestClose_WaitsForASlowDiskThatKeepsWriting(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	saved := gobStallAfter
	gobStallAfter = time.Second
	t.Cleanup(func() { gobStallAfter = saved })
	dir := t.TempDir()
	disk := newSlowDisk(t, dir, 64<<10, 100*time.Millisecond)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 280
	insertGobMocks(t, ys, n)
	start := time.Now()
	if err := ys.Close(); err != nil {
		t.Fatalf("Close gave up after %s with %d bytes on the disk: %v", time.Since(start).Round(time.Millisecond), disk.read.Load(), err)
	}
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
// taking 32 KiB every 100 ms takes 800 ms over, against a window of 500 ms.
func TestClose_WaitsForAWriteTheDiskTakesLongOver(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	saved := gobStallAfter
	gobStallAfter = 500 * time.Millisecond
	t.Cleanup(func() { gobStallAfter = saved })
	dir := t.TempDir()
	disk := newSlowDisk(t, dir, 32<<10, 100*time.Millisecond)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 60
	insertGobMocks(t, ys, n)
	if err := ys.Close(); err != nil {
		t.Fatalf("Close gave up with %d bytes on the disk: %v", disk.read.Load(), err)
	}
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
	saved := gobStallAfter
	gobStallAfter = 300 * time.Millisecond
	t.Cleanup(func() { gobStallAfter = saved })
	dir := t.TempDir()
	disk := newSlowDisk(t, dir, 64<<10, 10*time.Millisecond)
	disk.paused.Store(true)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 100
	insertGobMocks(t, ys, n)
	err := ys.Close()
	if !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close returned %v on a stuck disk, want models.ErrMocksNotWritten", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("of the %d mocks handed to it were not encoded", n)) {
		t.Fatalf("Close said %q; want how many of the %d mocks were not written", err, n)
	}

	disk.paused.Store(false)
	if err := ys.Close(); err != nil {
		t.Fatalf("Close, once the disk took the rest: %v", err)
	}
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
	saved := gobStallAfter
	gobStallAfter = 300 * time.Millisecond
	t.Cleanup(func() { gobStallAfter = saved })
	dir := t.TempDir()
	disk := newSlowDisk(t, dir, 256<<10, time.Millisecond)
	disk.paused.Store(true)
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
	if err := ys.Close(); !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close returned %v behind an insert held on a stuck disk, want models.ErrMocksNotWritten", err)
	}

	disk.paused.Store(false)
	if err := <-insertDone; err != nil {
		t.Fatal(err)
	}
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
	disk := newSlowDisk(t, dir, 64<<10, 100*time.Millisecond)
	ys := New(zap.NewNop(), dir, "mocks")
	const n = 120
	insertGobMocks(t, ys, n)
	stop := make(chan struct{})
	close(stop)
	err := ys.CloseUnless(stop)
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), "the stop was asked to end now") {
		t.Fatalf("CloseUnless returned %v once told to stop, want models.ErrMocksNotWritten saying why", err)
	}
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
