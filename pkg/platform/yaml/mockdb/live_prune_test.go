package mockdb

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// What a round prunes and reads of a live set while its recording appends to
// it: a prune drops only mocks the round read, appends made across prunes are
// all kept once, and a read never takes an append part-way through.

func prunedLiveMock(i int, at time.Time, payload int) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.GENERIC,
		Spec: models.MockSpec{
			Metadata: map[string]string{"operation": "GET"},
			GenericRequests: []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{
				Type: "binary", Data: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("GET key%05d", i))),
			}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{
				Type: "binary", Data: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v", payload))),
			}}}},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

func newPrunedSet(t *testing.T, n int, start time.Time) (*MockYaml, string) {
	t.Helper()
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", "")
	for i := 0; i < n; i++ {
		if err := ys.InsertMock(context.Background(), prunedLiveMock(i, start.Add(time.Duration(i)*time.Millisecond), 512), "set-0"); err != nil {
			t.Fatalf("record mock %d: %v", i, err)
		}
	}
	return ys, "set-0"
}

func prunedSetNames(t *testing.T, ys *MockYaml, set string) []string {
	t.Helper()
	var names []string
	if err := ys.WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		names = append(names, m.Name)
	}); err != nil {
		t.Fatalf("walk the set: %v", err)
	}
	sort.Strings(names)
	return names
}

