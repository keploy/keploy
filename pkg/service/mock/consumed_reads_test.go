package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A recording the application used in its test, again while keploy waited
// for the series the run owes, and once more as it stopped, is in the reads
// of all three: each read hands back only what was served since the one
// before. The end's read also carries a recording no earlier read saw. It is
// one recording consumed per name: the receipt, the replay summary and the
// metered count say six recordings, not the eight the reads carry, and its
// test's line names it once, with where it answered over the reads.
func TestARecordingEveryReadCarriesIsConsumedOnce(t *testing.T) {
	var metered []ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { metered = append(metered, o) })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	w := seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed)
	w[0].Seq, w[1].Seq = 1, 2
	inA, inB := w[0].Start.Add(time.Millisecond).UnixNano(), w[1].Start.Add(time.Millisecond).UnixNano()
	after := w[1].End.Add(time.Second).UnixNano()
	in := func(name string, seq uint64, at int64) models.MockState {
		return models.MockState{Name: name, Timestamp: at, AnsweredIn: []models.WindowAnswer{{Seq: seq, First: at}}}
	}
	outside := func(name string, at int64) models.MockState {
		return models.MockState{Name: name, Timestamp: at, AnsweredOutsideAt: at}
	}
	code, r, tr, logs, instr := seriesWaitRun(t, w, [][]models.MockState{
		{in("mock-0", 1, inA), in("mock-1", 2, inB), in("mock-6", 2, inB)},
		{outside("mock-3", after), outside("mock-0", after), outside("mock-5", after)},
	}, []models.MockState{outside("mock-0", after+1), outside("mock-4", after+1)}, runsOnForGood, 3)
	require.Zero(t, code)
	require.Equal(t, 3, instr.consumedReadCount, "the wait's first read, one wake-up, and the run's end")
	require.Equal(t, 6, r.Consumed)
	summary := logs.FilterMessage("mock replay summary").All()
	require.Len(t, summary, 1)
	require.Equal(t, int64(6), summary[0].ContextMap()["consumed"])
	require.Len(t, metered, 1)
	require.Equal(t, 6, metered[0].Consumed)
	require.Equal(t, 1, testLine(t, tr, "o::TestA").Consumed)
	for _, f := range metered[0].Mocks {
		if f.Flow == "o::TestA" {
			require.Equal(t, []models.MockState{{Name: "mock-0", Timestamp: inA, AnsweredIn: []models.WindowAnswer{{Seq: 1, First: inA}}, AnsweredOutsideAt: after}},
				f.Consumed, "at its first use, in its test, and its first answer outside every test")
		}
	}
}

// readsInstr is keploy mock's agent client handing each read of what it
// consumed the next of reads, then nothing, with no miss; it says it reads
// the session's tests as client tests as clientTestsInstr says.
type readsInstr struct {
	clientTestsInstr
	reads *[][]models.MockState
}

func (o readsInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	if len(*o.reads) == 0 {
		return nil, nil
	}
	read := (*o.reads)[0]
	*o.reads = (*o.reads)[1:]
	return read, nil
}

func (o readsInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}

