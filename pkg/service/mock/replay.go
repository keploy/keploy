package mock

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Replay runs the wrapped test command with the configured named mock set
// served in place of the real dependencies. It needs no incoming test cases —
// the runner drives the requests, Keploy answers the outgoing calls from the
// set. On a miss it applies the configured policy (fail / passthrough /
// record). It propagates the runner's exit code, and with --strict also exits
// non-zero when any recorded mock was missed.
func (m *mockService) Replay(ctx context.Context) error {
	name := m.setName()

	// 0. Materialise the set locally (registry download in enterprise; no-op on files).
	if err := m.store.Pull(ctx, name); err != nil {
		utils.LogError(m.logger, err, "failed to fetch the mock set", zap.String("mock-set", name))
		return err
	}

	policy := models.MissPolicy(m.config.Mock.OnMiss)
	if !policy.Valid() {
		return fmt.Errorf("invalid --on-miss value %q: allowed values are fail, passthrough, record", m.config.Mock.OnMiss)
	}

	m.logger.Info("Replaying mocks for your test command",
		zap.String("mock-set", name),
		zap.String("command", m.config.Command),
		zap.String("on-miss", string(policy)))

	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	ctx, cancel := context.WithCancel(ctx)

	defer func() {
		m.notifyShutdown()
		// Cancel so agent-monitor / app-runner goroutines unwind before drain.
		cancel()
		if err := utils.DrainErrGroup(m.logger, "mock-replay", errGrp, 30*time.Second); err != nil {
			utils.LogError(m.logger, err, "failed to drain mock-replay goroutines")
		}
	}()

	// 1. Instrument in mock (test) mode — no ingress port relocation.
	if err := m.instrumentation.Setup(ctx, m.config.Command, models.SetupOptions{
		Container:   m.config.ContainerName,
		CommandType: m.config.CommandType,
		DockerDelay: m.config.BuildDelay,
		BuildDelay:  m.config.BuildDelay,
		Mode:        models.MODE_TEST,
		MockMode:    true,
		ConfigPath:  m.config.ConfigPath,
	}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		utils.LogError(m.logger, err, "failed setting up the environment")
		return err
	}

	// 2. Put the proxy in mock-serving mode with the miss policy.
	//
	// Opting into strict enforcement is what makes a changed request VALUE
	// miss: the default request-body check is PerformBodyMatch -> bodyMatch,
	// which compares top-level key presence only, so a payload that keeps its
	// shape and changes a value is served the stale recorded response.
	//
	// NoiseConfig is forwarded only alongside it. It also feeds header noise,
	// so passing it unconditionally would loosen matching for anyone with an
	// existing test.globalNoise block; gating it keeps the default literal
	// byte-for-byte the zero-valued one this call has always used.
	//
	// SchemaNoiseDetection is deliberately NOT forwarded. Its rule is
	// "changed && !alreadyKnownNoise -> noise" on a single observation, and
	// MergeLearned is monotonic, so running the learner on the very PR that
	// broke something amnesties that field permanently. A learner pointed at a
	// gate defeats the gate.
	var mockNoiseConfig map[string]map[string][]string
	if m.config.Test.SchemaNoiseStrict {
		mockNoiseConfig = replay.PrepareMockNoiseConfig(m.config.Test.GlobalNoise.Global, m.config.Test.GlobalNoise.Testsets, name)
	}
	if err := m.instrumentation.MockOutgoing(ctx, models.OutgoingOptions{
		Rules:                     m.config.BypassRules,
		MongoPassword:             m.config.Test.MongoPassword,
		SQLDelay:                  time.Duration(m.config.Test.Delay) * time.Second,
		Mocking:                   true,
		OnMiss:                    policy,
		NoiseConfig:               mockNoiseConfig,
		SchemaNoiseStrict:         m.config.Test.SchemaNoiseStrict,
		MysqlPorts:                m.config.MysqlPorts,
		DisableMysqlAutoDetect:    m.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: m.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          m.config.Record.PassThroughPorts,
		PassThroughHosts:          m.config.Record.PassThroughHosts,
	}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		utils.LogError(m.logger, err, "failed to enable mock serving")
		return err
	}

	// 3. Load the whole set and push it into the proxy.
	empty := map[string]bool{}
	filtered, err := m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), empty, empty)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load per-test mocks", zap.String("mock-set", name))
		return err
	}
	unfiltered, err := m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), empty, empty)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load session mocks", zap.String("mock-set", name))
		return err
	}
	loaded := len(filtered) + len(unfiltered)
	if loaded == 0 {
		m.logger.Warn("the mock set is empty; the runner will hit a miss on every dependency call",
			zap.String("mock-set", name),
			zap.String("next_step", fmt.Sprintf("record it first with: keploy mock record -c %q --name %s", m.config.Command, name)))
	}
	if err := m.instrumentation.StoreMocks(ctx, filtered, unfiltered); err != nil {
		utils.LogError(m.logger, err, "failed to store mocks on the agent")
		return err
	}

	// 4. Hand the agent the per-test table so the runner's /agent/scope/begin
	//    calls can narrow the served pool per test (best-effort / optional).
	m.pushScopeTable(ctx, name)

	// 5. Stage the whole pool as the initial serving window (BaseTime..now, no
	//    mapping) — same call RunTestSet makes before the first test.
	if err := m.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		AfterTime:  models.BaseTime,
		BeforeTime: time.Now(),
	}); err != nil {
		utils.LogError(m.logger, err, "failed to arm the mock pool")
		return err
	}

	if err := m.instrumentation.MakeAgentReadyForDockerCompose(ctx); err != nil {
		m.logger.Debug("failed to make agent ready for docker compose", zap.Error(err))
	}

	// 6. Run the wrapped runner against the served mocks and block until it exits.
	appErr := m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})

	if ctx.Err() != nil { // user Ctrl+C
		return nil
	}

	// 7. Under --on-miss record, append any calls served live-from-upstream to
	//    the set so the next replay serves them from the mock (VCR new_episodes).
	if policy.RecordsOnMiss() {
		m.persistCaptured(context.WithoutCancel(ctx), name)
	}

	// 8. Summarise what was served and missed.
	missed := m.reportOutcome(ctx, loaded)

	// 9. Exit code: mirror the runner; with --strict also fail on any miss.
	m.propagateExit(appErr, "replay")
	if m.config.Mock.Strict && missed > 0 && utils.ErrCode == 0 {
		utils.ErrCode = 1
		m.logger.Error("replay failed under --strict: recorded dependency calls were missed",
			zap.Int("missed", missed),
			zap.String("next_step", "a dependency contract drifted; re-record the set (keploy mock record) or add the new calls with --on-miss record"))
	}
	return nil
}

