package realdeps

import (
	"fmt"
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
//
// In a run of more than one test worker each worker's test is open on its
// own, keyed by the keploy instance the kernel stamped its connections with
// (Origin.Instance); a run of one test process has one, instance 0. A
// connection is its own worker's: what it does is charged to that worker's
// open test, and, with none open, is the run's. A connection from a process
// in no worker's instance (instance 0 while only workers' tests are open)
// could be any open test's, so it is charged to each: its evidence as
// capture_incomplete, and its REAL use as a call keploy cannot attribute
// (models.CaptureLossUnattributed), never as a REAL row of a test it guessed.
type Tracker struct {
	mu sync.Mutex
	// open are the open tests, by their worker's instance.
	open map[uint32]*openTest
	// reached are the destinations the run reached since the last read (Run);
	// outside, those it first reached in that time while no test was open.
	reached, outside map[string]bool
	// run is the run's own capture_incomplete evidence: what touched no test.
	run []models.CaptureIncomplete
	// pending are the connections whose name is still being read.
	pending map[int64]*pendingConn
	// failed are connects that failed while keploy could not yet tell
	// whether their destination is a mock.real one (FailedUndecided).
	failed map[int64]*failedConn
	// failedIDs numbers failed's entries.
	failedIDs int64
	// seq numbers the tests begun: what a failed connect's tests were.
	seq uint64
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

// Origin is who made a connection: the keploy instance (a test worker's) the
// kernel stamped it with at its connect, 0 for none, and its process's
// kernel PID, 0 when unknown.
type Origin struct {
	Instance uint32
	Pid      uint32
}

// openTest is a test open on its worker's instance.
type openTest struct {
	// seq numbers it among the run's tests (Tracker.seq).
	seq uint64
	// uses are its REAL uses, by destination.
	uses map[string]*use
	// events are its capture_incomplete evidence, for its end.
	events []models.CaptureIncomplete
	// unattributed are the connections from a process in no worker's
	// instance that sent a REAL destination bytes while it was open, and
	// unattributedFailed their dials to one that failed then; latest, the
	// last such use.
	unattributed       map[int64]struct{}
	unattributedFailed int
	latest             unattributedUse
}

// unattributedUse names a REAL use keploy cannot attribute to one test.
type unattributedUse struct {
	dest string
	pid  uint32
}

type use struct {
	conns  map[int64]struct{}
	failed int
	h2     bool
}

type failedConn struct {
	dst  string
	at   time.Time
	from Origin
	// seqs are the tests the connect failed in, and still uncharged with it:
	// its worker's open test, or, from a process in no worker, every test
	// open then.
	seqs []uint64
	// reported: a test's end, or a read, charged it as undecided.
	reported bool
}

type pendingConn struct {
	dst        string
	candidates []string
	instance   uint32
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
	t.open = nil
	t.reached, t.outside = nil, nil
	t.run = nil
	t.used = nil
	for _, p := range t.pending {
		p.reported = false
	}
	// The last run's failed connects are its own: told later, they find
	// nothing.
	t.failed = nil
}

// Interrupt drops the tests left open by a session that ended without their
// ends: their uses go with them, and the evidence they would have been
// charged is the run's.
func (t *Tracker) Interrupt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, inst := range t.openInstancesLocked() {
		t.dropOpenLocked(inst)
	}
}

// dropOpenLocked gives the run the evidence of instance's test, which never
// ended, and drops its uses.
func (t *Tracker) dropOpenLocked(instance uint32) {
	o := t.open[instance]
	if o == nil {
		return
	}
	t.run = append(t.run, o.events...)
	delete(t.open, instance)
}

// openInstancesLocked are the instances with a test open, in the order their
// tests began.
func (t *Tracker) openInstancesLocked() []uint32 {
	out := make([]uint32, 0, len(t.open))
	for inst := range t.open {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return t.open[out[i]].seq < t.open[out[j]].seq })
	return out
}

// BeginTest opens a test of a run of one test process: BeginTestOf(0).
func (t *Tracker) BeginTest() { t.BeginTestOf(0) }

