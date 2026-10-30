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
	"testing/synctest"
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
// it does not return from it until free, and then takes the rest. took is
// what the writes that returned took. The test's end frees it, so a row that
// fails leaves no write hanging.
type stuckDisk struct {
	take     int
	fail     error
	took     []byte
	blocked  chan struct{}
	release  chan struct{}
	freeOnce sync.Once
}

func (d *stuckDisk) free() { d.freeOnce.Do(func() { close(d.release) }) }

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
	t.Cleanup(d.free)
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
// does not return: no clock, of any length, gives up on it and drops the
// mocks it holds. Once the disk takes the rest, every mock is on it. The row
// runs in a synctest bubble, whose clock moves a day at once while the write
// has not returned: a cutoff of any length would fire in it. Main gave up
// after 30 s with no progress.
func TestClose_WaitsForAWriteThatDoesNotReturnUntilItDoes(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	const n = 20 // about 320 KiB: the buffer writes once it holds 256 KiB
	synctest.Test(t, func(t *testing.T) {
		disk := useStuckDisk(t, 100<<10, nil)
		ys := New(zap.NewNop(), dir, "mocks")
		insertGobMocks(t, ys, n)
		closed := make(chan error, 1)
		go func() { closed <- ys.Close() }()
		<-disk.blocked
		time.Sleep(24 * time.Hour) // the bubble's clock: every timer in it fires
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("Close returned (%v) a day after the disk's write had not", err)
		default:
		}
		disk.free()
		if err := <-closed; err != nil {
			t.Fatalf("Close, once the disk took the rest: %v", err)
		}
	})
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
	disk.free()
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

// Close says a mock handed to the writer that the file never took as not
// written even when nothing failed: a recording that is not complete never
// looks complete. (A mock counted as handed over that the writer never saw
// stands in for any such mock.)
func TestClose_SaysAMockTheWriterNeverWroteWhenNothingFailed(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	insertGobMocks(t, ys, 3)
	ys.gobQueued.Add(1)
	err := ys.Close()
	want := "1 of the 4 mocks handed to the gob mock writer were not written: the gob mock writer ended without writing them"
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Close returned %v, want models.ErrMocksNotWritten saying %q", err, want)
	}
}

// insertTinyGobMock hands ys one mock of a few hundred bytes for set.
func insertTinyGobMock(t *testing.T, ys *MockYaml, set string) {
	t.Helper()
	m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
		HTTPResp: &models.HTTPResp{StatusCode: 200},
	}}
	if err := ys.InsertMock(context.Background(), m, set); err != nil {
		t.Fatal(err)
	}
}

// fillGobQueue fills ys's gob queue with set-0 mocks while the caller holds
// gobMu: the writer holds the first, waiting for gobMu, and the queue the
// rest, so the next insert finds it full. It returns how many it handed.
func fillGobQueue(t *testing.T, ys *MockYaml) int {
	t.Helper()
	insertTinyGobMock(t, ys, "set-0")
	for len(ys.gobQueue) > 0 { // until the writer took it
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < cap(ys.gobQueue); i++ {
		insertTinyGobMock(t, ys, "set-0")
	}
	return 1 + cap(ys.gobQueue)
}

// overflowGobQueue fills ys's gob queue (fillGobQueue) and hands it one more
// mock, for set, then lets the writer write: it returns once that insert
// did, with how many mocks it handed in all.
func overflowGobQueue(t *testing.T, ys *MockYaml, set string) int {
	t.Helper()
	ys.gobMu.Lock()
	n := fillGobQueue(t, ys)
	inserted := make(chan error, 1)
	go func() {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		}}
		inserted <- ys.InsertMock(context.Background(), m, set)
	}()
	for ys.gobOverflows.Load() == 0 { // until it found the queue full
		time.Sleep(time.Millisecond)
	}
	ys.gobMu.Unlock()
	if err := <-inserted; err != nil {
		t.Fatalf("the insert that found the queue full: %v", err)
	}
	return n + 1
}

// mockNamesIn are the names of the mocks in a test set's gob file, in the
// file's order.
func mockNamesIn(t *testing.T, dir, set string) []string {
	t.Helper()
	got, err := readGobMocks(filepath.Join(dir, set, "mocks.gob"))
	if err != nil {
		t.Fatalf("reading %s: %v", set, err)
	}
	names := make([]string, len(got))
	for i, m := range got {
		names[i] = m.Name
	}
	return names
}

