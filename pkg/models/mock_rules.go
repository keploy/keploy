package models

import "sync/atomic"

// mockRulesGen counts the changes to what a read of a mock file derives for
// its mocks beyond their documents: a registration of one of this package's
// per-kind hooks (the Register functions) or its removal, and the declared
// session calls (SetSessionCalls). A lifetime, a registered predicate's
// answer, a request fingerprint, a summary or a mock's links are each read
// through them. So a read kept for reuse (mockdb's index of a mock file) is
// reused only while the generation it was read at is current.
var mockRulesGen atomic.Uint64

// MockRulesGeneration is the current generation of the rules a read of a mock
// file derives its mocks by (see mockRulesGen).
func MockRulesGeneration() uint64 { return mockRulesGen.Load() }

// mockRulesChanged starts a new generation. Every registry writer calls it,
// after its change is visible to readers.
func mockRulesChanged() { mockRulesGen.Add(1) }