// BeginTestOf opens a test of instance's worker (0: of a run of one test
// process).
func (t *Tracker) BeginTestOf(instance uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The worker's test never ended (its window left open: its read never
	// reached the agent) gives the run its evidence, never the test that
	// begins.
	t.dropOpenLocked(instance)
	t.seq++
	if t.open == nil {
		t.open = map[uint32]*openTest{}
	}
	t.open[instance] = &openTest{seq: t.seq, uses: map[string]*use{}}
}

// EndTest closes the open test of a run of one test process, and returns its
// REAL uses and capture_incomplete evidence (EndTestOf(0)).
func (t *Tracker) EndTest() ([]models.RealUse, []models.CaptureIncomplete) {
	end := t.EndTestOf(0)
	return end.Uses, end.Incomplete
}

// TestEnd is what a test's end reads of its REAL connections.
type TestEnd struct {
	// Uses are its REAL uses, by destination.
	Uses []models.RealUse
	// Incomplete is the capture_incomplete evidence charged to it: what was
	// made on its worker's connections, or on a connection from a process
	// in no worker, while it was open, and every such connection whose name
	// is still being read, which was undecided during it, whatever earlier
	// tests it was charged to.
	Incomplete []models.CaptureIncomplete
	// Unattributed are the REAL uses made while it was open from a process
	// in no worker's instance, which keploy cannot tell were its
	// (models.CaptureLossUnattributed); empty for none.
	Unattributed models.CaptureLosses
}

// EndTestOf closes instance's open test, and returns what it did.
func (t *Tracker) EndTestOf(instance uint32) TestEnd {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := t.open[instance]
	if o == nil {
		o = &openTest{}
	}
	end := TestEnd{Uses: make([]models.RealUse, 0, len(o.uses))}
	for dest, u := range o.uses {
		end.Uses = append(end.Uses, models.RealUse{Dest: dest, Conns: len(u.conns), Failed: u.failed, H2: u.h2})
	}
	sort.Slice(end.Uses, func(i, j int) bool { return end.Uses[i].Dest < end.Uses[j].Dest })
	end.Incomplete = append(o.events, t.undecidedLocked("the test ended", &instance)...)
	end.Incomplete = append(end.Incomplete, t.failedUndecidedLocked("the test ended", true, o.seq)...)
	if n := len(o.unattributed) + o.unattributedFailed; n > 0 {
		what := "the latest: a connection to mock.real destination " + o.latest.dest
		if o.latest.pid != 0 {
			what += fmt.Sprintf(", from process %d", o.latest.pid)
		}
		end.Unattributed = models.CaptureLosses{{Kind: models.CaptureLossUnattributed, Count: uint64(n), What: what}}
	}
	delete(t.open, instance)
	if len(end.Uses) == 0 {
		end.Uses = nil
	}
	return end
}

// undecidedLocked charges connections still being read as capture_incomplete:
// at a test's end (ending, its worker's instance), every one of its worker's
// or of a process in no worker (each was undecided during it), else (the
// run's read) only those no test's end or read charged yet. when says what
// it was undecided at.
func (t *Tracker) undecidedLocked(when string, ending *uint32) []models.CaptureIncomplete {
	var out []models.CaptureIncomplete
	ids := make([]int64, 0, len(t.pending))
	for id := range t.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		p := t.pending[id]
		if ending == nil && p.reported {
			continue
		}
		if ending != nil && p.instance != *ending && p.instance != 0 {
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
	out.Incomplete = append(t.run, t.undecidedLocked("the run's results were read", nil)...)
	out.Incomplete = append(out.Incomplete, t.failedUndecidedLocked("the run's results were read", false, 0)...)
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
// connection closed unnamed is charged to the open test its origin's
// (testsOfLocked), during which it was undecided, whatever earlier tests'
// ends charged it; with none open, it is the run's, unless a test's end or a
// read charged it already.
func (t *Tracker) Pending(from Origin, id int64, dst string, candidates []string) (done func(unnamed string)) {
	t.mu.Lock()
	if t.pending == nil {
		t.pending = map[int64]*pendingConn{}
	}
	t.pending[id] = &pendingConn{dst: dst, candidates: candidates, instance: from.Instance}
	t.mu.Unlock()
	return func(unnamed string) {
		t.mu.Lock()
		defer t.mu.Unlock()
		p := t.pending[id]
		delete(t.pending, id)
		if unnamed == "" || (len(t.testsOfLocked(from)) == 0 && p != nil && p.reported) {
			return
		}
		t.incompleteLocked(from, models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: dst, Why: unnamed, At: t.stamp()})
	}
}

// Incomplete adds capture_incomplete evidence (class e) for a connection
// from from to dst, with why: the open test's its origin's (testsOfLocked),
// during which keploy ended the connection, or, with none open, the run's.
func (t *Tracker) Incomplete(from Origin, dst, why string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.incompleteLocked(from, models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: dst, Why: why, At: t.stamp()})
}