// An insert that finds the queue full waits for room, in its turn: the file
// holds the mocks in the order they were handed over. It used to write its
// mock itself, ahead of the thousands queued before it.
func TestInsertMock_AFullQueueKeepsTheMocksInTheOrderTheyCame(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	n := overflowGobQueue(t, ys, "set-0")
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	names := mockNamesIn(t, dir, "set-0")
	if len(names) != n {
		t.Fatalf("set-0 holds %d mocks, want the %d handed", len(names), n)
	}
	for i, name := range names {
		if want := fmt.Sprint("mock-", i); name != want {
			t.Fatalf("set-0's mock %d is %s, want %s: the file is not in the order the mocks came", i, name, want)
		}
	}
}

// An insert for another test set that finds the queue full does not move the
// writer to that set's file ahead of the first set's queued mocks: the first
// set's file keeps every mock. Writing it ahead made the writer reopen the
// first set's file for the rest, which truncated it under the mocks it had
// written there and counted as written.
func TestInsertMock_AFullQueueAtATestSetSwitchKeepsTheFirstSetsMocks(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	n := overflowGobQueue(t, ys, "set-1")
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if got := mockNamesIn(t, dir, "set-0"); len(got) != n-1 {
		t.Fatalf("set-0 holds %d mocks, want the %d handed for it", len(got), n-1)
	}
	if got := mockNamesIn(t, dir, "set-1"); len(got) != 1 || got[0] != fmt.Sprint("mock-", n-1) {
		t.Fatalf("set-1 holds %v, want mock-%d alone", got, n-1)
	}
}

// An insert waiting for room that its caller gives up on (its context ends)
// says so, and its mock is not counted as handed over: Close then says the
// rest were all written. The caller has the error for it.
func TestInsertMock_AFullQueueGivesUpAtTheCallersCancel(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	ys.gobMu.Lock()
	n := fillGobQueue(t, ys)
	ctx, cancel := context.WithCancel(context.Background())
	inserted := make(chan error, 1)
	go func() {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		}}
		inserted <- ys.InsertMock(ctx, m, "set-0")
	}()
	for ys.gobOverflows.Load() == 0 { // until it waits for room
		time.Sleep(time.Millisecond)
	}
	cancel()
	err := <-inserted
	ys.gobMu.Unlock()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the insert given up on returned %v, want context.Canceled", err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close after the cancelled insert: %v", err)
	}
	if got := mockNamesIn(t, dir, "set-0"); len(got) != n {
		t.Fatalf("set-0 holds %d mocks, want the %d handed before the cancelled one", len(got), n)
	}
}

// stuckDiskCallEndsAtCancel hands 20 mocks to a gob store at a disk whose
// write never returns and, once the disk holds that write, runs call, which
// closes the store, with a ctx that is then cancelled: call must end at the
// cancel. It returns what call returned, with the store's dir.
func stuckDiskCallEndsAtCancel(t *testing.T, call func(ys *MockYaml, ctx context.Context) error) (error, string) {
	t.Helper()
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	disk := useStuckDisk(t, 100<<10, nil)
	ys := New(zap.NewNop(), dir, "mocks")
	// The test's end lets the disk go and waits for the writer to end, so it
	// does not outlive the row (it reads the package's seams as it ends).
	t.Cleanup(func() { disk.free(); _ = ys.Close() })
	insertGobMocks(t, ys, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	<-disk.blocked // the writer has opened the set's file, and is stuck in it
	done := make(chan error, 1)
	go func() { done <- call(ys, ctx) }()
	cancel()
	select {
	case err := <-done:
		return err, dir
	case <-time.After(time.Minute): // only turns a hang into a failure
		disk.free()
		<-done
		t.Fatal("it did not end at its caller's cancel, on a write that never returns")
		return nil, dir
	}
}

// keploy mock record promotes its staging set (PromoteStagedSet), which
// closes the gob writer: on a disk that is stuck, it ends at its caller's
// cancel with how many mocks were not written, and leaves the target as it
// was. Its close had no stop: it hung.
func TestPromoteStagedSet_OnAStuckDiskEndsAtItsCallersCancel(t *testing.T) {
	err, dir := stuckDiskCallEndsAtCancel(t, func(ys *MockYaml, ctx context.Context) error {
		if err := os.MkdirAll(filepath.Join(ys.MockPath, "set-target"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(ys.MockPath, "set-target", "mocks.gob"), []byte("old"), 0o644); err != nil {
			return err
		}
		return ys.PromoteStagedSet(ctx, "set-0", "set-target")
	})
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), "of the 20 mocks handed to the gob mock writer were not written") {
		t.Fatalf("PromoteStagedSet at its caller's cancel returned %v, want models.ErrMocksNotWritten saying how many of the 20", err)
	}
	if got, rerr := os.ReadFile(filepath.Join(dir, "set-target", "mocks.gob")); rerr != nil || string(got) != "old" {
		t.Fatalf("the target set holds %q (%v), want it as it was", got, rerr)
	}
}

