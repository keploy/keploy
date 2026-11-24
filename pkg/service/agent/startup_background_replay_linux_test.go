//go:build linux

package agent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// realInstanceProxy embeds a real *proxyPkg.Proxy (real ServeInstance, real
// MockManager per worker, real stage/exact/background wiring) while stubbing
// only the marked-listener kernel counters that require a bound eBPF listener.
type realInstanceProxy struct{ *proxyPkg.Proxy }

func (realInstanceProxy) Accepted() (uint64, error)                  { return 0, nil }
func (realInstanceProxy) WaitAccepted(context.Context, uint64) error { return nil }
func (realInstanceProxy) ListenDrops() (models.ListenDrops, error)   { return models.ListenDrops{}, nil }

func httpStartupMock(name, path, body string, at time.Time) *models.Mock {
	fullURL := "http://127.0.0.1:18093" + path
	var urlParams map[string]string
	if parsed, err := url.Parse(fullURL); err == nil && len(parsed.Query()) > 0 {
		urlParams = make(map[string]string, len(parsed.Query()))
		for k, vals := range parsed.Query() {
			if len(vals) > 0 {
				urlParams[k] = vals[0]
			}
		}
	}
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Name:    name,
		TestModeInfo: models.TestModeInfo{
			Lifetime: models.LifetimePerTest,
		},
		Spec: models.MockSpec{
			Metadata: map[string]string{"name": "Http"},
			HTTPReq: &models.HTTPReq{
				Method:    "GET",
				URL:       fullURL,
				URLParams: urlParams,
				Header:    map[string]string{"Host": "127.0.0.1:18093"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "text/plain", "Content-Length": fmt.Sprint(len(body))},
				Body:       body,
			},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(5 * time.Millisecond),
		},
	}
}

func mongoIndexMock(name, cmd, coll, reply string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.Mongo,
		Name:    name,
		TestModeInfo: models.TestModeInfo{
			Lifetime: models.LifetimePerTest,
		},
		Spec: models.MockSpec{
			Metadata: map[string]string{"connID": "1", "destAddr": "127.0.0.1:27017"},
			MongoRequests: []models.MongoRequest{{
				Message: &models.MongoOpMessage{
					Sections: []string{`{ SectionSingle msg: {"` + cmd + `":"` + coll + `","$db":"keploy"} }`},
				},
			}},
			MongoResponses: []models.MongoResponse{{
				Message: &models.MongoOpMessage{
					Sections: []string{`{ SectionSingle msg: {"ok":1,"note":"` + reply + `"} }`},
				},
			}},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(5 * time.Millisecond),
		},
	}
}

