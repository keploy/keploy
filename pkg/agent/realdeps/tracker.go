package realdeps

import (
	"slices"
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
// test that uses it. capture_incomplete evidence of class e (design v14
// §3.2, as amended for class e) is charged to every test during which its
// connection was undecided or used, never as a taint of every later test: a
// connection still waiting for its client to name it is charged to each test
// that ends while it waits, and to the open test when keploy closes it
// unnamed or ends it; evidence that touched no test (made while none was
// open: before the first test, between two, after the last), and what a
// test that never ended was charged, is the run's. A test that never touched a connection is never charged for it, and
// nothing is a false green: keploy relays nothing of such a connection.
type Tracker struct {
	mu sync.Mutex
	// open: a test is open.
	open bool
	// uses are the open test's REAL uses, by destination.
	uses map[string]*use
	// reached are the destinations the run reached since the last read (Run);
	// outside, those it first reached in that time while no test was open.
	reached, outside map[string]bool
	// events are the open test's capture_incomplete evidence, for its end;
	// run is the run's own: what touched no test.
	events, run []models.CaptureIncomplete
	// pending are the connections whose name is still being read.
	pending map[int64]*pendingConn
	// used is, per REAL destination, when the run's connections used it (a
	// send, or a dial that failed), in the order they were told: keploy
	// record names a test case's REAL destinations by them (UsedDuring), as
	// it names its mocks, by time. Unix nanoseconds, the run's whole length.
	used map[string][]int64
	// keepUses counts who keeps used (keploy record's incoming streams: a
	// reconnect's stream may open before the last one closes); with none,
	// none is kept: a replay names no test case by time.
	keepUses int
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
	// reported: a test's end, or a read, charged it as undecided.
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
	t.used = nil
	for _, p := range t.pending {
		p.reported = false
	}
}

// Interrupt drops a test left open by a session that ended without its end:
// its uses go with it, and the evidence it would have been charged is the
// run's.
func (t *Tracker) Interrupt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropOpenLocked()
}

// dropOpenLocked gives the run the evidence of a test that never ended, and
// drops its uses.
func (t *Tracker) dropOpenLocked() {
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
	// A test never ended (its window left open: its read never reached the
	// agent) gives the run its evidence, never the test that begins.
	t.dropOpenLocked()
	t.open, t.uses = true, map[string]*use{}
}

// EndTest closes the open test, and returns its REAL uses, by destination,
// and the capture_incomplete evidence charged to it: what was made while it
// was open, and every connection whose name is still being read, which was
// undecided during it, whatever earlier tests it was charged to.
func (t *Tracker) EndTest() ([]models.RealUse, []models.CaptureIncomplete) {
	t.mu.Lock()
	defer t.mu.Unlock()
	uses := make([]models.RealUse, 0, len(t.uses))
	for dest, u := range t.uses {
		uses = append(uses, models.RealUse{Dest: dest, Conns: len(u.conns), Failed: u.failed, H2: u.h2})
	}
	sort.Slice(uses, func(i, j int) bool { return uses[i].Dest < uses[j].Dest })
	ev := append(t.events, t.undecidedLocked("the test ended", true)...)
	t.events = nil
	t.open, t.uses = false, nil
	if len(uses) == 0 {
		uses = nil
	}
	return uses, ev
}

// undecidedLocked charges connections still being read as capture_incomplete:
// every one when all (a test's end: each was undecided during it), else only
// those no test's end or read charged yet (the run's read). when says what it
// was undecided at.
func (t *Tracker) undecidedLocked(when string, all bool) []models.CaptureIncomplete {
	var out []models.CaptureIncomplete
	ids := make([]int64, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := t.pending[id]
		if p.reported && !all {
			continue
		}
		p.reported = true
		out = append(out, models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: p.dst, At: t.stamp(),
			Why: UnreadableWhy(p.dst, p.candidates, "its client had sent no name when "+when)})
	}
	return out
}

// Run is what the run saw beyond its tests: the destinations it reached, those
// it first reached outside a test, and the evidence charged to no test: what
// touched none (before the first, between two, after the last), what a test
// that never ended was charged (left open, or interrupted), and every
// connection still being read that no test's end or read charged yet. It
// takes that evidence: a second Run does not repeat it.
func (t *Tracker) Run() models.RealRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := models.RealRun{Reached: sortedKeys(t.reached), Outside: sortedKeys(t.outside)}
	out.Incomplete = append(t.run, t.undecidedLocked("the run's results were read", false)...)
	t.run = nil
	// What was reached is since the last read: keploy test reads each test
	// set's, keploy mock and keploy record their run's once.
	t.reached, t.outside = nil, nil
	return out
}

