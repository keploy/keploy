package variables

import (
	"sort"
	"sync"

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
// it, until NewSet. What is bound once a scope began, in it or in the gap
// after it, is that scope's, forgotten when the next begins.
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	// run are the run's bindings (KeepRun), and runOf the table whose run's
	// bindings t reads as its run's too (ReadRunOf).
	run   map[string]Binding
	runOf *Table
	// began is whether a scope has begun since the table was made or NewSet.
	began bool
	// keepRun: what is bound before the first scope began is the run's
	// (KeepRun).
	keepRun bool
	// gen counts the scopes the table has begun (Reset): a call checked in
	// one scope never binds in the next (BindAll).
	gen uint64
}

// lookup is the binding of name in force: the scope's, else the run's.
// t.mu is held.
func (t *Table) lookup(name string) (Binding, bool) {
	if b, ok := t.vals[name]; ok {
		return b, true
	}
	if b, ok := t.run[name]; ok {
		return b, true
	}
	if t.runOf != nil {
		return t.runOf.runBinding(name)
	}
	return Binding{}, false
}

// runBinding is the run's binding of name, if any. It takes t.mu: a table
// that reads t's run's (ReadRunOf) calls it under its own lock, and t never
// locks that one.
func (t *Table) runBinding(name string) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.run[name]
	return b, ok
}

// runBindings is a copy of the run's bindings, nil for none. It takes t.mu,
// as runBinding.
func (t *Table) runBindings() map[string]Binding {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.run) == 0 {
		return nil
	}
	out := make(map[string]Binding, len(t.run))
	for n, b := range t.run {
		out[n] = b
	}
	return out
}

// runInForce is a copy of every run's binding t reads, its own and those of
// the table it reads the run's of (ReadRunOf); nil for none. t.mu is held.
func (t *Table) runInForce() map[string]Binding {
	var from map[string]Binding
	if t.runOf != nil {
		from = t.runOf.runBindings()
	}
	if len(t.run) == 0 {
		return from
	}
	if from == nil {
		from = make(map[string]Binding, len(t.run))
	}
	for n, b := range t.run {
		from[n] = b
	}
	return from
}

// all is every binding in force, the scope's over the run's. t.mu is held.
func (t *Table) all() map[string]Binding {
	run := t.runInForce()
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

// Gen is the scope the table is in now: what a call that is about to bind
// was checked against.
func (t *Table) Gen() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gen
}

// Lookup returns the binding of name in force, if any.
func (t *Table) Lookup(name string) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lookup(name)
}

// Bind records b unless its name is already bound. It returns the binding in
// force afterwards and whether it is b. It is for a value of no call (a
// start-up's, a given one): a call checked in a scope binds with BindAll or
// BindEach, which refuse a call of a scope that ended since.
func (t *Table) Bind(b Binding) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if have, ok := t.lookup(b.Name); ok {
		return have, false
	}
	t.put(b)
	return b, true
}

// KeepRun makes what is bound before the first scope begins the run's, which
// every scope of the set sees, instead of the first scope's to forget. It is
// called before the start-up binds anything (MockManager.NewVariableSet, as
// a test set's mocks are set up), and holds until the table is dropped.
func (t *Table) KeepRun() {
	t.mu.Lock()
	t.keepRun = true
	t.mu.Unlock()
}

// ReadRunOf makes t read the run's bindings of from as its run's too, in
// every scope: a table whose scope is a test's begun early (the next test's
// calls in flight, answered in the gap before it) sees what the run's
// start-up bound, as the test does once it begins. t keeps no run's of its
// own from it: what t binds is its scope's. from never reads t's.
func (t *Table) ReadRunOf(from *Table) {
	if from == t {
		return
	}
	t.mu.Lock()
	t.runOf = from
	t.mu.Unlock()
}

// put binds b in the scope, or the run's when none has begun and the table
// keeps the run's (KeepRun). t.mu is held.
func (t *Table) put(b Binding) {
	if !t.began && t.keepRun {
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
// in scope gen (Gen): each name holds one value (a name already bound must be
// bound to the same value), and two variables recorded with two values do not
// share a live value. It returns the first binding that does not hold and the
// binding it collides with; stale is true when a new scope began since gen
// (nothing is bound); ok is true when every binding of bs is in force
// afterwards.
func (t *Table) BindAll(gen uint64, bs []Binding) (bad, with Binding, stale, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return Binding{}, Binding{}, true, false
	}
	inForce := t.all()
	for i, b := range bs {
		if have, bound := inForce[b.Name]; bound && have.Value != b.Value {
			return b, have, false, false
		}
		for _, other := range inForce {
			if !other.Given && other.Name != b.Name && other.Value == b.Value && other.Recorded != b.Recorded {
				return b, other, false, false
			}
		}
		for _, other := range bs[:i] {
			if other.Name == b.Name && other.Value != b.Value {
				return b, other, false, false
			}
			if other.Name != b.Name && other.Value == b.Value && other.Recorded != b.Recorded {
				return b, other, false, false
			}
		}
	}
	for _, b := range bs {
		if _, bound := t.lookup(b.Name); !bound {
			t.put(b)
		}
	}
	return Binding{}, Binding{}, false, true
}

// BindEach binds each of bs on its own, under one lock, for a call checked in
// scope gen: a name already bound, or given twice in bs, must hold the same
// value, and nothing else is required of it (two variables may share a live
// value: two clock reads of one millisecond). stale is true when a new scope
// began since gen; nothing is bound unless ok.
func (t *Table) BindEach(gen uint64, bs []Binding) (stale, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return true, false
	}
	for i, b := range bs {
		if have, bound := t.lookup(b.Name); bound && have.Value != b.Value {
			return false, false
		}
		for _, other := range bs[:i] {
			if other.Name == b.Name && other.Value != b.Value {
				return false, false
			}
		}
	}
	for _, b := range bs {
		if _, bound := t.lookup(b.Name); !bound {
			t.put(b)
		}
	}
	return false, true
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
			t.vals[b.Name] = b
		}
	}
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
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &Table{gen: t.gen, began: t.began, keepRun: t.keepRun}
	if t.vals != nil {
		c.vals = make(map[string]Binding, len(t.vals))
		for k, v := range t.vals {
			c.vals[k] = v
		}
	}
	c.run = t.runInForce()
	return c
}

// Snapshot returns the bindings in force, the run's included, by name.
func (t *Table) Snapshot() []Binding {
	t.mu.Lock()
	in := t.all()
	out := make([]Binding, 0, len(in))
	for _, b := range in {
		out = append(out, b)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
