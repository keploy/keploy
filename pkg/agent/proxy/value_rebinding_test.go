package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http" // also registers the HTTP value tokenizer
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const (
	vrRec  = "0f8fad5b-d9cb-469f-a165-70867728950e"
	vrLive = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

func vrMock(name, url, body string, at time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq:          &models.HTTPReq{Method: "POST", URL: url, Body: body},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: body},
			ReqTimestampMock: at,
		},
	}
}

func newValueManager(t *testing.T) *MockManager {
	t.Helper()
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mgr.Close)
	return mgr
}

// newBoundManager is a manager with a set staged that can be rebound, so it
// keeps bindings.
func newBoundManager(t *testing.T) *MockManager {
	t.Helper()
	mgr := newValueManager(t)
	mgr.SetRebinding(true, nil)
	seed := vrMock("seed", "http://inv/seed", `{"id":"5c0d3f2a-7b1e-4a9d-8c6f-0e1d2c3b4a59"}`, time.Now().Add(-time.Hour))
	mgr.SetMocksWithWindow([]*models.Mock{seed}, nil, models.BaseTime, time.Now())
	require.NotNil(t, mgr.Bindings())
	return mgr
}

// The whole set's values are indexed when it is staged, so the first carrier
// of a value is a fact about the recording: it stays known after that mock is
// consumed, and a later, mapping-restricted staging of the same set does not
// replace it.
func TestValueIndexKnowsTheFirstCarrierAfterConsumption(t *testing.T) {
	mgr := newValueManager(t)
	mgr.SetRebinding(true, nil)
	base := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`"}`, base)
	read := vrMock("read", "http://inv/items/"+vrRec, "", base.Add(time.Second))
	mgr.SetMocksWithWindow([]*models.Mock{create, read}, nil, models.BaseTime, time.Now())

	require.Equal(t, []string{vrRec}, mgr.FirstCarried(create))
	require.Empty(t, mgr.FirstCarried(read), "a read-back is not the first carrier")
	require.Equal(t, []string{vrRec}, mgr.RequestValues(read))

	mgr.DeleteFilteredMock(*create)                                                // consumed
	mgr.SetMocksWithWindow([]*models.Mock{read}, nil, models.BaseTime, time.Now()) // a mid-set subset
	require.Empty(t, mgr.FirstCarried(read), "the first carrier survives consumption and a subset staging")

	mgr.ResetForReplaySession()
	mgr.SetMocksWithWindow([]*models.Mock{read}, nil, models.BaseTime, time.Now()) // the next set
	require.Equal(t, []string{vrRec}, mgr.FirstCarried(read), "a new set is indexed afresh")
}

// Bindings live in an epoch: a reset starts a new, empty one, and what a match
// of the old one commits afterwards is dropped — and reported as dropped, so
// that match answers as recorded. Bound pairs also feed the ids the replay CLI
// compares the app's own answers with.
func TestValueBindingsEpochAndIDs(t *testing.T) {
	mgr := newBoundManager(t)
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)

	old := mgr.Bindings()
	require.False(t, old.Active())
	require.True(t, old.Commit("create", map[string]string{vrRec: vrLive}))
	b := mgr.Bindings()
	require.True(t, b.Active())
	require.True(t, b.Claimed("create"))
	live, ok := b.Live(vrRec)
	require.True(t, ok)
	require.Equal(t, vrLive, live)
	rec, _ := b.Recorded(vrLive)
	require.Equal(t, vrRec, rec)
	require.Equal(t, map[string]string{vrRec: vrLive}, ids.Default.Pairs(), "a binding is an id the run made in place of a recorded one")

	mgr.ResetValueBindings()
	require.False(t, mgr.Bindings().Active(), "a reset empties the table")
	require.Empty(t, ids.Default.Pairs(), "and the ids the CLI reads with it")
	require.False(t, old.Commit("create", map[string]string{vrRec: vrLive}), "what a match of an ended epoch commits is refused")
	require.False(t, mgr.Bindings().Active(), "and dropped")
	require.False(t, mgr.Bindings().Claimed("create"))
	require.Empty(t, ids.Default.Pairs())
}

