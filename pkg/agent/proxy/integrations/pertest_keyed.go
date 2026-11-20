package integrations

import "go.keploy.io/server/v3/pkg/models"

// PerTestKeyRanger is an optional MockMemDb extension: the
// GetPerTestMocksInWindow read narrowed to the mocks an index files under one
// key, handed one at a time, in that read's order, until the caller stops.
// Under exact serving the open test's own unanswered mocks under the key come
// first, read past the ones that answered by their links, and the tier the
// read takes after them only then: a matcher that stops at the key's next
// recording reads neither the test's other recordings nor the mocks it
// answered, so a call costs the same, and copies nothing, whether the test
// owns ten recordings or ten thousand. A whole per-test read copies every
// one of them on each call.
//
// The open test's own mocks are filed as a SessionKeyReader files them
// (MockIndex.OwnedKeys when set, Keys otherwise), every other mock by Keys.
type PerTestKeyRanger interface {
	// RangePerTestMocksWithKey calls fn with each mock GetPerTestMocksInWindow
	// would return that ix files under key, in its order, until fn returns
	// false. fn is called without the store's locks held, so it may call
	// back into the store (a consume); the walk then sees the tier as it is
	// by the time it gets there, as successive reads would.
	RangePerTestMocksWithKey(ix *MockIndex, key string, fn func(*models.Mock) bool) error
}
