package models

import "sync/atomic"

// mockRulesGen counts the changes to how a read of a mock file decodes its
// documents and derives its mocks: a registration of one of this package's
// per-kind hooks (the Register functions) or its removal, the declared
// session calls (SetSessionCalls), and a registry outside this package that
// a read decodes through (mockdb's YAML mappers, see MockRulesChanged). A
// lifetime, a registered predicate's answer, a request fingerprint, a
// summary or a mock's links are each read through them. So a read kept for
// reuse (mockdb's index of a mock file) is reused only while the generation
// it was read at is current.
var mockRulesGen atomic.Uint64

// MockRulesGeneration is the current generation of the rules a read of a mock
// file derives its mocks by (see mockRulesGen).
func MockRulesGeneration() uint64 { return mockRulesGen.Load() }

// mockRulesChanged starts a new generation. Every registry writer calls it,
// after its change is visible to readers.
func mockRulesChanged() { mockRulesGen.Add(1) }

// MockRulesChanged starts a new generation (see mockRulesGen) for a registry
// outside this package that a read of a mock file decodes or derives its
// mocks through. Its writer calls it after each change, once the change is
// visible to readers.
func MockRulesChanged() { mockRulesChanged() }