// The replay has ONE binding table, whichever test worker a call comes from:
// a scoped worker's view reads and writes the manager's own. Two parallel
// workers that each mint an id for one shared recording are therefore not
// both followed — the first binds it, the second is refused and replays as it
// does without rebinding.
//
// A table per worker would follow both, and the replay CLI, which compares
// the app's answers, could not: it is told one id per recorded id.
func TestScopedMockDb_ValueBindingsAreTheReplaysOne(t *testing.T) {
	mgr := newBoundManager(t)
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)
	const other = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	view := func(pid uint32) *integrations.Bindings {
		return integrations.MockMemDb(&scopedMockDb{MockMemDb: mgr, pid: pid, view: "w"}).(integrations.ValueBindings).Bindings()
	}
	require.True(t, view(101).Commit("create", map[string]string{vrRec: vrLive}))
	require.False(t, view(202).Commit("create", map[string]string{vrRec: other}), "a second worker's id for the same recording is refused")

	for _, b := range []*integrations.Bindings{view(101), view(202), mgr.Bindings()} {
		v, _ := b.Live(vrRec)
		require.Equal(t, vrLive, v, "every caller reads the one table")
		require.True(t, b.Claimed("create"))
		_, bound := b.Recorded(other)
		require.False(t, bound)
	}
	require.Equal(t, map[string]string{vrRec: vrLive}, ids.Default.Pairs(), "the CLI is told the one pair, and keeps being told it")

	// A store that keeps no bindings gives a scoped view none.
	require.Nil(t, integrations.MockMemDb(&scopedMockDb{MockMemDb: newValueManager(t)}).(integrations.ValueBindings).Bindings())
}

// A scoped worker's view answers from the store's value index: the index
// describes the whole recording, whichever worker asks. A view that answered
// with nothing would leave every call of a parallel replay unfollowed.
func TestScopedMockDb_ReadsTheStoresValueIndex(t *testing.T) {
	mgr := newValueManager(t)
	mgr.SetRebinding(true, nil)
	at := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	read := vrMock("read", "http://inv/items/"+vrRec, "", at.Add(time.Second))
	mgr.SetMocksWithWindow([]*models.Mock{create, read}, nil, models.BaseTime, time.Now())

	scoped := integrations.MockMemDb(&scopedMockDb{MockMemDb: mgr, pid: 101, view: "w"}).(integrations.ValueIndex)
	require.Equal(t, []string{vrRec}, scoped.FirstCarried(create))
	require.Empty(t, scoped.FirstCarried(read))
	require.Equal(t, []string{vrRec}, scoped.RequestValues(read))
	require.True(t, scoped.Carries(vrRec))
	require.False(t, scoped.Carries(vrLive))

	// Over a store with no index, a view knows nothing.
	bare := integrations.MockMemDb(&scopedMockDb{MockMemDb: newValueManager(t)}).(integrations.ValueIndex)
	require.Empty(t, bare.FirstCarried(create))
	require.Empty(t, bare.RequestValues(read))
	require.False(t, bare.Carries(vrRec))
}

// Mocks recorded in the same instant order as the recorder numbered them
// (mock-9 before mock-10), and a value an untimed mock carries cannot be
// ordered, so it has no first carrier and is never bound.
func TestValueIndexOrdering(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	nine := vrMock("mock-9", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	ten := vrMock("mock-10", "http://inv/items/"+vrRec, "", at)
	ix, _ := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{ten, nine})
	require.Equal(t, []string{vrRec}, ix.FirstCarried(nine), "mock-9 was recorded before mock-10")
	require.Empty(t, ix.FirstCarried(ten))

	untimed := vrMock("mock-0", "http://inv/x", `{"id":"`+vrLive+`"}`, time.Time{})
	timed := vrMock("mock-1", "http://inv/y", `{"id":"`+vrLive+`"}`, at)
	ix, _ = integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{untimed, timed})
	require.Empty(t, ix.FirstCarried(timed), "an untimed carrier makes the value unorderable")
	require.Empty(t, ix.FirstCarried(untimed))
}