// An auto-replay round reads the live set at its start (the index read) and
// prunes it at its end, keeping what its passing tests consumed and what was
// requested after the round began (pruneBefore, the set's start). A mock
// requested just before the round began that reaches the file after the
// round's read (it was still on its way from the agent) is one no test of
// the round could consume. The set-end prune deletes it, although its test
// case is in the live set, and the next round replays that case without it.
// The same mock appended a moment later, during the prune's own pass, is kept
// (TestAnAppendIsNotHeldByAPruneOfItsFile).
func TestAMockTheRoundNeverReadIsNotPrunedByIt(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	ys, set := newPrunedSet(t, 10, start)
	ctx := context.Background()
	roundStart := time.Now()
	read, err := ys.GetTestSetMocks(ctx, set, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("the round's read: %v", err)
	}
	consumed := map[string]models.MockState{}
	for _, m := range append(append([]*models.Mock(nil), read.AllPerTest...), read.AllSession...) {
		consumed[m.Name] = models.MockState{Name: m.Name}
	}
	if len(consumed) != 10 {
		t.Fatalf("precondition: the round read %d mocks, want 10", len(consumed))
	}
	// Requested a second before the round began, saved after its read.
	if err := ys.InsertMock(ctx, prunedLiveMock(10, roundStart.Add(-time.Second), 512), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.UpdateMocks(ctx, set, consumed, roundStart.UTC(), time.Time{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got := prunedSetNames(t, ys, set)
	for _, n := range got {
		if n == "mock-10" {
			return
		}
	}
	t.Fatalf("the set-end prune deleted mock-10, a mock the round never read and so could not have consumed (set now: %v)", got)
}

// A walk of the set's mocks (a replay's hold on calls with no answer reads them
// so, WalkTestSetMocks) serves no test: a mock saved after the round's read
// and seen only by such a walk is still one the round never read, and its
// prune keeps it.
func TestAWalkOfTheMocksAfterTheRoundsReadLeavesWhatTheRoundNeverReadUnpruned(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	ys, set := newPrunedSet(t, 10, start)
	ctx := context.Background()
	roundStart := time.Now()
	read, err := ys.GetTestSetMocks(ctx, set, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("the round's read: %v", err)
	}
	consumed := map[string]models.MockState{}
	for _, m := range append(append([]*models.Mock(nil), read.AllPerTest...), read.AllSession...) {
		consumed[m.Name] = models.MockState{Name: m.Name}
	}
	if len(consumed) != 10 {
		t.Fatalf("precondition: the round read %d mocks, want 10", len(consumed))
	}
	if err := ys.InsertMock(ctx, prunedLiveMock(10, roundStart.Add(-time.Second), 512), set); err != nil {
		t.Fatal(err)
	}
	walked := 0
	if err := ys.WalkTestSetMocks(ctx, set, func(*models.Mock, bool, bool) { walked++ }); err != nil {
		t.Fatalf("the walk: %v", err)
	}
	if walked != 11 {
		t.Fatalf("precondition: the walk saw %d mocks, want 11", walked)
	}
	if err := ys.UpdateMocks(ctx, set, consumed, roundStart.UTC(), time.Time{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got := prunedSetNames(t, ys, set)
	for _, n := range got {
		if n == "mock-10" {
			return
		}
	}
	t.Fatalf("after a walk of the mocks the set-end prune deleted mock-10, a mock the round never read (set now: %v)", got)
}

// Appends go on while the set is pruned again and again (a keep-everything
// prune: pruneBefore before every mock), three prunes at least running while
// they go on: afterwards the set holds every mock appended, each once.
func TestAppendsDuringRepeatedPrunesAreAllKeptOnce(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	ys, set := newPrunedSet(t, 50, start)
	ctx := context.Background()
	const batches, perBatch = 3, 500
	const appended = batches * perBatch
	pruned := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		for b := 0; b < batches; b++ {
			// A prune has run since the last batch began: the appends
			// meet one prune at least each batch.
			if b > 0 {
				select {
				case <-pruned:
				case <-time.After(time.Minute):
					done <- fmt.Errorf("no prune ran in a minute")
					return
				}
			}
			for i := 50 + b*perBatch; i < 50+(b+1)*perBatch; i++ {
				if err := ys.InsertMock(ctx, prunedLiveMock(i, start.Add(time.Duration(i)*time.Millisecond), 512), set); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	prunes := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			got := prunedSetNames(t, ys, set)
			seen := map[string]int{}
			for _, n := range got {
				seen[n]++
			}
			var missing, twice []string
			for i := 0; i < 50+appended; i++ {
				n := fmt.Sprintf("mock-%d", i)
				switch seen[n] {
				case 0:
					missing = append(missing, n)
				case 1:
				default:
					twice = append(twice, n)
				}
			}
			t.Logf("prunes run during the appends: %d", prunes)
			if len(missing) > 0 || len(twice) > 0 || len(got) != 50+appended {
				t.Fatalf("after appends during %d prunes the set holds %d mocks, want %d: missing %d %v, more than once %d %v",
					prunes, len(got), 50+appended, len(missing), firstTen(missing), len(twice), firstTen(twice))
			}
			return
		default:
		}
		if err := ys.UpdateMocks(ctx, set, map[string]models.MockState{}, start.Add(-time.Hour).UTC(), time.Time{}); err != nil {
			t.Fatalf("prune %d: %v", prunes, err)
		}
		prunes++
		select {
		case pruned <- struct{}{}:
		default:
		}
	}
}

func firstTen(s []string) []string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// halfAppended starts what appendMock does for a mock larger than its 4 KB
// buffer, between its two writes: it holds the set's mock file lock and has
// written the separator and the first half of a whole document. finish
// writes the rest and lets the lock go.
func halfAppended(t *testing.T, ys *MockYaml, set string) (finish func()) {
	t.Helper()
	scratch := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	for i := 0; i < 2; i++ {
		if err := scratch.InsertMock(context.Background(), prunedLiveMock(100+i, time.Now().Add(-time.Hour), 16<<10), "s"); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(scratch.MockPath, "s", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	at := strings.Index(string(data), "\n---\n")
	if at < 0 {
		t.Fatal("precondition: no second document in the scratch file")
	}
	doc := data[at+len("\n---\n"):]
	path := filepath.Join(ys.MockPath, set)
	lock := getMockFileLock(mockFileLockKey(path, "mocks", yaml.FormatYAML))
	lock.Lock()
	f, err := os.OpenFile(filepath.Join(path, "mocks.yaml"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	half := len(doc) / 2
	if _, err := f.Write(append([]byte("---\n"), doc[:half]...)); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	return func() {
		_, werr := f.Write(doc[half:])
		cerr := f.Close()
		lock.Unlock()
		if werr != nil || cerr != nil {
			t.Fatalf("finish the append: %v %v", werr, cerr)
		}
	}
}

// The skipped-mock read waits out an append in flight: it never reports the
// mock being appended as cut short.
func TestTheSkippedMockReadWaitsOutAnAppendInFlight(t *testing.T) {
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	if err := ys.InsertMock(context.Background(), prunedLiveMock(0, time.Now().Add(-time.Hour), 512), "set-0"); err != nil {
		t.Fatal(err)
	}
	finish := halfAppended(t, ys, "set-0")
	got := make(chan map[string]models.SkippedMock, 1)
	go func() {
		_, skipped := ys.SkippedMocks(context.Background(), "set-0")
		got <- skipped
	}()
	time.Sleep(300 * time.Millisecond)
	finish()
	skipped := <-got
	if skipped == nil {
		t.Fatal("the skipped-mock read could not read the file")
	}
	for name, sk := range skipped {
		t.Fatalf("the skipped-mock read reported %s, the mock being appended, as skipped (%s): it read the file part-way through an append", name, sk.Why)
	}
}

// The replay's read (the round-start index read) waits out an append in
// flight: the mock being appended is never one it skips as cut short.
func TestTheReplaysReadWaitsOutAnAppendInFlight(t *testing.T) {
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	if err := ys.InsertMock(context.Background(), prunedLiveMock(0, time.Now().Add(-time.Hour), 512), "set-0"); err != nil {
		t.Fatal(err)
	}
	finish := halfAppended(t, ys, "set-0")
	type res struct {
		m   models.TestSetMocks
		err error
	}
	got := make(chan res, 1)
	go func() {
		m, err := ys.GetTestSetMocks(context.Background(), "set-0", time.Time{}, time.Time{}, nil, nil)
		got <- res{m, err}
	}()
	time.Sleep(300 * time.Millisecond)
	finish()
	r := <-got
	if r.err != nil {
		t.Fatalf("the replay's read failed: %v", r.err)
	}
	if len(r.m.Skipped) > 0 {
		t.Fatalf("the replay's read skipped %v: it read the file part-way through an append", r.m.Skipped)
	}
}

// The round's write-back of what it learned rewrites the file between its
// read and its prune, keeping every mock: the prune still keeps the mock the
// round never read.
func TestAMockTheRoundNeverReadIsNotPrunedAfterAWriteBackOfWhatItLearned(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	ys, set := newPrunedSet(t, 10, start)
	ctx := context.Background()
	roundStart := time.Now()
	read, err := ys.GetTestSetMocks(ctx, set, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("the round's read: %v", err)
	}
	consumed := map[string]models.MockState{}
	for _, m := range append(append([]*models.Mock(nil), read.AllPerTest...), read.AllSession...) {
		consumed[m.Name] = models.MockState{Name: m.Name}
	}
	if err := ys.InsertMock(ctx, prunedLiveMock(10, roundStart.Add(-time.Second), 512), set); err != nil {
		t.Fatal(err)
	}
	learned := map[string]models.MockState{"mock-3": {Name: "mock-3", ReqBodyNoise: map[string][]string{"body.ts": {}}}}
	if _, err := ys.PersistLearned(ctx, set, learned); err != nil {
		t.Fatalf("write back what the round learned: %v", err)
	}
	if err := ys.InsertMock(ctx, prunedLiveMock(11, roundStart.Add(-time.Second), 512), set); err != nil {
		t.Fatal(err)
	}
	if err := ys.UpdateMocks(ctx, set, consumed, roundStart.UTC(), time.Time{}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got := prunedSetNames(t, ys, set)
	want := map[string]bool{"mock-10": false, "mock-11": false}
	for _, n := range got {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	if !want["mock-10"] || !want["mock-11"] || len(got) != 12 {
		t.Fatalf("after a write-back between the round's read and its prune, the prune dropped mocks the round never read: set now %v", got)
	}
}

// An auto-replay round's read of the live set as it starts (its index read)
// holds less than the mocks file: each mock's payload stays in the file, read
// back one at a time as it is sent, and only a small entry per mock is kept.
// A long recording's live file reaches hundreds of MB in a k8s-proxy of a few
// GB.
func TestTheRoundsIndexReadOfTheLiveSetHoldsLessThanTheFile(t *testing.T) {
	ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", yaml.FormatYAML)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	const n = 20000
	for i := 0; i < n; i++ {
		if err := ys.InsertMock(ctx, prunedLiveMock(i, start.Add(time.Duration(i)*time.Millisecond), 2700), "set-0"); err != nil {
			t.Fatalf("record mock %d: %v", i, err)
		}
	}
	info, err := os.Stat(filepath.Join(ys.MockPath, "set-0", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	size := uint64(info.Size())
	runtime.GC()
	before := liveHeap()
	var set models.TestSetMocks
	peak := indexPeakHeap(func() { set, err = ys.IndexTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil) })
	if err != nil {
		t.Fatalf("index read: %v", err)
	}
	runtime.GC()
	kept := liveHeap() - before
	if got := len(set.Filtered) + len(set.Unfiltered); got != n {
		t.Fatalf("the index read %d mocks, want %d", got, n)
	}
	t.Logf("mocks file %d bytes (%d mocks): the index read held %d bytes at its peak (%.2fx the file) and keeps %d (%.2fx)",
		size, n, peak, float64(peak)/float64(size), kept, float64(kept)/float64(size))
	if peak > size || kept > size/2 {
		t.Fatalf("the index read of a %d-byte mocks file held %.2fx it at its peak and keeps %.2fx: it holds the payloads", size, float64(peak)/float64(size), float64(kept)/float64(size))
	}
	runtime.KeepAlive(set)
}

func liveHeap() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// indexPeakHeap is the most heap fn held above what was live before it,
// sampled every millisecond, with the collector at its default pace.
func indexPeakHeap(fn func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	runtime.GC()
	base := liveHeap()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			if v := liveHeap(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	fn()
	close(stop)
	<-stopped
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}