func (t *Tracker) incompleteLocked(from Origin, c models.CaptureIncomplete) {
	tests := t.testsOfLocked(from)
	if len(tests) == 0 {
		t.run = append(t.run, c)
		return
	}
	for _, o := range tests {
		o.events = append(o.events, c)
	}
}

// testsOfLocked are the open tests what a connection from from does is
// charged to: its worker's open test; from a process in no worker's
// instance (Instance 0) while no test of a run of one test process is open,
// every test open (in the order they began), which it could be any of
// (unattributed); none when its worker has no test open, or no test is.
func (t *Tracker) testsOfLocked(from Origin) []*openTest {
	if o := t.open[from.Instance]; o != nil {
		return []*openTest{o}
	}
	if from.Instance != 0 {
		return nil
	}
	insts := t.openInstancesLocked()
	out := make([]*openTest, 0, len(insts))
	for _, inst := range insts {
		out = append(out, t.open[inst])
	}
	return out
}

// usesLocked notes that connection id from from used dest: a REAL row's use
// of its worker's open test; from a process in no worker while workers'
// tests are open, a use none of them can be told to have made, charged to
// each (TestEnd.Unattributed). failed: a dial that failed, else bytes sent.
func (t *Tracker) usesLocked(from Origin, id int64, dest string, failed bool) {
	if o := t.open[from.Instance]; o != nil {
		u := o.useLocked(dest)
		if failed {
			u.failed++
		} else {
			u.conns[id] = struct{}{}
		}
		return
	}
	for _, o := range t.testsOfLocked(from) {
		if failed {
			o.unattributedFailed++
		} else {
			if o.unattributed == nil {
				o.unattributed = map[int64]struct{}{}
			}
			o.unattributed[id] = struct{}{}
		}
		o.latest = unattributedUse{dest: dest, pid: from.Pid}
	}
}

// Dialed notes a dial of REAL destination dest for connection id from from:
// one that failed counts in the row of its origin's open test (usesLocked);
// one that connected reaches dest.
func (t *Tracker) Dialed(from Origin, id int64, dest string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !ok {
		t.usesLocked(from, id, dest, true)
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
		if len(t.open) == 0 {
			if t.outside == nil {
				t.outside = map[string]bool{}
			}
			t.outside[dest] = true
		}
	}
}

// Sent notes that connection id from from sent dest bytes: its origin's open
// test uses dest (usesLocked).
func (t *Tracker) Sent(from Origin, id int64, dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usesLocked(from, id, dest, false)
	t.usedLocked(dest)
}

// SentLive is Sent for a call the live handler serves: a caller's call to a
// listener its own process started, which keploy mock's request capture
// records as a test case itself. Its origin's open test uses dest, and no use
// point is kept: that test case is the call, not a use its handler made, and
// UsedDuring would name dest in the test case's own REAL list (its body's
// reads fall inside its window).
func (t *Tracker) SentLive(from Origin, id int64, dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usesLocked(from, id, dest, false)
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
	t.usedAtLocked(dest, t.stamp())
}

