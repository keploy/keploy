package realdeps

import (
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Tracker keeps what a run's REAL connections did, for its tests' REAL rows
// and evidence and for the run's verdict (design v14 §4, §8.2 rule 4): which
// REAL destinations each test used while it was open, which destinations the
// run reached, and the capture_incomplete evidence of connections keploy
// could not decide or had to end. The zero Tracker is ready to use.
//
// A test's REAL use is counted from the bytes its connections send while it
// is open, so a pooled connection opened before the test counts for every
// test that uses it. capture_incomplete evidence is charged as a miss is
// (design v14 §5.1): to the open test, or, made while no test is open, to the
// next test that ends; what the run's processes did before a test process
// claimed the run, and after its last test, is the run's.
type Tracker struct {
	mu sync.Mutex
	// open: a test is open.
	open bool
	// uses are the open test's REAL uses, by destination.
	uses map[string]*use
	// reached are the destinations the run connected to; outside, those it
	// first reached while no test was open.
	reached, outside map[string]bool
	// events are capture_incomplete evidence since the last test ended, for
	// the next test's end; run is the run's own.
	events, run []models.CaptureIncomplete
	// pending are the connections whose name is still being read.
	pending map[int64]*pendingConn
	// now is the clock evidence is stamped with; time.Now when nil.
	now func() time.Time
}

type use struct {
	conns  map[int64]struct{}
	failed int
	h2     bool
}

type pendingConn struct {
	dst        string
	candidates []string
	// reported: a test's end already charged it.
	reported bool
}

func (t *Tracker) stamp() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Reset starts a new run: everything kept goes. Connections still being read
// stay pending: they belong to the run that is starting.
func (t *Tracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open, t.uses = false, nil
	t.reached, t.outside = nil, nil
	t.events, t.run = nil, nil
	for _, p := range t.pending {
		p.reported = false
	}
}

// Claimed is a test process claiming the run: the evidence made before it is
// the run's, not its first test's (design v14 §5.1).
func (t *Tracker) Claimed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		t.run = append(t.run, t.events...)
		t.events = nil
	}
}

// Interrupt drops a test left open by a session that ended without its end:
// its uses go with it, and the evidence it would have been charged is the
// run's.
func (t *Tracker) Interrupt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		return
	}
	t.run = append(t.run, t.events...)
	t.events = nil
	t.open, t.uses = false, nil
}

// BeginTest opens a test.
func (t *Tracker) BeginTest() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open, t.uses = true, map[string]*use{}
}

// EndTest closes the open test, and returns its REAL uses, by destination,
// and the capture_incomplete evidence charged to it: what was made since the
// last test ended, and every connection whose name is still being read.
func (t *Tracker) EndTest() ([]models.RealUse, []models.CaptureIncomplete) {
	t.mu.Lock()
	defer t.mu.Unlock()
	uses := make([]models.RealUse, 0, len(t.uses))
	for dest, u := range t.uses {
		uses = append(uses, models.RealUse{Dest: dest, Conns: len(u.conns), Failed: u.failed, H2: u.h2})
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].Dest < uses[j].Dest })
	ev := append(t.events, t.undecidedLocked("the test ended")...)
	t.events = nil
	t.open, t.uses = false, nil
	if len(uses) == 0 {
		uses = nil
	}
	return uses, ev
}

// undecidedLocked charges every connection still being read that no end
// charged yet, as capture_incomplete: when says what it was undecided at.
func (t *Tracker) undecidedLocked(when string) []models.CaptureIncomplete {
	var out []models.CaptureIncomplete
	ids := make([]int64, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := t.pending[id]
		if p.reported {
			continue
		}
		p.reported = true
		out = append(out, models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: p.dst, At: t.stamp(),
			Why: UnreadableWhy(p.dst, p.candidates, "its client had sent no name when "+when)})
	}
	return out
}

// Run is what the run saw beyond its tests: the destinations it reached, those
// it first reached outside a test, and the evidence charged to no test (what
// came after the last test ended, and every connection still being read).
// It takes that evidence: a second Run does not repeat it.
func (t *Tracker) Run() models.RealRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := models.RealRun{Reached: sortedKeys(t.reached), Outside: sortedKeys(t.outside)}
	if !t.open {
		t.run = append(t.run, t.events...)
		t.events = nil
	}
	out.Incomplete = append(t.run, t.undecidedLocked("the run's results were read")...)
	t.run = nil
	return out
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Pending notes that connection id, to dst, waits for its client's first
// bytes to name it; candidates are the name entries resolving to dst. done
// ends the wait, whatever it decided, and reports whether a test's end (or
// the run's) already charged the connection as undecided: the caller then
// charges it nothing more.
func (t *Tracker) Pending(id int64, dst string, candidates []string) (done func() (charged bool)) {
	t.mu.Lock()
	if t.pending == nil {
		t.pending = map[int64]*pendingConn{}
	}
	t.pending[id] = &pendingConn{dst: dst, candidates: candidates}
	t.mu.Unlock()
	return func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		p := t.pending[id]
		delete(t.pending, id)
		return p != nil && p.reported
	}
}

// Incomplete adds capture_incomplete evidence (class e) for a connection to
// dst, with why.
func (t *Tracker) Incomplete(dst, why string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: dst, Why: why, At: t.stamp()})
}

// Dialed notes a dial of REAL destination dest for connection id: one that
// failed counts in the open test's row; one that connected reaches dest.
func (t *Tracker) Dialed(id int64, dest string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !ok {
		if t.open {
			t.useLocked(dest).failed++
		}
		return
	}
	if t.reached == nil {
		t.reached = map[string]bool{}
	}
	if !t.reached[dest] {
		t.reached[dest] = true
		if !t.open {
			if t.outside == nil {
				t.outside = map[string]bool{}
			}
			t.outside[dest] = true
		}
	}
}

// Sent notes that connection id sent dest bytes: the open test uses dest.
func (t *Tracker) Sent(id int64, dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		t.useLocked(dest).conns[id] = struct{}{}
	}
}

// H2 notes that a connection to dest offered h2 while the open test used it.
func (t *Tracker) H2(dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open {
		t.useLocked(dest).h2 = true
	}
}

func (t *Tracker) useLocked(dest string) *use {
	u := t.uses[dest]
	if u == nil {
		u = &use{conns: map[int64]struct{}{}}
		t.uses[dest] = u
	}
	return u
}
