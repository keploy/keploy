package integrations

import "go.keploy.io/server/v3/pkg/models"

// ExactServingReader is an optional MockMemDb capability: a store that can
// serve each test exactly its own mocks (the proven switch, design v14
// §5.2). While it does, every read shows one view, which changes as a test
// begins and ends, and a consume the view refuses fails: a test mock that
// already answered, perhaps on another connection, or a mock the view no
// longer shows (one read before a test began or ended, one another consume
// evicted). The agent's MockManager implements it, and every MockMemDb wrapper
// that passes the consume doors through to its store must forward it: one that
// drops it hides exact serving from the parsers it serves, and they take a
// refused consume for failed bookkeeping and answer with the mock. A wrapper
// whose doors never fail (http's answeredOnly) has nothing to forward.
type ExactServingReader interface {
	ExactServing() bool
}

// ExactServing reports whether db serves each test exactly its own mocks now.
// Exact serving can begin and end while a connection is open, so a parser
// asks at each decision that depends on it, never once per connection.
func ExactServing(db MockMemDb) bool {
	es, ok := db.(ExactServingReader)
	return ok && es.ExactServing()
}

// ConsumeRefused reports whether MarkMockAsUsed's false for the mock named
// name is a refusal: the mock may not answer, and the parser matches again
// against what the store shows now, or misses.
//
// It is one while db serves exactly, for a named mock. Without exact serving
// MarkMockAsUsed's false only ever said its bookkeeping failed, and the mock
// answers as before. MarkMockAsUsed turns a mock with no name away before it
// asks the view, so that false is bookkeeping too, and refusing it would have
// a parser match it again forever.
//
// It is one too, for any mock, from a store HoldToRound returned that could
// not hold its call to its round (UnheldWhy: the fail-closed store, or a view
// over one). That store refuses every consume and offers no exact serving,
// so read by exact serving alone its false would be bookkeeping, and the
// parser would answer with a recording the hold never let it read. Every
// read of it finds no recording, so a parser that matches again misses,
// and saying so (UnheldNote).
//
// The other doors need no such question. A delete door's false
// (DeleteFilteredMock, DeleteUnFilteredMock, DeleteStartupMock) already says
// that door did not take the mock, with or without exact serving; under exact
// serving every door refuses what the view refuses, so a parser that falls
// through from one to MarkMockAsUsed learns of the refusal there (for a named
// mock).
// UpdateUnFilteredMock's false is never a refusal on its own: under exact
// serving it is also a mock the view shows that the tree it updates does not
// hold, so a parser that updates a reusable mock asks MarkMockAsUsed after it.
func ConsumeRefused(db MockMemDb, name string) bool {
	if UnheldWhy(db) != "" {
		return true
	}
	return name != "" && ExactServing(db)
}

// ExactConcurrentReader is an optional MockMemDb capability under exact
// serving: what the open test's view says of the recordings it shares with
// the tests whose requests were open beside its own at record time
// (models.ExactServing.Concurrent, models.Mock.TestModeInfo.Concurrent on the
// copies it shows). Those recordings answer at most once in the staging, for
// whichever of the tests makes its call first, and a parser's learn step never
// learns against one (another of those tests may have made it): it reads here
// which of them another test took, so a call closest to one is a conflict.
// Only keploy record's full round shares recordings; every other view has
// none, and every answer here is empty.
type ExactConcurrentReader interface {
	// ClaimantOf is the test that answered a call with the shared recording
	// name in the current staging, if one did.
	ClaimantOf(name string) (test string, ok bool)
	// ExactTakenCandidates are, of the open test's shared recordings of kind
	// (every kind for ""), those another test answered a call with: the view
	// shows them no longer, and a parser only compares a call with them. The
	// manager's copies, in recorded order; read-only.
	ExactTakenCandidates(kind models.Kind) []*models.Mock
}

// StagingGenerations is what a store that serves exactly offers a parser that
// keeps an index of what it reads, so the index is built once per staging
// generation instead of on every consume (MockManager.StagingGeneration):
//
//   - StagingGeneration (StagingGenerationByKind for one kind's reads) moves
//     whenever Revision (RevisionByKind) does, except for a consume or a spend
//     under exact serving.
//   - MockSpent reports the mocks such a consume or spend dropped from the
//     reads since the generation began: while one generation holds, a read
//     gives what it gave when the generation began, less those.
//
// So a parser samples the generation, reads, builds its index, and samples it
// again (keeping the index only when both samples agree, as with Revision);
// at lookup it skips any candidate MockSpent reports. The consume it then
// makes is still settled against the view in force, and refused when the mock
// may not answer (ConsumeRefused), so the skip saves a refused consume and
// never decides alone whether a mock answers.
//
// A store held to a call (RoundHolder) reports the generation of what it can
// read: the store's while the staging is still the call's, and
// RestagedGeneration once it moved, when it reads nothing. So an index a
// parser built from a held read the staging moved under is never kept for
// the generation of the set staged after the call, which an unheld read (or
// a call marked after the staging) finds whole.
type StagingGenerations interface {
	StagingGeneration() uint64
	StagingGenerationByKind(kind models.Kind) uint64
	MockSpent(name string) bool
}

// RestagedGeneration is the staging generation (and the revision) a store held
// to a call (RoundHolder) reports once the staging moved from the call's: it
// reads nothing then, so the generation names the empty set, the same for
// every such call. A store's own generations and revisions count up from zero
// by one per change (a view over a store adds its own small version) and
// never reach it, so no index of a staged set is keyed on it.
const RestagedGeneration uint64 = 1 << 63

// StagingGenerationOf returns db's staging generation for kind's reads (every
// kind's for ""), and false when db keeps none: the parser then keys its index
// on Revision as before.
func StagingGenerationOf(db MockMemDb, kind models.Kind) (uint64, bool) {
	sg, ok := db.(StagingGenerations)
	if !ok {
		return 0, false
	}
	if kind == "" {
		return sg.StagingGeneration(), true
	}
	return sg.StagingGenerationByKind(kind), true
}
