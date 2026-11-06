package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// gatedInstr is a composeInstr that takes the proven gate (ScopeSkipPusher).
type gatedInstr struct {
	*composeInstr
	skip map[string]string
}

func (g *gatedInstr) PushScopeSkips(_ context.Context, skip map[string]string, _ map[string][]models.ScopeNeed) error {
	g.skip = skip
	return nil
}

func httpMock(name, method string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: name,
		Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: models.Method(method), URL: "http://localhost:8200/v1/secret/data/pat"}}}
}

// A read that a test the gate skips recorded (the app made it once and kept
// the answer: a token it looks up and caches) answers whichever test makes it
// once that test is skipped: it is the worker scope's for the replay, under
// the skipped test's key, at most once and required of no test. Without it
// the read misses in the next test's window, which is set aside in turn, and
// the read moves on to the test after it until no test is kept. The skipped
// test's write stays its own, served to no test, and a test that runs keeps
// its reads.
func TestASkippedTestsReadAnswersWhicheverTestMakesIt(t *testing.T) {
	provenOn(t)
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-put\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\nworker:\n    - name: mock-bg\n      key: o\n"), 0o644))
	instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET"), httpMock("mock-bg", "POST")}}
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

	require.Equal(t, map[string]string{"o::TestCRUD": "flaky: flipped"}, instr.skip, "the gate is pushed as read")
	require.Equal(t, []string{"mock-put"}, instr.table["o::TestCRUD"], "the skipped test's write stays its own")
	require.Equal(t, []string{"mock-own"}, instr.table["o::TestList"], "a test that runs keeps its reads")
	require.Equal(t, map[string][]string{"o": {"mock-bg", "mock-pat"}}, instr.worker, "the skipped test's read answers whichever test makes it, beside worker:")
	require.Equal(t, map[string]string{"mock-pat": "o::TestCRUD"}, instr.skippedReads, "the agent is told which of worker: is a skipped test's read, and whose: it answers a test that recorded the read too, whose extra-call line names the skipped test")
	require.ElementsMatch(t, []string{"mock-bg", "mock-pat"}, instr.pool["o"])

	// A replay that skips no test serves the read to its own test alone.
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return nil, nil, nil
	})
	instr2 := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg2 := instrConfig(instr2.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg2.Path, cfg2.Mock.OnMiss = keployDir, "fail"
	require.NoError(t, New(zap.NewNop(), instr2, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg2).Replay(context.Background()))
	require.Equal(t, []string{"mock-pat", "mock-put"}, instr2.table["o::TestCRUD"])
	require.Equal(t, map[string][]string{"o": {"mock-bg"}}, instr2.worker)
	require.Empty(t, instr2.skippedReads, "a replay that skips no test names no skipped read")
}

// A skipped test's read that keploy's judging marked served, not required
// moves to worker: as an unmarked one does, and is marked for no test: the
// mark is its test's own, and the read is no longer in that test's list.
// The skipped test's write keeps its mark under its test, and a test that
// runs keeps its marks.
func TestASkippedTestsMarkedReadMovesToWorkerAndIsMarkedForNoTest(t *testing.T) {
	provenOn(t)
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n          notRequired: served, not required\n        - name: mock-put\n          notRequired: served, not required\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\n          notRequired: served, not required\n"), 0o644))
	instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET")}}
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

	require.Equal(t, map[string][]string{"o": {"mock-pat"}}, instr.worker, "the skipped test's marked read answers whichever test makes it")
	require.Equal(t, map[string]string{"mock-pat": "o::TestCRUD"}, instr.skippedReads)
	require.Equal(t, map[string][]string{"o::TestCRUD": {"mock-put"}, "o::TestList": {"mock-own"}}, instr.notRequired, "the moved read is marked for no test; the write and the running test keep their marks")
}

// A skipped test's read that a test that runs lists too (one recording under
// two tests, as an edit that copies an entry makes) leaves the skipped test's
// list and stays the running test's: it joins no worker slice, so it answers
// only that test's window, and that test keeps its mark on it. One recording
// answers at most one call: served to whichever test makes the call first, it
// would be taken from the test that lists it.
func TestASkippedReadATestThatRunsListsStaysThatTestsWithItsMark(t *testing.T) {
	provenOn(t)
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-put\n          notRequired: served, not required\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n          notRequired: served, not required\n        - name: mock-own\n"), 0o644))
	instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET")}}
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

	require.Equal(t, []string{"mock-put"}, instr.table["o::TestCRUD"], "the read leaves the skipped test's list")
	require.Equal(t, []string{"mock-pat", "mock-own"}, instr.table["o::TestList"], "the running test keeps its read")
	require.Empty(t, instr.worker, "a read a running test lists joins no worker slice: it answers only that test's window")
	require.Equal(t, map[string]string{"mock-pat": "o::TestCRUD"}, instr.skippedReads)
	require.Equal(t, map[string][]string{"o::TestCRUD": {"mock-put"}, "o::TestList": {"mock-pat"}}, instr.notRequired, "the running test keeps its mark on the shared read")
}

