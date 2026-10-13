package variables

import (
	"sort"
	"sync"
)

// Binding is the value a variable has in a scope, and where it was taken.
type Binding struct {
	// Name is the variable.
	Name string
	// Value is the live value taken at the binding place.
	Value string
	// Recorded is the value the recording held at the binding place. Every
	// other place of the variable held it too when the call was recorded.
	Recorded string
	// Mock and Place are where the value was taken: the mock whose call bound
	// it, and the place in that call.
	Mock  string
	Place string
}

// Table holds the variables one scope has bound: a test set in keploy test,
// one test in keploy mock replay. A variable is bound once; a later binding of
// the same name does not take (Bind). Safe for concurrent use.
type Table struct {
	mu   sync.Mutex
	vals map[string]Binding
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

// Reset forgets every binding: a new scope begins.
func (t *Table) Reset() {
	t.mu.Lock()
	t.vals = nil
	t.mu.Unlock()
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
