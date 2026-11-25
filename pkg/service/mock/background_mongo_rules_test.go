package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type bgMongoInstr struct {
	*gatedInstr
	gotRules []models.BackgroundMongoRule
	called   bool
}

func (f *bgMongoInstr) PushProvenTableWithStartupRules(ctx context.Context, table, pool, worker map[string][]string, skippedReads, trails map[string]string, windows []models.TestWindow, given map[string][]models.GivenValue, repeats map[string][]models.RepeatRead, notRequired map[string][]string, background []string, series map[string][]string, sources map[string][]models.DeclaredSource, once []string, every map[string]string, gates map[string]models.SeriesGate, timeForm, _ bool) error {
	return f.PushProvenTable(ctx, table, pool, worker, skippedReads, trails, windows, given, repeats, notRequired, background, series, sources, once, every, gates, timeForm)
}

func (f *bgMongoInstr) PushProvenTableWithBackgroundMongo(ctx context.Context, table, pool, worker map[string][]string, skippedReads, trails map[string]string, windows []models.TestWindow, given map[string][]models.GivenValue, repeats map[string][]models.RepeatRead, notRequired map[string][]string, background []string, series map[string][]string, sources map[string][]models.DeclaredSource, once []string, every map[string]string, gates map[string]models.SeriesGate, timeForm, startupRules bool, backgroundMongo []models.BackgroundMongoRule) error {
	f.called = true
	f.gotRules = models.CloneBackgroundMongoRules(backgroundMongo)
	return f.PushProvenTableWithStartupRules(ctx, table, pool, worker, skippedReads, trails, windows, given, repeats, notRequired, background, series, sources, once, every, gates, timeForm, startupRules)
}

