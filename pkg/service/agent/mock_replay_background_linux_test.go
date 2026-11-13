//go:build linux

package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// bgServingProxy is a servingProxy that takes the set's background
// recordings (coreAgent.BackgroundRecordingsSeeder) into its real mock
// manager, the store a parser reads them from
// (integrations.BackgroundRecordingsOf).
type bgServingProxy struct {
	*servingProxy
	seeds int
}

func (p *bgServingProxy) SeedBackgroundRecordings(recs []models.BackgroundRecording) {
	p.seeds++
	p.mm.SeedBackgroundRecordings(recs)
}

// x2At is a request time of the m1c-x2 record (2026-11-10, UTC), as its
// staged mocks.yaml holds it.
func x2At(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, "2026-11-10T"+s+"Z")
	require.NoError(t, err)
	return at
}

// x2Mongo is a Mongo recording of the m1c-x2 record: its name, its command
// on its collection (the section's text as recorded, trimmed to the
// command), and its request and answer times.
func x2Mongo(t *testing.T, name, section, req, res string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.Mongo, Name: name, Spec: models.MockSpec{
		Metadata:         map[string]string{"connID": "14", "destAddr": "127.0.0.1:27017"},
		MongoRequests:    []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{section}}}},
		ReqTimestampMock: x2At(t, req), ResTimestampMock: x2At(t, res),
	}}
}

// x2Set is the slice of the m1c-x2 record this file replays
// (r24/runs/m1c-x2/out/staged-set/.../api-server-e2e: mocks.yaml and
// mappings.yaml): mock-0, the app's first start-up call (startup:); mock-3734,
// a test's own call (TestGetCurrentUser's list); mock-3733 and mock-3735, the
// schema reconcile poll's two recordings made after the last test (worker:,
// findAndModify ptw_endpoints, 2 s apart, answered n:0); and mock-3736, the
// worker: call recorded after them.
func x2Set(t *testing.T) []*models.Mock {
	poll := `{ SectionSingle msg: {"findAndModify":"ptw_endpoints","new":true,"query":{"dirtyAt":{"$gt":{"$numberLong":"0"}}}} }`
	return []*models.Mock{
		x2Mongo(t, "mock-0", `{ SectionSingle msg: {"hello":1} }`, "08:23:27.117969907", "08:23:27.118"),
		x2Mongo(t, "mock-3734", `{ SectionSingle msg: {"find":"users"} }`, "08:25:06.9", "08:25:06.91"),
		x2Mongo(t, "mock-3733", poll, "08:25:07.245614511", "08:25:07.246023491"),
		x2Mongo(t, "mock-3735", poll, "08:25:09.245765684", "08:25:09.246"),
		x2Mongo(t, "mock-3736", `{ SectionSingle msg: {"ping":1} }`, "08:25:11.170604268", "08:25:11.171"),
	}
}

func bgNames(recs []models.BackgroundRecording) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Mock.Name)
	}
	return out
}

// keploy mock replay (record's rounds, and CI's lane) stages the set on the
// agent from the CLI's scope table, never from a staging call of keploy
// test's. The set's background recordings, its mappings.yaml worker: and
// startup: mocks as the table names them (SetScopeBackground), must reach
// the proxy's store as they do under keploy test
// (MockFilterParams.BackgroundMocks), or no parser can tell that a poll made
// once more than recorded is the application's own work: integrations'
// Mongo parser reads them there (BackgroundRecordingsOf) and marks such a
// miss Background, which counts against no test. Replayed with the m1c-x2
// record's own names and times. Each comes with its recorded span: the poll
// mock-3735's starts at mock-3733, the call before it. A name the table does
// not give (mock-3736, a worker: call it left out) is not handed, so a call
// equal to it stays its test's.
//
// Before the fix the staging handed the proxy none (0 background: marks in
// 2,043 misses of five keploy mock runs, TRACE-M2 section 7.2).
func TestKeployMockReplayHandsTheProxyTheSetsBackgroundRecordings(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &bgServingProxy{servingProxy: newServingProxy()}
	a.Proxy = p
	ctx := context.Background()
	require.NoError(t, a.StoreMocks(ctx, nil, x2Set(t)))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{".::TestGetCurrentUser": {"mock-3734"}}))
	require.NoError(t, a.SetRecordedWindows(ctx, []models.TestWindow{{TestCase: ".::TestGetCurrentUser", Start: x2At(t, "08:25:06.8"), End: x2At(t, "08:25:07.0")}}))
	require.NoError(t, a.SetScopeWorker(ctx, map[string][]string{".": {"mock-3733", "mock-3735", "mock-3736"}}))
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-3733", "mock-3735", "mock-0"}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0", "mock-3733", "mock-3735", "mock-3736"}}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
	require.Nil(t, serr)

	recs := p.mm.BackgroundRecordings(models.Mongo)
	require.Equal(t, []string{"mock-0", "mock-3733", "mock-3735"}, bgNames(recs),
		"the proxy's store does not hand a Mongo parser the set's worker: and startup: recordings on the keploy mock path, in recorded order")
	require.Equal(t, x2At(t, "08:25:07.245614511"), recs[2].From, "mock-3735's span starts at the call before it, mock-3733")
	require.Equal(t, x2At(t, "08:25:06.9"), recs[1].From, "mock-3733's span starts at the test's call before it")
	require.True(t, recs[0].From.IsZero(), "the set's first call has no call before it")
	seeds := p.seeds

	// The same names again: the set stays staged, and the proxy keeps them.
	require.NoError(t, a.SetScopeBackground(ctx, []string{"mock-3733", "mock-3735", "mock-0"}))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0", "mock-3733", "mock-3735", "mock-3736"}}))
	require.Equal(t, seeds, p.seeds, "the same background restaged the set")

	// A table that names other ones (here, none) stages the set again before
	// it is served: the proxy never keeps the previous names.
	require.NoError(t, a.SetScopeBackground(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0", "mock-3733", "mock-3735", "mock-3736"}}))
	require.Greater(t, p.seeds, seeds, "a table naming other background recordings did not stage the set again")
	require.Empty(t, p.mm.BackgroundRecordings(models.Mongo), "a table naming none kept the previous background recordings")
}

// keploy mock replay's staging hands the proxy the set's background
// recordings whatever windows its table sent: a set with no recorded test
// window (FirstRecordedTestStart zero, no RecordedWindows) clears the
// previous set's too, so no miss of this set is marked by another set's
// recordings.
func TestKeployMockReplaysStagingSeedsTheBackgroundWithNoWindows(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	p := &bgServingProxy{servingProxy: newServingProxy()}
	a.Proxy = p
	ctx := context.Background()
	set := x2Set(t)
	p.mm.SeedBackgroundRecordings([]models.BackgroundRecording{{Mock: set[2]}})
	require.NoError(t, a.StoreMocks(ctx, nil, set))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{}))
	require.NoError(t, a.SetScopeBackground(ctx, nil))
	require.NoError(t, a.SetScopePool(ctx, map[string][]string{".": {"mock-0", "mock-3733"}}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "."}, os.Getpid())
	require.Nil(t, serr)
	require.Equal(t, 1, p.seeds, "the staging of a set with no window did not hand the proxy its background recordings")
	require.Empty(t, p.mm.BackgroundRecordings(models.Mongo), "the previous set's background recordings survived a staging that names none")
}
