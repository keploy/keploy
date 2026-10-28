package variables

import (
	"sort"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
)

// Binding is the value a variable has in a scope, and where it was taken:
// Name, the live Value, the Recorded value at the binding place, and the Mock
// and Place where it was taken.
type Binding = models.VariableBinding

// Table holds the variables one scope has bound: one test case in keploy
// test, one test in keploy mock replay. A variable is bound once; a later binding of
// the same name does not take (Bind). Safe for concurrent use.
//
// A scope ends only when the next begins (Start, Reset): between two tests
// the ended test's bindings stay in force, and a call of the gap reads and
// binds in them (the proxy's gap rule).
//
// With KeepRun, a value bound before the set's first scope began (the run's
// start-up, before its first test) is the run's: every scope of the set sees
// it, until NewSet. So is a run's variable (models.RunVariable) bound in a
// scope: an earlier test's call binds it for the tests after it. Every other
// value bound once a scope began, in it or in the gap after it, is that
// scope's, forgotten when the next begins. A scope begins in the step that
// makes its mocks live (StartOpening), so no call its mocks answer binds in
// the start-up's. A table whose
// scope is the next test's, begun early, reads the run's (ReadRunOf); the
// run's are bound beside its scope's (Beside), as its are beside the run's,
// so the first test, which carries both in, holds no two variables recorded
// with two values at one live value, whichever bound first.
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	// run are the run's bindings (KeepRun).
	run map[string]Binding
	// runOf is the table whose run's bindings t reads as its run's too
	// (ReadRunOf), and readers, under t.mu, the tables that read t's. A
	// table is locked before every table that reads its run's, never after
	// (lock, readersScopes): no two tables wait on each other.
	runOf   atomic.Pointer[Table]
	readers []*Table
	// began is whether a scope has begun since the table was made or NewSet.
	began bool
	// keepRun: what is bound before the first scope began is the run's
	// (KeepRun).
	keepRun bool
	// noRun: KeepRun keeps none until AllowRun (KeepNoRun).
	noRun bool
	// gen counts the scopes the table has begun (Reset): a call checked in
	// one scope never binds in the next (BindAll).
	gen uint64
}

// lock takes t.mu, and before it the lock of the table whose run's t reads
// (ReadRunOf), which it returns (nil for none): t's state and that table's
// run's are then read directly, until unlock.
func (t *Table) lock() *Table {
	for {
		from := t.runOf.Load()
		if from != nil {
			from.mu.Lock()
		}
		t.mu.Lock()
		// ReadRunOf changes runOf under t.mu: once t.mu is held it holds.
		if t.runOf.Load() == from {
			return from
		}
		t.mu.Unlock()
		if from != nil {
			from.mu.Unlock()
		}
	}
}

// unlock releases what lock took.
func (t *Table) unlock(from *Table) {
	t.mu.Unlock()
	if from != nil {
		from.mu.Unlock()
	}
}

// lookup is the binding of name in force: the scope's, else the run's. t is
// locked (lock), with from, the table whose run's it reads.
func (t *Table) lookup(from *Table, name string) (Binding, bool) {
	if b, ok := t.vals[name]; ok {
		return b, true
	}
	if b, ok := t.run[name]; ok {
		return b, true
	}
	if from != nil {
		b, ok := from.run[name]
		return b, ok
	}
	return Binding{}, false
}

// runInForce is a copy of every run's binding t reads, its own and those of
// from, the table it reads the run's of (ReadRunOf); nil for none. t is
// locked (lock).
func (t *Table) runInForce(from *Table) map[string]Binding {
	var theirs map[string]Binding
	if from != nil {
		theirs = from.run
	}
	if len(t.run)+len(theirs) == 0 {
		return nil
	}
	out := make(map[string]Binding, len(t.run)+len(theirs))
	for n, b := range theirs {
		out[n] = b
	}
	for n, b := range t.run {
		out[n] = b
	}
	return out
}

