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

// A set's staging (WarmSet) reads with half the
// CPUs Go runs on (stageReaders: GOMAXPROCS/2, at least one) at once, so a
// large set is read before its first heavy test opens and the replay keeps
// the other half: with one reader, api-server's set (about 270 CPU-seconds
// of reads) was still being read when TestSeeded opened about 100 s in.
// Each payload is still read once, by the staging, and none by a call; the
// set's line says how many read it (SetUse.Readers). At the set's NewSet the
// readers take nothing more: those reading end their read, and the set
// keeps none of it. Counted, not timed: each reader is held in its payload's
// decode until the row's count of readers is held at once; a staging that
// read with fewer would never get there, and ends at go test's timeout with
// its readers parked in the hold.
func TestASetsStagingReadsWithHalfTheCPUsAtOnce(t *testing.T) {
	for _, c := range []struct{ procs, want int }{{1, 1}, {2, 1}, {3, 1}, {8, 4}, {16, 8}} {
		was := runtime.GOMAXPROCS(c.procs)
		got := halfTheCPUs()
		runtime.GOMAXPROCS(was)
		if got != c.want {
			t.Errorf("with GOMAXPROCS %d the staging reads with %d readers: want %d", c.procs, got, c.want)
		}
	}
	was := stageReaders
	if got, want := was(), halfTheCPUs(); got != want {
		t.Errorf("the staging reads with %d readers: want half the CPUs Go runs on (%d)", got, want)
	}
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
