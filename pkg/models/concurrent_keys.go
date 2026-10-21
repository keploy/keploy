package models

import "sync"

// ConcurrentKeys is how a kind's parser reads its recordings for keploy
// record's full round, which shares the recordings made while several tests'
// requests were open between those tests (Mapping.Groups,
// ExactServing.Concurrent). A kind that registers none shares nothing: each
// of its recordings stays the test that lists it.
//
// Each key is coarser than what the parser's matching under the proven switch
// accepts: two shareable recordings that could both answer one call have equal
// Request keys. The parser's tests pin why. Keys and values never leave the
// process: they are compared, never logged or written.
type ConcurrentKeys struct {
	// Statement is the key of what the recording's call does, apart from
	// the values it sends (for SQL: the statement's shape, its arity, and
	// how many constants it holds): only recordings of one statement are
	// compared. ok is false for a recording the kind does not key.
	Statement func(m *Mock) (key string, ok bool)
	// Request is the key of the recording's request: Statement, and every
	// value it sends but at the places excused names (the places the learn
	// step learned on a recording of the statement).
	Request func(m *Mock, excused []string) (key string, ok bool)
	// Answer is the key of the recording's answer: what the dependency
	// answered, with each cell that holds the value the recording sent at
	// one of echoes (the places the learn step learned as echoed) read as
	// that place.
	Answer func(m *Mock, echoes []string) (key string, ok bool)
	// Positions are the values the recording sends, by place (as the learn
	// step names a place: for SQL bind.$N and literal.N), each whole, as it
	// was sent.
	Positions func(m *Mock) map[string]string
	// Shareable reports whether the recording may be shared at all: false,
	// with why, for one whose recording states what widens its matching
	// beyond equal values (noise, volatile places, variables, a redacted
	// value), or one the kind's matching never answers a data call with.
	// A recording that is not shareable stays the test that lists it.
	Shareable func(m *Mock) (ok bool, why string)
	// Describe, when set, is the protocol and the call a charge of the
	// recording names (no value: for SQL, its statement with its constants
	// written ?); MockSummaryFromSpec and its kind when not.
	Describe func(m *Mock) (protocol, summary string)
}

var concurrentKeys = struct {
	mu   sync.RWMutex
	keys map[Kind]ConcurrentKeys
}{keys: map[Kind]ConcurrentKeys{}}

// RegisterConcurrentKeys registers kind's ConcurrentKeys, replacing any, and
// returns what puts back the previous registration (for tests). A parser
// registers in its package's init.
func RegisterConcurrentKeys(kind Kind, k ConcurrentKeys) (restore func()) {
	concurrentKeys.mu.Lock()
	defer concurrentKeys.mu.Unlock()
	prev, had := concurrentKeys.keys[kind]
	concurrentKeys.keys[kind] = k
	return func() {
		concurrentKeys.mu.Lock()
		defer concurrentKeys.mu.Unlock()
		if had {
			concurrentKeys.keys[kind] = prev
		} else {
			delete(concurrentKeys.keys, kind)
		}
	}
}

// ConcurrentKeysOf is kind's registered ConcurrentKeys; ok is false for a kind
// that registered none, or registered one without every key.
func ConcurrentKeysOf(kind Kind) (ConcurrentKeys, bool) {
	concurrentKeys.mu.RLock()
	defer concurrentKeys.mu.RUnlock()
	k, ok := concurrentKeys.keys[kind]
	if !ok || k.Statement == nil || k.Request == nil || k.Answer == nil || k.Positions == nil || k.Shareable == nil {
		return ConcurrentKeys{}, false
	}
	return k, true
}
