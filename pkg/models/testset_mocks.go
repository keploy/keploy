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

// MockSkip is why a read of a set's mock file serves no mock from a document
// the file has: the read skips it and goes on, and a mapping that lists its
// name lists a mock the set does not have.
type MockSkip string

const (
	// MockSkipCutShort is the file's last document when the file ends
	// part-way through its last line (no final newline), as a write cut
	// short leaves it. A YAML one is never served, whether or not what is
	// there parses. An NDJSON line is cut short only when its JSON does not
	// parse, or its spec fails to decode with an error; otherwise it is read
	// as any other line, served or skipped for one of the other reasons.
	MockSkipCutShort MockSkip = "cut_short"
	// MockSkipNoKind is a document with no kind, or an empty one.
	MockSkipNoKind MockSkip = "no_kind"
	// MockSkipUnknownKind is a document of a kind this keploy has no decoder
	// for: recorded by a newer keploy, read only by keploy enterprise, in a
	// format this keploy no longer reads, or mistyped by hand.
	MockSkipUnknownKind MockSkip = "unknown_kind"
	// MockSkipYAMLOnlyKind is an NDJSON document of a kind keploy reads only
	// from a YAML mock file: a kind with a "-" in it (only keploy enterprise
	// reads one, from YAML), or one an out-of-tree mapper decodes (keploy
	// enterprise's). No keploy reads it from NDJSON.
	MockSkipYAMLOnlyKind MockSkip = "yaml_only_kind"
	// MockSkipConnFailure is a connection failure this keploy cannot replay.
	MockSkipConnFailure MockSkip = "connection_failure"
)

// SkippedMock is a named document of a set's mock file that the set's read
// skips, with its kind and why.
type SkippedMock struct {
	Name string
	Kind Kind
	Why  MockSkip
	// Cause is what the read found wrong with it, when it knows more than
	// Why says: of an NDJSON last line cut short whose JSON is whole, why
	// its spec does not decode. Empty otherwise.
	Cause string
}
