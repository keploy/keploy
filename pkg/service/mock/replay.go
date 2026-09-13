package mock

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/config"
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
	// --pass-through-ports lands in BypassRules (config.SetByPassPorts, driven by
	// cli/provider/cmd.go). The proxy-level rules were already forwarded via
	// OutgoingOptions.Rules below, but the KERNEL-level bypass is a separate
	// channel and mock mode never populated it -- so a bypassed port was still
	// pulled through the proxy and recorded. Integration record/test have always
	// set this (service/record/record.go, service/replay/replay.go); mock mode
	// was the only path that did not.
	passPortsUint := config.GetByPassPorts(m.config)

	if err := m.instrumentation.Setup(ctx, m.config.Command, models.SetupOptions{
		Container:        m.config.ContainerName,
		CommandType:      m.config.CommandType,
		DockerDelay:      m.config.BuildDelay,
		BuildDelay:       m.config.BuildDelay,
		Mode:             models.MODE_TEST,
		MockMode:         true,
		ConfigPath:       m.config.ConfigPath,
		PassThroughPorts: passPortsUint,
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
	m.pushScopeTable(ctx, name, filtered, unfiltered)

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
	// Prime the name sequences from what is already on disk, so appended mocks
	// get fresh names (InsertMock always renames); without this the replay-side
	// counters start at 0 and every appended mock reuses a recorded name,
	// colliding with the set (consumed-mock tracking and mappings both key on
	// name). Per owner, since a name's ordinal counts within its owner -- one
	// int64 could only ever have seeded one of the sequences.
	m.mockDB.SeedCounters(m.existingMocks(ctx, name))
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

// existingMocks loads every mock already recorded in the set, across both pools.
// Handed to MockDB.SeedCounters so an append continues each name sequence past
// what is on disk.
//
// This replaces highestMockIndex, which parsed the "mock-" prefix here and
// returned a single highest N. That shape cannot survive owner-scoped names:
// there is no longer one sequence to be highest in, and the parsing belongs next
// to the mint that defines the format, not in the CLI. Errors are ignored for
// the same reason they were before -- an unreadable set means no seed, and the
// append is best-effort.
func (m *mockService) existingMocks(ctx context.Context, name string) []*models.Mock {
	all := map[string]bool{}
	filtered, _ := m.mockDB.GetFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	unfiltered, _ := m.mockDB.GetUnFilteredMocks(ctx, name, models.BaseTime, time.Now(), all, all)
	return append(filtered, unfiltered...)
}

// deriveScopeTable builds the per-test table from the mocks themselves.
//
// Every mock already carries the scope that captured it, so the table is a
// regrouping of data we are holding, not a second source of truth that can go
// missing. A mock with NO owner is deliberately left out: absence from the
// table is what makes it shared overflow, reachable by every test.
//
// Order within an owner is preserved -- the tape is replayed in the order it
// was recorded, and the caller hands us the mocks in on-disk order.
func deriveScopeTable(slices ...[]*models.Mock) map[string][]string {
	table := map[string][]string{}
	for _, slice := range slices {
		for _, mk := range slice {
			if mk == nil || mk.Owner == "" {
				continue
			}
			table[mk.Owner] = append(table[mk.Owner], mk.Name)
		}
	}
	return table
}

// pushScopeTable hands the agent the test-name→mock-names table so the runner's
// /agent/scope/begin calls can narrow the served pool per test.
//
// The table is DERIVED from each mock's owner. mappings.yaml remains only as a
// fallback for sets recorded before mocks carried an owner; it is no longer
// transported, and a set whose mocks are owned needs no such file to be scoped.
func (m *mockService) pushScopeTable(ctx context.Context, name string, mocks ...[]*models.Mock) {
	pusher, ok := m.instrumentation.(ScopePusher)
	if !ok {
		return
	}

	source := "owner"
	table := deriveScopeTable(mocks...)
	if len(table) == 0 {
		source = "mappings.yaml"
		table = m.scopeTableFromMappings(ctx, name)
	}
	if len(table) == 0 {
		return
	}

	if err := pusher.PushScopeTable(ctx, table); err != nil {
		m.logger.Debug("failed to push per-test scope table; per-test scoping disabled for this run", zap.Error(err))
		return
	}
	m.logger.Info("per-test scoping enabled",
		zap.Int("tests", len(table)),
		zap.String("derived_from", source),
		zap.String("mock-set", name))
}

// scopeTableFromMappings is the pre-Owner fallback: a set recorded by a build
// that did not stamp owners still has its mappings.yaml on disk.
func (m *mockService) scopeTableFromMappings(ctx context.Context, name string) map[string][]string {
	if m.mappingDB == nil {
		return nil
	}
	mappings, meaningful, err := m.mappingDB.Get(ctx, name)
	if err != nil || !meaningful || len(mappings) == 0 {
		return nil
	}
	table := make(map[string][]string, len(mappings))
	for testName, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		table[testName] = names
	}
	return table
}

// ReplayOutcome is what a replay produced, for a wrapping build that meters or
// reports usage. Counts only; no payloads, no destinations.
type ReplayOutcome struct {
	SetName  string
	Loaded   int
	Consumed int
	Missed   int
}

// replayOutcomeReporter is installed by a wrapping build (enterprise) from
// init(). Mirrors the RegisterNativeCommandSupport extension point in
// cli/provider/hooks.go: OSS stays free of any billing/api-server dependency,
// and a build that has one opts in.
var replayOutcomeReporter func(context.Context, ReplayOutcome)

// RegisterReplayOutcomeReporter installs the reporter. A nil fn disables it.
func RegisterReplayOutcomeReporter(fn func(context.Context, ReplayOutcome)) {
	replayOutcomeReporter = fn
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

	// Metering LAST: it may do network I/O, and the miss warnings above are what
	// the user actually needs to see first. Never for --local — that is the free
	// offline loop and is deliberately unmetered and untracked.
	if replayOutcomeReporter != nil && !m.config.Mock.Local {
		replayOutcomeReporter(ctx, ReplayOutcome{
			SetName:  m.config.Mock.Name,
			Loaded:   loaded,
			Consumed: len(consumed),
			Missed:   len(misses),
		})
	}
	return len(misses)
}
