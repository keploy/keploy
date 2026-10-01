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
}