func TestKeployMockReplayForwardsBackgroundMongoRules(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(x2Mappings), 0o644))
	mongo := func(name string) *models.Mock {
		return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.Mongo, Name: name}
	}
	db := namedMockDB{mocks: []*models.Mock{mongo("mock-0"), mongo("mock-326"), mongo("mock-463"), mongo("mock-3733"), mongo("mock-3734"), mongo("mock-3735"), mongo("mock-dup"), httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT")}}
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return nil, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })

	want := []models.BackgroundMongoRule{{
		ID:          "ui-join-backfill-index-ready",
		DB:          "api-server",
		Command:     "listIndexes",
		Collections: []string{"uiTestSetJoins", "testsets"},
	}}

	instr := &bgMongoInstr{gatedInstr: &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	cfg.Mock.BackgroundMongo = want
	cfg.Mock.BackgroundRulesFilter = func(*models.Mapping, []*models.Mock) ([]models.BackgroundMongoRule, []models.BackgroundHTTPRule) {
		return want, nil
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.True(t, instr.called, "expected Replay to call PushProvenTableWithBackgroundMongo")
	require.Equal(t, want, instr.gotRules)
}

func TestUncoveredBackgroundRulesFailsClosedWithoutFilterOrMapping(t *testing.T) {
	mp := &models.Mapping{
		Worker: []models.MockEntry{
			{Name: "mock-1", Rule: "covered-mongo@12345678"},
			{Name: "mock-2", Rule: "covered-http@abcdef01"},
		},
		Startup: []models.MockEntry{
			{Name: "mock-3", Rule: "startup-mongo@99999999"},
		},
	}
	mongoIn := []models.BackgroundMongoRule{
		{ID: "covered-mongo", DB: "api-server", Command: "find", Collections: []string{"a"}},
		{ID: "startup-mongo", DB: "api-server", Command: "find", Collections: []string{"b"}},
		{ID: "uncovered-mongo", DB: "api-server", Command: "find", Collections: []string{"c"}},
	}
	httpIn := []models.BackgroundHTTPRule{
		{ID: "covered-http", Method: "GET", Host: "localhost:8200", Path: "/v1/sys/health"},
		{ID: "uncovered-http", Method: "GET", Host: "localhost:8200", Path: "/v1/sys/ready"},
	}

	// Nil filter fails closed (returns nil for both protocols) so rules are
	// never forwarded without full coverage evaluation against mp and decoded.
	gotM, gotH := uncoveredBackgroundRules(mp, nil, mongoIn, httpIn, nil)
	require.Nil(t, gotM)
	require.Nil(t, gotH)

	// Nil mapping fails closed (returns nil for both protocols).
	nilM, nilH := uncoveredBackgroundRules(nil, nil, mongoIn, httpIn, func(*models.Mapping, []*models.Mock) ([]models.BackgroundMongoRule, []models.BackgroundHTTPRule) {
		t.Fatal("filter must not run when mapping is nil")
		return mongoIn, httpIn
	})
	require.Nil(t, nilM)
	require.Nil(t, nilH)

	// When a custom filter evaluates per-(rule, collection) coverage, its
	// trimmed multi-collection Mongo rule must be preserved even though a
	// worker: entry carries the same rule ID for a covered sibling collection.
	trimmed := []models.BackgroundMongoRule{{
		ID:          "covered-mongo",
		DB:          "api-server",
		Command:     "listIndexes",
		Collections: []string{"uiTestSetJoins", "testsets"},
	}}
	gotFilteredM, gotFilteredH := uncoveredBackgroundRules(mp, nil, mongoIn, httpIn, func(*models.Mapping, []*models.Mock) ([]models.BackgroundMongoRule, []models.BackgroundHTTPRule) {
		return trimmed, []models.BackgroundHTTPRule{httpIn[1]}
	})
	require.Equal(t, trimmed, gotFilteredM)
	require.Equal(t, []models.BackgroundHTTPRule{httpIn[1]}, gotFilteredH)
}

type bgRulesInstr struct {
	*bgMongoInstr
	gotMongo   []models.BackgroundMongoRule
	gotHTTP    []models.BackgroundHTTPRule
	rulesCalls int
}

func (f *bgRulesInstr) PushProvenTableWithBackgroundRules(ctx context.Context, table, pool, worker map[string][]string, skippedReads, trails map[string]string, windows []models.TestWindow, given map[string][]models.GivenValue, repeats map[string][]models.RepeatRead, notRequired map[string][]string, background []string, series map[string][]string, sources map[string][]models.DeclaredSource, once []string, every map[string]string, gates map[string]models.SeriesGate, timeForm, startupRules bool, backgroundMongo []models.BackgroundMongoRule, backgroundHTTP []models.BackgroundHTTPRule) error {
	f.rulesCalls++
	f.gotMongo = models.CloneBackgroundMongoRules(backgroundMongo)
	f.gotHTTP = models.CloneBackgroundHTTPRules(backgroundHTTP)
	return f.PushProvenTableWithBackgroundMongo(ctx, table, pool, worker, skippedReads, trails, windows, given, repeats, notRequired, background, series, sources, once, every, gates, timeForm, startupRules, backgroundMongo)
}

func TestKeployMockReplayForwardsBackgroundRulesAndRunsFilter(t *testing.T) {
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(x2Mappings), 0o644))
	mongo := func(name string) *models.Mock {
		return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.Mongo, Name: name}
	}
	db := namedMockDB{mocks: []*models.Mock{mongo("mock-0"), mongo("mock-326"), mongo("mock-463"), mongo("mock-3733"), mongo("mock-3734"), mongo("mock-3735"), mongo("mock-dup"), httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT")}}
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return nil, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })

	wantMongo := []models.BackgroundMongoRule{{
		ID:          "ui-join-backfill-lease",
		DB:          "api-server",
		Command:     "insert",
		Collections: []string{"janitor_leases"},
		Where:       []models.BackgroundMongoWhere{{Field: "body.docs.msgs.0._id", Prefix: "ui-join-backfill:"}},
		PeriodBy:    "body.docs.msgs.0._id",
	}}
	wantHTTP := []models.BackgroundHTTPRule{{
		ID:     "openbao-liveness-check",
		Method: "GET",
		Host:   "localhost:8200",
		Path:   "/v1/sys/health",
	}}
	filterCalled := false

	instr := &bgRulesInstr{bgMongoInstr: &bgMongoInstr{gatedInstr: &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}}}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	cfg.Mock.BackgroundMongo = wantMongo
	cfg.Mock.BackgroundHTTP = wantHTTP
	cfg.Mock.BackgroundRulesFilter = func(mp *models.Mapping, decoded []*models.Mock) ([]models.BackgroundMongoRule, []models.BackgroundHTTPRule) {
		filterCalled = true
		require.NotNil(t, mp)
		require.NotEmpty(t, decoded)
		return wantMongo, wantHTTP
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))
	require.True(t, filterCalled, "expected BackgroundRulesFilter to run")
	require.Equal(t, 1, instr.rulesCalls, "expected Replay to call PushProvenTableWithBackgroundRules")
	require.Equal(t, wantMongo, instr.gotMongo)
	require.Equal(t, wantHTTP, instr.gotHTTP)
}
