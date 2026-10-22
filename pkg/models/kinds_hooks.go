package models

import "sync"

// WRITE KINDS AND EXACT KEYS (S12 v16 §2.2, §2.3, §3).
//
// Two declarations a parser makes about its kind's recordings, for a test
// set's background (the worker scope and the pool), under the proven switch:
//
//   - a write kind (RegisterWriteKind): the recorded writes that may be
//     answered by their kind alone, whatever the background recording they
//     take, and that keploy record routes to the worker scope when one was
//     sent before a test and answered inside it, on one trusted clock;
//   - an exact key (RegisterExactKey): what makes two recordings identical
//     siblings under the kind's exact-serving comparator, so that a
//     background recording with no sibling may answer from the set's start.
//
// A kind nobody registers has neither: none of its writes is answered by
// kind, and its background recordings keep today's Place gating whole. Each
// is one registration per kind, from the parser's init function.

// RegisterWriteKind declares how kind's parser names a recorded write's kind,
// for by-kind answering and record-time routing (S12 v16 §2.2, §3). of
// returns the kind ("Mongo insert keploy.events": protocol, operation,
// db.collection) and true for a write that may be answered by kind; false
// for anything else. It reads the mock alone and must be cheap. Call it from
// an init function. One registration per kind: a second panics, as does a
// nil of. The returned function removes it (tests).
func RegisterWriteKind(kind Kind, of func(*Mock) (writeKind string, ok bool)) (unregister func()) {
	return writeKinds.register("RegisterWriteKind", kind, of)
}

// WriteKindOf is m's write kind, from its kind's registration: false when
// none is registered, or when m is no by-kind write. A registration that
// names a write "" names none.
func WriteKindOf(m *Mock) (string, bool) {
	return writeKinds.of(m)
}

// RegisterExactKey declares kind's exact key: two recordings of kind with
// equal keys are identical siblings under its exact-serving comparator
// (S12 v16 §2.3's no-sibling rule). key returns false for a recording it
// cannot key; that recording counts as having siblings, and stays gated by
// its Place. A kind nobody registers keeps today's Place gating whole.
// Registration rules as RegisterWriteKind's.
func RegisterExactKey(kind Kind, key func(*Mock) (string, bool)) (unregister func()) {
	return exactKeys.register("RegisterExactKey", kind, key)
}

// ExactKeyOf is m's exact key, from its kind's registration: false when none
// is registered, or when it cannot key m. A registration that keys a
// recording "" cannot key it.
func ExactKeyOf(m *Mock) (string, bool) {
	return exactKeys.of(m)
}

var writeKinds, exactKeys mockNamers

// mockNamers is a registry of one naming function per kind: the
// RegisterWriteKind and RegisterExactKey registries.
type mockNamers struct {
	mu     sync.RWMutex
	byKind map[Kind]*mockNamer
}

// mockNamer is one registration; its identity is what its unregister removes.
type mockNamer struct {
	fn func(*Mock) (string, bool)
}

// register adds fn as kind's, panicking on a nil fn or a kind that has one
// already (api names the caller in the panic). The returned function removes
// exactly this registration: once removed, or replaced after its removal by
// another, it does nothing.
func (r *mockNamers) register(api string, kind Kind, fn func(*Mock) (string, bool)) (unregister func()) {
	if fn == nil {
		panic("models." + api + "(" + string(kind) + "): a nil function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byKind[kind]; taken {
		panic("models." + api + "(" + string(kind) + "): the kind is registered already")
	}
	if r.byKind == nil {
		r.byKind = map[Kind]*mockNamer{}
	}
	n := &mockNamer{fn: fn}
	r.byKind[kind] = n
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.byKind[kind] == n {
			delete(r.byKind, kind)
		}
	}
}

// of is m's name by its kind's registration: false for a nil m, a kind with
// none, or a mock the registration does not name, or names "".
func (r *mockNamers) of(m *Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	r.mu.RLock()
	n := r.byKind[m.Kind]
	r.mu.RUnlock()
	if n == nil {
		return "", false
	}
	name, ok := n.fn(m)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}
