package integrations

import "go.keploy.io/server/v3/pkg/models"

// LifetimeStater is the optional part of a MockMemDb that knows whether the
// recording of the set being replayed states its mocks' lifetimes
// (models.MetadataLifetimeKey) for a kind, once its own mocks are consumed
// too. The proxy's mock manager implements it.
type LifetimeStater interface {
	StatesLifetimes(kind models.Kind) bool
}

// StatesLifetimes reports whether the recording db serves states the
// lifetimes of its kind mocks: then each test is served the per-test mocks it
// recorded only, and a call it misses may be one another test recorded. A db
// that does not implement LifetimeStater, or that has not seen one, is asked
// by what it holds for the test: a mock of kind, its own or reusable, that
// states its lifetime.
func StatesLifetimes(db MockMemDb, kind models.Kind) bool {
	if db == nil {
		return false
	}
	if s, ok := db.(LifetimeStater); ok && s.StatesLifetimes(kind) {
		return true
	}
	stated := func(ms []*models.Mock) bool {
		for _, m := range ms {
			if m != nil && m.Kind == kind && m.StatesLifetime() {
				return true
			}
		}
		return false
	}
	if per, err := db.GetPerTestMocksInWindow(); err == nil && stated(per) {
		return true
	}
	//nolint:staticcheck // SA1019: the startup∪session union is the reusable pool a test is served.
	if sess, err := db.GetSessionMocks(); err == nil && stated(sess) {
		return true
	}
	return false
}

// SpentReader is the optional part of a MockMemDb that hides the per-test
// recordings of stateful dependencies a test kept for its repeats (cursor
// mocks) once that test is over: no later test, and no call between tests,
// is served them, as when a match consumes a per-test mock, while a later run
// of the same test is.
type SpentReader interface {
	Spent(m *models.Mock) bool
}

// WithoutSpent returns mocks less what db reports spent (SpentReader); mocks
// itself when db reports nothing.
func WithoutSpent(db MockMemDb, mocks []*models.Mock) []*models.Mock {
	sr, ok := db.(SpentReader)
	if !ok {
		return mocks
	}
	out := mocks
	for i, m := range mocks {
		if m == nil || !sr.Spent(m) {
			if len(out) != len(mocks) {
				out = append(out, m)
			}
			continue
		}
		if len(out) == len(mocks) {
			out = append(make([]*models.Mock, 0, len(mocks)), mocks[:i]...)
		}
	}
	return out
}

// TestOwner is the optional part of a MockMemDb that tells whether a per-test
// mock it serves for the open test (GetPerTestMocksInWindow) is that test's
// own: a test is open to the call, and the mock is its recording (its
// mapping names it or, for a store that serves a test by window, the mock is
// in the window's pool). A mock every test may see because it belongs to no
// test (one mapped to none), and every mock while no test is open (before
// the first test, between tests, or a replay that opens none), is no test's
// own: a test's rules for its own recordings (consumed once served, or kept
// for its repeats) do not apply to it, and it is served as a reusable one.
type TestOwner interface {
	OwnedByOpenTest(m *models.Mock) bool
}

// OwnedByOpenTest reports whether m, a per-test mock db serves for the open
// test, is that test's own (TestOwner). A db that does not tell owns every
// mock it serves for the test, as before it could.
func OwnedByOpenTest(db MockMemDb, m *models.Mock) bool {
	if o, ok := db.(TestOwner); ok {
		return o.OwnedByOpenTest(m)
	}
	return true
}

// RepeatKeeper is the optional part of a MockMemDb that remembers the per-test
// recordings a test's exact match kept for its byte-identical repeats (a
// stateful dependency's cursor), until the store stages the next test.
// KeptForRepeats returns whether a recording, by name, is kept: nil when
// none is, so a caller with nothing to drop does no lookups.
type RepeatKeeper interface {
	KeepForRepeats(name string)
	KeptForRepeats() func(name string) bool
}

// KeepForRepeats records, when db keeps them, that the named recording was
// kept for its test's byte-identical repeats.
func KeepForRepeats(db MockMemDb, name string) {
	if k, ok := db.(RepeatKeeper); ok {
		k.KeepForRepeats(name)
	}
}

// WithoutKept returns mocks less the recordings db kept for a test's
// byte-identical repeats (RepeatKeeper); mocks itself when there are none.
func WithoutKept(db MockMemDb, mocks []*models.Mock) []*models.Mock {
	k, ok := db.(RepeatKeeper)
	if !ok {
		return mocks
	}
	kept := k.KeptForRepeats()
	if kept == nil {
		return mocks
	}
	out := mocks[:0:0]
	dropped := false
	for _, m := range mocks {
		if m != nil && kept(m.Name) {
			dropped = true
			continue
		}
		out = append(out, m)
	}
	if !dropped {
		return mocks
	}
	return out
}