// TestMockReplayServesStartupBackgroundInTestWindowParityWithKeployTest proves
// that @startup background recordings (including app-boot recordings with no
// process key, PoolKeyNone, or recorded under a package key) arriving inside
// Test 1's window are served via the ONE shared ServeExactBackground engine in
// BOTH KEPLOY_PROVEN=1 and KEPLOY_PROVEN unset modes, by keploy mock replay
// and keploy test alike:
//   - served their real recorded reply once,
//   - never required of Test 1 when startup finished before Test 1,
//   - never re-served in Test 2 once consumed.
//
// Under KEPLOY_PROVEN=1 that holds here of a recording a declared rule
// covers (a startup: true rule places it in worker:, mapdb's MoveToWorker):
// mock replay's scope table places it in worker:, and keploy test's staging
// call names it in no StartupMocks. A plain startup: recording
// (StartupMocks: in no worker scope) of a kind its user can declare is, on
// both paths, no background of a test's window: a test's identical call is
// its own call (TestMockReplayPlainStartupIsTheTestsOwnCallUnderTheSwitch,
// TestAPlainStartupRecordingIsATestsOwnCallOnlyWhereItsCallCanBeDeclared).
func TestMockReplayServesStartupBackgroundInTestWindowParityWithKeployTest(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, mode := range []struct {
		name   string
		proven string
	}{
		{"KEPLOY_PROVEN=1", "1"},
		{"KEPLOY_PROVEN_unset", ""},
	} {
		t.Run(mode.name+"_mock_replay_and_keploy_test_parity", func(t *testing.T) {
			ctx := context.Background()

			// 1) keploy mock replay path
			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.Equal(t, mode.proven == "1", provenswitch.On())
			p := newServingProxy()
			a.Proxy = p

			bootIdx := mongoIndexMock("mock-startup-idx", "createIndexes", "test_case_audits", "from-startup", base)
			ownT1 := mongoIndexMock("mock-t1-own", "find", "apps", "from-t1", base.Add(2*time.Second))
			ownT2 := mongoIndexMock("mock-t2-own", "find", "users", "from-t2", base.Add(4*time.Second))

			require.NoError(t, a.StoreMocks(ctx, []*models.Mock{bootIdx, ownT1, ownT2}, nil))
			require.NoError(t, a.SetScopeTable(ctx, map[string][]string{
				".::TestAppCRUD": {"mock-t1-own"},
				".::TestList":    {"mock-t2-own"},
			}))
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: ".::TestAppCRUD", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
				{TestCase: ".::TestList", Start: base.Add(3 * time.Second), End: base.Add(5 * time.Second)},
			}))
			// App-boot startup recording sits under PoolKeyNone while the test binary claims key ".".
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-idx"}))
			require.NoError(t, a.SetScopePool(ctx, map[string][]string{
				models.PoolKeyNone: {"mock-startup-idx"},
				".":                {},
			}))
			if mode.proven == "1" {
				// Under the switch a test's window is served a recording made
				// before the first test only when a declared rule covers it: a
				// startup: true rule places it in worker: (mapdb's
				// MoveToWorker).
				require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{models.PoolKeyNone: {"mock-startup-idx"}}))
			}

			if mode.proven == "1" {
				_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestAppCRUD", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestAppCRUD", 0))
			}

			// Open Test 1: late @startup createIndexes arrives inside Test 1's window.
			// Under KEPLOY_PROVEN=1 own calls use exact serving; with KEPLOY_PROVEN
			// unset own calls keep today's legacy matching (ExactServing == false)
			// while S12Background stays active for exact-only background passes.
			require.Equal(t, mode.proven == "1", p.mm.ExactServing(), "ExactServing must track KEPLOY_PROVEN in mode %s", mode.name)
			require.True(t, integrations.S12Background(p.mm), "S12Background must be active in mode %s", mode.name)
			bg := p.mm.Background(models.Mongo)
			require.Len(t, bg, 1, "mock replay must expose @startup recording via shared ServeExactBackground inside Test 1's window")
			require.Equal(t, "mock-startup-idx", bg[0].Name)
			require.True(t, p.mm.DeleteStartupMock(*bg[0]), "late @startup recording must be consumable inside Test 1")
			require.Empty(t, p.mm.Background(models.Mongo), "spent @startup recording must not remain available")

			// Consume Test 1's own mock and close Test 1.
			perTest, err := p.mm.GetFilteredMocksByKind(models.Mongo)
			require.NoError(t, err)
			require.Len(t, perTest, 1)
			require.Equal(t, "mock-t1-own", perTest[0].Name)
			require.True(t, p.mm.DeleteFilteredMock(*perTest[0]))
			if mode.proven == "1" {
				endResp, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestAppCRUD", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serr)
				require.Empty(t, endResp.Action, "TestAppCRUD must pass cleanly: %s", endResp.Reason)
			} else {
				require.NoError(t, a.EndScope(ctx, ".::TestAppCRUD", 0))
			}

			// Open Test 2: spent @startup mock must NOT be re-served.
			if mode.proven == "1" {
				_, serr := a.ProvenBegin(ctx, models.ScopeReq{Name: "TestList", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestList", 0))
			}
			require.Empty(t, p.mm.Background(models.Mongo), "once-spent @startup recording must not be re-served in Test 2")
			perTest2, err := p.mm.GetFilteredMocksByKind(models.Mongo)
			require.NoError(t, err)
			require.Len(t, perTest2, 1)
			require.True(t, p.mm.DeleteFilteredMock(*perTest2[0]))
			if mode.proven == "1" {
				endResp2, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestList", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serr)
				require.Empty(t, endResp2.Action, "TestList must pass cleanly: %s", endResp2.Reason)
			} else {
				require.NoError(t, a.EndScope(ctx, ".::TestList", 0))
			}

			// 2) keploy test parity path (same ServeExactBackground engine).
			// Its staging call names the recording in no StartupMocks: under
			// the switch, a declared or learned one (worker:).
			keployTest := func(startup []string) (*Agent, *servingProxy) {
				t.Helper()
				ktAgent, _ := wireAgent(t, models.MODE_TEST)
				ktProxy := newServingProxy()
				ktAgent.Proxy = ktProxy
				require.NoError(t, ktAgent.StoreMocks(ctx, nil, []*models.Mock{bootIdx.DeepCopy(), ownT1.DeepCopy(), ownT2.DeepCopy()}))
				require.NoError(t, ktAgent.UpdateMockParams(ctx, models.MockFilterParams{
					AfterTime:              models.BaseTime,
					BeforeTime:             time.Now(),
					StrictMockWindow:       true,
					FirstRecordedTestStart: base.Add(time.Second),
					BackgroundMocks:        []string{"mock-startup-idx"},
					StartupMocks:           startup,
					Exact: &models.ExactServing{
						Stage:      true,
						Background: models.RecordedWithS12Routing,
						Pool:       []string{"mock-startup-idx"},
						Order:      []string{"test-1", "test-2"},
					},
				}))
				require.NoError(t, ktAgent.UpdateMockParams(ctx, models.MockFilterParams{
					Exact: &models.ExactServing{Test: "test-1", Owned: []string{"mock-t1-own"}},
				}))
				return ktAgent, ktProxy
			}
			ktAgent, ktProxy := keployTest(nil)
			ktBG := ktProxy.mm.Background(models.Mongo)
			require.Len(t, ktBG, 1, "keploy test parity: @startup recording exposed via ServeExactBackground")
			require.Equal(t, bg[0].Name, ktBG[0].Name)
			require.True(t, ktProxy.mm.DeleteStartupMock(*ktBG[0]))
			require.Empty(t, ktProxy.mm.Background(models.Mongo))
			require.NoError(t, ktAgent.UpdateMockParams(ctx, models.MockFilterParams{
				Exact: &models.ExactServing{Test: "test-2", Owned: []string{"mock-t2-own"}},
			}))
			require.Empty(t, ktProxy.mm.Background(models.Mongo), "keploy test parity: spent @startup not re-served in test-2")
		})
	}
}