// usedAtLocked notes that dest was used at when.
func (t *Tracker) usedAtLocked(dest string, when time.Time) {
	if t.keepUses == 0 {
		return
	}
	if t.used == nil {
		t.used = map[string][]int64{}
	}
	at, now := t.used[dest], when.UnixNano()
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

// H2 notes that a connection from from sent dest bytes, over a connection
// that offered h2 (Relay.H2, told after Sent): other names may share it. Only
// its worker's open test has a row to note it in; a test that has not sent
// dest bytes (one that began between the send and this) gets no row from it:
// its own sends say h2.
func (t *Tracker) H2(from Origin, dest string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	o := t.open[from.Instance]
	if o == nil {
		return
	}
	if u := o.uses[dest]; u != nil && len(u.conns) > 0 {
		u.h2 = true
	}
}

func (o *openTest) useLocked(dest string) *use {
	u := o.uses[dest]
	if u == nil {
		u = &use{conns: map[int64]struct{}{}}
		o.uses[dest] = u
	}
	return u
}

// FailedUndecided notes, as it happens, that a connect to dst failed while
// keploy could not yet tell whether dst is a mock.real destination (a
// container the index is still placing). resolve says what it was: label,
// "" for a dependency; unresolved when that cannot be told. A failed connect
// is a finished event of the tests it failed in (testsOfLocked, from its
// origin), and only of them: told REAL while such a test is open, it counts
// as a failed dial does (usesLocked); such a test ending first is charged
// with it as capture_incomplete, as is one still open when it is told
// unresolved. One that failed while no such test was open, or whose tests
// never ended, is the run's when a read finds it untold, or it is told
// unresolved. Told REAL, a keploy record use point is kept at when it failed.
func (t *Tracker) FailedUndecided(from Origin, dst string) (resolve func(label string, unresolved bool)) {
	t.mu.Lock()
	if t.failed == nil {
		t.failed = map[int64]*failedConn{}
	}
	t.failedIDs++
	id := t.failedIDs
	f := &failedConn{dst: dst, at: t.stamp(), from: from}
	for _, o := range t.testsOfLocked(from) {
		f.seqs = append(f.seqs, o.seq)
	}
	t.failed[id] = f
	t.mu.Unlock()
	return func(label string, unresolved bool) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.failed[id] != f {
			return // told already, or Reset began a new run
		}
		delete(t.failed, id)
		// Its tests still open and not yet charged with it: one that ended
		// first, or a read, holds it already.
		var its []*openTest
		for _, o := range t.open {
			if slices.Contains(f.seqs, o.seq) {
				its = append(its, o)
			}
		}
		sort.Slice(its, func(i, j int) bool { return its[i].seq < its[j].seq })
		switch {
		case unresolved && len(its) > 0:
			for _, o := range its {
				o.events = append(o.events, f.incomplete("its index could not place the containers there"))
			}
		case unresolved && !f.reported:
			t.run = append(t.run, f.incomplete("its index could not place the containers there"))
		case label != "" && len(its) > 0:
			if its[0] == t.open[f.from.Instance] {
				its[0].useLocked(label).failed++
				break
			}
			for _, o := range its {
				o.unattributedFailed++
				o.latest = unattributedUse{dest: label, pid: f.from.Pid}
			}
		}
		if !unresolved && label != "" {
			t.usedAtLocked(label, f.at)
		}
	}
}

func (f *failedConn) incomplete(why string) models.CaptureIncomplete {
	return models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: f.dst, At: f.at,
		Why: "keploy could not tell whether a connection to " + f.dst + " that failed to connect was to a container mock.real lists: " +
			why + "; list the dependency by address"}
}

// failedUndecidedLocked charges failed connects still untold: at a test's end
// (ending, seq the test's), those that failed in it, each once; at a read
// (ending false), every one no end or read charged yet: the run's, which
// then holds it for every test.
func (t *Tracker) failedUndecidedLocked(when string, ending bool, seq uint64) []models.CaptureIncomplete {
	ids := make([]int64, 0, len(t.failed))
	for id := range t.failed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out []models.CaptureIncomplete
	for _, id := range ids {
		f := t.failed[id]
		if ending {
			i := slices.Index(f.seqs, seq)
			if i < 0 {
				continue
			}
			f.seqs = slices.Delete(f.seqs, i, i+1)
		} else {
			if f.reported {
				continue
			}
			f.seqs = nil
		}
		f.reported = true
		out = append(out, f.incomplete("Docker was starting containers there, not yet placed when "+when))
	}
	return out
}