// A call one past a skipped test's read (the agent's note: SkippedRead)
// names, on the replay's receipt, the skipped test the read was moved from
// (RepeatNote.SkippedTest), so judging can say which test the call is one
// more than: the per-test table the replay pushed no longer lists the read
// under that test, so the miss names no RecordedIn. A note whose read the
// replay did not move names none, nor does any note when the replay skips
// no test. The agent's windows are read, never written.
func TestTheReceiptNamesTheSkippedTestACallPastItsReadIsOneMoreThan(t *testing.T) {
	for _, skips := range []bool{true, false} {
		t.Run(map[bool]string{true: "the gate skips TestCRUD", false: "the gate skips none"}[skips], func(t *testing.T) {
			provenOn(t)
			RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
				if !skips {
					return nil, nil, nil
				}
				return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
			})
			t.Cleanup(func() { RegisterScopeSkipSource(nil) })
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-put\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\n"), 0o644))
			instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
			instr.consumedMocks = []models.MockState{{Name: "mock-own"}, {Name: "mock-pat"}}
			misses := []models.UnmatchedCall{
				{Protocol: "HTTP", ActualSummary: "GET /v1/secret/data/pat", Destination: "openbao:8200", ClosestMock: "mock-pat", Repeat: &models.RepeatNote{Mock: "mock-pat", SkippedRead: true}},
				{Protocol: "HTTP", ActualSummary: "GET /v1/secret/data/own", Destination: "openbao:8200", ClosestMock: "mock-own", Repeat: &models.RepeatNote{Mock: "mock-own", SkippedRead: true}},
				{Protocol: "HTTP", ActualSummary: "GET /v1/secret/data/repeat", Destination: "openbao:8200", ClosestMock: "mock-pat", Repeat: &models.RepeatNote{Mock: "mock-pat", Test: "o::TestList"}},
				{Protocol: "HTTP", ActualSummary: "GET /v1/secret/data/none", Destination: "openbao:8200", ClosestMock: "mock-pat"},
			}
			instr.windows = []models.ScopeWindow{
				{Name: "o::TestList", Outcome: models.ScopeOutcomeFailed, Evidence: &models.ScopeEvidence{Misses: misses}},
			}
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET")}}
			_ = New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
			tests, err := ReadTestsReceipt(keployDir, "set")
			require.NoError(t, err)
			require.NotNil(t, tests, "the replay writes what each test did")
			got := map[string]string{}
			for _, tr := range tests.Tests {
				if tr.Evidence == nil {
					continue
				}
				for _, c := range tr.Evidence.Misses {
					named := "no note"
					if c.Repeat != nil {
						named = c.Repeat.SkippedTest
					}
					got[tr.Name+" "+c.ActualSummary] = named
				}
			}
			// The repeat of a test's own recording of the moved read is no
			// call past a skipped read: it names no skipped test.
			want := map[string]string{"o::TestList GET /v1/secret/data/pat": "", "o::TestList GET /v1/secret/data/own": "", "o::TestList GET /v1/secret/data/repeat": "", "o::TestList GET /v1/secret/data/none": "no note"}
			if skips {
				want["o::TestList GET /v1/secret/data/pat"] = "o::TestCRUD"
			}
			require.Equal(t, want, got, "only a call past a read the replay moved from a skipped test names that test")
			require.Empty(t, misses[0].Repeat.SkippedTest, "the agent's window is read, never written")
		})
	}
}

// A read two skipped tests list moves to worker: under each one's key and
// is named with the first of them by name, whichever order the table is
// read in: the name a test's extra-call line gives it never moves from run
// to run. A test that runs keeps its reads, and a skipped test its write.
func TestASkippedReadTwoSkippedTestsListIsNamedWithTheFirstByName(t *testing.T) {
	mp := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "o::TestB", Key: "b"}, {ID: "o::TestA", Key: "a"}, {ID: "o::TestC", Key: "c"}}}
	table := map[string][]string{"o::TestB": {"mock-r"}, "o::TestA": {"mock-r", "mock-w"}, "o::TestC": {"mock-c"}}
	skipped := map[string]string{"o::TestA": "flaky", "o::TestB": "flaky"}
	decoded := []*models.Mock{httpMock("mock-r", "GET"), httpMock("mock-w", "PUT"), httpMock("mock-c", "GET")}
	served, out, moved := skippedReads(mp, table, skipped, decoded)
	require.Equal(t, map[string]string{"mock-r": "o::TestA"}, moved)
	require.ElementsMatch(t, []models.MockEntry{{Name: "mock-r", Key: "a"}, {Name: "mock-r", Key: "b"}}, served.Worker)
	require.Equal(t, map[string][]string{"o::TestA": {"mock-w"}, "o::TestB": {}, "o::TestC": {"mock-c"}}, out)
}