// Only a value first sent where a match can line it up with a live request (a
// URL segment, a query value, a whole JSON string) can be bound: one first
// sent in a per-request header (a trace id, a request signature) is carried,
// so a copy can still swap it, but nobody first-carries it; nor a value a
// dependency handed the app before any request sent it.
func TestValueIndexFirstCarriesOnlyBindableValues(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	const trace = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	traced := vrMock("traced", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	traced.Spec.HTTPReq.Header = map[string]string{"Traceparent": trace}
	echoTrace := vrMock("echo", "http://inv/echo", `{"trace":"`+trace+`"}`, at.Add(time.Second))

	given := vrMock("given", "http://inv/new", "", at.Add(2*time.Second))
	given.Spec.HTTPResp.Body = `{"id":"` + vrLive + `"}`
	uses := vrMock("uses", "http://inv/use", `{"id":"`+vrLive+`"}`, at.Add(3*time.Second))

	// A dependency that names what it made in a header only (201, Location).
	const located = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	makes := vrMock("makes", "http://inv/make", "", at.Add(4*time.Second))
	makes.Spec.HTTPResp.Body = ""
	makes.Spec.HTTPResp.Header = map[string]string{"Location": "/things/" + located}
	fetches := vrMock("fetches", "http://inv/things/"+located, "", at.Add(5*time.Second))

	ix, _ := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{traced, echoTrace, given, uses, makes, fetches})
	require.Equal(t, []string{vrRec}, ix.FirstCarried(traced), "the header's trace id is not bindable")
	require.Contains(t, ix.RequestValues(traced), trace, "but it is carried")
	require.Empty(t, ix.FirstCarried(echoTrace), "a value first sent in a header is first carried there, not here")
	require.Empty(t, ix.FirstCarried(uses), "a value a dependency handed the app is not the app's to bind")
	require.Empty(t, ix.FirstCarried(fetches), "nor one it handed over in a response header")
}

// A recorded id is bound once, and the pair the CLI reads for it never goes
// away within a test set. A second live id for it (a retried create) is
// refused: it stands for nothing, the first pair stays, and the creator's
// claim with it. (Published as a second pair, it would make the CLI's map
// drop the recorded id altogether, as it does any id it sees paired two ways.)
func TestValueBindingsAreMadeOnce(t *testing.T) {
	mgr := newBoundManager(t)
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)
	const retry = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	require.True(t, mgr.Bindings().Commit("create", map[string]string{vrRec: vrLive}))
	require.False(t, mgr.Bindings().Commit("retry", map[string]string{vrRec: retry}))
	_, ok := mgr.Bindings().Recorded(retry)
	require.False(t, ok, "the retry's id stands for nothing")
	require.False(t, mgr.Bindings().Claimed("retry"), "a refused commit claims nothing")
	live, _ := mgr.Bindings().Live(vrRec)
	require.Equal(t, vrLive, live)
	require.Equal(t, map[string]string{vrRec: vrLive}, ids.Default.Pairs())
	require.True(t, mgr.Bindings().Commit("create", map[string]string{vrRec: vrLive}), "the same pair again is no conflict")
	require.Equal(t, map[string]string{vrRec: vrLive}, ids.Default.Pairs())
}

// A new replay session (the next keploy mock replay on a long-lived agent)
// starts a new, empty binding epoch.
func TestReplaySessionStartsANewBindingEpoch(t *testing.T) {
	mgr := newBoundManager(t)
	before := mgr.Bindings()
	before.Commit("create", map[string]string{vrRec: vrLive})
	mgr.ResetForReplaySession()
	require.False(t, mgr.Bindings().Active())
	before.Commit("create", map[string]string{vrRec: vrLive})
	require.False(t, mgr.Bindings().Active(), "the session before cannot bind into this one")
}

// Staging a set starts a new binding epoch too, not only the session reset
// before it: the previous set's index is still in place between the two, and
// a match of that set which commits in that gap must not leave its id in the
// table the next set starts with.
func TestStagingASetStartsANewBindingEpoch(t *testing.T) {
	mgr := newBoundManager(t)
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)
	mgr.ResetForReplaySession()
	late := mgr.Bindings() // a match of the old set, begun after the reset
	require.NotNil(t, late, "the old set's index is still staged")
	require.True(t, late.Commit("create", map[string]string{vrRec: vrLive}))

	next := vrMock("next", "http://inv/next", `{"id":"9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"}`, time.Now().Add(-time.Hour))
	mgr.SetMocksWithWindow([]*models.Mock{next}, nil, models.BaseTime, time.Now())
	require.False(t, mgr.Bindings().Active(), "the next set starts with an empty table")
	require.False(t, mgr.Bindings().Claimed("create"))
	require.Empty(t, ids.Default.Pairs())
	require.False(t, late.Commit("create", map[string]string{vrRec: vrLive}), "and the old set's match can no longer bind into it")
	require.False(t, mgr.Bindings().Active())
}