// KeepUses starts (keep) or ends (!keep) one keeper of the run's uses for
// UsedDuring: keploy record's incoming stream, whose test cases they name.
// They are kept while any keeper is; when the last ends, nothing reads them,
// and those kept go.
func (t *Tracker) KeepUses(keep bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case keep:
		t.keepUses++
	case t.keepUses > 0:
		t.keepUses--
	}
	if t.keepUses == 0 {
		t.used = nil
	}
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
// ends the wait, whatever it decided: unnamed is why keploy closed the
// connection without a name ("" when it was named, or the run ended). A
// connection closed unnamed is charged to the open test, during which it was
// undecided, whatever earlier tests' ends charged it; with no test open, it
// is the run's, unless a test's end or a read charged it already.
func (t *Tracker) Pending(id int64, dst string, candidates []string) (done func(unnamed string)) {
	t.mu.Lock()
	if t.pending == nil {
		t.pending = map[int64]*pendingConn{}
	}
	t.pending[id] = &pendingConn{dst: dst, candidates: candidates}
	t.mu.Unlock()
	return func(unnamed string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		p := t.pending[id]
		delete(t.pending, id)
		if unnamed == "" || (!t.open && p != nil && p.reported) {
			return
		}
		t.incompleteLocked(models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: dst, Why: unnamed, At: t.stamp()})
	}
}

// Incomplete adds capture_incomplete evidence (class e) for a connection to
// dst, with why: the open test's, during which keploy ended the connection,
// or, with no test open, the run's.
func (t *Tracker) Incomplete(dst, why string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.incompleteLocked(models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: dst, Why: why, At: t.stamp()})
}

func (t *Tracker) incompleteLocked(c models.CaptureIncomplete) {
	if t.open {
		t.events = append(t.events, c)
		return
	}
	t.run = append(t.run, c)
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
		t.usedLocked(dest)
		return
	}
	t.reachedLocked(dest)
}

// reachedLocked notes that the run reached dest: by a dial that connected,
// or by an answer on a connection to it.
func (t *Tracker) reachedLocked(dest string) {
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
	t.usedLocked(dest)
}

// Answered notes that dest answered a connection: it is reached, as by a dial
// that connected (a pooled connection dialled before the last read reaches
// it again only when it answers, not when bytes are written to it).
func (t *Tracker) Answered(dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reachedLocked(dest)
}

// usedLocked notes that dest was used now, keeping its uses in time order:
// told under t.mu they come in order, except when the wall clock steps back.
func (t *Tracker) usedLocked(dest string) {
	if t.keepUses == 0 {
		return
	}
	if t.used == nil {
		t.used = map[string][]int64{}
	}
	at, now := t.used[dest], t.stamp().UnixNano()
	if n := len(at); n == 0 || at[n-1] <= now {
		t.used[dest] = append(at, now)
		return
	}
	t.used[dest] = slices.Insert(at, sort.Search(len(at), func(i int) bool { return at[i] > now }), now)
}

// UsedDuring is the REAL destinations the run used from from to to, both
// included, sorted: those a keploy record test case whose request came at
// from and whose response went at to used (TestCase.Real). A use is told
// before its bytes reach the destination, so every use the request caused
// is told by the time its response is.
func (t *Tracker) UsedDuring(from, to time.Time) []string {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	lo, hi := from.UnixNano(), to.UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for dest, at := range t.used {
		if i := sort.Search(len(at), func(i int) bool { return at[i] >= lo }); i < len(at) && at[i] <= hi {
			out = append(out, dest)
		}
	}
	sort.Strings(out)
	return out
}

// H2 notes that the open test sent dest bytes over a connection that offered
// h2 (Relay.H2, told after Sent): other names may share it. A test that has
// not sent dest bytes (one that began between the send and this) gets no
// row from it: its own sends say h2.
func (t *Tracker) H2(dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		return
	}
	if u := t.uses[dest]; u != nil && len(u.conns) > 0 {
		u.h2 = true
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