// readOnly knows a read by its HTTP method alone; a call of any other
// protocol is not known to read only.
func TestReadOnlyKnowsAnHTTPReadByItsMethod(t *testing.T) {
	for _, m := range []string{"GET", "get", "HEAD", "OPTIONS"} {
		require.True(t, readOnly(httpMock("m", m)), m)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", ""} {
		require.False(t, readOnly(httpMock("m", m)), m)
	}
	require.False(t, readOnly(&models.Mock{Kind: models.Mongo, Name: "m", Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET"}}}), "a request of another kind is not read as HTTP")
	require.False(t, readOnly(&models.Mock{Kind: models.HTTP, Name: "m"}), "an HTTP mock with no request is not known to read")
	require.False(t, readOnly(nil))
}

// The registered namer of repeated reads is handed each test's own
// recordings as the replay serves them: a read a skipped test recorded is
// the worker scope's for the replay, no longer the test's, so it is not
// handed over as the skipped test's, and a read named of it is not sent.
func TestANamerIsNotHandedTheReadsASkippedTestMovedToWorker(t *testing.T) {
	provenOn(t)
	RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
		return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
	})
	t.Cleanup(func() { RegisterScopeSkipSource(nil) })
	handed := map[string][]string{}
	t.Cleanup(RegisterRepeatReadNamer(func(_ string, _ httpint.RepeatReadNoise, owned map[string][]*models.Mock) map[string][]models.RepeatRead {
		for test, mocks := range owned {
			for _, mk := range mocks {
				handed[test] = append(handed[test], mk.Name)
			}
		}
		return map[string][]models.RepeatRead{"o::TestCRUD": {{Name: "GET /v1/secret/data/pat", Mocks: []string{"mock-pat", "mock-pat-2"}}}}
	}))
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-pat-2\n        - name: mock-put\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\n"), 0o644))
	instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-pat-2", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET")}}
	require.NoError(t, New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background()))

	require.Equal(t, map[string]string{"mock-pat": "o::TestCRUD", "mock-pat-2": "o::TestCRUD"}, instr.skippedReads, "precondition: both reads moved to worker:")
	require.Equal(t, []string{"mock-put"}, handed["o::TestCRUD"], "the namer is handed the skipped test's write alone, not the reads moved to worker:")
	require.Equal(t, []string{"mock-own"}, handed["o::TestList"])
	require.Empty(t, instr.repeats, "a read named of the moved reads is not sent as the skipped test's")
}

// A test that passed on a skipped test's read (the agent's window:
// SkippedReads) says so on the replay's receipt, with the skipped test the
// read was moved from (TestReceipt.SkippedReads), so judging can tell it
// passed on a recording keploy's CI replay of the published set never
// loads. Every run of the test counts: a read that answered an earlier
// attempt answered a call the published set has no answer for. A name the
// replay did not move is left off, and so is every name when the replay
// skips no test.
func TestTheReceiptNamesTheSkippedReadsThatAnsweredATest(t *testing.T) {
	for _, skips := range []bool{true, false} {
		t.Run(map[bool]string{true: "the gate skips TestCRUD", false: "the gate skips none"}[skips], func(t *testing.T) {
			provenOn(t)
			RegisterScopeSkipSource(func(context.Context, ScopeGateRequest) (map[string]string, map[string][]models.ScopeNeed, error) {
				if !skips {
					return nil, nil, nil
				}
				return map[string]string{"o::TestCRUD": "flaky: flipped"}, nil, nil
			})
			t.Cleanup(func() { RegisterScopeSkipSource(nil) })
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestCRUD\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-pat\n        - name: mock-put\n    - id: o::TestList\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-own\n    - id: o::TestOther\n      dir: o\n      key: o\n      mock_entries:\n        - name: mock-other\n"), 0o644))
			instr := &gatedInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})}
			instr.consumedMocks = []models.MockState{{Name: "mock-own"}, {Name: "mock-pat"}, {Name: "mock-other"}}
			windows := []models.ScopeWindow{
				{Name: "o::TestList", Outcome: models.ScopeOutcomeFailed, SkippedReads: []string{"mock-pat"}},
				{Name: "o::TestList", Outcome: models.ScopeOutcomePassed, SkippedReads: []string{"mock-own"}},
				{Name: "o::TestOther", Outcome: models.ScopeOutcomePassed},
			}
			instr.windows = windows
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			db := namedMockDB{mocks: []*models.Mock{httpMock("mock-pat", "GET"), httpMock("mock-put", "PUT"), httpMock("mock-own", "GET"), httpMock("mock-other", "GET")}}
			_ = New(zap.NewNop(), instr, db, mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
			tests, err := ReadTestsReceipt(keployDir, "set")
			require.NoError(t, err)
			require.NotNil(t, tests, "the replay writes what each test did")
			got := map[string][]SkippedReadAnswer{}
			for _, tr := range tests.Tests {
				if tr.Name == "o::TestCRUD" {
					continue
				}
				got[tr.Name] = tr.SkippedReads
			}
			want := map[string][]SkippedReadAnswer{"o::TestList": nil, "o::TestOther": nil}
			if skips {
				want["o::TestList"] = []SkippedReadAnswer{{Mock: "mock-pat", Test: "o::TestCRUD"}}
			}
			require.Equal(t, want, got, "a test names each read the replay moved from a skipped test that answered it, in any of its runs")
			require.Equal(t, []string{"mock-pat"}, windows[0].SkippedReads, "the agent's window is read, never written")
		})
	}
}