// all is every binding in force, the scope's over the run's. t is locked
// (lock).
func (t *Table) all(from *Table) map[string]Binding {
	run := t.runInForce(from)
	if len(run) == 0 {
		return t.vals
	}
	out := make(map[string]Binding, len(t.vals)+len(run))
	for n, b := range run {
		out[n] = b
	}
	for n, b := range t.vals {
		out[n] = b
	}
	return out
}

// readersScopes is, while t binds the run's (put), every binding of the
// scopes of the tables that read t's run's (ReadRunOf): the next test's calls
// in flight bound them before the set's first test began, and that test
// carries them in beside the run's (StartCarrying). nil once a scope began:
// what t binds then is its scope's, and two tests' variables may share a
// value. t is locked (lock); each reader is locked after it.
func (t *Table) readersScopes() []Binding {
	if t.began || !t.keepRun {
		return nil
	}
	var out []Binding
	for _, r := range t.readers {
		r.mu.Lock()
		for _, b := range r.vals {
			out = append(out, b)
		}
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Gen is the scope the table is in now: what a call that is about to bind
// was checked against.
func (t *Table) Gen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// Lookup returns the binding of name in force, if any.
func (t *Table) Lookup(name string) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	return t.lookup(from, name)
}

// Beside returns every binding a bind of t is checked beside now (BindAll's
// TwoValues rule), by name: those in force (Snapshot), then, while t binds
// the run's (KeepRun, before the first scope began), those of the scopes
// that read the run's (ReadRunOf), which the first test carries in beside
// them. An adapter that checks a call before it binds reads these (http's
// holdVariables), so the check and the bind refuse alike.
func (t *Table) Beside() []Binding {
	from := t.lock()
	defer t.unlock(from)
	return append(sorted(t.all(from)), t.readersScopes()...)
}

// Bind records b unless its name is already bound, or b's value is another
// variable's that was recorded with another value (TwoValues, BindAll's rule,
// the run's and Beside's included). It returns the binding in force afterwards
// of b's name, or the one b's value is, and whether it is b. A call binds with
// BindAll, a start-up's call too (its Gen is the table's before any scope
// begins), which also refuses a call of a scope that ended since: Bind checks
// no scope.
func (t *Table) Bind(b Binding) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	if have, ok := t.lookup(from, b.Name); ok {
		return have, false
	}
	if with, ok := collides(t.all(from), t.readersScopes(), b, nil); ok {
		return with, false
	}
	t.put(b)
	return b, true
}

// KeepRun makes what is bound before the first scope begins the run's, which
// every scope of the set sees, instead of the first scope's to forget, unless
// KeepNoRun refused it since the last AllowRun. It is called before the
// start-up binds anything (MockManager.NewVariableSet, as a test set's mocks
// are set up), and holds until KeepNoRun.
func (t *Table) KeepRun() {
	t.mu.Lock()
	t.keepRun = !t.noRun
	t.mu.Unlock()
}

// KeepNoRun undoes KeepRun, and refuses it until AllowRun: what is bound
// before the first scope begins is the first scope's to forget, as without
// KeepRun, and what the run's held so far goes with it
// (MockManager.SetMocksWithWindow: a start-up that is served every test's
// recordings binds nothing the run's can be told from a test's).
func (t *Table) KeepNoRun() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.keepRun, t.noRun = false, true
	if len(t.run) == 0 {
		return
	}
	if t.vals == nil {
		t.vals = make(map[string]Binding, len(t.run))
	}
	for n, b := range t.run {
		if _, ok := t.vals[n]; !ok {
			t.vals[n] = b
		}
	}
	t.run = nil
}

// AllowRun lets a later KeepRun keep the run's again (a staging that serves
// the start-up exactly). It keeps none itself: a scope a call was checked in
// before it may have bound what is no run's.
func (t *Table) AllowRun() {
	t.mu.Lock()
	t.noRun = false
	t.mu.Unlock()
}