// A set is followed whole or not at all. With a mock of a kind that cannot be
// followed in it — an id is rarely sent to one dependency only — it has no
// value index, whatever the mock is named, and the manager keeps no bindings;
// a DNS mock, which carries no id, does not count. The builder says why, for a
// person: which mock, of which kind.
func TestValueIndexIsForSetsThatCanBeFollowedWhole(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	sql := &models.Mock{Name: "insert", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: at.Add(time.Millisecond)}}
	nameless := &models.Mock{Kind: models.Mongo}
	dns := &models.Mock{Name: "lookup", Kind: models.DNS, Spec: models.MockSpec{ReqTimestampMock: at}}

	ix, why := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{create, sql})
	require.Nil(t, ix)
	require.Contains(t, why, `"insert"`)
	require.Contains(t, why, string(models.Postgres))
	ix, why = integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{create, nameless})
	require.Nil(t, ix)
	require.Contains(t, why, string(models.Mongo))
	ix, why = integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, []*models.Mock{create, dns})
	require.Empty(t, why)
	require.Equal(t, []string{vrRec}, ix.FirstCarried(create))

	mgr := newValueManager(t)
	mgr.SetRebinding(true, nil)
	require.Nil(t, mgr.Bindings(), "nothing staged")
	mgr.SetMocksWithWindow([]*models.Mock{create, sql}, nil, models.BaseTime, time.Now())
	require.Nil(t, mgr.Bindings(), "a set with a Postgres mock keeps no bindings")
	mgr.ResetForReplaySession()
	mgr.SetMocksWithWindow([]*models.Mock{create, dns}, nil, models.BaseTime, time.Now())
	require.NotNil(t, mgr.Bindings())
}

// A set in which nothing can be followed gets no index, and no reason: there
// is nothing a replay would have followed. No request carries an id the
// replay may bind where it can be lined up — none at all, one of another
// shape than the replay follows, or one only in a header.
func TestValueIndexIsNilWhenNothingCanBeFollowed(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	inHeader := vrMock("in-header", "http://inv/ping", `{"ok":true}`, at)
	inHeader.Spec.HTTPReq.Header = map[string]string{"X-Request-Id": vrRec}
	for name, mocks := range map[string][]*models.Mock{
		"no id at all":                   {vrMock("plain", "http://inv/ping", `{"ok":true}`, at)},
		"a digest, not a generated UUID": {vrMock("put", "http://inv/blobs", `{"sum":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`, at)},
		"an id only in a header":         {inHeader},
		"no mocks":                       nil,
	} {
		ix, why := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, mocks)
		require.Nil(t, ix, name)
		require.Empty(t, why, name)
	}
}

