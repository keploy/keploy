package learn

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// heldAtOnce makes each read of one of raw's payloads (decodeForms) wait
// until want reads are held at once; all is closed then. The row's end lets
// them go too.
func heldAtOnce(t *testing.T, raw []Raw, want int64, release <-chan struct{}) (counts *sync.Map, all chan struct{}) {
	all = make(chan struct{})
	var held atomic.Int64
	var once sync.Once
	end := make(chan struct{})
	t.Cleanup(func() { close(end) })
	counts = countDecodes(t, raw, func(string) {
		if held.Add(1) == want {
			once.Do(func() { close(all) })
		}
		select {
		case <-release:
		case <-end:
		}
	})
	return counts, all
}

// A set's staging (WarmSet) reads with readersFor the CPUs Go runs on
// (stageReaders) at once: half of them, and at least two on two CPUs or
// more, so a large set is read before its heavy tests open and the replay
// keeps CPU for its calls: with one reader, api-server's set (about 270
// CPU-seconds of reads) was still being read when TestSeeded opened about
// 100 s in, and on two CPUs one reader left the calls 7,936 of its 15,600
// recordings to read on their answer path, two readers 1,606, at the same
// CPU per call answered (readersFor's doc has the numbers). Each payload is
// still read once, by the staging, and none by a call; the set's line says
// how many read it (SetUse.Readers). At the set's NewSet the readers take
// nothing more: those reading end their read, and the set keeps none of it.
// Counted, not timed: each reader is held in its payload's decode until the
// row's count of readers is held at once; a staging that read with fewer
// would never get there, so each held subtest first checks the count it
// waits for.
func TestASetsStagingReadsWithHalfTheCPUsAndAtLeastTwo(t *testing.T) {
	for _, c := range []struct{ procs, want int }{{1, 1}, {2, 2}, {3, 2}, {4, 2}, {5, 2}, {6, 3}, {8, 4}, {16, 8}} {
		if got := readersFor(c.procs); got != c.want {
			t.Errorf("on %d CPUs the staging reads with %d readers: want %d", c.procs, got, c.want)
		}
	}
	was := stageReaders
	for _, procs := range []int{1, 2, 8} {
		prev := runtime.GOMAXPROCS(procs)
		got, want := was(), readersFor(procs)
		runtime.GOMAXPROCS(prev)
		if got != want {
			t.Errorf("with GOMAXPROCS %d the staging reads with %d readers: want readersFor the CPUs Go runs on (%d)", procs, got, want)
		}
	}

	t.Run("two readers at once on two CPUs", func(t *testing.T) {
		prev := runtime.GOMAXPROCS(2)
		t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
		if got := stageReaders(); got != 2 {
			t.Fatalf("on two CPUs the staging reads with %d readers: want 2", got)
		}
		row := newHeldRow(8, 4)
		raw := row.raw()
		release := make(chan struct{})
		counts, all := heldAtOnce(t, raw, 2, release)
		tr := NewTracker()
		tr.NewSet()
		done := tr.WarmSet(srcOf(row.mocks))
		<-all
		close(release)
		use := <-done
		if use.Readers != 2 || use.Staged != 8 || use.Called != 0 {
			t.Fatalf("the set's staging on two CPUs reported %+v: want 2 readers, all 8 payloads read by it, none by a call", use)
		}
		if payloads, decodes := decodesOf(counts); payloads != 8 || decodes != 8 {
			t.Fatalf("2 readers decoded %d of the set's 8 payloads, %d times in all: want each once", payloads, decodes)
		}
	})

	const n, readers = 32, 4
	stageReaders = func() int { return readers }
	t.Cleanup(func() { stageReaders = was })
	row := newHeldRow(n, 4)
	raw := row.raw()

	t.Run("each payload once, by the readers at once", func(t *testing.T) {
		go1 := make(chan struct{})
		counts, all := heldAtOnce(t, raw, readers, go1)
		tr := NewTracker()
		tr.NewSet()
		done := tr.WarmSet(srcOf(row.mocks))
		<-all
		close(go1)
		use := <-done
		if use.Readers != readers || use.Staged != n || use.Called != 0 {
			t.Fatalf("the set's staging reported %+v: want %d readers, all %d payloads read by it, none by a call", use, readers, n)
		}
		if payloads, decodes := decodesOf(counts); payloads != n || decodes != n {
			t.Fatalf("%d readers decoded %d of the set's %d payloads, %d times in all: want each once", readers, payloads, n, decodes)
		}
	})

	t.Run("the next set stops every reader", func(t *testing.T) {
		go2 := make(chan struct{})
		counts, all := heldAtOnce(t, raw, readers, go2)
		tr := NewTracker()
		tr.NewSet()
		s := tr.Drifts().set
		done := tr.WarmSet(srcOf(row.mocks))
		<-all
		tr.NewSet()
		close(go2)
		<-done
		if staged, _, _ := readsOf(s); staged != readers {
			t.Errorf("the staging of a set that was over read %d of its payloads: want only the %d its readers were reading as it ended", staged, readers)
		}
		if payloads, _ := decodesOf(counts); payloads != readers {
			t.Errorf("the staging of a set that was over decoded %d of its %d payloads: want only the %d being read as it ended", payloads, n, readers)
		}
		if b := s.usedBytes(); b != 0 {
			t.Errorf("a set that is over keeps %d bytes its staging read: want none", b)
		}
	})
}