// What the learn step learned on a recording two reads carry is what the
// agent kept of it last (its latest state carries what each pairing before
// learned: the agent's consumed of a session holds it): written onto it as
// that, and, where the agent does not read client tests, named once among
// what was not kept.
func TestALearnedRecordingTwoReadsCarryIsLearnedOnce(t *testing.T) {
	provenOn(t)
	first := models.MockState{Name: "mock-1", Timestamp: 10, ReqBodyNoise: map[string][]string{"body.ts": {}},
		Variables: models.MockVariables{"id": {Recorded: "41", Use: []string{"body.id"}}}}
	last := models.MockState{Name: "mock-1", Timestamp: 20, ReqBodyNoise: map[string][]string{"body.ts": {}, "body.id": {}},
		Variables: models.MockVariables{"id": {Recorded: "42", Use: []string{"body.id"}}}}
	for _, reads := range []bool{true, false} {
		t.Run(map[bool]string{true: "an agent that reads client tests", false: "an agent that does not"}[reads], func(t *testing.T) {
			store := &learnedStore{}
			cfg := &config.Config{}
			cfg.Test.Learn = true
			core, logs := observer.New(zap.WarnLevel)
			queue := [][]models.MockState{{first}, {last}}
			m := &mockService{config: cfg, mockDB: store, logger: zap.New(core), instrumentation: readsInstr{clientTestsInstr{reads: reads}, &queue}}
			m.readOwedSeries(context.Background(), nil, nil)
			counts := m.reportOutcome(context.Background(), 1, replayDetail{set: "s"}, func() bool { return false })
			require.Equal(t, 1, counts.consumed)
			if reads {
				require.NoError(t, counts.learnErr)
				require.Len(t, store.got, 1)
				require.Equal(t, last.ReqBodyNoise, store.got["mock-1"].ReqBodyNoise, "the agent's latest state, as before")
				require.Equal(t, last.Variables, store.got["mock-1"].Variables, "the variables its latest read carries")
				require.Equal(t, first.Timestamp, store.got["mock-1"].Timestamp, "at its first use")
				return
			}
			require.Error(t, counts.learnErr)
			kept := logs.FilterMessageSnippet("keploy kept nothing its agent learned").All()
			require.Len(t, kept, 1)
			require.Equal(t, []interface{}{"mock-1 body.id (noise)", "mock-1 body.ts (noise)", "mock-1 id (variable)"}, kept[0].ContextMap()["notKept"])
		})
	}
}

// Under docker compose no read is made before the account the agent left as
// it stopped, so nothing a wait for the series the run owes read can be
// missing from it: that wait runs only under the proven switch, which
// refuses a docker command before anything starts (refuseProvenDocker), and
// the compose project's runner is never handed it (startComposeApp). The
// account is what the run consumed. The same set replayed natively under the
// switch is waited for (the control: its wait reads twice, its end once).
func TestUnderComposeTheAccountIsTheRunsOnlyRead(t *testing.T) {
	w := seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed)
	for _, tc := range []struct {
		name     string
		cmd      utils.CmdType
		proven   bool
		reads    int
		consumed any
	}{
		{name: "natively, under the switch", cmd: utils.Native, proven: true, reads: 3, consumed: int64(5)},
		{name: "under compose", cmd: utils.DockerCompose, reads: 0, consumed: int64(5)},
		{name: "under compose, under the switch", cmd: utils.DockerCompose, proven: true, reads: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.proven {
				provenOn(t)
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(seriesMappings), 0o644))
			core, logs := observer.New(zapcore.DebugLevel)
			lifetime, command := agentUpFromSetup, "go test -p 1 -json ./..."
			if tc.cmd == utils.DockerCompose {
				lifetime, command = agentUpAfterRun, "docker compose up"
			}
			instr := newInstr(t, lifetime, false, models.AppError{AppErrorType: models.ErrAppStopped})
			instr.runUntilReady = tc.cmd == utils.DockerCompose
			instr.consumedReads = [][]models.MockState{{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}}, {{Name: "mock-3"}, {Name: "mock-0"}, {Name: "mock-5"}}}
			instr.windows, instr.runsOn = w, runsOnForGood
			instr.leftOutcome = models.MockOutcome{Consumed: []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}, {Name: "mock-3"}, {Name: "mock-5"}}, Windows: w}
			cfg := instrConfig(instr, tc.cmd, command)
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-3", "mock-4", "mock-5", "mock-6", "mock-7"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
			require.Equal(t, tc.reads, instr.consumedReadCount)
			summary := logs.FilterMessageSnippet("mock replay summary").All()
			if tc.consumed == nil {
				require.Empty(t, summary, "refused before anything ran")
				return
			}
			require.Len(t, summary, 1)
			require.Equal(t, tc.consumed, summary[0].ContextMap()["consumed"])
		})
	}
}

