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
// With KeepRun, a value bound while no scope is open (the run's start-up,
// before its first test, or between two tests once a scope was closed) is
// the run's: every later scope sees it.
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	run  map[string]Binding
	// open is whether a scope is open (Reset, Start; until Close).
	open bool
	// keepRun: what is bound while no scope is open is the run's (KeepRun).
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
	b, ok := t.run[name]
	return b, ok
}

// all is every binding in force, the scope's over the run's. t.mu is held.
func (t *Table) all() map[string]Binding {
	out := make(map[string]Binding, len(t.vals)+len(t.run))
	for n, b := range t.run {
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
// force afterwards and whether it is b.
func (t *Table) Bind(b Binding) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if have, ok := t.lookup(b.Name); ok {
		return have, false
	}
	t.put(b)
	return b, true
}

// KeepRun makes what is bound while no scope is open the run's, which every
// later scope sees, instead of the next scope's to forget.
func (t *Table) KeepRun() {
	t.mu.Lock()
	t.keepRun = true
	t.mu.Unlock()
}

// put binds b in the open scope, or the run's when none is open and the
// table keeps the run's (KeepRun). t.mu is held.
func (t *Table) put(b Binding) {
	if !t.open && t.keepRun {
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
// scope gen: a name already bound must hold the same value, and nothing else
// is required of it (two variables may share a live value: two clock reads
// of one millisecond). stale is true when a new scope began since gen;
// nothing is bound unless ok.
func (t *Table) BindEach(gen uint64, bs []Binding) (stale, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return true, false
	}
	for _, b := range bs {
		if have, bound := t.lookup(b.Name); bound && have.Value != b.Value {
			return false, false
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
	t.open = true
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
	t.open = true
	t.gen++
	t.mu.Unlock()
}

// Close ends the open scope: what is bound until the next one begins is the
// run's.
func (t *Table) Close() {
	t.mu.Lock()
	t.vals = nil
	t.open = false
	t.gen++
	t.mu.Unlock()
}

// Clone is a table holding t's bindings in t's scope, sharing nothing with
// it: a match checked against the clone binds nothing in t.
func (t *Table) Clone() *Table {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &Table{gen: t.gen, open: t.open, keepRun: t.keepRun}
	if t.vals != nil {
		c.vals = make(map[string]Binding, len(t.vals))
		for k, v := range t.vals {
			c.vals[k] = v
		}
	}
	if t.run != nil {
		c.run = make(map[string]Binding, len(t.run))
		for k, v := range t.run {
			c.run[k] = v
		}
	}
	return c
}

// Snapshot returns the bindings in force, by name.
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
