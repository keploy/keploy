package models

import "sync"

// REQUEST KEYS (design v14, a2).
//
// A miss under exact serving that is a recording made elsewhere names that
// recording, which keploy record's judging bakes into the set's worker scope.
// The recording may be of a test that has not begun, which the agent keeps on
// disk until the test opens. The disk store indexes its mocks by the request
// keys their parser declares, so a miss's diagnosis reads, and decodes, only
// the recordings that may be its call: those under the keys it asks by.
//
// A key narrows; the parser's own comparison decides. So a parser declares
// keys such that every call its comparison accepts as a recording asks by at
// least one key the recording is indexed under: an HTTP call's method and
// path, say, and, for a recording whose comparison tolerates another path (a
// segment it learned as noise), its method and number of path segments.

// RegisterRequestKeys declares the request keys of mocks of kind: keys
// returns those of a recorded mock, none when it has none. One per kind; a
// later registration replaces an earlier one. The returned function puts back
// what was there; production code ignores it, tests use it.
func RegisterRequestKeys(kind Kind, keys func(*Mock) []string) (unregister func()) {
	requestKeys.mu.Lock()
	defer requestKeys.mu.Unlock()
	if requestKeys.byKind == nil {
		requestKeys.byKind = map[Kind]func(*Mock) []string{}
	}
	prev := requestKeys.byKind[kind]
	requestKeys.byKind[kind] = keys
	return func() {
		requestKeys.mu.Lock()
		defer requestKeys.mu.Unlock()
		requestKeys.byKind[kind] = prev
	}
}

var requestKeys struct {
	mu     sync.RWMutex
	byKind map[Kind]func(*Mock) []string
}

// RequestKeys are m's request keys, as its kind's parser declared them
// (RegisterRequestKeys); none when none are declared.
func RequestKeys(m *Mock) []string {
	if m == nil {
		return nil
	}
	requestKeys.mu.RLock()
	keys := requestKeys.byKind[m.Kind]
	requestKeys.mu.RUnlock()
	if keys == nil {
		return nil
	}
	return keys(m)
}

// HasRequestKeys reports whether kind's parser declared request keys
// (RegisterRequestKeys). A store narrows a lookup by a call's keys only for
// such a kind: a kind with none files no recording under any key.
func HasRequestKeys(kind Kind) bool {
	requestKeys.mu.RLock()
	defer requestKeys.mu.RUnlock()
	return requestKeys.byKind[kind] != nil
}