// ReadRunOf makes t read the run's bindings of from as its run's too, in
// every scope: a table whose scope is a test's begun early (the next test's
// calls in flight, answered in the gap before it) sees what the run's
// start-up bound, as the test does once it begins. t keeps no run's of its
// own from it: what t binds is its scope's. from never reads t's, but checks
// a bind of its run's beside t's scope's (Beside): whichever of the two binds
// a value first, the other is refused it for another variable.
func (t *Table) ReadRunOf(from *Table) {
	if from == t || from == nil {
		return
	}
	from.mu.Lock()
	t.mu.Lock()
	old := t.runOf.Load()
	if old == from {
		t.mu.Unlock()
		from.mu.Unlock()
		return
	}
	t.runOf.Store(from)
	t.mu.Unlock()
	from.readers = append(from.readers, t)
	from.mu.Unlock()
	if old != nil {
		old.mu.Lock()
		for i, r := range old.readers {
			if r == t {
				old.readers = append(old.readers[:i:i], old.readers[i+1:]...)
				break
			}
		}
		old.mu.Unlock()
	}
}

// put binds b in the scope, or the run's where the table keeps the run's
// (KeepRun) and b is one: bound before any scope began (the start-up's), or
// a run's variable (models.RunVariable) an earlier test's call binds for the
// tests after it. t is locked (lock).
func (t *Table) put(b Binding) {
	if t.keepRun && (!t.began || models.RunVariable(b.Name)) {
		if t.run == nil {
			t.run = make(map[string]Binding)
		}
		t.run[b.Name] = b
		return
	}
	if t.vals == nil {
		t.vals = make(map[string]Binding)
	}
	t.vals[b.Name] = b
}

// BindAll binds bs together, all or none, under one lock, for a call checked
// in scope gen (Gen): each name holds one value (a name already bound, the
// run's included, must be bound to the same value), and two variables
// recorded with two values do not share a live value, unless both are clock
// reads the learn step marked, made in one second (TwoValues). It returns the
// first binding that does not hold and the binding it collides with; stale is
// true when a new scope began since gen (nothing is bound); ok is true when
// every binding of bs is in force afterwards.
func (t *Table) BindAll(gen uint64, bs []Binding) (bad, with Binding, stale, ok bool) {
	from := t.lock()
	defer t.unlock(from)
	if gen != t.gen {
		return Binding{}, Binding{}, true, false
	}
	inForce, beside := t.all(from), t.readersScopes()
	for i, b := range bs {
		if have, bound := inForce[b.Name]; bound && have.Value != b.Value {
			return b, have, false, false
		}
		if with, bad := collides(inForce, beside, b, bs[:i]); bad {
			return b, with, false, false
		}
	}
	for _, b := range bs {
		if _, bound := t.lookup(from, b.Name); !bound {
			t.put(b)
		}
	}
	return Binding{}, Binding{}, false, true
}

// collides is the binding b cannot be bound beside, of those in force, beside
// (the scopes that read the run's while the run's binds, readersScopes) and
// earlier (bound with b, before it): one of b's name with another value
// (earlier: in force, a name already bound keeps its own), or one whose
// value b's is (TwoValues).
func collides(inForce map[string]Binding, beside []Binding, b Binding, earlier []Binding) (Binding, bool) {
	for _, other := range inForce {
		if TwoValues(other, b) {
			return other, true
		}
	}
	for _, other := range beside {
		if TwoValues(other, b) {
			return other, true
		}
	}
	for _, other := range earlier {
		if other.Name == b.Name && other.Value != b.Value || TwoValues(other, b) {
			return other, true
		}
	}
	return Binding{}, false
}

// TwoValues reports whether b may not be bound beside held, a binding in
// force or bound with it, because they are two variables that were two values
// when the test was recorded and now share one live value: held is not a
// given value, and is not b's name. The one pair of such variables that may
// share a live value is two clock reads the learn step marked, made in one
// second as their recordings say (models.ReadInOneSecond, FD-3): nothing
// about the live values decides it. BindAll and Bind refuse such a b, and an
// adapter that checks a call before it binds asks the same (http's
// holdVariables).
func TwoValues(held, b Binding) bool {
	return !held.Given && held.Name != b.Name && held.Value == b.Value && held.Recorded != b.Recorded && !models.ReadInOneSecond(held.Clock, b.Clock)
}

// Start begins a new scope (as Reset) with the values its test case gave
// bound, under one lock: no call of the scope binds before them. A name given
// twice keeps its first value.
func (t *Table) Start(given []Binding) {
	t.StartCarrying(given, nil)
}

