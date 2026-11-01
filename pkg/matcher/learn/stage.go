package learn

import (
	"runtime"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// WarmSet reads, in the background, what the rules of every call read of
// each payload of the running set (warm), for each recording src visits,
// once for the set: the agent calls it as it stages a set of record's full
// round, src visiting every recording of the set (those staged and those
// the agent keeps on disk), so the set's payloads are read before its tests'
// calls read them, off their answer path. Before, the set's first call that
// drifted read every payload recorded before it while its client waited
// (api-server's round: tens of seconds on one core, past the client's 30 s).
//
// It waits on no clock. A call reads what it needs of the set as it did:
// each recording neither it nor the staging has taken it reads itself, and
// it waits only for those of its own recordings another is reading (each
// of the staging's readers reads one at a time), never for the staging to
// end. Of a recording a call took first, which the call read only what
// every call's rules read of it, the staging reads the rest (stageRest),
// so no later call reads a part of it on its answer path. It reads with
// stageReaders readers at once (readersFor the CPUs Go runs on), so the set
// is read before its heavy tests open while the replay keeps CPU for its
// calls: one reader alone was still reading api-server's set when its
// first heavy test opened (about 270 CPU-seconds of the set's reads, that
// test about 100 s in). It stops at the set's NewSet (the set
// keeps nothing more) or where the set's budget is spent (setBudget: the
// set reads the rest once per test, as before), whichever comes first, and
// the channel it returns gives then what the set it staged read and kept
// (SetUse), with src's error where src failed, and is closed. A nil src
// reads nothing.
func (t *Tracker) WarmSet(src func(visit func(*models.Mock) bool) error) <-chan SetUse {
	done := make(chan SetUse, 1)
	if t == nil || src == nil {
		close(done)
		return done
	}
	t.mu.Lock()
	// A cache of its own, on the set's (newDrifts): what it reads past the
	// set's budget it keeps for none of the set's tests, and lets go of as
	// it ends.
	dc := t.newDrifts()
	t.mu.Unlock()
	readers := stageReaders()
	go func() {
		defer close(done)
		err := dc.warmSet(src, readers)
		u := dc.set.use()
		u.Readers, u.Err = readers, err
		done <- u
	}()
	return done
}

// stageReaders is how many readers a set's staging reads its recordings
// with (WarmSet): readersFor the CPUs Go runs on (GOMAXPROCS, which follows
// the CPU quota of the agent's own cgroup, as a container's limit sets it,
// and not a parent's: memoryLimit says why memory differs). A var for the
// rows that hold one reader's read.
var stageReaders = func() int { return readersFor(runtime.GOMAXPROCS(0)) }

// readersFor is how many readers a set's staging reads with on procs CPUs:
// half of them, so the replay keeps the other half for its calls, and at
// least two on two CPUs or more (two on two and three CPUs, where half
// gives one); one on one CPU.
//
// Two, not one, on a small agent, because a recording the staging has not
// begun is read by the first call that reads it, on that call's answer
// path: a second reader moves reads off the calls, it adds none. On two
// CPUs (the agent's own cgroup quota, GOMAXPROCS 2), api-server's real
// round (15,600 recordings) measured:
//   - one reader: the calls read 7,936 of the recordings themselves, the
//     staging ended 24 s after the round's heaviest test opened, and that
//     test took 27 s, against its client's 30 s timeout per request;
//   - two readers: the calls read 1,606, the staging ended 9 s after the
//     test opened, and the test took 15 s;
//   - the same CPU per call answered (0.19 s and 0.20 s) and the same peak
//     memory (4.87 GB and 4.85 GB).
func readersFor(procs int) int {
	if procs < 2 {
		return 1
	}
	if n := procs / 2; n > 2 {
		return n
	}
	return 2
}

// warmSet is WarmSet's read, on dc's set (setOf), by readers readers at
// once: each recording src visits is read (warmOne) as the payload it
// recorded (a lazy Raw of it unserved: LazyRawOf, its answer the agent
// parked on disk read back first, withAnswer). src visits them one at a
// time, in recorded order, and each is taken (take) only once a reader is
// free to read it, so none is taken ahead of the readers: a call reads
// itself every recording no reader has begun, and a set over (take: past
// its budget, or after its NewSet) is visited no further. A recording a
// call took first is read too, for the parts the call's read leaves
// (stageRest), whether or not the call has ended it. What is derived
// of a payload depends on its text alone, the recording's, however a test
// was served it (setDerived), so the calls' reads of a recording, served
// or not, find the staging's; the readers share dc as one test's handlers
// do.
func (dc *DriftCache) warmSet(src func(visit func(*models.Mock) bool) error, readers int) error {
	if dc == nil || dc.set == nil {
		return nil
	}
	s := dc.set
	if readers < 1 {
		readers = 1
	}
	type read struct {
		x  Raw
		ch chan struct{}
		// rest: a call took x first, and the reader reads only what the
		// call's read of it leaves (stageRest).
		rest bool
	}
	// free holds a token for each reader not reading; next never holds
	// more than the readers' count, so a hand-off never waits.
	free, next := make(chan struct{}, readers), make(chan read, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		free <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range next {
				if r.rest {
					dc.stageRest(s, r.x)
				} else {
					dc.warmOne(s, r.x, r.ch, true)
				}
				free <- struct{}{}
			}
		}()
	}
	err := src(func(m *models.Mock) bool {
		if m == nil {
			return true
		}
		<-free
		x := dc.LazyRawOf(m, false, withAnswer(m))
		ch, mine := s.take(x)
		if ch == nil {
			free <- struct{}{}
			return false
		}
		next <- read{x: x, ch: ch, rest: !mine}
		return true
	})
	close(next)
	wg.Wait()
	return err
}

// withAnswer loads m with its recorded answer, one the agent parked on disk
// read back (models.Mock.HydrateResponse) into a copy, so m stays parked.
func withAnswer(m *models.Mock) func() (*models.Mock, error) {
	return func() (*models.Mock, error) {
		if !m.HasSpilledResponse() {
			return m, nil
		}
		c := m.ShallowCopy()
		if err := c.HydrateResponse(); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// SetUse is what the running set's learn step has read and kept of its
// payloads (setDerived), for the agent's log: how many its staging read
// (WarmSet) and its calls read for the set, of those its calls read how
// many the staging read the rest of (Finished: stageRest), the readers the
// staging read with (stageReaders; 0 where no staging read), the bytes each
// tier keeps (Bodies: the payloads, bodyStore), how many payloads were
// decoded for the set (Decoded: each first read, and each read again after
// it was let go), the set's budget (setBudget), and whether the tiers that
// let none go (held, forms) were full, past which the set's payloads are
// read once per test. Err is why the staging stopped short of the set where src failed
// (the on-disk store, mid-read): its calls then read the rest themselves.
type SetUse struct {
	Staged, Called, Finished, Readers, Decoded int
	Held, Words, Forms, Bodies, Budget         int64
	HeldFull, FormsFull                        bool
	Err                                        error
}

func (s *setDerived) use() SetUse {
	if s == nil {
		return SetUse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return SetUse{Staged: s.staged, Called: s.called, Finished: s.finished, Decoded: s.bodies.loads, Held: s.held.used, Words: s.words.used, Forms: s.forms.used,
		Bodies: s.bodies.kept.used, Budget: s.budget.limit, HeldFull: s.held.full, FormsFull: s.forms.full}
}