// TestMockReplayBackgroundMatchOrderOwnThenWorkerThenStartup pins the strict
// match order: (1) open test's own recordings -> (2) mp.Worker -> (3) @startup,
// even when @startup has an earlier recorded timestamp than mp.Worker, through
// the ONE shared ServeExactBackground engine in BOTH KEPLOY_PROVEN modes.
// A Mongo call is declarable only once a rule system registered Mongo
// (models.StartupDeclarable): until then a plain @startup recording keeps
// this order under KEPLOY_PROVEN=1 too, as in v3.7.18. Once Mongo is
// registered, under KEPLOY_PROVEN=1 a plain @startup recording (in no
// worker scope) is no third tier: a test's window shows it in neither read,
// as the test's identical call is its own call; a declared one (a startup:
// true rule's, placed in worker:) is the worker scope's, after the test's
// own (TestMockReplayDeclaredStartupFollowsTheTestsOwnUnderTheSwitch).
func TestMockReplayBackgroundMatchOrderOwnThenWorkerThenStartup(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, mode := range []struct {
		name       string
		proven     string
		declarable bool
	}{
		{"KEPLOY_PROVEN=1", "1", false},
		{"KEPLOY_PROVEN=1_Mongo_declarable", "1", true},
		{"KEPLOY_PROVEN_unset", "", false},
		{"KEPLOY_PROVEN_unset_Mongo_declarable", "", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			if mode.declarable {
				t.Cleanup(models.RegisterStartupDeclarableKinds(models.Mongo))
			}
			require.Equal(t, mode.proven == "1", provenswitch.On())
			p := newServingProxy()
			a.Proxy = p

			// @startup has earliest timestamp; worker has latest timestamp.
			startupMk := mongoIndexMock("mock-startup", "listIndexes", "apps", "from-startup", base)
			ownMk := mongoIndexMock("mock-own", "listIndexes", "apps", "from-own", base.Add(2*time.Second))
			workerMk := mongoIndexMock("mock-worker", "listIndexes", "apps", "from-worker", base.Add(5*time.Second))

			require.NoError(t, a.StoreMocks(ctx, []*models.Mock{startupMk, ownMk, workerMk}, nil))
			require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestOrder": {"mock-own"}}))
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: ".::TestOrder", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
			}))
			require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{".": {"mock-worker"}}))
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-worker", "mock-startup"}))
			require.NoError(t, a.SetScopePool(ctx, map[string][]string{
				models.PoolKeyNone: {"mock-startup"},
				".":                {"mock-worker"},
			}))

			if mode.proven == "1" {
				_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestOrder", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestOrder", 0))
			}

			// Per-test read shows (1) open test's own recording first; under
			// KEPLOY_PROVEN=1 the exact view also lists worker after own, while
			// with KEPLOY_PROVEN unset the legacy stageScope view holds only own
			// and background passes read Background(Mongo) separately.
			perTest, err := p.mm.GetFilteredMocksByKind(models.Mongo)
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(perTest), 1)
			require.Equal(t, "mock-own", perTest[0].Name, "1st: open test's own recording")
			if mode.proven == "1" {
				require.GreaterOrEqual(t, len(perTest), 2)
				require.Equal(t, "mock-worker", perTest[1].Name, "2nd: worker recording")
			}

			// Background read shows (2) worker strictly BEFORE (3) @startup despite earlier startup timestamp.
			bg := p.mm.Background(models.Mongo)
			if mode.proven == "1" && mode.declarable {
				// Under the switch a plain startup: recording of a kind its
				// user can declare, made before the first test and covered by
				// no declared rule, is no background of a test's window: a
				// test's identical call is its own call. The learned worker:
				// recording stays the background's.
				require.Equal(t, []string{"mock-own", "mock-worker"}, namesOf(perTest), "under the switch a test's window must show its own, then the worker scope, and not the plain startup recording")
				require.Equal(t, []string{"mock-worker"}, namesOf(bg), "under the switch a test's window must not show the plain startup recording as background")
				return
			}
			require.Len(t, bg, 2)
			require.Equal(t, "mock-worker", bg[0].Name, "worker must precede @startup in background order")
			require.Equal(t, "mock-startup", bg[1].Name, "@startup must follow worker")
		})
	}
}

// TestMockReplayDeclaredStartupFollowsTheTestsOwnUnderTheSwitch: under
// KEPLOY_PROVEN=1 a recording made before the first test that a startup:
// true rule declared is placed in worker: (mapdb's MoveToWorker), and a
// test's window shows it after the test's own, with the learned worker:
// recording, as the background's.
func TestMockReplayDeclaredStartupFollowsTheTestsOwnUnderTheSwitch(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "1")
	p := newServingProxy()
	a.Proxy = p

	startupMk := mongoIndexMock("mock-startup", "listIndexes", "apps", "from-startup", base)
	ownMk := mongoIndexMock("mock-own", "listIndexes", "apps", "from-own", base.Add(2*time.Second))
	workerMk := mongoIndexMock("mock-worker", "listIndexes", "apps", "from-worker", base.Add(5*time.Second))
	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{startupMk, ownMk, workerMk}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestOrder": {"mock-own"}}))
	require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
		{TestCase: ".::TestOrder", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
	}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{".": {"mock-worker"}, models.PoolKeyNone: {"mock-startup"}}))
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-worker", "mock-startup"}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{
		models.PoolKeyNone: {"mock-startup"},
		".":                {"mock-worker"},
	}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestOrder", CanSkip: true})
	require.Nil(t, serr)

	perTest, err := p.mm.GetFilteredMocksByKind(models.Mongo)
	require.NoError(t, err)
	require.NotEmpty(t, perTest)
	require.Equal(t, "mock-own", perTest[0].Name, "1st: open test's own recording")
	require.ElementsMatch(t, []string{"mock-own", "mock-worker", "mock-startup"}, namesOf(perTest), "the declared startup recording is not shown after the test's own")
	require.ElementsMatch(t, []string{"mock-worker", "mock-startup"}, namesOf(p.mm.Background(models.Mongo)), "the declared startup recording is not the background's in the test's window")
}