// Why a set is not followed is a debug line, whatever the replay asked for —
// the ids it named (keploy test) or whatever the app mints (keploy mock
// replay). A run that follows nothing is the run keploy always made, and a
// healthy one reads the same with rebinding as without: nothing is logged at
// Info or above — not that a set with a database mock was not followed, nor
// which of the named ids had no creator.
func TestValueIndexSaysWhyASetIsNotFollowedAtDebugOnly(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	sql := &models.Mock{Name: "insert", Kind: models.Postgres, Spec: models.MockSpec{ReqTimestampMock: at.Add(time.Millisecond)}}
	opaque := vrMock("opaque", "http://inv/new", "", at.Add(time.Second))
	opaque.Spec.HTTPResp.Header = map[string]string{"Content-Encoding": "deflate"}
	stage := func(minted bool, values []string, mocks ...*models.Mock) *observer.ObservedLogs {
		core, logs := observer.New(zap.DebugLevel)
		mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
		t.Cleanup(mgr.Close)
		mgr.SetRebinding(minted, values)
		mgr.SetMocksWithWindow(mocks, nil, models.BaseTime, time.Now())
		return logs
	}
	const said = "are not followed in this test set"
	for name, c := range map[string]struct {
		minted bool
		values []string
		mocks  []*models.Mock
		why    string // a part of the reason; "" when the set is followed, or has nothing to follow
	}{
		"named ids, a database mock":            {false, []string{vrRec}, []*models.Mock{create, sql}, `mock "insert" is of kind Postgres`},
		"minted ids, a database mock":           {true, nil, []*models.Mock{create, sql}, `mock "insert" is of kind Postgres`},
		"named ids, a response it cannot read":  {false, []string{vrRec}, []*models.Mock{create, opaque}, `the recorded response of mock "opaque"`},
		"minted ids, a response it cannot read": {true, nil, []*models.Mock{create, opaque}, `the recorded response of mock "opaque"`},
		"named ids, a set that is followed":     {false, []string{vrRec}, []*models.Mock{create}, ""},
		"a named id no request introduces":      {false, []string{vrRec, vrLive}, []*models.Mock{create}, ""},
		"named ids, a set that carries none":    {false, []string{vrRec}, []*models.Mock{vrMock("plain", "http://inv/ping", `{"ok":true}`, at)}, ""},
		"nothing asked for, a database mock":    {false, nil, []*models.Mock{create, sql}, ""},
	} {
		logs := stage(c.minted, c.values, c.mocks...)
		about := logs.FilterMessageSnippet(said)
		require.Zero(t, logs.Filter(func(e observer.LoggedEntry) bool {
			return e.Level >= zapcore.InfoLevel && (strings.Contains(e.Message, " id") || strings.Contains(e.Message, "followed"))
		}).Len(), "%s: nothing about ids at Info or above: %v", name, logs.All())
		if c.why == "" {
			require.Zero(t, about.Len(), "%s: %v", name, about.All())
			continue
		}
		require.Equal(t, 1, about.Len(), name)
		require.Equal(t, zapcore.DebugLevel, about.All()[0].Level, name)
		require.Contains(t, about.All()[0].ContextMap()["why"], c.why, name)
	}

	// A manager without a logger still stages.
	bare := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), nil)
	t.Cleanup(bare.Close)
	bare.SetRebinding(false, []string{vrRec})
	bare.SetMocksWithWindow([]*models.Mock{create, sql}, nil, models.BaseTime, time.Now())
	require.Nil(t, bare.Bindings())
}

// Only a generated UUID (mocknoise.IsMintedUUID) is ever taken for an id the
// app mints, whether the replay follows what the app mints or names values: a
// model name or a digest that a replay names is still not followed.
func TestValueIndexTakesOnlyAGeneratedUUIDForAnID(t *testing.T) {
	const model, digest = "gpt-4o-mini-2024-07-18", "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	at := time.Now().Add(-time.Hour)
	call := vrMock("call", "http://dep/call", `{"id":"`+vrRec+`","model":"`+model+`","sum":"`+digest+`"}`, at)
	stage := func(minted bool, values []string) *MockManager {
		mgr := newValueManager(t)
		mgr.SetRebinding(minted, values)
		mgr.SetMocksWithWindow([]*models.Mock{call}, nil, models.BaseTime, time.Now())
		return mgr
	}
	require.Equal(t, []string{vrRec}, stage(true, nil).FirstCarried(call))
	require.Equal(t, []string{vrRec}, stage(false, []string{vrRec, model, digest}).FirstCarried(call))
	require.Empty(t, stage(false, []string{model, digest}).FirstCarried(call))
}

