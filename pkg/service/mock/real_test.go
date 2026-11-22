package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// realAgentClient is an agent client that takes mock.real.
type realAgentClient struct {
	Instrumentation
	pushed []string
	calls  int
	run    models.RealRun
	err    error
}

func (c *realAgentClient) PushRealDeps(_ context.Context, real []string) error {
	c.calls++
	c.pushed = real
	return c.err
}

func (c *realAgentClient) RealRun(context.Context) (models.RealRun, error) { return c.run, c.err }

func realService(instr Instrumentation, real ...string) *mockService {
	cfg := &config.Config{}
	cfg.Mock.Real = real
	return &mockService{logger: zap.NewNop(), instrumentation: instr, config: cfg}
}

// Under the switch a mock.real keploy cannot read refuses the run before
// anything starts; without it the key is not read.
func TestCheckRealDepsRefusesAListKeployCannotRead(t *testing.T) {
	m := realService(nil, "localhost")
	t.Setenv(provenswitch.Env, "1")
	err := m.checkRealDeps()
	require.ErrorContains(t, err, "keploy.yml mock.real")
	require.NoError(t, realService(nil, "localhost:5432", "container:redis").checkRealDeps())
	t.Setenv(provenswitch.Env, "")
	require.NoError(t, m.checkRealDeps())
}

// The run's list is handed to the agent under the switch, and only then; an
// agent client that cannot take a list that names anything refuses the run.
func TestPushRealDepsHandsTheAgentTheList(t *testing.T) {
	ctx := context.Background()
	c := &realAgentClient{}
	t.Setenv(provenswitch.Env, "1")
	require.NoError(t, realService(c, "localhost:5432").pushRealDeps(ctx))
	require.Equal(t, []string{"localhost:5432"}, c.pushed)
	// An empty list is handed too: it clears what a reused agent kept. An
	// agent without the route has nothing to clear.
	require.NoError(t, realService(c).pushRealDeps(ctx))
	require.Equal(t, 2, c.calls)
	require.Empty(t, c.pushed)
	require.NoError(t, realService(&realAgentClient{err: models.ErrNoRealRoute}).pushRealDeps(ctx))
	require.ErrorIs(t, realService(&realAgentClient{err: models.ErrNoRealRoute}, "db:5432").pushRealDeps(ctx), models.ErrNoRealRoute)

	require.ErrorContains(t, realService(&setTablePusher{}, "localhost:5432").pushRealDeps(ctx), "cannot hand its agent mock.real")
	require.NoError(t, realService(&setTablePusher{}).pushRealDeps(ctx))
	c.err = errors.New("refused")
	require.ErrorIs(t, realService(c, "localhost:5432").pushRealDeps(ctx), c.err)

	t.Setenv(provenswitch.Env, "")
	c = &realAgentClient{}
	require.NoError(t, realService(c, "localhost:5432").pushRealDeps(ctx))
	require.Zero(t, c.calls, "without the switch the agent is never told")
}

func TestRealRunIsReadUnderTheSwitchOnly(t *testing.T) {
	ctx := context.Background()
	c := &realAgentClient{run: models.RealRun{Reached: []string{"db:5432"}}}
	t.Setenv(provenswitch.Env, "1")
	got, err := realService(c, "db:5432").realRun(ctx)
	require.NoError(t, err)
	require.Equal(t, &c.run, got)
	_, err = realService(&setTablePusher{}, "db:5432").realRun(ctx)
	require.Error(t, err, "a list keploy cannot ask about is not left unsaid")
	got, err = realService(&setTablePusher{}).realRun(ctx)
	require.NoError(t, err)
	require.Nil(t, got, "no list, nothing in the receipt")
	c.run = models.RealRun{Reached: []string{"x:1"}}
	got, err = realService(c).realRun(ctx)
	require.NoError(t, err)
	require.Nil(t, got, "with no list the agent is not asked")
	t.Setenv(provenswitch.Env, "")
	got, err = realService(c, "db:5432").realRun(ctx)
	require.NoError(t, err)
	require.Nil(t, got)
}

// Each test's recording lists the REAL destinations of every window it had.
func TestStampRealWritesEachTestsDestinations(t *testing.T) {
	byCase := map[string]models.MappedTestCase{"pkg::TestA": {Dir: "pkg"}}
	stampReal(byCase, []models.ScopeWindow{
		{Name: "pkg::TestA", Real: []models.RealUse{{Dest: "db:5432", Conns: 2}}},
		{Name: "pkg::TestA", Real: []models.RealUse{{Dest: "auth.example.com:443", Failed: 1}, {Dest: "db:5432", Conns: 1}}},
		{Name: "pkg::TestB"},
	})
	require.Equal(t, models.MappedTestCase{Dir: "pkg", Real: []string{"auth.example.com:443", "db:5432"}}, byCase["pkg::TestA"])
	_, ok := byCase["pkg::TestB"]
	require.False(t, ok, "a test with no REAL use gets no entry from it")
}

// Replay reads which tests run on the real clock from the recording.
func TestTestRealReadsTheRecordingsRealTests(t *testing.T) {
	require.Nil(t, testReal(nil))
	got := testReal(&models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "pkg::TestA", Real: []string{"db:5432"}},
		{ID: "pkg::TestB"},
		{ID: "pkg::TestA", Real: []string{"auth.example.com:443"}},
	}})
	require.Equal(t, map[string][]string{"pkg::TestA": {"auth.example.com:443", "db:5432"}}, got)
	require.Nil(t, testReal(&models.Mapping{TestCases: []models.MappedTestCase{{ID: "pkg::TestB"}}}))
}