// TestMockReplayRepeatsOwnNeverBorrowsStartup proves in BOTH KEPLOY_PROVEN
// modes that a repeat of the open test's own recording NEVER borrows an
// identical @startup recording (leaving @startup unspent).
func TestMockReplayRepeatsOwnNeverBorrowsStartup(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, mode := range []struct {
		name   string
		proven string
	}{
		{"KEPLOY_PROVEN=1", "1"},
		{"KEPLOY_PROVEN_unset", ""},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()

			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.Equal(t, mode.proven == "1", provenswitch.On())
			p := newServingProxy()
			a.Proxy = p

			startupHTTP := httpStartupMock("mock-startup-cfg", "/config", "boot-cfg", base)
			ownHTTP := httpStartupMock("mock-own-cfg", "/config", "test-cfg", base.Add(2*time.Second))

			require.NoError(t, a.StoreMocks(ctx, []*models.Mock{startupHTTP}, []*models.Mock{ownHTTP}))
			require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestCfg": {"mock-own-cfg"}}))
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: ".::TestCfg", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
			}))
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-cfg"}))
			if mode.proven == "1" {
				// A declared startup: true rule places the recording in worker:
				// (mapdb's MoveToWorker): under the switch only a declared or
				// learned recording is a test window's background.
				require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{models.PoolKeyNone: {"mock-startup-cfg"}}))
			}
			require.NoError(t, a.SetScopePool(ctx, map[string][]string{
				models.PoolKeyNone: {"mock-startup-cfg"},
				".":                {},
			}))
			if mode.proven == "1" {
				_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestCfg", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestCfg", 0))
			}

			// 1st GET /config answers with TestCfg's own recording.
			status1, body1 := p.call(t, "/config")
			require.Equal(t, 200, status1)
			require.Equal(t, "test-cfg", body1)

			// 2nd GET /config:
			// - Under KEPLOY_PROVEN=1, own recording was consumed once so the
			//   repeat must fail closed and NEVER borrow mock-startup-cfg.
			// - With KEPLOY_PROVEN unset, HTTP own recordings stay on main's
			//   legacy reusable tier (200 "test-cfg" again) and NEVER touch
			//   mock-startup-cfg either.
			status2, body2 := p.call(t, "/config")
			if mode.proven == "1" {
				require.NotEqual(t, 200, status2, "repeat of open test's own recording must fail closed, never borrow @startup")
			} else {
				require.Equal(t, 200, status2, "unset mode keeps main's legacy HTTP reusable serving for own repeats")
				require.Equal(t, "test-cfg", body2, "unset mode repeat must answer from own recording, never @startup")
			}

			// And mock-startup-cfg must STILL be unspent in the background pool.
			bg := p.mm.Background(models.HTTP)
			require.Len(t, bg, 1)
			require.Equal(t, "mock-startup-cfg", bg[0].Name, "@startup recording must remain unspent after own repeat")
		})
	}
}