// Rebinding is something a replay asks for: a manager that was asked for none
// indexes nothing and keeps no bindings; asked for some recorded values, it
// follows only those, though every id of the set has a creator.
func TestValueIndexFollowsWhatTheReplayAskedFor(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`","hash":"`+vrLive+`"}`, at)
	stage := func(minted bool, values []string) *MockManager {
		mgr := newValueManager(t)
		mgr.SetRebinding(minted, values)
		mgr.SetMocksWithWindow([]*models.Mock{create}, nil, models.BaseTime, time.Now())
		return mgr
	}
	require.Nil(t, stage(false, nil).Bindings(), "asked for nothing: off")
	require.Nil(t, newValueManager(t).Bindings(), "never asked: off")
	minted := stage(true, nil)
	require.ElementsMatch(t, []string{vrRec, vrLive}, minted.FirstCarried(create), "every id has a creator")
	require.True(t, minted.MayBind(vrRec) && minted.MayBind(vrLive), "every minted value is followed")
	only := stage(false, []string{vrRec})
	require.ElementsMatch(t, []string{vrRec, vrLive}, only.FirstCarried(create), "every id has a creator, whatever is followed")
	require.True(t, only.MayBind(vrRec), "the value named is followed")
	require.False(t, only.MayBind(vrLive), "and only that one")
}

// A replay that named the ids to follow follows them only while a test case
// is being replayed: a named id is one a test case's answer produces, so a
// call made with no test window open (the app starting up, keploy's readiness
// probe reaching a handler) is not the one that introduces it, and must not
// take the one binding the id gets. A replay that follows whatever the app
// mints has no such window to go by.
func TestNamedIDsAreFollowedOnlyWhileATestCaseIsReplayed(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	create := vrMock("create", "http://inv/items", `{"id":"`+vrRec+`"}`, at)
	stage := func(minted bool, values []string) *MockManager {
		mgr := newValueManager(t)
		mgr.SetRebinding(minted, values)
		mgr.SetMocksWithWindow([]*models.Mock{create}, nil, models.BaseTime, time.Now())
		return mgr
	}
	named := stage(false, []string{vrRec})
	require.Nil(t, named.Bindings(), "no test case is being replayed")
	named.SetCurrentTestWindow(at.Add(-time.Minute), at.Add(time.Minute))
	require.NotNil(t, named.Bindings(), "a test case is")
	named.SetCurrentTestWindow(time.Time{}, time.Time{})
	require.Nil(t, named.Bindings(), "between two test cases")

	require.NotNil(t, stage(true, nil).Bindings(), "what the app mints is followed with or without a window")
}

// orderSet is a recording in which a DEPENDENCY mints an id: POST /orders is
// answered with the new order's id, in a response big enough for the agent's
// disk store to keep apart from its mock, and the app then reads the order
// back by that id. The app itself mints nothing there; it does mint the id of
// the note it posts last.
func orderSet(at time.Time) (place, read, note *models.Mock) {
	place = spilledHTTPMock("place", "POST", "/orders", at)
	place.Spec.HTTPResp.Body = `{"id":"` + vrRec + `","pad":"` + strings.Repeat("x", spilledBodyBytes) + `"}`
	read = spilledHTTPMock("read", "GET", "/orders/"+vrRec, at.Add(time.Second))
	read.Spec.HTTPResp.Body = `{"id":"` + vrRec + `","status":"paid"}`
	note = spilledHTTPMock("note", "PUT", "/notes/"+vrLive, at.Add(2*time.Second))
	note.Spec.HTTPResp.Body = `{"ok":true}`
	return place, read, note
}

// throughDisk passes mocks through the agent's disk store and returns the
// whole set as the store hands it back for the set's first staging: a big
// response is kept apart from its mock.
func throughDisk(t *testing.T, mocks ...*models.Mock) (*DiskMocks, []*models.Mock) {
	t.Helper()
	disk, err := NewDiskMocks(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = disk.Close() })
	for _, m := range mocks {
		require.NoError(t, disk.Add(m))
	}
	disk.Finalize()
	all, err := disk.LoadWindow(models.BaseTime, time.Now())
	require.NoError(t, err)
	require.Len(t, all, len(mocks))
	return disk, all
}