// StartCarrying is Start, with the bindings carried also bound as the scope
// begins, as they were made (not given): what the answers of the test's calls
// in flight when it began bound before it, in a scope of their own. A given
// value wins over a carried one of its name, and the first of two carried
// ones wins.
//
// One lock, as Start: no call of the scope binds before them.
func (t *Table) StartCarrying(given, carried []Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.start(given, carried)
}

// StartOpening is StartCarrying in the step that opens the scope: open, what
// makes the scope's mocks live (a test's view, the pool of its window), runs
// under the table's lock, and the scope begins before the lock is let go. A
// call reads the scope it is checked in (Gen) before the mocks it is checked
// against: one that read the scope before, and the scope's mocks after, binds
// stale (BindAll), and is checked again in the scope. So no call answered by a
// mock of the scope binds before the scope began, in the scope before it or
// in the run's (KeepRun), which every later scope of the set would read.
// Opened in two steps, the mocks live and then the scope begun, a call of the
// first scope answered between the two bound in the run's.
//
// open must call none of t's methods: it holds t's lock.
func (t *Table) StartOpening(open func(), given, carried []Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	open()
	t.start(given, carried)
}

// start begins a scope (StartCarrying). t.mu is held.
func (t *Table) start(given, carried []Binding) {
	t.gen++
	t.vals = nil
	t.began = true
	if len(given)+len(carried) == 0 {
		return
	}
	t.vals = make(map[string]Binding, len(given)+len(carried))
	for _, b := range given {
		b.Given = true
		if _, dup := t.vals[b.Name]; !dup {
			t.vals[b.Name] = b
		}
	}
	for _, b := range carried {
		if _, dup := t.vals[b.Name]; !dup {
			t.put(b)
		}
	}
}

// Give binds b, a value the open scope's test gave (models.GivenReq), as a
// given value (Binding.Given) of the scope, under the table's lock, without
// beginning a new scope: what the scope bound before stays, and a call
// checked in it binds on (Gen does not move). It refuses, binding nothing,
// when no scope has begun (the value would be the run's, which every later
// scope reads) and when b's name is already bound, and returns the binding
// in force of b's name then. A given's name is the test's own
// (models.GivenPrefix): no learned variable takes it.
func (t *Table) Give(b Binding) (Binding, bool) {
	from := t.lock()
	defer t.unlock(from)
	if !t.began {
		return Binding{}, false
	}
	if have, ok := t.lookup(from, b.Name); ok {
		return have, false
	}
	b.Given = true
	if t.vals == nil {
		t.vals = make(map[string]Binding)
	}
	t.vals[b.Name] = b
	return b, true
}

// Reset forgets every binding of the scope: a new scope begins. The run's
// stay.
func (t *Table) Reset() {
	t.mu.Lock()
	t.vals = nil
	t.began = true
	t.gen++
	t.mu.Unlock()
}

// NewSet forgets every binding, the run's too, and no scope has begun: the
// next test set's replay starts (as learn.Tracker.NewSet).
func (t *Table) NewSet() {
	t.mu.Lock()
	t.vals, t.run = nil, nil
	t.began = false
	t.gen++
	t.mu.Unlock()
}

// Clone is a table holding t's bindings in t's scope, the run's it reads
// included, sharing nothing with it: a match checked against the clone binds
// nothing in t.
func (t *Table) Clone() *Table {
	from := t.lock()
	defer t.unlock(from)
	c := &Table{gen: t.gen, began: t.began, keepRun: t.keepRun}
	if t.vals != nil {
		c.vals = make(map[string]Binding, len(t.vals))
		for k, v := range t.vals {
			c.vals[k] = v
		}
	}
	c.run = t.runInForce(from)
	return c
}

// Snapshot returns the bindings in force, the run's included, by name.
func (t *Table) Snapshot() []Binding {
	from := t.lock()
	defer t.unlock(from)
	return sorted(t.all(from))
}

// sorted is the bindings of in, by name.
func sorted(in map[string]Binding) []Binding {
	out := make([]Binding, 0, len(in))
	for _, b := range in {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