// TestMockReplayStartupAnswerDataflowRunsSequentially proves the dataflow
// clause (parity with keploy test) in BOTH KEPLOY_PROVEN modes:
//  1. When a served @startup answer reaches a test's window / output, the open
//     test's served account (ServedInOpenTest) and learn.Tracker record the
//     served @startup mock while ConsumedTestMocks holds only the test's own
//     mocks (never requiring @startup), and a second test running sequentially
//     afterwards does not re-consume the spent @startup mock.
//  2. Parallel test workers (Workers > 1) are refused when @startup background
//     serving is active, forcing sequential execution just as keploy test does.
func TestMockReplayStartupAnswerDataflowRunsSequentially(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, mode := range []struct {
		name   string
		proven string
	}{
		{"KEPLOY_PROVEN=1", "1"},
		{"KEPLOY_PROVEN_unset", ""},
	} {
		t.Run(mode.name+"_served_startup_answer_tracked_and_spent_sequentially", func(t *testing.T) {
			ctx := context.Background()

			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.Equal(t, mode.proven == "1", provenswitch.On())
			p := newServingProxy()
			tr := learn.NewTracker()
			p.mm.SetServedObserver(tr.ServedMock)
			a.Proxy = p

			startupToken := httpStartupMock("mock-startup-seed", "/seed", "seed-value-42", base)
			ownT1 := httpStartupMock("mock-t1-out", "/echo?v=seed-value-42", "echo:seed-value-42", base.Add(2*time.Second))
			ownT2 := httpStartupMock("mock-t2-out", "/next", "ok", base.Add(4*time.Second))

			require.NoError(t, a.StoreMocks(ctx, []*models.Mock{startupToken}, []*models.Mock{ownT1, ownT2}))
			require.NoError(t, a.SetScopeTable(ctx, map[string][]string{
				".::TestConsumer": {"mock-t1-out"},
				".::TestAfter":    {"mock-t2-out"},
			}))
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: ".::TestConsumer", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
				{TestCase: ".::TestAfter", Start: base.Add(3 * time.Second), End: base.Add(5 * time.Second)},
			}))
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-seed"}))
			if mode.proven == "1" {
				// A declared startup: true rule places the recording in worker:
				// (mapdb's MoveToWorker): under the switch only a declared or
				// learned recording is a test window's background.
				require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{models.PoolKeyNone: {"mock-startup-seed"}}))
			}
			require.NoError(t, a.SetScopePool(ctx, map[string][]string{
				models.PoolKeyNone: {"mock-startup-seed"},
				".":                {},
			}))

			tr.Begin()
			if mode.proven == "1" {
				_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestConsumer", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestConsumer", 0))
			}

			// Startup answer arrives inside TestConsumer and its output feeds TestConsumer's own call.
			st1, body1 := p.call(t, "/seed")
			require.Equal(t, 200, st1)
			require.Equal(t, "seed-value-42", body1)

			st2, body2 := p.call(t, "/echo?v="+body1)
			require.Equal(t, 200, st2)
			require.Equal(t, "echo:seed-value-42", body2)

			// Dataflow account: under KEPLOY_PROVEN=1 ServedInOpenTest holds BOTH
			// startup seed and own call in order while ConsumedTestMocks holds
			// ONLY TestConsumer's own call (startup never required). With
			// KEPLOY_PROVEN unset, exactView is off (main's path) and the served
			// observer (tr) still sees both mocks served in order while @startup
			// is spent once.
			if mode.proven == "1" {
				_, served := p.mm.ServedInOpenTest()
				var servedNames []string
				for _, mk := range served {
					servedNames = append(servedNames, mk.Name)
				}
				require.Equal(t, []string{"mock-startup-seed", "mock-t1-out"}, servedNames,
					"served @startup answer reaching test output must be recorded in ServedInOpenTest before subsequent calls")
				ownConsumed := p.mm.ConsumedTestMocks()
				require.Len(t, ownConsumed, 1)
				require.Equal(t, "mock-t1-out", ownConsumed[0].Name)
			} else {
				require.False(t, p.mm.ExactServing(), "unset mode must keep exactView off")
				require.Empty(t, p.mm.Background(models.HTTP), "served @startup mock must be marked spent")
			}

			if mode.proven == "1" {
				_, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestConsumer", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestAfter", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.EndScope(ctx, ".::TestConsumer", 0))
				require.NoError(t, a.BeginScope(ctx, ".::TestAfter", 0))
			}

			// Sequential next test: spent @startup seed is no longer available.
			st3, _ := p.call(t, "/seed")
			require.NotEqual(t, 200, st3, "spent @startup mock must not be re-served to the next sequential test")
			if mode.proven == "1" {
				_, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestAfter", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.EndScope(ctx, ".::TestAfter", 0))
			}
		})

		t.Run(mode.name+"_parallel_workers_plain_startup_allowed_declared_rule_refused", func(t *testing.T) {
			ctx := context.Background()

			a, _, _ := instanceAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.Equal(t, mode.proven == "1", provenswitch.On())

			// 1) Plain recorded startup: entries (written by main's mock record)
			// WITHOUT a declared startup: true rule MUST NOT refuse parallel
			// workers — replays identically to main.
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-seed"}))
			require.NoError(t, a.SetScopeStartupRules(ctx, false))
			require.Nil(t, a.startupBackgroundWorkersRefusal(2), "plain recorded startup: entries must not trigger parallel-worker refusal")
			require.True(t, a.takesWorkers(ctx, os.Getpid()), "parallel workers must be allowed for sets with only plain recorded startup: entries")
			_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: ".", Workers: 2}, os.Getpid())
			require.Nil(t, serr, "ClaimInstance(Workers: 2) must succeed for plain recorded startup: set")

			// 2) A set with a DECLARED startup: true rule MUST refuse parallel workers.
			a2, _, _ := instanceAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.NoError(t, a2.SetScopeBackground(ctx, []string{"mock-startup-rule"}))
			require.NoError(t, a2.SetScopeStartupRules(ctx, true))
			require.False(t, a2.takesWorkers(ctx, os.Getpid()), "parallel workers must be refused when a declared startup rule applies")

			_, serr2 := a2.ClaimInstance(ctx, models.InstanceReq{Key: ".", Workers: 2}, os.Getpid())
			require.NotNil(t, serr2)
			require.Equal(t, models.ScopeErrTooManyWorkers, serr2.Code)
			require.Contains(t, serr2.Message, "sequentially")

			// 3) Clearing SetScopeStartupRules(false) clears the refusal.
			require.NoError(t, a2.SetScopeStartupRules(ctx, false))
			require.Nil(t, a2.startupBackgroundWorkersRefusal(2), "clearing declared startup rule must clear parallel-worker refusal")

			// 4) Real Proxy with ServeInstance + plain startup: + Workers: 2:
			// ClaimInstance -> serveWorker -> servePoolExactly -> ProvenBegin
			// succeeds on a real *proxy.Proxy (not just fake instanceWire) and
			// leaves the worker's ExactServer serving T1's own recording.
			a3, _ := workerAgent(t, models.MODE_TEST, 4)
			t.Setenv(provenswitch.Env, mode.proven)
			realProxy := proxyPkg.New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{Mode: models.MODE_TEST}})
			require.NoError(t, realProxy.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}))
			drainCtx, cancelDrain := context.WithCancel(ctx)
			t.Cleanup(cancelDrain)
			realProxy.StartErrorDrain(drainCtx)
			realProxy.ContinueTestErrorCapture()
			a3.Proxy = realInstanceProxy{Proxy: realProxy}
			seed := httpStartupMock("mock-startup-seed", "/seed/1001", "boot-seed", base)
			own1 := httpStartupMock("mock-own-1", "/items/1001", "own-1", base.Add(2*time.Second))
			require.NoError(t, a3.StoreMocks(ctx, []*models.Mock{seed}, []*models.Mock{own1}))
			require.NoError(t, a3.SetScopeTable(ctx, map[string][]string{".::T1": {"mock-own-1"}}))
			require.NoError(t, a3.SetScopeBackground(ctx, []string{"mock-startup-seed"}))
			require.NoError(t, a3.SetScopeStartupRules(ctx, false))
			require.NoError(t, a3.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-startup-seed"}, ".": {}}))
			resp3, serr3 := a3.ClaimInstance(ctx, models.InstanceReq{Key: ".", Workers: 2}, os.Getpid())
			require.Nil(t, serr3, "real Proxy ClaimInstance(Workers: 2) must succeed for plain startup: set (%s)", mode.name)
			require.NotEmpty(t, resp3.Instance)
			if mode.proven == "1" {
				callerCtx := coreAgent.WithCaller(ctx, func() (int, error) { return os.Getpid(), nil })
				_, serrBeg := a3.ProvenBegin(callerCtx, models.ScopeReq{Name: "T1", CanSkip: true})
				require.Nil(t, serrBeg, "real Proxy ProvenBegin on claimed parallel worker must succeed (%s)", mode.name)
				_, serrEnd := a3.ProvenEnd(callerCtx, models.ScopeReq{Name: "T1", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.Nil(t, serrEnd, "real Proxy ProvenEnd on claimed parallel worker must succeed (%s)", mode.name)
			} else {
				require.NoError(t, a3.BeginScope(ctx, ".::T1", 0))
				require.NoError(t, a3.EndScope(ctx, ".::T1", 0))
			}
		})
	}
}

