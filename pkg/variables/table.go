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
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
	// gen counts the scopes the table has begun (Reset): a call checked in
	// one scope never binds in the next (BindAll).
	gen uint64
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
	for i, b := range bs {
		if have, bound := t.vals[b.Name]; bound && have.Value != b.Value {
			return b, have, false, false
		}
		for _, other := range t.vals {
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
	if t.vals == nil {
		t.vals = make(map[string]Binding, len(bs))
	}
	for _, b := range bs {
		if _, bound := t.vals[b.Name]; !bound {
			t.vals[b.Name] = b
		}
	}
	return Binding{}, Binding{}, false, true
}

// Start begins a new scope (as Reset) with the values its test case gave
// bound, under one lock: no call of the scope binds before them. A name given
// twice keeps its first value.
func (t *Table) Start(given []Binding) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.gen++
	t.vals = nil
	if len(given) == 0 {
		return
	}
	t.vals = make(map[string]Binding, len(given))
	for _, b := range given {
		b.Given = true
		if _, dup := t.vals[b.Name]; !dup {
			t.vals[b.Name] = b
		}
	}
}

// Reset forgets every binding: a new scope begins.
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
	return c
}

// Snapshot returns the bindings in force, by name.
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
