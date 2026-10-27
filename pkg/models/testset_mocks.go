package models

// TestSetMocks is a test set's recorded mocks, split into the pools a replay
// loads, as read from the set's mock file in one pass.
type TestSetMocks struct {
	// Filtered is the per-test pool: what GetFilteredMocks returns for the
	// same arguments.
	Filtered []*Mock
	// Unfiltered is the session pool: what GetUnFilteredMocks returns for the
	// same arguments.
	Unfiltered []*Mock
	// AllSession is the session pool before the mapping prune: what
	// GetUnFilteredMocks returns with no mapping maps. Unfiltered holds the
	// same *Mock values, not copies, so read it and do not change it.
	AllSession []*Mock
	// AllPerTest is every per-test candidate in the file, in file order,
	// before the mapping prune and the window filter: the per-test mocks as
	// recorded, which Filtered is drawn from. It holds the decoded *Mock
	// values Filtered holds or was copied from, so read it and do not change
	// it.
	AllPerTest []*Mock
	// Skipped is the documents of the mock file the decoders skipped (a kind
	// this keploy cannot read, a connection failure it cannot replay, the
	// file's incomplete last document), by name, with their kinds. A document
	// whose name is not known is not listed. A mapping entry, or the name of a
	// mock appended to the set, can still refer to one.
	Skipped map[string]Kind
	// SkippedWindowed is the documents of Skipped whose request time could be
	// read, as test windows see them (NewWindowedMock of their name, kind,
	// metadata and times, async-egress ones not test mocks): a test owns, from
	// its window, a recorded call this keploy cannot serve, and is judged for
	// it. A store that reads gob files lists none.
	SkippedWindowed []WindowedMock
	// Payloads is the mock file the read left the mocks' payloads in
	// (Mock.LeavePayloadOnDisk), nil when it left none and every mock is
	// whole. Its mutator changes each payload read back before it is sent.
	Payloads PayloadSource
}
