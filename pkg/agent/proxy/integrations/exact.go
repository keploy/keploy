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

// SharedGen is the generation of the shared part of an open test's reads
// (SharedReads): View names what the test's view shows beyond the trees and
// its own recordings, the same for every test view that shows the same (the
// reached worker scope, the background rules, the kept learning), and Store
// moves on each staging, store into a tree or delete of the kind, never on a
// test's open or close.
type SharedGen struct {
	View, Store uint64
}

// SharedReads is what a store that serves exactly offers a parser whose index
// of the reads would otherwise be built again at each test's open or close,
// which moves the staging generation (StagingGenerations): while a test is
// open its per-test and session reads are the open test's unanswered
// recordings (OwnedUnanswered), which lead each read, then a shared part
// (SharedMocks), which every test's view with the same SharedGen shows alike.
//
//   - GetPerTestMocksInWindow is OwnedUnanswered then perTest less what
//     MockSpent reports.
//   - GetSessionMocks is OwnedUnanswered then session less what MockSpent
//     reports.
//
// The two parts never overlap: the shared part holds none of the test's own
// recordings, answered or not, so each read holds each recording once, and a
// recording the test answered with is in neither.
//
// The shared part is static: it holds what a view spent too, since a spend
// lasts only as long as its view, so a parser keeps one index of it per
// SharedGen and skips at lookup what MockSpent reports. Every method reports
// false (or nothing) while no test is open, and for a store held to a call
// once the staging moved from the call's (RoundHolder): such a parser keys on
// the staging generation then.
//
// A parser builds its index in this order: sample SharedGeneration, read
// SharedMocks, sample SharedGeneration again, and keep the index only when
// both samples agree (and report true). A staging swaps the trees and then
// moves the generation, and SharedMocks never reads between the two, so when
// both samples agree the read is of the trees they name. A parser that reads
// SharedMocks before its first sample may file the trees a staging is
// replacing under the generation that staging moves to, and keep serving
// them until the next move.
//
// The open test's own part can be indexed too: OwnedRecordings is all of the
// test's own recordings, answered or not, in OwnedUnanswered's order, with
// the number of the view that shows them, and the list never changes while
// that number holds (each test's open shows a new one). A parser files them
// once per number, and reads what is left of a bucket with
// OwnedUnansweredAt: OwnedUnanswered is OwnedUnansweredAt of every position.
type SharedReads interface {
	StagingGenerations
	SharedGeneration(kind models.Kind) (SharedGen, bool)
	SharedMocks() (perTest, session []*models.Mock, ok bool)
	OwnedUnanswered() ([]*models.Mock, bool)
	// OwnedRecordings is the open test's own recordings and the number of
	// the view that shows them; the caller never changes the list. False
	// while no test is open.
	OwnedRecordings() (view uint64, owned []*models.Mock, ok bool)
	// OwnedUnansweredAt is the positions of at, in OwnedRecordings of the
	// view numbered view, whose recordings have not answered, in at's
	// order. False when that view is not the open test's now.
	OwnedUnansweredAt(view uint64, at []int) ([]int, bool)
}

// SharedGenerationOf returns db's SharedGen for kind's reads, and false where
// db offers none now (SharedReads).
func SharedGenerationOf(db MockMemDb, kind models.Kind) (SharedGen, bool) {
	sr, ok := db.(SharedReads)
	if !ok {
		return SharedGen{}, false
	}
	return sr.SharedGeneration(kind)
}

// Restaged is a store that tells a parser when it lets go of the recordings
// its reads handed out: at a staging (which moves SharedGen's Store) and at
// its Close, never at a store into its trees or a delete from them, whose
// recordings are of the same set. A parser that keeps data built
// from a store's reads (an index of one staging's recordings) asks to be told
// (WhenRestaged), so the data, and the recordings it points at, go with the
// store's own rather than at the parser's next call, which may come only in a
// later set, or never.
type Restaged interface {
	// WhenRestaged runs release once, at the next staging (after its moves)
	// or Close; at once on a closed store. One slot keeps one release: a
	// later one for the same slot replaces it.
	WhenRestaged(slot any, release func())
}

// WhenRestaged asks db to run release once it lets go of the recordings its
// reads handed out (Restaged), and reports false where db does not tell: the
// parser then keeps its data until its next call reads another generation.
func WhenRestaged(db MockMemDb, slot any, release func()) bool {
	r, ok := db.(Restaged)
	if !ok {
		return false
	}
	r.WhenRestaged(slot, release)
	return true
}
