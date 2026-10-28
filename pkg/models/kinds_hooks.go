package models

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// WRITE KINDS, EXACT KEYS AND CALL WRITES.
//
// Three declarations a parser makes about its kind's recordings, under the
// proven switch. The first two are for a test set's background (the worker
// scope and the pool):
//
//   - a write kind (RegisterWriteKind): the recorded writes that may be
//     answered by their kind alone, whatever the background recording they
//     take, and that keploy record routes to the worker scope when one was
//     sent before a test and answered inside it, on one trusted clock;
//   - an exact key (RegisterExactKey): what makes two recordings identical
//     siblings under the kind's exact-serving comparator, so that a
//     background recording with no sibling may answer from the set's start.
//
// The third is for judging a test's calls:
//
//   - a call write (RegisterCallWrite): the recordings whose call changes
//     what its dependency holds, told from a read. A call a test makes once
//     more than it recorded names, as its miss, the write it repeats
//     (models.UnmatchedCall.Write); judging marks such a recording as the
//     application's background work only when it is a write: a read made
//     once more is a cached copy expiring on the application's clock, which
//     judging sets aside instead.
//
// A kind nobody registers has none of them: none of its writes is answered
// by kind, its background recordings keep today's Place gating whole, and
// none of its calls is told to be a write. A write kind and a call write are
// each one registration per kind, from the parser's init function.
// A kind may have several exact keys, each for the recordings its parser
// owns (enterprise's SQS keys the HTTP recordings it serves, and another
// HTTP parser may key its own): they are tried in registration order, and
// one that does not own a recording says so (ok false).

// RegisterWriteKind declares how kind's parser names a recorded write's kind,
// for by-kind answering and record-time routing. of
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

// RegisterCallWrite declares how kind's parser tells a recorded call that
// writes (changes what its dependency holds) from one that reads it. of
// returns what the call writes ("HTTP POST", "Mongo update keploy.keys":
// protocol, operation, and a collection's name where the kind has one; names,
// never a value) and true for a write; false for a read and for a call the
// parser cannot tell. It reads the mock alone and must be cheap. Call it from
// an init function. One registration per kind: a second panics, as does a
// nil of. The returned function removes it (tests).
func RegisterCallWrite(kind Kind, of func(*Mock) (write string, ok bool)) (unregister func()) {
	return callWrites.register("RegisterCallWrite", kind, of)
}

// CallWriteOf is what m's call writes, by its kind's registration: false
// when none is registered, or when m is a read or a call the registration
// cannot tell. A registration that names a write "" names none.
func CallWriteOf(m *Mock) (string, bool) {
	return callWrites.of(m)
}

// RegisterExactKey declares one of kind's exact keys: two recordings of kind
// with equal keys are identical siblings under its exact-serving comparator
// (the background's no-sibling rule). key returns false for a recording it
// does not own or cannot key. A kind may have several keys, each registered
// by the parser that owns the recordings it keys: they are tried in
// registration order, and the one that keys a recording is its key. Each
// registration's keys are its own: equal strings two registrations return
// are two keys (ExactSiblingKey names each by its registration), so one
// parser's recordings never count as another's siblings. A recording no key
// keys counts as having siblings, and stays gated by its Place; so does one
// two or more keys claim (ExactKeyErr says which): the registry refuses to
// pick between them, as an error when it is asked, never as a panic at init.
// A kind nobody registers keeps today's Place gating whole. Call it from an
// init function; a nil key panics. The returned function removes this
// registration alone (tests).
func RegisterExactKey(kind Kind, key func(*Mock) (string, bool)) (unregister func()) {
	return exactKeys.register(kind, key, callerName(2))
}

// ExactKeyOf is m's exact key as the one registration of its kind that keys
// it gave it: false when none is registered, none keys m (or keys it ""), or
// more than one does (ExactKeyErr).
func ExactKeyOf(m *Mock) (string, bool) {
	key, ok, _ := exactKeys.of(m)
	return key, ok
}

// ExactKeyErr is why m has no exact key although its kind's registrations
// key it: two or more of them claim it, named by where each was registered.
// nil when at most one does. The same recording and registrations give the
// same error every time.
func ExactKeyErr(m *Mock) error {
	_, _, err := exactKeys.of(m)
	return err
}

// ExactKeyClaims are the keys m's kind's registrations give it, as they gave
// them, in registration order: one for a recording one key owns, two or more
// for one several claim (ExactKeyErr), none otherwise.
func ExactKeyClaims(m *Mock) []string {
	keys, _ := exactKeys.claimed(m)
	return keys
}