// TestMockReplayUnsetOwnKeepsLegacyMatchingWhileBackgroundStaysExact proves the
// N2 ruling for HTTP replay: when a set carries @startup background entries,
// KEPLOY_PROVEN unset keeps today's legacy matching for a test's own calls
// (e.g. dynamic numeric path segment) while background passes remain exact-only
// and fail-closed in BOTH modes; KEPLOY_PROVEN=1 enforces exact matching on
// both own and background calls.
func TestMockReplayUnsetOwnKeepsLegacyMatchingWhileBackgroundStaysExact(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	for _, mode := range []struct {
		name   string
		proven string
	}{
		{"KEPLOY_PROVEN_unset", ""},
		{"KEPLOY_PROVEN=1", "1"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx := context.Background()
			a, _ := wireAgent(t, models.MODE_TEST)
			t.Setenv(provenswitch.Env, mode.proven)
			require.Equal(t, mode.proven == "1", provenswitch.On())
			p := newServingProxy()
			a.Proxy = p

			startupSeed := httpStartupMock("mock-startup-seed", "/seed/1001", "boot-seed", base)
			ownItem := httpStartupMock("mock-own-item", "/items/1001", "own-item", base.Add(2*time.Second))

			require.NoError(t, a.StoreMocks(ctx, []*models.Mock{startupSeed}, []*models.Mock{ownItem}))
			require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestOwnDrift": {"mock-own-item"}}))
			require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
				{TestCase: ".::TestOwnDrift", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
			}))
			require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-seed"}))
			if mode.proven == "1" {
				// A declared startup: true rule places the recording in worker:
				// (mapdb's MoveToWorker): under the switch only a declared or
				// learned recording is a test window's background.
				require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{models.PoolKeyNone: {"mock-startup-seed"}}))
			}
			require.NoError(t, a.SetScopePool(ctx, map[string][]string{
				models.PoolKeyNone: {"mock-startup-seed"},
				".":                {},
			}))

			if mode.proven == "1" {
				_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
				require.Nil(t, serr)
				_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "TestOwnDrift", CanSkip: true})
				require.Nil(t, serr)
			} else {
				require.NoError(t, a.BeginScope(ctx, ".::TestOwnDrift", 0))
			}

			// 1. Drifted own call (/items/9999 vs recorded /items/1001).
			stOwn, bodyOwn := p.call(t, "/items/9999")
			if mode.proven == "" {
				require.Equal(t, 200, stOwn, "unset mode must keep legacy matching for a test's own call")
				require.Equal(t, "own-item", bodyOwn)
			} else {
				require.NotEqual(t, 200, stOwn, "KEPLOY_PROVEN=1 must reject drifted own call")
				stExact, bodyExact := p.call(t, "/items/1001")
				require.Equal(t, 200, stExact)
				require.Equal(t, "own-item", bodyExact)
			}

			// 2. Drifted call against @startup (/seed/9999 vs recorded /seed/1001) MUST FAIL CLOSED in BOTH modes.
			stBadBG, _ := p.call(t, "/seed/9999")
			require.NotEqual(t, 200, stBadBG, "background pass must stay exact-only (fail closed) in %s", mode.name)

			// 3. Exact recorded @startup call (/seed/1001) IS served once, then misses on 2nd call.
			stBG, bodyBG := p.call(t, "/seed/1001")
			require.Equal(t, 200, stBG, "exact recorded @startup call must be served in %s", mode.name)
			require.Equal(t, "boot-seed", bodyBG)
			stBG2, _ := p.call(t, "/seed/1001")
			require.NotEqual(t, 200, stBG2, "spent @startup call must miss on 2nd call in %s", mode.name)
		})
	}
}

