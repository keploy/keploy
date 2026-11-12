package models

import (
	"sync"
)

// A recorded mock can depend on another mock of its test set. A MySQL
// COM_STMT_EXECUTE names its statement by an ID, and only the
// COM_STMT_PREPARE recorded before it on the same connection says which query
// that ID ran. Whatever removes mocks from a test set must not keep the one and
// drop the other: the prune after a replay (--remove-unused-mocks, auto-replay)
// did, judging each mock alone, and kept EXECUTEs whose PREPARE it deleted
// (keploy #4810). The next replay could no longer tell which statement such an
// EXECUTE ran, and served another test's EXECUTE in its place.
//
// A kind declares these links once, with RegisterMockDeps, and what removes
// mocks reads them with DepsOf, without knowing the protocol.

// MockDeps are a mock's links to the other mocks of its test set. A key is
// opaque to everything but the kind that declares it, and compared for
// equality only; it reads as what it names, as reports quote it.
type MockDeps struct {
	// Provides are the keys this mock satisfies.
	Provides []string
	// Requires are the keys this mock was recorded against.
	Requires []MockRequirement
}

// MockRequirement is one key a mock was recorded against (see MockDeps).
//
// Whatever keeps the mock keeps every mock of its set that provides Key.
type MockRequirement struct {
	Key string
	// Standalone says the mock replays as recorded without the mocks that
	// provide Key: it holds its own copy of what it takes from them (a MySQL
	// COM_STMT_EXECUTE that recorded its statement's query), or takes nothing
	// from them at replay (a COM_STMT_CLOSE, which the server never
	// answers). The providers are kept with it all the same: a keploy that
	// does not read the copy still needs them, and a replay resolves the key
	// as it did before.
	Standalone bool
}

// MockDepsFunc reads a mock's links. It must depend only on the mock as
// recorded, and must not change it.
type MockDepsFunc func(*Mock) MockDeps

var mockDeps = struct {
	sync.RWMutex
	byKind map[Kind]MockDepsFunc
}{byKind: map[Kind]MockDepsFunc{}}

// RegisterMockDeps declares how mocks of kind link to the other mocks of
// their test set; it replaces any earlier declaration for kind. A parser that
// lives outside this module registers from its own package, so the links of
// its kind are known only in a binary that links that package: one that
// removes mocks without it (a CLI built without the parser) judges each of
// its mocks alone. It returns the declaration it replaced (nil: none), for a
// caller that puts it back.
func RegisterMockDeps(kind Kind, fn MockDepsFunc) (previous MockDepsFunc) {
	mockDeps.Lock()
	defer mockDeps.Unlock()
	defer mockRulesChanged()
	previous = mockDeps.byKind[kind]
	if fn == nil {
		delete(mockDeps.byKind, kind)
		return previous
	}
	mockDeps.byKind[kind] = fn
	return previous
}

// DepsOf reads m's links to the other mocks of its test set; none when its
// kind declares none.
func DepsOf(m *Mock) MockDeps {
	if m == nil {
		return MockDeps{}
	}
	mockDeps.RLock()
	fn := mockDeps.byKind[m.Kind]
	mockDeps.RUnlock()
	if fn == nil {
		return MockDeps{}
	}
	return fn(m)
}

// UnmetRequirement is a mock recorded against a key that no mock of its test
// set provides, and that is not standalone for it: it cannot be replayed as it
// was recorded.
type UnmetRequirement struct {
	Mock string
	Kind Kind
	Key  string
}

// MockDepsCheck finds, over every mock of a test set, the requirements no
// mock of the set meets (see UnmetRequirement). Mocks are added in any order.
type MockDepsCheck struct {
	provided map[string]struct{}
	required []UnmetRequirement
}

// Add reads m's links.
func (c *MockDepsCheck) Add(m *Mock) {
	deps := DepsOf(m)
	for _, key := range deps.Provides {
		if c.provided == nil {
			c.provided = map[string]struct{}{}
		}
		c.provided[key] = struct{}{}
	}
	for _, r := range deps.Requires {
		if !r.Standalone {
			c.required = append(c.required, UnmetRequirement{Mock: m.Name, Kind: m.Kind, Key: r.Key})
		}
	}
}

// Unmet returns the requirements of the mocks added that none of them meets,
// in the order the mocks were added.
func (c *MockDepsCheck) Unmet() []UnmetRequirement {
	var out []UnmetRequirement
	for _, r := range c.required {
		if _, ok := c.provided[r.Key]; !ok {
			out = append(out, r)
		}
	}
	return out
}