// ExactSiblingKey is ExactKeyOf named by the registration that gave it: the
// key two recordings are siblings under only when one registration gave both
// the same key. It is what a comparison of siblings reads; it names a
// registration of this process, so it is never written down.
func ExactSiblingKey(m *Mock) (string, bool) {
	keys, regs := exactKeys.claimedBy(m)
	if len(keys) != 1 {
		return "", false
	}
	return regs[0].scoped(keys[0]), true
}

// ExactSiblingClaims are ExactKeyClaims, each named by the registration that
// gave it (ExactSiblingKey). Siblings are counted by each: a recording two
// keys claim has no key of its own, and is a sibling under either of the
// keys it was given.
func ExactSiblingClaims(m *Mock) []string {
	keys, regs := exactKeys.claimedBy(m)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = regs[i].scoped(k)
	}
	return out
}

var (
	writeKinds mockNamers
	callWrites mockNamers
	exactKeys  exactKeyRegistry
)

// mockNamers is a registry of one naming function per kind: the
// RegisterWriteKind and RegisterCallWrite registries.
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

// exactKeyRegistry is the RegisterExactKey registry: each kind's keys, in
// registration order.
type exactKeyRegistry struct {
	mu     sync.RWMutex
	byKind map[Kind][]*exactKey
	// next is the id the next registration takes.
	next uint64
}

// exactKey is one RegisterExactKey registration; its identity is what its
// unregister removes, where names the function that registered it, and id,
// unique to it, names the keys it gives (scoped).
type exactKey struct {
	fn    func(*Mock) (string, bool)
	where string
	id    uint64
}

// scoped is key as registration k gives it: named by k, so no other
// registration's key equals it.
func (k *exactKey) scoped(key string) string {
	return strconv.FormatUint(k.id, 10) + "\x00" + key
}

// register appends fn to kind's keys (where names its registrant),
// panicking on a nil fn. The returned function removes exactly this
// registration: once removed, it does nothing.
func (r *exactKeyRegistry) register(kind Kind, fn func(*Mock) (string, bool), where string) (unregister func()) {
	if fn == nil {
		panic("models.RegisterExactKey(" + string(kind) + "): a nil function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKind == nil {
		r.byKind = map[Kind][]*exactKey{}
	}
	r.next++
	k := &exactKey{fn: fn, where: where, id: r.next}
	r.byKind[kind] = append(r.byKind[kind], k)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		keys := r.byKind[kind]
		for i, o := range keys {
			if o == k {
				r.byKind[kind] = append(keys[:i:i], keys[i+1:]...)
				if len(r.byKind[kind]) == 0 {
					delete(r.byKind, kind)
				}
				return
			}
		}
	}
}

// of is m's key by the one registration of its kind that keys it, every one
// asked in registration order: false for a nil m, a kind with none, or a
// mock none keys (or keys ""); false with err when two or more key it.
func (r *exactKeyRegistry) of(m *Mock) (string, bool, error) {
	keys, by := r.claimed(m)
	if len(by) > 1 {
		return "", false, fmt.Errorf("%d of the %s kind's exact keys key recording %s (registered by %s): keploy does not pick one, so it counts as having identical siblings and answers only from its place; each key must decline (ok false) a recording its parser does not own",
			len(by), m.Kind, m.Name, strings.Join(by, ", "))
	}
	if len(keys) == 0 {
		return "", false, nil
	}
	return keys[0], true, nil
}

// claimed asks every registration of m's kind, in registration order, and
// returns the keys of those that key m (none for a nil m, or a key "") and
// where each was registered.
func (r *exactKeyRegistry) claimed(m *Mock) (keys, by []string) {
	keys, regs := r.claimedBy(m)
	for _, k := range regs {
		by = append(by, k.where)
	}
	return keys, by
}

// claimedBy is claimed, with the registrations that gave each key.
func (r *exactKeyRegistry) claimedBy(m *Mock) (keys []string, regs []*exactKey) {
	if m == nil {
		return nil, nil
	}
	r.mu.RLock()
	all := append([]*exactKey(nil), r.byKind[m.Kind]...)
	r.mu.RUnlock()
	for _, k := range all {
		got, owns := k.fn(m)
		if !owns || got == "" {
			continue
		}
		keys = append(keys, got)
		regs = append(regs, k)
	}
	return keys, regs
}

// callerName is the function skip frames above callerName's caller, by its
// full name ("" when it cannot be told).
func callerName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip)
	if !ok {
		return ""
	}
	if fn := runtime.FuncForPC(pc); fn != nil {
		return fn.Name()
	}
	return ""
}
