package mock

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy alone names no read; a registered namer names them from each test's
// own recordings the agent holds, and only reads of at least two of them go
// to the agent.
func TestARegisteredNamerNamesEachTestsRepeatedReads(t *testing.T) {
	loaded := []*models.Mock{{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}, nil, {Name: "mock-5"}}
	table := map[string][]string{"o::TestA": {"mock-1", "mock-2", "mock-4"}, "o::TestB": {"mock-3", "mock-5"}}
	named := func() (got map[string][]models.RepeatRead) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("namedRepeatReads panicked: %v", r)
			}
		}()
		return namedRepeatReads("o", httpint.RepeatReadNoise{}, table, loaded)
	}
	if got := named(); got != nil {
		t.Fatalf("keploy alone named %v", got)
	}
	var gotSet string
	var gotOwned map[string][]string
	unregister := RegisterRepeatReadNamer(func(set string, _ httpint.RepeatReadNoise, owned map[string][]*models.Mock) map[string][]models.RepeatRead {
		gotSet, gotOwned = set, map[string][]string{}
		for test, mks := range owned {
			for _, mk := range mks {
				gotOwned[test] = append(gotOwned[test], mk.Name)
			}
		}
		return map[string][]models.RepeatRead{
			"o::TestA": {
				{Name: "onboarding", Mocks: []string{"mock-1", "mock-2"}},
				{Name: "once", Mocks: []string{"mock-1"}},
				{Name: "", Mocks: []string{"mock-1", "mock-2"}},
				{Name: "not-held", Mocks: []string{"mock-1", "mock-4"}},
				{Name: "another-tests", Mocks: []string{"mock-1", "mock-3"}},
			},
			"o::TestB": {{Name: "list", Mocks: []string{"mock-3", "mock-5"}}},
		}
	})
	defer unregister()
	got := named()
	if gotSet != "o" || !reflect.DeepEqual(gotOwned, map[string][]string{"o::TestA": {"mock-1", "mock-2"}, "o::TestB": {"mock-3", "mock-5"}}) {
		t.Fatalf("the namer was handed %q %v", gotSet, gotOwned)
	}
	want := map[string][]models.RepeatRead{
		"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-1", "mock-2"}}},
		"o::TestB": {{Name: "list", Mocks: []string{"mock-3", "mock-5"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("named %v, want %v", got, want)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a second namer registered")
			}
		}()
		RegisterRepeatReadNamer(func(string, httpint.RepeatReadNoise, map[string][]*models.Mock) map[string][]models.RepeatRead {
			return nil
		})
	}()
	unregister()
	if got := named(); got != nil {
		t.Fatalf("an unregistered namer named %v", got)
	}
	// An old registration's unregister leaves a newer one.
	again := RegisterRepeatReadNamer(func(string, httpint.RepeatReadNoise, map[string][]*models.Mock) map[string][]models.RepeatRead {
		return map[string][]models.RepeatRead{"o::TestB": {{Name: "list", Mocks: []string{"mock-3", "mock-5"}}}}
	})
	defer again()
	unregister()
	if got := named(); len(got) != 1 {
		t.Fatalf("an old unregister removed the newer namer: %v", got)
	}
	again()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a nil namer registered")
			}
		}()
		defer RegisterRepeatReadNamer(nil)()
	}()
}

// A replay under the switch hands the agent the reads the registered namer
// names, in the same push as the table (pushProvenTable ->
// namedRepeatReads): without them the agent serves each such read its exact
// count.
func TestAReplayPushesTheNamedRepeatedReadsWithTheTable(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	mdb := mapdb.New(zap.NewNop(), keployDir, "")
	ctx := context.Background()
	require.NoError(t, mdb.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"orders::TestPlace": {{Name: "mock-0"}, {Name: "mock-1"}}, "orders::TestNone": nil}))
	require.NoError(t, mdb.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
		"orders::TestPlace": {At: runnerT0}, "orders::TestNone": {At: runnerT0},
	}, nil, nil))
	read := models.RepeatRead{Name: "GET /api/onboarding", Mocks: []string{"mock-0", "mock-1"}}
	var gotSet string
	var gotNoise httpint.RepeatReadNoise
	defer RegisterRepeatReadNamer(func(set string, noise httpint.RepeatReadNoise, _ map[string][]*models.Mock) map[string][]models.RepeatRead {
		gotSet, gotNoise = set, noise
		return map[string][]models.RepeatRead{"orders::TestPlace": {read}}
	})()
	rinstr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	rcfg := instrConfig(rinstr, utils.Native, "go test -p 1 -json ./...")
	rcfg.Path, rcfg.Mock.OnMiss = keployDir, "fail"
	// The set's own keploy.yml entry names Referer noise: the namer proves
	// reads under it, as the agent matches the set's calls.
	// The run turns the static list off: the namer compares Traceparent.
	rcfg.Test.GlobalNoise.Testsets = config.TestsetNoise{"set": {"header": {"Referer": {}}}}
	rcfg.Test.DisableAutoHeaderNoise = true
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	require.NoError(t, New(zap.NewNop(), rinstr, named("mock-0", "mock-1"), mdb, nil, nil, rcfg).Replay(ctx))
	require.Equal(t, "set", gotSet)
	callWith := func(header, value string) string {
		call, _, ok := httpint.RepeatReadOf(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://crm.test/api/onboarding", Header: map[string]string{header: value}},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "{}"},
		}}, gotNoise)
		require.True(t, ok)
		return call
	}
	require.Equal(t, callWith("Referer", "/a"), callWith("Referer", "/b"), "the namer was not handed the set's header noise")
	require.NotEqual(t, callWith("Traceparent", "00-a"), callWith("Traceparent", "00-b"), "the namer was handed the static list test.disableAutoHeaderNoise turns off")
	require.Equal(t, []string{"mock-0", "mock-1"}, rinstr.table["orders::TestPlace"])
	require.Equal(t, map[string][]models.RepeatRead{"orders::TestPlace": {read}}, rinstr.repeats, "the replay pushes each test's named repeated reads")
}
