package integrations

import "go.keploy.io/server/v3/pkg/models"

// RecordedSetRanger is an optional MockMemDb capability: the staged test
// set's recordings of a kind as recorded, whatever any read shows of them. A
// read shows a test's window, what was not consumed yet, and the tiers it
// reads, so a parser that decides what a recording is from the recordings
// made around it on its connection (a Redis MULTI ... EXEC whose commands two
// tests' windows split) reads that from the whole set, and answers each call
// from its read as before.
//
// RangeRecordedSet calls visit with each recording of kind the staged set
// holds, once each by name, in no set order, until visit returns false: its
// name, and load, which returns it (decoding it only then, for one the store
// keeps on disk). whole is false, and nothing is visited, when the store
// cannot tell the whole set (it holds only what its stagings were handed,
// which may be one test's window): the parser then decides from its reads,
// as with a store that does not implement it.
type RecordedSetRanger interface {
	RangeRecordedSet(kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) (whole bool, err error)
}

// RangeRecordedSet is db's RangeRecordedSet (RecordedSetRanger); whole is
// false for a store that does not implement it.
func RangeRecordedSet(db MockMemDb, kind models.Kind, visit func(name string, load func() (*models.Mock, error)) bool) (whole bool, err error) {
	if r, ok := db.(RecordedSetRanger); ok {
		return r.RangeRecordedSet(kind, visit)
	}
	return false, nil
}