// Whatever reads the run made before the account a docker compose agent left
// as it stopped, the account is added to them as the end's read is: a
// recording both carry is consumed once, and none the earlier read alone
// carries is lost.
func TestTheComposeAccountJoinsTheReadsBeforeIt(t *testing.T) {
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedReads = [][]models.MockState{{{Name: "mock-0", Timestamp: 10}, {Name: "mock-2", Timestamp: 11}}}
	instr.leftOutcome = models.MockOutcome{Consumed: []models.MockState{{Name: "mock-0", Timestamp: 20}, {Name: "mock-1", Timestamp: 21}}}
	cfg := instrConfig(instr, utils.DockerCompose, "docker compose up")
	m := &mockService{logger: zap.NewNop(), instrumentation: instr, config: cfg}
	m.readOwedSeries(context.Background(), nil, nil)
	counts := m.reportOutcome(context.Background(), 3, replayDetail{set: "s"}, func() bool { return false })
	require.Equal(t, 3, counts.consumed)
}

// A recording several reads carry is one entry, as the agent keeps one it
// served twice within a read: in the place its first read listed it, at its
// first use, with its latest state otherwise (which carries what its earlier
// pairings learned: the agent's later read of a recording does), the
// variables, clocks and binds record's full round learned on it included;
// and learned after the learn step ended when its latest read says so, any
// of that read's consumes of it, as the agent says so of one drain. One read
// alone carries is as it read.
func TestConsumedReadsKeepOneEntryPerRecording(t *testing.T) {
	vars := func(v string) models.MockVariables {
		return models.MockVariables{"id": {Recorded: v, Use: []string{"body.id"}}}
	}
	clocks := func(d time.Duration) map[string]models.ClockOffset {
		return map[string]models.ClockOffset{"body.at": {Min: -d, Max: d, Res: time.Second}}
	}
	binds := func(v string) map[string]models.MockVariables { return map[string]models.MockVariables{"a": vars(v)} }
	var c consumedReads
	require.Nil(t, c.list(), "no read handed back any")
	c.add([]models.MockState{
		{Name: "a", Timestamp: 1, LearnedAfterEnd: true, Variables: vars("1"), Clocks: clocks(time.Second), Binds: binds("1")},
		{Name: "b", Timestamp: 2, Usage: models.Updated},
	})
	c.add(nil)
	c.add([]models.MockState{
		{Name: "c", Timestamp: 3},
		{Name: "a", Timestamp: 4, ReqBodyNoise: map[string][]string{"body.id": {}}, Variables: vars("2"), Clocks: clocks(2 * time.Second), Binds: binds("2")},
	})
	c.add([]models.MockState{
		{Name: "b", Timestamp: 5, Usage: models.Deleted, LearnedAfterEnd: true},
		{Name: "a", Timestamp: 6, ReqBodyNoise: map[string][]string{"body.id": {}}, Variables: vars("3"), Clocks: clocks(3 * time.Second), Binds: binds("3")},
		{Name: "d", Timestamp: 7, LearnedAfterEnd: true},
		{Name: "b", Timestamp: 8, Usage: models.Deleted},
		{Name: "d", Timestamp: 9},
	})
	require.Equal(t, []models.MockState{
		{Name: "a", Timestamp: 1, ReqBodyNoise: map[string][]string{"body.id": {}}, Variables: vars("3"), Clocks: clocks(3 * time.Second), Binds: binds("3")},
		{Name: "b", Timestamp: 2, Usage: models.Deleted, LearnedAfterEnd: true},
		{Name: "c", Timestamp: 3},
		{Name: "d", Timestamp: 7, LearnedAfterEnd: true},
	}, c.list(), "a's first read alone says it learned after the learn step ended; b's and d's latest read says so of one of its consumes")
}

// A service that replays again starts its run's consumed afresh: its second
// run's receipt and summary count only what that run's reads carried, none
// of the first run's.
func TestASecondRunCountsOnlyItsOwnReads(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedMocks = []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = keployDir
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(zap.New(core), instr, named("mock-0", "mock-1", "mock-3"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg)
	var consumed []any
	for _, read := range [][]models.MockState{instr.consumedMocks, {{Name: "mock-3"}}} {
		instr.consumedMocks = read
		_ = svc.Replay(context.Background())
		for _, e := range logs.TakeAll() {
			if e.Message == "mock replay summary" {
				consumed = append(consumed, e.ContextMap()["consumed"])
			}
		}
		require.Equal(t, len(read), mustReceipt(t, keployDir).Consumed)
	}
	require.Equal(t, []any{int64(2), int64(1)}, consumed)
}