// The discard of a staging set (DiscardStagedSet: keploy mock record's at any
// failure, the user's give-up included) ends at its caller's cancel on a disk
// that is stuck, and removes the staging set. Its close had no stop: it hung.
func TestDiscardStagedSet_OnAStuckDiskEndsAtItsCallersCancel(t *testing.T) {
	err, dir := stuckDiskCallEndsAtCancel(t, func(ys *MockYaml, ctx context.Context) error {
		return ys.DiscardStagedSet(ctx, "set-0")
	})
	if err != nil {
		t.Fatalf("DiscardStagedSet at its caller's cancel: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "set-0")); !os.IsNotExist(serr) {
		t.Fatalf("the staging set is still there (%v)", serr)
	}
}

// The prune (UpdateMocks) quiesces the gob writer before it rewrites the
// file: on a disk that is stuck, it ends at its caller's cancel with how many
// mocks were not written. Its close had no stop: it hung.
func TestUpdateMocks_OnAStuckDiskEndsAtItsCallersCancel(t *testing.T) {
	err, _ := stuckDiskCallEndsAtCancel(t, func(ys *MockYaml, ctx context.Context) error {
		return ys.UpdateMocks(ctx, "set-0", map[string]models.MockState{}, time.Time{}, time.Time{})
	})
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), "of the 20 mocks handed to the gob mock writer were not written") {
		t.Fatalf("UpdateMocks at its caller's cancel returned %v, want models.ErrMocksNotWritten saying how many of the 20", err)
	}
}

// The write-back of learned noise (PersistMockNoise) quiesces the gob writer
// as the prune does, and ends at its caller's cancel the same way.
func TestPersistMockNoise_OnAStuckDiskEndsAtItsCallersCancel(t *testing.T) {
	err, _ := stuckDiskCallEndsAtCancel(t, func(ys *MockYaml, ctx context.Context) error {
		return ys.PersistMockNoise(ctx, "set-0", map[string]models.MockState{"mock-0": {ReqBodyNoise: map[string][]string{"body.x": nil}}})
	})
	if !errors.Is(err, models.ErrMocksNotWritten) || !strings.Contains(err.Error(), "of the 20 mocks handed to the gob mock writer were not written") {
		t.Fatalf("PersistMockNoise at its caller's cancel returned %v, want models.ErrMocksNotWritten saying how many of the 20", err)
	}
}

// A file that took every mock, then failed as it was closed (a write back the
// OS reports only then), may not hold them: Close says so, with the file's
// error, as models.ErrMocksNotWritten. Returning nil would have a recording
// that is not on disk look complete.
func TestClose_SaysAFileThatFailedAsItClosedAfterTakingEveryMock(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	saved := closeGobFile
	closeGobFile = func(f *os.File) error {
		if err := f.Close(); err != nil {
			return err
		}
		return syscall.EIO
	}
	t.Cleanup(func() { closeGobFile = saved })
	ys := New(zap.NewNop(), t.TempDir(), "mocks")
	insertGobMocks(t, ys, 3)
	err := ys.Close()
	want := "the 3 mocks handed to the gob mock writer were written, but the mock file failed as it was closed"
	if !errors.Is(err, models.ErrMocksNotWritten) || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), want) {
		t.Fatalf("Close returned %v, want models.ErrMocksNotWritten with the file's error, saying %q", err, want)
	}
}

// An insert whose caller has already given up (its ctx is done) may still be
// queued while the queue has room, or not: one that is not queued is not
// counted as handed to the writer, so Close does not say a mock was not
// written that was never handed over. Run often enough that both happen.
func TestInsertMock_AnInsertWhoseCallerHasGivenUpIsCountedOnlyIfQueued(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	insertTinyGobMock(t, ys, "set-0") // the writer is running
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queued, refused := 1, 0
	for i := 0; i < 200; i++ {
		m := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		}}
		switch err := ys.InsertMock(ctx, m, "set-0"); {
		case err == nil:
			queued++
		case errors.Is(err, context.Canceled):
			refused++
		default:
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if refused == 0 {
		t.Fatal("setup: no insert was refused at its done ctx in 200")
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close after %d inserts refused at their done ctx: %v", refused, err)
	}
	if got := mockNamesIn(t, dir, "set-0"); len(got) != queued {
		t.Fatalf("set-0 holds %d mocks, want the %d queued", len(got), queued)
	}
}