// Who carried an id first does not depend on where a response lives. A
// response the disk store keeps apart from its mock is loaded to be read when
// the set is indexed — onto a copy: the mock a replay serves stays as the
// store handed it over. Unread, the response that handed the app an id was
// invisible, and the request that sends the id back passed for its creator.
func TestValueIndexReadsAResponseTheDiskStoreKeptApart(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	index := func(mocks []*models.Mock) *MockManager {
		mgr := newValueManager(t)
		mgr.SetRebinding(true, nil)
		mgr.SetMocksWithWindow(mocks, nil, models.BaseTime, time.Now())
		require.NotNil(t, mgr.Bindings(), "the set is followed")
		return mgr
	}
	place, read, note := orderSet(at)
	resident := index([]*models.Mock{place, read, note})

	place, read, note = orderSet(at)
	_, staged := throughDisk(t, place, read, note)
	require.True(t, staged[0].HasSpilledResponse(), "precondition: the store must keep the response of %s apart", staged[0].Name)
	spilled := index(staged)

	for _, m := range staged {
		require.Equal(t, resident.FirstCarried(m), spilled.FirstCarried(m), "first carrier of the values of %s", m.Name)
	}
	require.Empty(t, spilled.FirstCarried(staged[1]), "the id the dependency handed over is not the read-back's to bind")
	require.Equal(t, []string{vrLive}, spilled.FirstCarried(staged[2]), "the id the app minted is its creator's")

	pooled, err := spilled.GetStartupMocks()
	require.NoError(t, err)
	for _, m := range append(pooled, staged[0]) {
		if m.Name == "place" {
			require.True(t, m.HasSpilledResponse(), "indexing loaded the response into the mock it serves")
			require.Nil(t, m.Spec.HTTPResp)
		}
	}
}

// The same through the HTTP integration: an app that asks its dependency for
// an order nobody made is answered as recorded, and no pair reaches the CLI.
// With the creating response unread, the wrong id was bound to the recorded
// order's and the answer rewritten to name it.
func TestAnIDBehindAResponseKeptApartIsNotTheAppsToBind(t *testing.T) {
	const nobody = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	disk, all := throughDisk(t, func() []*models.Mock { p, r, n := orderSet(at); return []*models.Mock{p, r, n} }()...)

	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetRebinding(true, nil)
	mgr.SetMocksWithWindow(all, nil, models.BaseTime, time.Now()) // the whole set, before any test
	window, err := disk.LoadWindow(at.Add(-time.Second), at.Add(time.Minute))
	require.NoError(t, err)
	mgr.SetMocksWithWindow(window, nil, at.Add(-time.Second), at.Add(time.Minute))
	require.NotNil(t, mgr.Bindings(), "the set is followed")

	h := httpint.New(zap.NewNop())
	resp, body, err := serveOne(t, h, mgr, "POST /orders HTTP/1.1\r\nHost: upstream\r\nContent-Length: 0\r\n\r\n")
	require.NotNil(t, resp, "no response for the create: %v", err)
	require.Contains(t, string(body), vrRec)
	resp, body, err = serveOne(t, h, mgr, "GET /orders/"+nobody+" HTTP/1.1\r\nHost: upstream\r\n\r\n")
	require.NotNil(t, resp, "no response for the read-back: %v", err)
	require.Equal(t, `{"id":"`+vrRec+`","status":"paid"}`, string(body), "a call for an order nobody made is answered as recorded")
	require.Empty(t, ids.Default.Pairs())
	require.False(t, mgr.Bindings().Active())
}

// A response that cannot be loaded is an unknown first carrier of every id,
// so the set is not followed, and the reason names the mock. Skipped, it would
// leave the requests that send its ids back looking like their creators.
func TestValueIndexFailsClosedOnAResponseItCannotLoad(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	place, read, note := orderSet(at)
	disk, staged := throughDisk(t, place, read, note)
	require.NoError(t, disk.Close()) // the store is gone before the set is indexed

	ix, why := integrations.BuildMockValueIndex(mocknoise.IsMintedUUID, staged)
	require.Nil(t, ix)
	require.Contains(t, why, `the recorded response of mock "place" could not be loaded`)

	core, logs := observer.New(zap.DebugLevel)
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.New(core))
	t.Cleanup(mgr.Close)
	mgr.SetRebinding(true, nil)
	mgr.SetMocksWithWindow(staged, nil, models.BaseTime, time.Now())
	require.Nil(t, mgr.Bindings(), "a set with a response that cannot be loaded keeps no bindings")
	require.Empty(t, mgr.FirstCarried(staged[1]))
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.DebugLevel).FilterMessageSnippet("are not followed in this test set").Len())
	require.Zero(t, logs.FilterLevelExact(zapcore.InfoLevel).Len()+logs.FilterLevelExact(zapcore.WarnLevel).Len())
}
