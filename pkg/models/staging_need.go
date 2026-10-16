package models

import "fmt"

// A test set's staging call (the replayer's call before the app starts, with
// the window BaseTime to now) reads from the agent's on-disk store only the
// per-test mocks recorded before the set's first test: the app's startup
// traffic. Reading the whole window brought every mock of the recording into
// the agent's memory only to stage it as startup traffic, and a long
// recording OOM-killed the agent (#4819).
//
// A parser that needs more of the recording at staging declares it with
// RegisterStagingNeed, by kind, by what the staging does with the mocks it
// declares (StagingUse) and by which mocks they are. The agent's on-disk
// store indexes the declared mocks as they are stored, so the staging call
// reads only them beside the startup band. What a staging reads is then the
// startup band and the declared mocks in its window: #4819's bound holds for
// every kind that declares nothing, and a kind that declares pays for its
// declared mocks, which grow with its recording.

// StagingUse is what a test set's staging does with the per-test mocks a
// StagingNeed declares.
type StagingUse uint8

const (
	// StagingWithStartup stages the declared mocks with the set's startup
	// traffic, whatever their recorded time within the staging's window.
	//
	// It is for a parser that files such mocks into state of its own when
	// the set is staged, and serves each from there at its own time: a
	// server push (a broker's pushed message) that the parser releases when
	// its recorded test window comes, a push recorded in the gap between
	// two tests included, which no test's own window holds. The mock
	// manager files what is staged as startup traffic, and the set's first
	// test refiles that tier to what was recorded before it, as for every
	// staged mock: a parser that needs a declared mock later keeps it.
	//
	// A carry-over mock (RegisterCarryOver) is never staged so: carry-over
	// keeps it reachable from its own release window, and staging it too
	// would leave two copies reachable.
	//
	// The need's Match is required.
	StagingWithStartup StagingUse = iota + 1
)

// StagingNeed is one declaration of what a parser needs of a test set's
// recording at staging: the per-test mocks of its kind that Match accepts,
// used as Use says.
type StagingNeed struct {
	Use StagingUse
	// Match looks at the mock alone (its Kind and Spec.Metadata), is called
	// on every per-test mock of the kind as the set's mocks are stored, and
	// must be cheap. What a use needs of it is the use's to say.
	Match func(*Mock) bool
}

// RegisterStagingNeed declares a staging need for kind. Call it from an init
// function. Several needs of one use for one kind are OR-ed. Kinds nobody
// registers are unaffected. A need its use cannot take (an unknown use, or
// StagingWithStartup with no Match) is a programming error, and panics.
//
// The returned function removes this registration; production code ignores
// it, tests use it to leave the registry as they found it.
func RegisterStagingNeed(kind Kind, need StagingNeed) (unregister func()) {
	switch need.Use {
	case StagingWithStartup:
		if need.Match == nil {
			panic(fmt.Sprintf("models.RegisterStagingNeed(%s): StagingWithStartup needs a Match", kind))
		}
		return stagedHooks.add(kind, need.Match)
	}
	panic(fmt.Sprintf("models.RegisterStagingNeed(%s): unknown use %d", kind, need.Use))
}

var stagedHooks mockPredicates

// IsStagedAtStaging reports whether m is a per-test mock that a
// StagingWithStartup need accepts: the set's staging stages it, whatever its
// recorded time. A carry-over mock never is (StagingWithStartup). The mock's
// lifetime must already be derived.
func IsStagedAtStaging(m *Mock) bool {
	return m != nil && m.TestModeInfo.Lifetime == LifetimePerTest && stagedHooks.match(m) && !IsCarryOver(m)
}

// StagedAtStagingRegistered reports whether any kind declared a
// StagingWithStartup need. When none did, a staging reads only its startup
// band, and the declared-mock load is skipped.
func StagedAtStagingRegistered() bool {
	return stagedHooks.any()
}
