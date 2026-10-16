package integrations

// ExactServingReader is an optional MockMemDb capability: a store that can
// serve each test exactly its own mocks (the proven switch, design v14 §5.2,
// S12). While it does, every read shows one view, which changes as a test
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
// It is one only while db serves exactly, and only for a named mock. Without
// exact serving MarkMockAsUsed's false only ever said its bookkeeping failed,
// and the mock answers as before. MarkMockAsUsed turns a mock with no name
// away before it asks the view, so that false is bookkeeping too, and
// refusing it would have a parser match it again forever.
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
	return name != "" && ExactServing(db)
}
