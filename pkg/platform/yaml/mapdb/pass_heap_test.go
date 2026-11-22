package mapdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Each pass a recording and its auto-replay rounds make over a live set's
// mappings file holds less heap than it did before the file was read and
// written a few entries at a time, in every shape such a file takes:
//
//   - one entry: most mocks under one test entry, the one keploy files every
//     call it could not place in a test under (a recording made before its
//     agent named such calls' tests has nothing else);
//   - mixed: that entry and 500 tests of 20 mocks;
//   - many entries: 2,000 tests of 20 mocks.
//
// The passes: the recorder's flush, a round's read of the tests' mocks (Get),
// the round's write (Insert), and the recorder's flush after that write. Each
// is measured as the most heap it held above what was live before it, sampled
// every millisecond with the collector at its default pace, the least of
// three runs: what the pass holds, not a moment the machine was busy. This
// file compiles on its own, so the same measurement runs against code that
// reads and writes the file whole. There, as multiples of the file, the
// passes held: one entry 64, 15, 65 and 74; mixed 64, 16, 65 and 65; many
// entries 64, 18, 65 and 73. The bounds below are under every one of those.

// passPeakHeap is the most heap fn held above what was live before it.
func passPeakHeap(fn func()) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	base := sample[0].Value.Uint64()
	var peak atomic.Uint64
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
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

// passEntries are n mock entries named prefix-0 on.
func passEntries(prefix string, n int) []models.MockEntry {
	out := make([]models.MockEntry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, models.MockEntry{Name: fmt.Sprintf("%s-%d", prefix, i), Kind: "Redis",
			ReqTimestampMock: "2026-11-19T10:00:00.000000001Z", ResTimestampMock: "2026-11-19T10:00:00.000000002Z"})
	}
	return out
}

func TestEachPassOverTheMappingsFileHoldsLessThanAWholeFilePassDid(t *testing.T) {
	// bound is each pass's, as a multiple of the file: under what the whole
	// file passes held in that shape (above).
	type bound struct{ flush, read, write, flushAfter float64 }
	for _, tc := range []struct {
		name  string
		big   int
		tests int
		bound bound
	}{
		{"one entry of 40,000 mocks", 40000, 0, bound{12, 13, 16, 12}},
		{"one entry of 40,000 mocks and 500 tests of 20", 40000, 500, bound{12, 13, 16, 12}},
		{"2,000 tests of 20 mocks", 0, 2000, bound{12, 13, 16, 12}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			recorder := New(zap.NewNop(), dir, "mappings")
			ctx := context.Background()
			first := map[string][]models.MockEntry{}
			if tc.big > 0 {
				first[""] = passEntries("async", tc.big)
			}
			for i := 0; i < tc.tests; i++ {
				first[fmt.Sprintf("test-%d", i)] = passEntries(fmt.Sprintf("m%d", i), 20)
			}
			if err := recorder.UpsertBatch(ctx, "set-0", first); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, "set-0", "mappings.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			size := float64(info.Size())
			flushes := 0
			flush := func() error {
				flushes++
				b := map[string][]models.MockEntry{fmt.Sprintf("late-%d", flushes): passEntries(fmt.Sprintf("late%d", flushes), 20)}
				if tc.big > 0 {
					b[""] = passEntries(fmt.Sprintf("async%d", flushes), 200)
				}
				return recorder.UpsertBatch(ctx, "set-0", b)
			}
			round := New(zap.NewNop(), dir, "mappings")
			least := func(what string, limit float64, pass func() error) {
				t.Helper()
				var held uint64
				for i := 0; i < 3; i++ {
					var err error
					p := passPeakHeap(func() { err = pass() })
					if err != nil {
						t.Fatalf("%s: %v", what, err)
					}
					if i == 0 || p < held {
						held = p
					}
				}
				x := float64(held) / size
				t.Logf("%s: a mappings file of %.0f bytes: %s held %d bytes of heap (%.1fx the file)", tc.name, size, what, held, x)
				if x > limit {
					t.Errorf("%s: %s held %.1fx the %.0f-byte mappings file, over %.0fx: a whole-file pass held more than that", tc.name, what, x, size, limit)
				}
			}
			least("the recorder's flush", tc.bound.flush, flush)
			least("the round's read of its tests' mocks", tc.bound.read, func() error {
				got, _, err := round.Get(ctx, "set-0")
				if err == nil && len(got) != tc.tests+flushes+map[bool]int{true: 1}[tc.big > 0] {
					err = fmt.Errorf("read %d tests", len(got))
				}
				return err
			})
			least("the round's write", tc.bound.write, func() error {
				return round.Insert(ctx, &models.Mapping{Version: string(models.V1Beta1), Kind: models.MappingKind, TestSetID: "set-0",
					TestCases: []models.MappedTestCase{{ID: "test-x", Mocks: passEntries("x", 20)}}})
			})
			least("the recorder's flush after the round's write", tc.bound.flushAfter, flush)
			got, _, err := recorder.Get(ctx, "set-0")
			if err != nil || len(got[""]) != map[bool]int{true: tc.big + 200*flushes}[tc.big > 0] || len(got["test-x"]) != 20 || len(got[fmt.Sprintf("late-%d", flushes)]) != 20 {
				t.Fatalf("the file after the passes: %d mocks under the one entry, test-x %d, late-%d %d, err %v",
					len(got[""]), len(got["test-x"]), flushes, len(got[fmt.Sprintf("late-%d", flushes)]), err)
			}
		})
	}
}
