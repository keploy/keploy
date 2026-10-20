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
// Beside the test's scope it holds the claim key's (models.MockVariable
// Outside): the variables a call made outside every test bound (a Playwright
// beforeAll), which every test of the key reads. A test's begin (Reset,
// Start) keeps them; a new claim drops them (StartOutside). The two are read
// apart: a test's variable is looked up in the test's scope (Lookup), the
// key's in the key's (LookupOutside), and a binding goes to the scope its
// Outside names (BindAll).
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	// gen counts the scopes the table has begun (Reset): a call checked in
	// one scope never binds in the next (BindAll).
	gen uint64
	// outside are the claim key's bindings.
	outside map[string]Binding
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
	b, ok := t.vals[name]
	return b, ok
}

// Bind records b unless its name is already bound. It returns the binding in
// force afterwards and whether it is b.
func (t *Table) Bind(b Binding) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if have, ok := t.vals[b.Name]; ok {
		return have, false
	}
	if t.vals == nil {
		t.vals = make(map[string]Binding)
	}
	t.vals[b.Name] = b
	return b, true
}

// LookupOutside returns the claim key's binding of name in force, if any.
func (t *Table) LookupOutside(name string) (Binding, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.outside[name]
	return b, ok
}

// BindAll binds bs together, all or none, under one lock, for a call checked
// in scope gen (Gen): each name holds one value (a name already bound must be
// bound to the same value), and two variables recorded with two values do not
// share a live value. A binding whose Outside is set is the claim key's, and
// is checked against the key's alone; a test's is checked against the test's
// and the key's. It returns the first binding that does not hold and the
// binding it collides with; stale is true when a new scope began since gen
// (nothing is bound); ok is true when every binding of bs is in force
// afterwards.
func (t *Table) BindAll(gen uint64, bs []Binding) (bad, with Binding, stale, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return Binding{}, Binding{}, true, false
	}
	for i, b := range bs {
		scope := t.vals
		if b.Outside {
			scope = t.outside
		}
		if have, bound := scope[b.Name]; bound && have.Value != b.Value {
			return b, have, false, false
		}
		others := []map[string]Binding{t.outside}
		if !b.Outside {
			others = append(others, t.vals)
		}
		for _, in := range others {
			for _, other := range in {
				if !other.Given && other.Name != b.Name && other.Value == b.Value && other.Recorded != b.Recorded {
					return b, other, false, false
				}
			}
		}
		for _, other := range bs[:i] {
			if other.Name == b.Name && other.Outside == b.Outside && other.Value != b.Value {
				return b, other, false, false
			}
			if other.Name != b.Name && other.Value == b.Value && other.Recorded != b.Recorded {
				return b, other, false, false
			}
		}
	}
	for _, b := range bs {
		scope := &t.vals
		if b.Outside {
			scope = &t.outside
		}
		if *scope == nil {
			*scope = make(map[string]Binding, len(bs))
		}
		if _, bound := (*scope)[b.Name]; !bound {
			(*scope)[b.Name] = b
		}
	}
	return Binding{}, Binding{}, false, true
}

// StartOutside begins a new claim's scope outside its tests: the claim key's
// bindings are dropped (a test process run again runs its beforeAll again,
// and mints its values anew), and so are the last test's, which belong to
// the previous claim.
func (t *Table) StartOutside() {
	t.mu.Lock()
	t.outside = nil
	t.vals = nil
	t.gen++
	t.mu.Unlock()
}

// OutsideSnapshot returns the claim key's bindings in force, by name.
func (t *Table) OutsideSnapshot() []Binding {
	t.mu.Lock()
	out := make([]Binding, 0, len(t.outside))
	for _, b := range t.outside {
		out = append(out, b)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start begins a new scope (as Reset) with the values its test case gave
// bound, under one lock: no call of the scope binds before them. A name given
// twice keeps its first value. The claim key's bindings stay.
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

// Reset forgets every binding of the test's scope: a new scope begins. The
// claim key's stay (StartOutside drops them).
func (t *Table) Reset() {
	t.mu.Lock()
	t.vals = nil
	t.gen++
	t.mu.Unlock()
}

// Clone is a table holding t's bindings in t's scope, sharing nothing with
// it: a match checked against the clone binds nothing in t.
func (t *Table) Clone() *Table {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := &Table{gen: t.gen}
	if t.vals != nil {
		c.vals = make(map[string]Binding, len(t.vals))
		for k, v := range t.vals {
			c.vals[k] = v
		}
	}
	if t.outside != nil {
		c.outside = make(map[string]Binding, len(t.outside))
		for k, v := range t.outside {
			c.outside[k] = v
		}
	}
	return c
}

// Snapshot returns the test scope's bindings in force, by name (the claim
// key's: OutsideSnapshot).
func (t *Table) Snapshot() []Binding {
	t.mu.Lock()
	out := make([]Binding, 0, len(t.vals))
	for _, b := range t.vals {
		out = append(out, b)
	}
	t.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