func TestPushTestBeginsSendsTheRealTests(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "pkg::TestA", At: t0, Real: []string{"db:5432"}}},
	}}}
	require.NoError(t, m.pushTestBegins(context.Background(), "rec"))
	require.Equal(t, map[string][]string{"pkg::TestA": {"db:5432"}}, p.real)
}

// A test's line carries its REAL rows, every attempt's connections counted.
func TestWithRealPutsEachTestsRowsOnItsLine(t *testing.T) {
	tests := withReal([]TestReceipt{{Name: "pkg::TestA"}, {Name: "pkg::TestB"}}, []models.ScopeWindow{
		{Name: "pkg::TestA", Real: []models.RealUse{{Dest: "db:5432", Conns: 1}, {Dest: "auth.example.com:443", Conns: 1, H2: true}}},
		{Name: "pkg::TestA", Real: []models.RealUse{{Dest: "db:5432", Conns: 2, Failed: 1}}},
	})
	require.Equal(t, []models.RealUse{
		{Dest: "auth.example.com:443", Conns: 1, H2: true},
		{Dest: "db:5432", Conns: 3, Failed: 1},
	}, tests[0].Real)
	require.Nil(t, tests[1].Real)
}

// A test's evidence keeps what keploy could not capture in every attempt.
func TestWithEvidenceKeepsUncapturedConnections(t *testing.T) {
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "unreadable"}
	tests := withEvidence([]TestReceipt{{Name: "pkg::TestA"}}, []models.ScopeWindow{
		{Name: "pkg::TestA", Evidence: &models.ScopeEvidence{Incomplete: []models.CaptureIncomplete{inc}, Reason: "keploy: x"}},
	})
	require.NotNil(t, tests[0].Evidence)
	require.Equal(t, []models.CaptureIncomplete{inc}, tests[0].Evidence.Incomplete)
}

// A test's REAL rows and a capture loss compose on its line, in a replay's
// file as in a record's: neither hides the other.
func TestATestsLineCarriesItsRealRowsAndACaptureLoss(t *testing.T) {
	keployDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(keployDir, "set"), 0o755))
	lost := models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}
	inc := models.CaptureIncomplete{Class: models.CaptureIncompleteRealName, Destination: "10.0.0.7:443", Why: "unreadable"}
	use := []models.RealUse{{Dest: "db:5432", Conns: 1}}
	windows := []models.ScopeWindow{
		{Name: "pkg::TestA", Outcome: models.ScopeOutcomeFailed, Real: use, Evidence: &models.ScopeEvidence{
			CaptureIncomplete: lost, Incomplete: []models.CaptureIncomplete{inc}, Reason: "keploy: capture incomplete; x"}},
		{Name: "pkg::TestB", Outcome: models.ScopeOutcomeFailed, Real: use, Evidence: &models.ScopeEvidence{
			CaptureIncomplete: lost, Reason: "keploy: capture incomplete"}},
	}

	// A replay's file, as the replay builds its lines.
	tests := withRealListed(withReal(withEvidence([]TestReceipt{
		{Name: "pkg::TestA", Outcome: models.ScopeOutcomeFailed}, {Name: "pkg::TestB", Outcome: models.ScopeOutcomeFailed},
	}, windows), windows), map[string][]string{"pkg::TestA": {"db:5432"}, "pkg::TestB": {"db:5432"}})
	writeTestsReceipt(zap.NewNop(), keployDir, TestsReceipt{Set: "set", Tests: tests})
	got, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Tests, 2)
	for _, tr := range got.Tests {
		require.Equal(t, use, tr.Real, tr.Name)
		require.Equal(t, []string{"db:5432"}, tr.RealListed, tr.Name)
		require.NotNil(t, tr.Evidence, tr.Name)
		require.Equal(t, lost, tr.Evidence.CaptureIncomplete, tr.Name)
	}
	require.Equal(t, []models.CaptureIncomplete{inc}, got.Tests[0].Evidence.Incomplete)
	require.Empty(t, got.Tests[1].Evidence.Incomplete)

	// A record's file.
	writeRecordTestsReceipt(zap.NewNop(), keployDir, TestsReceipt{Set: "rec"}, windows)
	rec, err := ReadRecordTestsReceipt(keployDir, "rec")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Len(t, rec.Tests, 2)
	for _, tr := range rec.Tests {
		require.Equal(t, use, tr.Real, tr.Name)
		require.NotNil(t, tr.Evidence, tr.Name)
		require.Equal(t, lost, tr.Evidence.CaptureIncomplete, tr.Name)
	}
	require.Equal(t, []models.CaptureIncomplete{inc}, rec.Tests[0].Evidence.Incomplete)
}

// A replayed test's line carries what its recording lists, for rule 4 and the
// UNFROZEN rows; a test whose recording lists nothing carries nothing.
func TestWithRealListedPutsTheRecordingsListOnEachLine(t *testing.T) {
	tests := withRealListed([]TestReceipt{{Name: "pkg::TestA"}, {Name: "pkg::TestB"}}, map[string][]string{"pkg::TestA": {"db:5432"}})
	require.Equal(t, []string{"db:5432"}, tests[0].RealListed)
	require.Nil(t, tests[1].RealListed)
	require.Nil(t, withRealListed([]TestReceipt{{Name: "pkg::TestA"}}, nil)[0].RealListed)
}

func TestPushTestBeginsKeepsWhatEachRecordingLists(t *testing.T) {
	p := &beginsPusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "pkg::TestA", Real: []string{"db:5432"}}, {ID: "pkg::TestB"}},
	}}}
	require.NoError(t, m.pushTestBegins(context.Background(), "rec"))
	require.Equal(t, map[string][]string{"pkg::TestA": {"db:5432"}}, m.realListed)
}