// TestUnsetOwnCallsIdenticalWithOrWithoutStartup proves the Review-3/4
// six-shape probe across BOTH KEPLOY_PROVEN modes without overriding
// KEPLOY_PROVEN inside the test:
//   - own-exact, own-drift, own-repeat, cross-test, unowned-pool, unowned-drift
//
// In KEPLOY_PROVEN unset mode (BeginScope), adding a startup: entry leaves all
// six shapes identical to bg=false (main's behavior) and ExactServing()==false.
// In KEPLOY_PROVEN=1 mode (ClaimInstance+ProvenBegin), adding a startup: entry
// leaves all six shapes identical to bg=false (exact serving).
func TestUnsetOwnCallsIdenticalWithOrWithoutStartup(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	origProven := os.Getenv(provenswitch.Env)
	provenOn := provenswitch.On()
	modeLabel := "unset"
	if provenOn {
		modeLabel = "proven1"
	}
	scen := map[string][]string{
		"own-exact":     {"/items/1001"},
		"own-drift":     {"/items/9999"},
		"own-repeat":    {"/items/1001", "/items/1001"},
		"cross-test":    {"/other/2002"},
		"unowned-pool":  {"/free/3003"},
		"unowned-drift": {"/free/3999"},
	}
	order := []string{"own-exact", "own-drift", "own-repeat", "cross-test", "unowned-pool", "unowned-drift"}
	res := map[string]map[bool]string{}
	for _, name := range order {
		res[name] = map[bool]string{}
		for _, bg := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/bg=%v", modeLabel, name, bg), func(t *testing.T) {
				ctx := context.Background()
				a, _ := wireAgent(t, models.MODE_TEST)
				// wireAgent forces KEPLOY_PROVEN=1 internally; restore the caller's
				// KEPLOY_PROVEN env so this test runs genuinely in whatever mode
				// (unset or =1) the caller selected.
				t.Setenv(provenswitch.Env, origProven)
				require.Equal(t, provenOn, provenswitch.On())
				p := newServingProxy()
				a.Proxy = p

				seed := httpStartupMock("mock-seed", "/seed/1001", "boot-seed", base)
				own := httpStartupMock("mock-own-1", "/items/1001", "own-item", base.Add(2*time.Second))
				free := httpStartupMock("mock-free", "/free/3003", "free", base.Add(2500*time.Millisecond))
				other := httpStartupMock("mock-own-2", "/other/2002", "other", base.Add(5*time.Second))

				require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{seed, own, free, other}))
				require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::T1": {"mock-own-1"}, ".::T2": {"mock-own-2"}}))
				require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
					{TestCase: ".::T1", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
					{TestCase: ".::T2", Start: base.Add(4 * time.Second), End: base.Add(6 * time.Second)},
				}))
				if bg {
					require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-seed"}))
				}
				if bg || provenOn {
					require.NoError(t, a.SetScopePool(ctx, map[string][]string{models.PoolKeyNone: {"mock-seed", "mock-free"}, ".": {}}))
				}
				if provenOn {
					_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
					require.Nil(t, serr)
					_, serr = a.ProvenBegin(ctx, models.ScopeReq{Name: "T1", CanSkip: true})
					require.Nil(t, serr)
					require.True(t, p.mm.ExactServing(), "KEPLOY_PROVEN=1 must arm ExactServing (bg=%v)", bg)
				} else {
					require.NoError(t, a.BeginScope(ctx, ".::T1", 0))
					require.False(t, p.mm.ExactServing(), "unset mode must never turn on ExactServing (bg=%v)", bg)
				}

				out := ""
				for _, path := range scen[name] {
					st, body := p.call(t, path)
					out += fmt.Sprintf("%d:%s ", st, body)
				}
				t.Logf("PROBE mode=%s shape=%s bg=%v out=%s", modeLabel, name, bg, out)
				res[name][bg] = out
			})
		}
	}
	for _, name := range order {
		require.Equal(t, res[name][false], res[name][true],
			"%s mode own-call shape %s must match bg=false", modeLabel, name)
	}
}