// persistCaptured appends any calls the proxy captured on miss (served live from
// the real dependency under --on-miss record) to the set, then republishes it.
func (m *mockService) persistCaptured(ctx context.Context, name string) {
	drainer, ok := m.instrumentation.(MissCapturer)
	if !ok {
		return
	}
	captured, err := drainer.DrainCapturedMocks(ctx)
	if err != nil {
		m.logger.Debug("failed to drain captured-on-miss mocks", zap.Error(err))
		return
	}
	if len(captured) == 0 {
		return
	}
	// Seed the name counter to the set's highest existing mock-N so appended
	// mocks get fresh names (InsertMock always renames to mock-<counter+1>);
	// without this the replay-side counter starts at 0 and appended mocks reuse
	// mock-0, mock-1, … colliding with the recorded set (consumed-mock tracking
	// and mappings both key on name).
	m.mockDB.SetCounterID(m.highestMockIndex(ctx, name))
	appended := 0
	for _, mk := range captured {
		if mk == nil {
			continue
		}
		if err := m.mockDB.InsertMock(ctx, mk, name); err != nil {
			m.logger.Debug("failed to append a captured-on-miss mock", zap.Error(err))
			continue
		}
		appended++
	}
	if appended > 0 {
		m.logger.Info("appended new dependency calls to the mock set (--on-miss record)",
			zap.Int("new", appended), zap.String("mock-set", name),
			zap.String("next_step", "review the added mocks and commit them; the next replay serves them without the real dependency"))
		if err := m.store.Push(ctx, name); err != nil {
			m.logger.Warn("failed to publish the refreshed mock set", zap.Error(err))
		}
	}
}

// highestMockIndex returns the largest N across the set's existing "mock-N"
// names, or -1 when the set is empty / has no mock-N names. Seeding the counter
// to this value makes the next InsertMock name its mock "mock-<N+1>".
func (m *mockService) highestMockIndex(ctx context.Context, name string) int64 {
	all := map[string]bool{}
	filtered, _ := m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	unfiltered, _ := m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	highest := int64(-1)
	consider := func(mocks []*models.Mock) {
		for _, mk := range mocks {
			if mk == nil {
				continue
			}
			if n, ok := strings.CutPrefix(mk.Name, "mock-"); ok {
				if idx, err := strconv.ParseInt(n, 10, 64); err == nil && idx > highest {
					highest = idx
				}
			}
		}
	}
	consider(filtered)
	consider(unfiltered)
	return highest
}

// pushScopeTable reads mappings.yaml for the set (if per-test mappings exist)
// and hands the agent the name→mock-names table so per-test scoping works.
func (m *mockService) pushScopeTable(ctx context.Context, name string) {
	if m.mappingDB == nil {
		return
	}
	pusher, ok := m.instrumentation.(ScopePusher)
	if !ok {
		return
	}
	mappings, meaningful, err := m.mappingDB.Get(ctx, name)
	if err != nil || !meaningful || len(mappings) == 0 {
		return
	}
	table := make(map[string][]string, len(mappings))
	for testName, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		table[testName] = names
	}
	if err := pusher.PushScopeTable(ctx, table); err != nil {
		m.logger.Debug("failed to push per-test scope table; per-test scoping disabled for this run", zap.Error(err))
		return
	}
	m.logger.Info("per-test scoping enabled", zap.Int("tests", len(table)), zap.String("mock-set", name))
}

// reportOutcome logs which mocks were consumed and which outgoing calls matched
// nothing, and returns the number of distinct missed calls.
func (m *mockService) reportOutcome(ctx context.Context, loaded int) int {
	consumed, err := m.instrumentation.GetConsumedMocks(ctx)
	if err != nil {
		m.logger.Debug("failed to read consumed mocks", zap.Error(err))
	}
	misses, err := m.instrumentation.GetMockErrors(ctx)
	if err != nil {
		m.logger.Debug("failed to read mock misses", zap.Error(err))
	}
	m.logger.Info("mock replay summary",
		zap.Int("loaded", loaded),
		zap.Int("consumed", len(consumed)),
		zap.Int("missed", len(misses)))
	for _, miss := range misses {
		m.logger.Warn("no recorded mock matched an outgoing call",
			zap.String("protocol", miss.Protocol),
			zap.String("call", miss.ActualSummary),
			zap.String("destination", miss.Destination),
			zap.String("next_step", "record this call with --on-miss record, or re-record the set"))
	}
	return len(misses)
}