// TestUnsetBackgroundRoundSnapshotWithinRoundAndOwnRepeat proves that in
// KEPLOY_PROVEN unset mode (exactView == nil), parsers reading the background
// via integrations.SnapshotRound + integrations.WithinRound (Mongo v2's
// readBackground / claimBackgroundExact path) see and consume @startup
// recordings once, and integrations.OwnRepeatRead hides background recordings
// when the open test repeats its own call.
func TestUnsetBackgroundRoundSnapshotWithinRoundAndOwnRepeat(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "")
	require.False(t, provenswitch.On())
	p := newServingProxy()
	a.Proxy = p

	bootIdx := mongoIndexMock("mock-startup-idx", "createIndexes", "test_case_audits", "from-startup", base)
	ownFind := mongoIndexMock("mock-own-find", "find", "apps", "from-own", base.Add(2*time.Second))

	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{bootIdx, ownFind}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestAppCRUD": {"mock-own-find"}}))
	require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{
		{TestCase: ".::TestAppCRUD", Start: base.Add(time.Second), End: base.Add(3 * time.Second)},
	}))
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-startup-idx"}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{
		models.PoolKeyNone: {"mock-startup-idx"},
		".":                {},
	}))
	require.NoError(t, a.BeginScope(ctx, ".::TestAppCRUD", 0))
	require.False(t, p.mm.ExactServing(), "unset mode must keep ExactServing() == false")

	// 1) SnapshotRound returns Background=true with mock-startup-idx unused.
	snap, ok := integrations.SnapshotRound(p.mm, integrations.RoundQuery{Kind: models.Mongo})
	require.True(t, ok, "SnapshotRound must succeed in unset mode when ServeExactBackground is active")
	require.True(t, snap.Background)
	require.Len(t, snap.BackgroundUnused, 1)
	require.Equal(t, "mock-startup-idx", snap.BackgroundUnused[0].Name)

	// 2) OwnRepeatRead installs owns on BackgroundHider when S12Background is active in unset mode.
	held, _ := p.mm.HoldToRound(integrations.CallRound{})
	wrapped := integrations.OwnRepeatRead(held, integrations.CallRound{}, func() bool { return true })
	hider, okHider := wrapped.(integrations.BackgroundHider)
	require.True(t, okHider)
	require.True(t, hider.HidesBackground(), "OwnRepeatRead must install owns on BackgroundHider in unset mode when S12Background is active")

	// 3) WithinRound consumes mock-startup-idx atomically once, then refuses a 2nd consume.
	takeDb := integrations.WithinRound(p.mm, snap.At)
	require.True(t, takeDb.DeleteStartupMock(*snap.BackgroundUnused[0]), "WithinRound must consume @startup mock once")
	require.False(t, takeDb.DeleteStartupMock(*snap.BackgroundUnused[0]), "WithinRound must refuse 2nd consume of spent @startup mock")

	snap2, ok2 := integrations.SnapshotRound(p.mm, integrations.RoundQuery{Kind: models.Mongo})
	require.True(t, ok2)
	require.Empty(t, snap2.BackgroundUnused)
	require.Len(t, snap2.BackgroundSpent, 1)
	require.Equal(t, "mock-startup-idx", snap2.BackgroundSpent[0].Name)
}

// TestUnsetBootDeleteFilteredFallsThroughToStartupAndSpendsOnce proves that at
// boot before any test scope opens in KEPLOY_PROVEN unset mode, calling
// DeleteFilteredMock on a @startup mock in m.startup falls through cleanly to
// deleteStartupTier (removing it from m.startup rather than stranding it),
// marks it spent in bg so it is not re-served inside Test 1, and returns false
// on any second consume attempt.
func TestUnsetBootDeleteFilteredFallsThroughToStartupAndSpendsOnce(t *testing.T) {
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "")
	require.False(t, provenswitch.On())
	p := newServingProxy()
	a.Proxy = p

	bootIdx := mongoIndexMock("mock-boot-idx", "listIndexes", "app_config_audits", "from-boot", base)
	ownFind := mongoIndexMock("mock-own-find", "find", "apps", "from-own", base.Add(2*time.Second))

	require.NoError(t, a.StoreMocks(ctx, []*models.Mock{bootIdx, ownFind}, nil))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestAppCRUD": {"mock-own-find"}}))
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-boot-idx"}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{
		models.PoolKeyNone: {"mock-boot-idx"},
		".":                {},
	}))
	// Stage initial boot pool (BaseTime..now) exactly as replay.go step 6 does.
	require.NoError(t, a.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime:  models.BaseTime,
		BeforeTime: time.Now(),
	}))
	// Before Test 1 opens, boot mock lives in m.startup and S12Background stays off.
	require.False(t, integrations.S12Background(p.mm), "S12Background must stay off during boot in unset mode")
	startupBefore, err := p.mm.GetStartupMocks()
	require.NoError(t, err)
	require.NotEmpty(t, startupBefore)

	// DeleteFilteredMock must fall through to deleteStartupMock, remove mock-boot-idx
	// from m.startup, and mark it spent so it cannot strand or loop.
	require.True(t, p.mm.DeleteFilteredMock(*bootIdx), "boot DeleteFilteredMock must fall through to startup tier")
	require.False(t, p.mm.DeleteFilteredMock(*bootIdx), "2nd boot consume must return false cleanly")
	startupAfter, err := p.mm.GetStartupMocks()
	require.NoError(t, err)
	for _, mk := range startupAfter {
		require.NotEqual(t, "mock-boot-idx", mk.Name, "consumed boot mock must not remain stranded in m.startup")
	}

	// Opening Test 1 must NOT re-serve the already boot-consumed @startup mock.
	require.NoError(t, a.BeginScope(ctx, ".::TestAppCRUD", 0))
	require.True(t, integrations.S12Background(p.mm))
	require.Empty(t, p.mm.Background(models.Mongo), "boot-consumed @startup mock must stay spent inside Test 1")
}
