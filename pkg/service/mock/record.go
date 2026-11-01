package mock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// mockDrainQuiet is how long the outgoing stream must stay silent before the
// drain asks the agent whether it still holds anything from before the run
// ended (drainTrailingMocks).
//
// The window it covers is small but real: the agent finishes parsing the last
// dependency response, emits the mock, and it travels agent -> gob stream ->
// CLI, while the CLI independently observes the wrapped runner exit. Measured
// on a failing reproduction that gap ran to a median of 6.5ms and a maximum of
// 22.8ms. 500ms is an order of magnitude of headroom on that, and is paid once
// per recording.
const mockDrainQuiet = 500 * time.Millisecond

// mockDrainNotice is how often drainTrailingMocks says it is still saving, and
// that another interrupt stops it now.
var mockDrainNotice = 5 * time.Second

// recordsJudged is set by a build that judges what keploy mock record keeps
// (JudgeRecords).
var recordsJudged atomic.Bool

// JudgeRecords says this build judges what keploy mock record keeps, test by
// test (keploy enterprise's judging): under the proven switch a record then
// keeps its recording whenever it read what each test did, even when the
// command failed, for the judging to commit. Without it a failed record
// keeps nothing, as before the switch, so the previous recording is never
// lost to a record no one judges.
//
// A build that calls it takes over the set's backup: under the switch Record
// then makes no .<set>.previous copy of its own and never puts the set back,
// so the caller must back the whole set up before Record (verdicts.yaml
// included) and restore it on any failure, or a failed record loses the
// previous recording.
func JudgeRecords() { recordsJudged.Store(true) }

// replaysJudged is set by a build that judges a replay (JudgeReplays).
var replaysJudged atomic.Bool

// JudgeReplays says this build sets a replay's exit code from keploy's
// verdict on each test (keploy enterprise's judging). Under the proven switch
// a replay in a build without it is refused: its exit code would be only the
// test command's, and a test keploy fails after it has ended (one left open
// when the command exited) or a call made outside every test that got no
// recorded answer does not fail the command.
func JudgeReplays() { replaysJudged.Store(true) }

// Record runs the wrapped test command and captures every outgoing dependency
// call into the configured named mock set. It writes ONLY mocks (no incoming
// test cases), overwrites the set in place so a re-record produces a clean
// diff, correlates any per-test scopes the runner reported into mappings.yaml,
// pushes the set to the store, and propagates the runner's exit code.
func (m *mockService) Record(ctx context.Context) error {
	// The recording's own teardown scope: what keploy sees its stops do
	// moves it, and the drain below waits while it moves (teardownDrain).
	ctx, teardown := utils.NewTeardownScope(ctx)
	name := m.setName()
	requests := m.requests()
	started := time.Now()
	if err := m.refuseProvenDocker("record"); err != nil {
		return err
	}
	if err := m.checkRealDeps(); err != nil {
		return err
	}
	m.logger.Info("Recording mocks for your test command",
		zap.String("mock-set", name),
		zap.String("command", m.userCommand))

	// The caller's context: only ITS cancellation is the user's Ctrl+C. The
	// errgroup below cancels the context it derives whenever any goroutine in
	// it fails -- the agent process exiting is one of them -- so testing that
	// one reported keploy's own failure as an interrupt: exit 0, the test
	// command never run, and a CI job gating on the exit code went green.
	parent := ctx
	errGrp, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	ctx, cancel := context.WithCancel(ctx)
	// Writes must outlive a SIGINT teardown so the tail of the recording still
	// reaches disk (same rationale as pkg/service/record's persistCtx).
	persistCtx := context.WithoutCancel(ctx)
	// storeCtx is what the staging set's promote and discard close the mock
	// store with. It outlives an interrupt, as persistCtx does, and ends only
	// when the user gives up keploy's wait for the store's writing or for its
	// consumers (closeStore, waitConsumers): a disk that is stuck then holds
	// neither the promote nor the discard after it.
	storeCtx, giveUpStore := context.WithCancel(persistCtx)
	defer giveUpStore()

	// The agent's lifetime (setupCtx). A Ctrl+C while it is still being set
	// up cancels the setup at once. Once it is up, it records until the drain
	// below has taken what it captured before the run ended, and the teardown
	// stops it then. Cancelled as the interrupt landed, it was stopped (its
	// stop request) before the drain, which then lost what it still held, and
	// the short set replaced the one it was recorded to update.
	// The interrupt and the agent being up are decided together
	// (utils.SetupGate).
	setupCtx, setupCancel := context.WithCancel(context.WithoutCancel(ctx))
	setupGate := utils.NewSetupGate(ctx, setupCancel)

	var stopReason string
	defer func() {
		m.notifyShutdown()
		// Cancel the errgroup ctx so the agent-monitor / app-runner goroutines
		// (bound to this ctx by Setup/Run) observe cancellation and unwind,
		// otherwise the drain below waits out its 30s stall bound every run; and
		// the agent's lifetime, now that the drain is done.
		cancel()
		setupCancel()
		if err := m.teardownDrain("mock-record", errGrp, teardown); err != nil {
			utils.LogError(m.logger, err, "failed to drain mock-record goroutines")
		}
	}()

	// 1. Instrument: start the agent, hooks and proxy in mock mode (no ingress
	//    port relocation — the runner is not a server).
	//    A start the agent stops making progress in is given up on
	//    (pkg.ErrAgentNotReady) and retried with a fresh agent, as record and
	//    test retry it (pkg.RetryAgentSetup). Any other failure is not
	//    retried.
	opts := models.SetupOptions{
		Container:        m.config.ContainerName,
		FromContainer:    m.config.FromContainer,
		CommandType:      m.config.CommandType,
		DockerDelay:      m.config.BuildDelay,
		BuildDelay:       m.config.BuildDelay,
		Mode:             models.MODE_RECORD,
		MockMode:         true,
		ConfigPath:       m.config.ConfigPath,
		PassThroughPorts: config.GetByPassPorts(m.config),
		RecordRequests:   requests,

		DisableHandshakeHold: m.config.Record.DisableHandshakeHold,
	}
	if err := pkg.RetryAgentSetup(setupCtx, m.logger, func(c context.Context, _ int) error {
		return m.instrumentation.Setup(c, m.config.Command, opts)
	}); err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}
	if !setupGate.Up() {
		// The run's context ended first, as the setup finished, and stopped
		// the agent: an interrupted setup when the user stopped it (parent),
		// keploy's own failure (the errgroup's) otherwise, which must not
		// pass for one.
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		cause := context.Cause(ctx)
		utils.LogError(m.logger, cause, stopReason)
		return fmt.Errorf("%s: %w", stopReason, cause)
	}
	// The agent is up: from here a Ctrl+C no longer stops it; the teardown
	// does, once the drain is done.

	// 2. Docker compose inverts this function's normal order: arming the
	//    capture (step 4) needs a live agent, but under compose the agent is a
	//    service inside the compose project keploy generates, so it does not
	//    exist until the wrapped `docker compose up` runs. startComposeApp
	//    brings the project up and waits for the agent to answer; the app
	//    itself stays parked at the agent's healthcheck until step 5 releases
	//    it, so nothing it does escapes the capture. Its exit is collected at
	//    step 7 instead of being started there.
	//
	//    Without this the run dialled an agent that was never started, failed
	//    to arm the capture, and ended having recorded nothing — while the app
	//    itself never came up at all.
	composeAppExit, err := m.startComposeApp(ctx, errGrp, "record")
	if err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to bring up the keploy-agent compose service"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Under the proven switch the record leaves what each test did against
	// real infra (design v14 §7.4's R0) for judging, whether or not the
	// recording is kept. Registered before the restore below, so it runs
	// after it: a restore puts the previous set back, or removes a fresh one.
	var recordTests []models.ScopeWindow
	var recordReal *models.RealRun
	// recordNotJudged is what the recording lacks because keploy cannot
	// capture it at all (decision 20), for whatever judges the recording.
	var recordNotJudged models.CaptureLosses
	// recordLosses is, for a judged record, what the agent could not capture
	// that no test was charged with: judging fails the run on it, as on
	// recordLossesUnread, why that could not be read.
	var recordLosses models.CaptureLosses
	var recordLossesUnread string
	recordJudged := false
	defer func() {
		// Only a build that judges the record reads these results; written
		// otherwise, they would recreate a fresh set's folder a failed
		// record's restore just removed.
		if recordJudged && provenswitch.On() && recordsJudged.Load() && m.config.Path != "" {
			writeRecordTestsReceipt(m.logger, m.config.Path, TestsReceipt{
				Set: name, At: started.UTC().Truncate(time.Second), Refusals: m.refusals, Live: m.live, Mocked: m.mocked, Real: recordReal,
				CannotCapture: recordNotJudged, CaptureLosses: recordLosses, CaptureLossesUnread: recordLossesUnread,
			}, recordTests)
		}
	}()
	saveSet := m.saveSet
	if provenswitch.On() && recordsJudged.Load() {
		// The judging build backs the whole set up itself, list carried, and
		// puts it back when the record is not judged: a second backup here
		// would restore the folder without its list.
		saveSet = m.existsOnly
	}
	restore, existed, err := saveSet(name)
	if err != nil {
		// No restorable copy of the set — refuse before deleting its mappings
		// or cases below, so a failed backup can never destroy the recording.
		stopReason = "could not back up the existing mock set before re-recording it"
		utils.LogError(m.logger, err, stopReason, zap.String("mock-set", name),
			zap.String("next_step", "free up disk space or fix permissions on your keploy mocks folder, then retry — your existing recording was left untouched"))
		return fmt.Errorf("%s: %w", stopReason, err)
	}
	keep := false
	defer func() { restore(keep) }()
	// 3. Capture into a staging set and promote it over the named set only once
	//    the run has produced mocks and finished cleanly. This replaces the old
	//    delete-first, which dropped the existing recording before capture even
	//    armed — so any failure, interrupt or zero-capture run left the user with
	//    nothing (gaps W1/W14; design §P0b "no delete-first"). See
	//    MockDB.PromoteStagedSet / DiscardStagedSet.
	staging := stagingSetID(name)
	// Clear any staging set a previously-crashed record left behind. This MUST
	// succeed before we capture: appending into a dirty staging base would
	// persist duplicate/stale mocks (yaml appends; a leftover gob would even be
	// promoted and, being preferred on read, shadow the fresh recording) and then
	// promote that polluted set over the real one. The real set is untouched
	// here, so aborting now is safe and leaves it intact.
	if err := m.mockDB.DiscardStagedSet(storeCtx, staging); err != nil {
		stopReason = "failed to clear a leftover staging set before recording"
		utils.LogError(m.logger, err, stopReason, zap.String("staging", staging),
			zap.String("next_step", "remove the leftover staging directory under your keploy mocks folder (its name ends in "+yaml.MockStagingSuffix+") and retry"))
		return fmt.Errorf("%s: %w", stopReason, err)
	}
	defer func() {
		if !keep {
			_ = m.mockDB.DiscardStagedSet(storeCtx, staging)
		}
	}()
	m.mockDB.ResetCounterID()
	m.deleteMappings(persistCtx, name)
	if requests {
		// The old cases stay until the run has ended so the new ones are numbered after them: a flow
		// re-recorded on its own must not reuse the names of the flows the set carries.
		defer m.deleteCases(persistCtx, name, m.caseNames(persistCtx, name))
	}

	// The run's mock.real (proven switch only), before the capture arms:
	// those destinations are reached for real from the first connection.
	if err := m.pushRealDeps(ctx); err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "keploy could not hand its agent mock.real"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// 4. Arm the record proxy and stream captured mocks.
	captureCtx, stopCapture := context.WithCancel(context.WithoutCancel(ctx))
	defer stopCapture()
	// Natively the drain below runs, and the capture's streams close, before
	// the agent is stopped: the agent holds a stop signal it gets meanwhile
	// (a `pkill keploy`, or a stop of its container, reaches it too) until
	// they have, or it ended the streams the drain still needed
	// (models.DrainsAfterStopHeader). Not under compose:
	// the streams close only once `docker compose up` has exited, which waits
	// on the agent, a service in the project; held for them, it would be
	// killed only at compose's stop timeout.
	agentHolds := composeAppExit == nil
	if agentHolds {
		captureCtx = models.WithDrainsAfterStop(captureCtx)
	}
	// This round's streams end by the agent's markers when it can
	// (round_end.go): its token names them to the agent.
	round := newRoundEnd(m.instrumentation)
	captureCtx = round.on(captureCtx)
	outgoing, err := m.instrumentation.GetOutgoing(captureCtx, models.OutgoingOptions{
		Rules:                     m.config.BypassRules,
		MongoPassword:             m.config.Test.MongoPassword,
		MysqlPorts:                m.config.MysqlPorts,
		DisableMysqlAutoDetect:    m.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: m.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          m.config.Record.PassThroughPorts,
		PassThroughHosts:          m.config.Record.PassThroughHosts,
	})
	if err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to start capturing outgoing calls"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// recorded remembers each captured mock's name, kind and place
	// (models.PlaceOf) so the scope-window correlation after the run can
	// build mappings.yaml.
	var recorded []capturedMock
	mockCount := 0
	// mocksSeen is bumped for every mock that arrives, whether or not it ends
	// up persisted. heldSeen counts only those requested before the drain
	// began (drainFrom; 0 until it does): what the agent held when the runner
	// exited, which the drain waits for, as opposed to calls something left
	// running keeps making. heldDone counts those the consumer has finished
	// with, and heldBusy is set while it works on one: a consumer persisting
	// is draining, however long one write takes (a 12 MB mock, a disk stall),
	// and the drain's quiet bound does not run meanwhile. All are read while
	// the consumer is still running - hence atomic. mocksDone counts every
	// mock the consumer has finished with (persisted, or not), for the wait
	// on it once the capture has stopped (waitConsumers). mockCount and
	// recorded stay plain: they are only read after consumerDone, as are
	// insertsFailed and firstInsertErr, the mocks the store refused to write
	// (a full disk, a permission) and the first of its errors.
	var mocksSeen, mocksDone, heldSeen, heldDone, drainFrom atomic.Int64
	var insertsFailed int
	var firstInsertErr error
	var heldBusy atomic.Bool
	consumerDone := make(chan struct{})
	// writeCtx is the consumers' writes': it outlives an interrupt, as
	// persistCtx does, and ends only when the user gives up the wait for
	// them (waitConsumers), so a write that waits for room can give up.
	writeCtx, stopWriting := context.WithCancel(persistCtx)
	defer stopWriting()
	// The document the MockDB writes for each mock, for AfterMockInsert. Used
	// only by persist, which runs on the one consumer goroutine.
	insertedDoc := rec.NewInsertedMockDoc(writeCtx, m.hooks)
	persist := func(mk *models.Mock) {
		mctx := &rec.MockContext{Mock: mk, TestSetID: staging}
		if err := m.hooks.BeforeMockInsert(ctx, mctx); err != nil {
			m.logger.Debug("BeforeMockInsert hook failed", zap.Error(err), zap.String("mock", mk.Name))
		}
		if mctx.Skip {
			return
		}
		encoded, encodedFormat, err := insertedDoc.Insert(m.mockDB, mk, staging)
		if err != nil {
			if errors.Is(err, models.ErrMockEncode) {
				m.logger.Warn("dropping one unencodable mock and continuing", zap.String("kind", mk.GetKind()), zap.Error(err))
				return
			}
			// Not in the recording: counted, and the run then ends as one
			// that is not saved (closeStore's step). Before, it was said at
			// ERROR and left out, and the set was saved as whole, exit 0.
			utils.LogError(m.logger, err, "failed to persist mock", zap.String("mock", mk.Name))
			if insertsFailed++; firstInsertErr == nil {
				firstInsertErr = err
			}
			return
		}
		if err := m.hooks.AfterMockInsert(ctx, &rec.MockContext{
			Mock: mk, TestSetID: staging, Encoded: encoded, EncodedFormat: encodedFormat,
		}); err != nil {
			m.logger.Debug("AfterMockInsert hook failed", zap.Error(err), zap.String("mock", mk.Name))
		}
		mockCount++
		typ := mk.Spec.Metadata["type"]
		// Under the proven switch a protocol-phase mock is the pool's, never
		// a test's (design v14 §5.2): the mapping lists data-plane mocks only.
		protocol := provenswitch.On() && mk.IsProtocolPhase()
		recorded = append(recorded, capturedMock{name: mk.Name, kind: string(mk.Kind), ts: models.PlaceOf(mk), pid: mk.SourcePID, boot: typ == "config" || typ == "connection", protocol: protocol, start: mk.Start, ref: mk.StartRef})
	}
	go func() {
		defer utils.Recover(m.logger)
		defer close(consumerDone)
		for mk := range outgoing {
			mocksSeen.Add(1)
			// An undated mock cannot be told apart, and is taken as held:
			// dropping it would be the loss the drain exists to prevent.
			// Mocks the agent's parsers emit are dated.
			held := false
			if from := drainFrom.Load(); from == 0 || mk.Spec.ReqTimestampMock.IsZero() || mk.Spec.ReqTimestampMock.UnixNano() <= from {
				held = true
				heldBusy.Store(true)
				heldSeen.Add(1)
			}
			persist(mk)
			mocksDone.Add(1)
			if held {
				heldDone.Add(1)
				heldBusy.Store(false)
			}
		}
	}()

	// 4b. With --record-requests, also store the app's incoming requests as test cases.
	var casesSeen atomic.Int64
	capture := m.captureCases(captureCtx, writeCtx, name, &casesSeen, requests)

	// 4c. What the agent had already refused a place in its PID maps before
	//     the test command runs: the run's own refusals are what it grows by
	//     (PIDMapReader). Read here, with the compose app still parked.
	pidBase := m.pidMapBaseline(ctx, "record")
	lossesBase := m.captureLossBaseline(ctx)

	// 5. Release the compose app now that the capture is armed and draining.
	//    This is the post-arm half of step 2 — see releaseComposeApp.
	if err := m.releaseComposeApp(ctx); err != nil {
		if parent.Err() != nil {
			return nil
		}
		stopReason = "failed to release the app behind the keploy-agent healthcheck"
		utils.LogError(m.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// 6. Optional record timer.
	if m.config.Mock.RecordTimer > 0 {
		errGrp.Go(func() error {
			m.logger.Info("recording will stop after " + m.config.Mock.RecordTimer.String())
			select {
			case <-time.After(m.config.Mock.RecordTimer):
				_ = utils.Stop(m.logger, "record timer elapsed")
			case <-ctx.Done():
			}
			return nil
		})
	}

	// 7. Block until the wrapped runner exits. Its exit — clean or failing —
	//    is the NORMAL end of a mock recording, not an app crash. Under
	//    compose it was already started at step 2, so wait on that same exit
	//    rather than starting it a second time.
	//    A plain receive is the same wait the native branch does: Run returns
	//    only once the app is fully down (its errgroup Wait is deferred), and
	//    the sender is the goroutine running exactly that Run.
	var appErr models.AppError
	if composeAppExit != nil {
		appErr = <-composeAppExit
	} else {
		appErr = m.instrumentation.Run(ctx, models.RunOptions{AppCommand: m.config.Command})
	}

	// 8. Drain the trailing mocks, THEN stop capturing.
	//
	// The order is the entire point. stopCapture() cancels the context the
	// outgoing stream is built on (http.NewRequestWithContext in
	// pkg/platform/http/agent.go), so cancelling first aborts the request, the
	// decoder errors, the mock channel closes, and consumerDone fires in tens
	// of microseconds. A grace period applied after that waits on an
	// already-closed channel and does nothing at all - which is how a mock the
	// agent had already written to the wire was still lost, silently, with the
	// agent-side accounting reporting success.
	//
	// So wait for the stream to fall quiet before tearing it down. Quiescence
	// rather than a fixed sleep: a sleep long enough to be safe would be paid
	// in full by every recording, and one short enough not to hurt would still
	// be a race. This returns as soon as the agent stops sending.
	drainStart := time.Now()
	drainFrom.Store(drainStart.UnixNano())
	lost, unconfirmed := m.drainTrailingMocks(consumerDone, drainStart, agentHolds, func() (uint64, bool) {
		return uint64(heldSeen.Load() + heldDone.Load() + casesSeen.Load()), heldBusy.Load()
	}, func() uint64 { return uint64(heldSeen.Load()) }, recordDrainWords, round)
	// sayUnconfirmed is said once the recording is saved, and only then: a
	// recording that is not kept (the tests failed, nothing was captured,
	// the agent died) promotes nothing, and "the recording is saved" would
	// not be true of it.
	sayUnconfirmed := func() {
		if unconfirmed {
			m.logger.Warn(composeTailUnconfirmed, zap.String("mock-set", name), zap.Int("mocks", mockCount),
				zap.String("next_step", composeTailUnconfirmedNext))
		}
	}
	// 8a's count, read while the recording's stream is still open: an agent
	// that is being stopped too (a stop sent to it, a container stop) holds
	// its HTTP server only while a recording's streams are open, and once
	// they close it answers no more, which would read as a lost count and
	// keep a recording that is short of an untracked process's calls.
	pidCheck := m.pidMapRefusedDuring(persistCtx, pidBase, "record")
	// So is what else it could not capture (under the proven switch).
	lostAll, lostErr := m.captureLossesDuring(persistCtx, lossesBase)
	// A record that is judged keeps what it captured whatever the agent
	// could not capture (design v14 §7.5): each test's window carries what
	// was lost while it ran or before it (a loss charges the test open when
	// it happened and every later one), so judging keeps the previous
	// recording of each test charged with one, and commits the new one of
	// each clean test. What no test was charged with (a loss after the last
	// test) is the run's, read after the windows (runLosses below), for
	// judging to fail the run on. Before, one loss dropped the whole set,
	// clean tests too, with no per-test file written.
	judgedRecord := provenswitch.On() && recordsJudged.Load()
	// 8'. The agent said it holds nothing more from before the run ended, and
	//     that it can end its streams: it puts on each what it still has,
	//     then its marker, and the round ends there, every mock it captured
	//     before the run ended read and saved -- not when the stream is ended
	//     here, which loses a mock still on its way (clear is not delivered).
	//     After 8a's reads above, which need the streams open. A stream that
	//     closes before its marker, from an agent only this client stops,
	//     leaves the round incomplete (lost).
	if !lost && round.endsByMarkers() {
		streams := []roundStream{{kind: models.MockStream, done: consumerDone}}
		if capture != nil {
			streams = append(streams, roundStream{kind: models.TestCaseStream, done: capture.done})
		}
		strict := agentHolds && startedAgent(m.instrumentation)
		lost = m.endRound(round, func() bool { return strict }, streams, recordDrainWords)
	}
	stopCapture()
	// 8''. The capture has stopped: its consumers write what the agent handed
	//      over to them, however long that takes (waitConsumers), and only
	//      then is what they wrote read. One the user gives up at a second
	//      interrupt left mocks unwritten: this recording is not saved, and the
	//      existing set is kept (the deferred discard and restore).
	consumers := []streamConsumer{{what: "mocks", done: consumerDone,
		taken: func() uint64 { return uint64(mocksSeen.Load()) }, handled: func() uint64 { return uint64(mocksDone.Load()) }}}
	if capture != nil {
		consumers = append(consumers, streamConsumer{what: "cases", done: capture.done,
			taken: func() uint64 { return uint64(casesSeen.Load()) }, handled: capture.handled.Load})
	}
	if !m.waitConsumers(recordDrainWords, consumers...) {
		// The deferred stopWriting ends the writes still under way, before
		// the deferred discard of the staging set, which does not wait for a
		// store the user gave up on either.
		giveUpStore()
		armGivenUp()
		return errors.New("the recording was not saved: keploy was stopped before it had written every mock and test case the agent handed over")
	}
	// 8'''. Every mock is now handed to the mock store: a store that writes
	//       in the background (the gob writer) writes the rest here, however
	//       long that takes (closeStore), before anything reads the recording
	//       or saves it. One the user gives up at a second interrupt, or that
	//       could not write every mock, is not saved: the existing set is
	//       kept (the deferred discard and restore), and the run says so at
	//       ERROR, with how many mocks were not written. A store that writes
	//       as it is handed mocks (the yaml one) refused them as it was
	//       handed them (insertsFailed): the same.
	gaveUp, storeErr := m.closeStore(recordDrainWords)
	if insertsFailed > 0 {
		refused := fmt.Errorf("%w: %d of the %d mocks handed to the mock store were not written; the first write failed: %w",
			models.ErrMocksNotWritten, insertsFailed, insertsFailed+mockCount, firstInsertErr)
		if storeErr == nil {
			gaveUp, storeErr = false, refused // a refused write is no give-up of the user's
		} else {
			storeErr = fmt.Errorf("%w; and %w", storeErr, refused)
		}
	}
	// With --record-requests, the test cases the test-case store refused
	// (casesErr) are not in the recording either: the run is not saved, the
	// same way, and the run says how many. Before, each was said at ERROR
	// and left out, and the set was saved without it, exit 0.
	casesErr := capture.notWritten()
	if storeErr != nil && casesErr != nil {
		storeErr = fmt.Errorf("%w; and %w", storeErr, casesErr)
	}
	// A run that could not write its recording fails: the test command's own
	// code first, keploy's 1 when it passed or the user stopped it (as 8a
	// below). keploy mock propagates the test command's exit code.
	failRun := func() {
		if parent.Err() == nil {
			m.propagateExit(appErr, "record")
		}
		utils.SetExitCodeOnce(utils.ExitKeployError)
	}
	if storeErr != nil {
		next := "check the disk the recording writes to (space, speed, permissions), then re-record; the existing mock set is kept"
		if gaveUp {
			giveUpStore()
			armGivenUp()
			next = recordDrainWords.nextStep
		} else {
			failRun()
		}
		m.logger.Error(recordDrainWords.storeNotWritten, zap.String("mock-set", name), zap.Error(storeErr), zap.String("next_step", next))
		return fmt.Errorf("the recording was not saved: %w", storeErr)
	}
	if casesErr != nil {
		failRun()
		m.logger.Error(recordDrainWords.casesNotWritten, zap.String("mock-set", name), zap.Error(casesErr),
			zap.String("next_step", "check the disk the recording writes to (space, speed, permissions), then re-record; the existing mock set is kept"))
		return fmt.Errorf("the recording was not saved: %w", casesErr)
	}

	// 8a. A process the agent could not track -- the kernel refused it a place
	//     in its PID map -- made calls the agent never saw, so the recording
	//     is missing them, with nothing to show where. Kept, it would replay
	//     green wherever the agent tracks every process, the missing calls
	//     reaching the real dependency. So it is not kept, however the run
	//     ended, and the run fails: the runner's own code first, keploy's 1
	//     when the runner passed or was stopped. A record that is judged is
	//     kept: the refusal is on the windows of the tests charged with it,
	//     and judging keeps their previous recordings (judgedRecord above).
	//     (Read above, before the stream closed.)
	if refused := pidCheck.refused; pidCheck.known && refused.Any() && judgedRecord {
		m.logger.Error("the agent could not track every process the test command ran, so their calls were not recorded: the tests charged with it keep their previous recordings in judging",
			append(pidMapFields(refused), zap.String("mock-set", name), zap.String("reason", refused.CaptureIncomplete()))...)
	} else if pidCheck.known && refused.Any() {
		if derr := m.mockDB.DiscardStagedSet(storeCtx, staging); derr != nil {
			m.logger.Debug("failed to discard staging set after an incomplete capture", zap.String("staging", staging), zap.Error(derr))
		}
		reason := refused.CaptureIncomplete()
		msg := "the recording was not kept: the agent could not track every process the test command ran, so their calls were not recorded; the existing mock set (if any) was left unchanged"
		if refused.Target == 0 {
			msg = "the recording was not kept: the agent could not exclude every process that predates the test command, so their children's calls may have been recorded as the test command's; the existing mock set (if any) was left unchanged"
		}
		m.logger.Error(msg, append(pidMapFields(refused), zap.Int("mocks", mockCount), zap.String("mock-set", name), zap.String("reason", reason))...)
		if parent.Err() == nil {
			m.propagateExit(appErr, "record")
		}
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return errors.New("the recording is incomplete: " + reason)
	}
	// Under the proven switch, any other loss of capture during the run:
	// the recording misses what was lost, so it is not kept either.
	//     (Read above, before the stream closed.) A call keploy cannot
	//     capture at all (decision 20) does not discard the recording: it is
	//     said below.
	lostLosses, lostNotJudged := lostAll.Split()
	recordNotJudged = lostNotJudged
	if t := lostLosses.Text(); t != "" && judgedRecord {
		m.logger.Error("keploy could not capture every call the test command made: the tests charged with it keep their previous recordings in judging ("+provenswitch.Env+"=1)",
			zap.String("mock-set", name), zap.String("reason", t))
	} else if t != "" {
		if derr := m.mockDB.DiscardStagedSet(storeCtx, staging); derr != nil {
			m.logger.Debug("failed to discard staging set after an incomplete capture", zap.String("staging", staging), zap.Error(derr))
		}
		m.logger.Error("the recording was not kept: keploy could not capture every call the test command made; the existing mock set (if any) was left unchanged ("+provenswitch.Env+"=1)",
			zap.Int("mocks", mockCount), zap.String("mock-set", name), zap.String("reason", t))
		if parent.Err() == nil {
			m.propagateExit(appErr, "record")
		}
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return errors.New("the recording is incomplete: " + t)
	}
	lossesUnread := false
	if pidCheck.unreadable == nil && lostErr != nil {
		pidCheck.unreadable, lossesUnread = lostErr, true // said below, as a lost PID-map count is
	}
	// The agent still held mocks when the drain ended (said above, at ERROR):
	// what was captured is not the whole run, and promoted it would replace
	// the set it was recorded to update with one that is short of mocks.
	if lost {
		stopReason = "the recording may be incomplete: Keploy could not confirm it saved all the mocks the keploy agent captured"
		err := errors.New(stopReason)
		if parent.Err() == nil {
			// Not the user: the agent died under the test command, which is
			// why it ended its stream early. Say why, as a run that did not
			// finish does.
			if cause := m.runFailure(ctx); cause != nil {
				err = fmt.Errorf("%s: %w", stopReason, cause)
			}
		}
		m.logger.Error(stopReason+"; the mocks it recorded were not saved over the existing set",
			zap.Int("mocks", mockCount), zap.String("mock-set", name), zap.Error(err),
			zap.String("next_step", "re-record the mock set"))
		armGivenUp() // a drain the agent ended has armed keploy's 1 already
		return err
	}
	// 8b'. The test command called what keploy cannot capture at all: the
	//      recording lacks those calls, and a replay of it would make them
	//      for real. Kept, not vouched for in silence.
	if t := lostNotJudged.NotJudgedText(); t != "" {
		m.logger.Warn("the recording is kept, but the test command called what keploy cannot capture, a TLS client abandoned its handshake, the kernel dropped connection attempts at keploy's proxy listener, or the Docker daemon did not answer keploy's read of a container a call reached: such calls are not in it, or not as that container's ("+provenswitch.Env+"=1)",
			zap.String("mock-set", name), zap.String("reason", t),
			zap.String("next_step", lostNotJudged.NotJudgedRemedy()+", then record again"))
	}
	// 8b. The agent counts, but its count could not be read: nothing shows a
	//     process went untracked, and nothing shows none did. The recording
	//     is kept -- a lost read is no evidence against it, and record has no
	//     --strict to ask for more -- but it is not vouched for in silence.
	if pidCheck.unreadable != nil {
		unread, reason := "tracked every process the test command ran", models.CaptureUnverified(pidCheck.unreadable)
		if lossesUnread {
			unread, reason = "captured every call the test command made", models.CaptureLossesUnverified(pidCheck.unreadable)
		}
		m.logger.Warn("the recording is kept, but keploy could not verify it is complete: it could not read whether the agent "+unread,
			zap.String("mock-set", name), zap.Error(pidCheck.unreadable), zap.String("reason", reason),
			zap.String("next_step", "record again once the agent answers, or replay the set: under --on-miss fail (the default) a call it is missing fails the replay"))
	}

	if parent.Err() != nil { // user stopped the recording (Ctrl+C, or a session stop)
		if mockCount == 0 && len(capture.list()) == 0 {
			if derr := m.mockDB.DiscardStagedSet(storeCtx, staging); derr != nil {
				m.logger.Debug("failed to discard an empty staging set after interrupt", zap.String("staging", staging), zap.Error(derr))
			}
			m.logger.Info("recording stopped; nothing was captured, the existing mock set was left unchanged", zap.String("mock-set", name))
			return nil
		}
		keep = true
		if err := m.saveMocks(storeCtx, staging, name, mockCount); err != nil {
			// A recording that was not saved fails the run, as it does when
			// the run ended on its own. The command does not fail a run the
			// user stopped for its error (an interrupt is not a failure), so
			// the exit code says it: before, it was 0.
			keep = false
			utils.LogError(m.logger, err, "recording stopped, but saving the captured mocks failed", zap.Int("mocks", mockCount), zap.String("mock-set", name))
			utils.SetExitCodeOnce(utils.ExitKeployError)
			return fmt.Errorf("the recording was not saved: %w", err)
		}
		m.logger.Info("recording stopped; captured mocks saved", zap.Int("mocks", mockCount), zap.String("mock-set", name))
		sayUnconfirmed()
		return nil
	}
	// Not the user: the agent died under the test command. What was captured
	// is on disk, but it is not a whole recording, and saying "recorded" over
	// it (or exiting 0) would vouch for one. Natively that fails the run's own
	// errgroup; under compose the agent is a service in the project, its death
	// stops the project -- test command included, whose exit is then compose's
	// stop and not its verdict -- and only the agent's container says so.
	if cause := m.runFailure(ctx); cause != nil {
		if derr := m.mockDB.DiscardStagedSet(storeCtx, staging); derr != nil {
			m.logger.Debug("failed to discard staging set after an incomplete recording", zap.String("staging", staging), zap.Error(derr))
		}
		stopReason = "the recording did not finish"
		utils.LogError(m.logger, cause, stopReason, zap.Int("mocks", mockCount), zap.String("mock-set", name))
		return fmt.Errorf("%s: %w", stopReason, cause)
	}

	// 9. Correlate per-test scope windows into mappings.yaml (best-effort).
	var windows []models.ScopeWindow
	if provenswitch.On() {
		// What each test did is the record's judging input: a record that
		// cannot read it keeps nothing and says so. The read waits on the
		// run's own context, not a clock: the user's interrupt, or the agent
		// process exiting, ends it.
		var werr error
		if windows, werr = m.readWindows(ctx); werr != nil {
			m.propagateExit(appErr, "record")
			stopReason = "keploy could not read what each test did from its agent, so nothing was kept"
			if parent.Err() != nil {
				stopReason = "the recording was stopped before keploy read what each test did, so nothing was kept"
			}
			utils.LogError(m.logger, werr, stopReason, zap.String("mock-set", name),
				zap.String("next_step", "check the agent's log, then record again"))
			return fmt.Errorf("%s: %w", stopReason, werr)
		}
		// What the run saw of mock.real beyond its tests, read once the
		// last test ended: as much the record's judging input as the
		// windows, and as strictly read.
		var rerr error
		if recordReal, rerr = m.realRun(ctx); rerr != nil {
			m.propagateExit(appErr, "record")
			stopReason = "keploy could not read the run's mock.real destinations from its agent, so nothing was kept"
			utils.LogError(m.logger, rerr, stopReason, zap.String("mock-set", name),
				zap.String("next_step", "check the agent's log, then record again"))
			return fmt.Errorf("%s: %w", stopReason, rerr)
		}
		// What the agent could not capture that no test was charged with:
		// read after the windows, whose read ends, failed, a test whose
		// process went away and charges it, so only what came after every
		// test's end is left.
		if judgedRecord {
			judgedLost, _ := lostAll.Split()
			var refused models.PIDMapRefusals
			if pidCheck.known {
				refused = pidCheck.refused
			}
			var unread error
			if recordLosses, unread = m.runLosses(ctx, lossesBase, judgedLost, lostErr, refused); unread != nil {
				recordLossesUnread = unread.Error()
			}
			if t := recordLosses.Text(); t != "" {
				m.logger.Error("keploy could not capture every call the test command made after its last test: no recording holds what those calls carried, so judging fails the run ("+provenswitch.Env+"=1)",
					zap.String("mock-set", name), zap.String("reason", t),
					zap.String("next_step", "fix what the reason names, then record again"))
			}
			if recordLossesUnread != "" {
				m.logger.Error("keploy could not read whether its agent captured every call after the test command's last test, so judging fails the run ("+provenswitch.Env+"=1)",
					zap.String("mock-set", name), zap.String("reason", recordLossesUnread),
					zap.String("next_step", "check the agent's log for why its count could not be read, then record again"))
			}
		}
	} else if m.mappingDB != nil {
		windows = m.agentWindows(persistCtx)
	}
	if len(windows) > 0 {
		root := gitTop()
		for i := range windows {
			windows[i].Dir = setName(windows[i].Dir, root)
		}
	}
	starts := appStarts(windows)
	windows, suites := splitSuites(windows)
	var retries []models.ScopeWindow
	if provenswitch.On() {
		// A test ID that ran more than once in a round is retried (design
		// v14 §6.1, U16): its last attempt is its recording, and it failed
		// if any attempt failed. The calls of the earlier attempts are no
		// test's and no pool's.
		var retried map[string]int
		windows, retries, retried = foldRetries(windows)
		for _, id := range slices.Sorted(maps.Keys(retried)) {
			m.logger.Warn("a test ran more than once: its last attempt is its recording, and it counts failed if any attempt failed",
				zap.String("test", id), zap.Int("attempts", retried[id]), zap.String("mock-set", name))
		}
	} else if err := repeatedScope(windows, existed); err != nil {
		m.propagateExit(appErr, "record")
		m.logger.Error(err.Error())
		return err
	}
	recordTests, recordJudged = windows, true
	keep = runnerPassed(appErr)
	judged := provenswitch.On() && recordsJudged.Load() && len(windows) > 0
	if judged {
		// Under the proven switch, in a build that judges what the record
		// keeps, what each test recorded is kept whether or not the command
		// passed, and whether or not it made a call: judging commits it test
		// by test (design v14 §7.5), keeping the previous recording of a
		// test that failed or did not run, and recording a test that made no
		// call as one with no mocks. Otherwise the record is kept or not as
		// before.
		keep = true
	}
	if keep && mockCount == 0 && len(capture.list()) == 0 && !judged {
		keep = false
		m.logger.Warn("no outgoing calls were captured; the existing mock set (if any) was left unchanged; the runner made no mockable dependency calls, or its traffic was not intercepted",
			zap.String("mock-set", name),
			zap.String("next_step", nothingCapturedNext()))
		m.propagateExit(appErr, "record")
		return nil
	}
	if keep {
		if err := m.saveMocks(storeCtx, staging, name, mockCount); err != nil {
			keep = false
			stopReason = "failed to promote the recorded mocks over the existing set"
			utils.LogError(m.logger, err, stopReason, zap.String("mock-set", name))
			return fmt.Errorf("%s: %w", stopReason, err)
		}
		sayUnconfirmed()
	}
	if keep && m.mappingDB != nil {
		if len(windows) > 0 {
			byTest := correlateScopes(windows, dataPlane(recorded))
			owned := recorded
			var plan bootPlan
			if len(starts) > 0 {
				plan = classify(windows, starts, suites, recorded)
				byTest, owned = plan.tests, plan.perTest
			}
			byCase := correlateCases(windows, owned, capture.list(), stepWindows(windows))
			for _, w := range windows {
				if _, ok := byTest[w.Name]; !ok {
					byTest[w.Name] = nil
				}
			}
			if len(byTest) > 0 {
				if err := m.mappingDB.UpsertBatch(persistCtx, name, byTest); err != nil {
					if provenswitch.On() {
						// Without them no test of the set can be judged
						// (design v14 §5.2): the record fails and says so.
						m.propagateExit(appErr, "record")
						stopReason = "keploy could not write which recorded calls each test made (mappings.yaml)"
						utils.LogError(m.logger, err, stopReason, zap.String("mock-set", name), zap.String("next_step", "free the disk or fix the set directory's permissions, then record again"))
						return fmt.Errorf("%s: %w", stopReason, err)
					}
					m.logger.Warn("failed to write per-test mappings; replay will serve the whole set per test", zap.Error(err))
				} else {
					m.logger.Info("wrote per-test mock mappings", zap.Int("tests", len(byTest)), zap.String("mock-set", name))
				}
			}
			for _, w := range windows {
				if w.Dir == "" {
					continue
				}
				tc := byCase[w.Name]
				tc.Dir = w.Dir
				byCase[w.Name] = tc
			}
			if provenswitch.On() {
				stampBegins(byCase, windows)
				stampReal(byCase, windows)
				stampGiven(byCase, windows)
			}
			if len(starts) > 0 {
				m.upsertCases(persistCtx, name, byCase, nil, suites)
				m.upsertBoots(persistCtx, name, plan.boots)
			} else {
				m.upsertCases(persistCtx, name, byCase, startupMocks(windows, withoutRetries(recorded, retries), m.claims), suites)
			}
		}
	}

	if keep {
		if err := rec.RunAfterRecordingHook(persistCtx, m.logger, m.hooks, &rec.RecordingCompleteContext{TestSetID: name, Path: m.config.Path}, mockDrainNotice); err != nil {
			m.logger.Warn("AfterRecordingComplete hook failed", zap.Error(err), zap.String("mock-set", name))
		}
	}

	// 10. Publish the set to the store (registry upload in enterprise; no-op on files).
	if !runnerPassed(appErr) {
		if keep {
			// Under the proven switch: judging commits it test by test.
			m.logger.Warn("tests failed; the recording is kept for judging, which keeps each failed test's previous recording, and is not published", zap.String("mock-set", name))
		} else {
			m.logger.Warn("tests failed; the recording was not kept or published, and the previous recording is left as it was", zap.String("mock-set", name))
		}
	} else if err := m.store.Push(persistCtx, name); err != nil {
		m.logger.Warn("failed to publish mock set to the store", zap.String("mock-set", name), zap.Error(err))
	}

	m.logger.Info("recorded mocks", zap.Int("mocks", mockCount), zap.String("mock-set", name))
	if capture != nil {
		m.logger.Info("recorded test cases", zap.Int("cases", len(capture.list())), zap.String("mock-set", name))
	}
	if mockCount == 0 {
		m.logger.Warn("no outgoing calls were captured; the runner made no mockable dependency calls, or its traffic was not intercepted",
			zap.String("next_step", nothingCapturedNext()))
	}

	// 13. Propagate the runner's exit code so a CI 're-record on merge' job
	//     fails when the tests fail.
	m.propagateExit(appErr, "record")
	return nil
}

func (m *mockService) saveMocks(ctx context.Context, staging, name string, n int) error {
	if n > 0 {
		return m.mockDB.PromoteStagedSet(ctx, staging, name)
	}
	if err := m.mockDB.DiscardStagedSet(ctx, staging); err != nil {
		m.logger.Debug("failed to discard an empty staging set", zap.String("staging", staging), zap.Error(err))
	}
	return m.mockDB.DeleteMocksForSet(ctx, name)
}

func runnerPassed(appErr models.AppError) bool {
	switch appErr.AppErrorType {
	case models.ErrAppStopped, models.ErrCtxCanceled, "":
		return true
	}
	return false
}

// agentWindows reads the windows the runner itself posted to the agent's scope API; none is not an error.
func (m *mockService) agentWindows(ctx context.Context) []models.ScopeWindow {
	windows, err := m.readWindows(ctx)
	if err != nil {
		m.logger.Debug("failed to read per-test scope windows from the agent", zap.Error(err))
		return nil
	}
	return windows
}

// readWindows reads the run's per-test windows from the agent. Under the
// proven switch they are what each test did, which judging and keploy's
// verdict rest on: the read has no deadline (the agent closes a test whose
// process went away before it answers; its answer, or the connection's
// close, ends the wait), and a failed read is an error, never "no tests".
func (m *mockService) readWindows(ctx context.Context) ([]models.ScopeWindow, error) {
	reader, ok := m.instrumentation.(ScopeReader)
	if !ok {
		if provenswitch.On() {
			return nil, errors.New("this keploy cannot read what each test did from its agent")
		}
		return nil, nil
	}
	if !provenswitch.On() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, agentEpilogueTimeout)
		defer cancel()
	}
	all, err := reader.GetScopeWindows(ctx)
	if err != nil {
		return nil, err
	}
	return m.takeRunWindows(all), nil
}

// takeRunWindows returns the windows of the run's tests, and keeps the
// windows that are no test's apart: the claims (m.claims), the refusals
// (m.refusals) and where the run's calls to local listeners went (m.live,
// m.mocked). Every reader of a run's windows goes through it.
func (m *mockService) takeRunWindows(all []models.ScopeWindow) []models.ScopeWindow {
	windows := all[:0:0]
	m.claims, m.refusals, m.live, m.mocked = nil, nil, nil, nil
	for _, w := range all {
		switch {
		case w.Refused != nil:
			m.refusals = append(m.refusals, *w.Refused)
		case w.Claim != "":
			m.claims = append(m.claims, w)
		case w.Live != nil:
			m.live = append(m.live, *w.Live)
		case w.Mocked != nil:
			m.mocked = append(m.mocked, *w.Mocked)
		default:
			windows = append(windows, w)
		}
	}
	return windows
}

// claimKeyAt is the key of the claim a call made at at belongs to: the
// latest claim made at or before it (claims are in the order they were
// made); "" before the first, which is models.PoolKeyNone's.
func claimKeyAt(claims []models.ScopeWindow, at time.Time) string {
	key := ""
	for _, c := range claims {
		if c.Start.After(at) {
			break
		}
		key = c.Claim
	}
	return key
}

// existsOnly is saveSet for a build whose judging keeps the set's backup: it
// only says whether the set exists, and restores nothing.
func (m *mockService) existsOnly(name string) (func(keep bool), bool, error) {
	if m.config.Path == "" {
		return func(bool) {}, true, nil
	}
	_, err := os.Stat(filepath.Join(m.config.Path, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, true, err
	}
	return func(bool) {}, err == nil, nil
}

// saveSet backs the named set up to a sibling .<name>.previous directory so a
// refused or interrupted re-record can be undone, and returns a restore(keep)
// closure plus whether the set existed. The backup is a HARD precondition: when
// it cannot be made, saveSet returns an error and makes NO change, because the
// caller is about to delete the live set's mappings and test cases — doing that
// without a restorable copy is unrecoverable data loss. The caller must abort
// on a non-nil error before any destructive step.
func (m *mockService) saveSet(name string) (func(keep bool), bool, error) {
	if m.config.Path == "" {
		return func(bool) {}, true, nil
	}
	set := filepath.Join(m.config.Path, name)
	saved := filepath.Join(m.config.Path, "."+name+".previous")
	_ = os.RemoveAll(saved)
	if _, err := os.Stat(set); errors.Is(err, fs.ErrNotExist) {
		// Nothing to lose: a fresh set. On a refused run, drop whatever the
		// aborted recording left behind.
		return func(keep bool) {
			if !keep {
				_ = os.RemoveAll(set)
			}
		}, false, nil
	}
	if err := os.CopyFS(saved, os.DirFS(set)); err != nil {
		_ = os.RemoveAll(saved)
		return nil, true, fmt.Errorf("back up mock set %q before re-recording: %w", name, err)
	}
	return func(keep bool) {
		if keep {
			_ = os.RemoveAll(saved)
			return
		}
		if rmErr := os.RemoveAll(set); rmErr != nil {
			m.logger.Warn("could not put the previous recording back", zap.String("mock-set", name), zap.Error(rmErr))
			return
		}
		if mvErr := os.Rename(saved, set); mvErr != nil {
			m.logger.Warn("could not put the previous recording back; it is kept at "+saved, zap.String("mock-set", name), zap.Error(mvErr))
		}
	}, true, nil
}

// caseNames lists the test cases the set holds before a re-record.
func (m *mockService) caseNames(ctx context.Context, name string) []string {
	if m.testDB == nil {
		return nil
	}
	tcs, err := m.testDB.GetTestCases(ctx, name)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(tcs))
	for _, tc := range tcs {
		ids = append(ids, tc.Name)
	}
	return ids
}

// deleteCases drops the set's old test cases so a re-record ends clean, as it does for mocks.
func (m *mockService) deleteCases(ctx context.Context, name string, ids []string) {
	if m.testDB == nil || len(ids) == 0 {
		return
	}
	if err := m.testDB.DeleteTests(ctx, name, ids); err != nil {
		m.logger.Debug("failed to delete the old test cases", zap.String("mock-set", name), zap.Error(err))
	}
}

// upsertCases writes which test cases each flow produced into the mapping.
func (m *mockService) upsertCases(ctx context.Context, name string, byCase map[string]models.MappedTestCase, startup []models.MockEntry, suites []models.SuiteSpan) {
	if len(byCase) == 0 && len(startup) == 0 && len(suites) == 0 {
		return
	}
	mapper, ok := m.mappingDB.(CaseMapper)
	if !ok {
		m.logger.Warn("the mapping store cannot record test cases per flow", zap.String("mock-set", name))
		return
	}
	if err := mapper.UpsertCases(ctx, name, byCase, startup, suites); err != nil {
		m.logger.Warn("failed to write per-flow test cases", zap.Error(err))
		return
	}
	m.logger.Info("wrote per-flow test cases", zap.Int("flows", len(byCase)), zap.String("mock-set", name))
}

func (m *mockService) upsertBoots(ctx context.Context, name string, boots []models.BootSpec) {
	mapper, ok := m.mappingDB.(BootMapper)
	if !ok {
		m.logger.Warn("the mapping store cannot record app starts", zap.String("mock-set", name))
		return
	}
	if err := mapper.UpsertBoots(ctx, name, boots); err != nil {
		m.logger.Warn("failed to write the app starts", zap.Error(err))
	}
}

// deleteMappings drops the set's old per-test mappings so a re-record cannot leave tests pointing at renamed mocks.
func (m *mockService) deleteMappings(ctx context.Context, name string) {
	if m.mappingDB == nil {
		return
	}
	deleter, ok := m.mappingDB.(MappingDeleter)
	if !ok {
		m.logger.Warn("the mapping store cannot delete, so old per-test mappings may linger", zap.String("mock-set", name))
		return
	}
	if err := deleter.Delete(ctx, name); err != nil {
		m.logger.Debug("no existing mappings to overwrite (or delete failed)", zap.String("mock-set", name), zap.Error(err))
	}
}

// capturedMock is one recorded mock's name, kind, place (models.PlaceOf)
// and source worker PID, or one recorded request's name and request and
// response times (record_requests.go), used to correlate them into
// per-test scope windows and to write their mapping entries.
type capturedMock struct {
	name string
	// kind is the mock's (models.Mock.Kind), which its mapping entry
	// states, as every other mapping writer does: keploy record's, the
	// window mappings', a pulled set's. Empty for a recorded request.
	kind string
	// ts is where the call sits (models.PlaceOf): its request timestamp, or
	// its answer's for one a parser holds until its answer (a consumer's
	// poll), which is the test's whose window holds its answer.
	ts   time.Time
	pid  uint32 // source worker PID (0 if unknown); enables exact parallel attribution
	end  time.Time
	boot bool
	// protocol: protocol-phase under the proven switch (models.Mock.IsProtocolPhase),
	// so in no test's mapping and in the pool.
	protocol bool
	start    string
	ref      string
}

// stagingSetID returns the staging set name record captures into while recording
// name, before promoting it over name on a complete, non-empty run. The suffix
// (yaml.MockStagingSuffix) is reserved and skipped by test-set enumeration, so a
// leftover staging dir from a hard kill is never listed as a phantom test-set.
// Appended to an already-single-segment set name it adds no path separator, so a
// promote or discard can never escape to touch a real set.
func stagingSetID(name string) string { return name + yaml.MockStagingSuffix }

// correlateScopes buckets each recorded mock into the per-test scope window its
// place (capturedMock.ts) falls within, producing the mappings.yaml structure. A mock
// that matches no window (e.g. a boot-time handshake before the first scope)
// is left out of every test's mapping — it stays reusable/session-tier at
// replay, exactly as the timestamp-based fallback would treat it.
//
// When a mock carries a source PID it is attributed to the SAME worker's window
// (exact, so overlapping parallel windows don't steal each other's mocks). That
// PID match is exact only when the worker made the call itself and the agent
// shares its PID namespace (the normal `keploy mock <cmd>` wrap); a call made by
// a CHILD of the worker, or a containerized/cross-namespace worker whose
// self-reported PID differs from the kernel PID, falls back to the timestamp
// scan — which is exact for sequential record and best-effort under overlap.
func correlateScopes(windows []models.ScopeWindow, mocks []capturedMock) map[string][]models.MockEntry {
	// Sort windows by start so overlapping scopes resolve to the innermost
	// (latest-started) window deterministically.
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Start.Before(windows[j].Start) })
	byTest := make(map[string][]models.MockEntry)
	// containingWindow returns the innermost (latest-started) window that contains
	// the mock's timestamp. When sameWorkerOnly is set (the mock carries a PID),
	// only windows recorded by that exact worker are considered — so overlapping
	// windows from OTHER parallel workers never steal the mock.
	containingWindow := func(mk capturedMock, sameWorkerOnly bool) int {
		best := -1
		for i, w := range windows {
			if sameWorkerOnly && w.PID != mk.pid {
				continue
			}
			if mk.ts.Before(w.Start) || mk.ts.After(w.End) {
				continue
			}
			if best == -1 || windows[i].Start.After(windows[best].Start) {
				best = i
			}
		}
		return best
	}
	for _, mk := range mocks {
		best := -1
		if mk.pid != 0 {
			// Exact, parallel-safe: attribute to the same worker's window.
			best = containingWindow(mk, true)
		}
		if best == -1 {
			// No same-worker window (PID-less mock/windows, or the call came from
			// a child process): fall back to a pure timestamp scan — correct when
			// windows don't overlap, i.e. sequential record.
			best = containingWindow(mk, false)
		}
		if best == -1 {
			continue
		}
		byTest[windows[best].Name] = append(byTest[windows[best].Name], models.MockEntry{Name: mk.name, Kind: mk.kind})
	}
	return byTest
}

// dataPlane is the captured mocks less the protocol-phase ones (none
// without the proven switch): only data-plane mocks go in a test's mapping.
func dataPlane(mocks []capturedMock) []capturedMock {
	out := mocks[:0:0]
	for _, mk := range mocks {
		if !mk.protocol {
			out = append(out, mk)
		}
	}
	return out
}

// correlateCases lists each flow's test cases by the window their request fell in; a case outside every window stays unmapped.
func correlateCases(windows []models.ScopeWindow, mocks, cases []capturedMock, steps []models.ScopeWindow) map[string]models.MappedTestCase {
	byTest := make(map[string]models.MappedTestCase)
	spans := make(map[string]models.ScopeWindow, len(cases))
	for _, c := range cases {
		spans[c.name] = models.ScopeWindow{Name: c.name, Start: c.ts, End: c.end}
	}
	for test, entries := range correlateScopes(windows, cases) {
		tc := models.MappedTestCase{CaseSteps: make(map[string]string, len(entries))}
		for _, e := range entries {
			tc.Cases = append(tc.Cases, e.Name)
			tc.CaseSteps[e.Name] = containing(steps, spans[e.Name].Start)
		}
		byTest[test] = tc
	}
	var own []capturedMock
	at := make(map[string]time.Time, len(mocks))
	for _, mk := range mocks {
		if !mk.boot {
			own = append(own, mk)
			at[mk.name] = mk.ts
		}
	}
	holder := make(map[string]string, len(cases))
	inFlow := make(map[string][]models.ScopeWindow)
	for test, tc := range byTest {
		for _, c := range tc.Cases {
			holder[c] = test
			inFlow[flowOf(windows, test)] = append(inFlow[flowOf(windows, test)], spans[c])
		}
	}
	for test, entries := range correlateScopes(windows, own) {
		for _, e := range entries {
			c := containing(inFlow[flowOf(windows, test)], at[e.Name])
			if c == "" {
				continue
			}
			tc := byTest[holder[c]]
			if tc.CaseMocks == nil {
				tc.CaseMocks = map[string][]string{}
			}
			tc.CaseMocks[c] = append(tc.CaseMocks[c], e.Name)
			byTest[holder[c]] = tc
		}
	}
	for _, tc := range byTest {
		for _, names := range tc.CaseMocks {
			sort.SliceStable(names, func(i, j int) bool { return at[names[i]].Before(at[names[j]]) })
		}
	}
	return byTest
}

// stampBegins keeps, for each test, when it began in this recording: its
// replay moves the clock of the run's processes to that instant while the
// test is open (design v14 §3.4). A name with more than one window (a
// subtest run twice) keeps its earliest begin.
func stampBegins(byCase map[string]models.MappedTestCase, windows []models.ScopeWindow) {
	for _, w := range windows {
		if w.Start.IsZero() {
			continue
		}
		tc := byCase[w.Name]
		if tc.At.IsZero() || w.Start.Before(tc.At) {
			tc.At = w.Start
		}
		byCase[w.Name] = tc
	}
}

// stampReal writes into each test's mapping the mock.real destinations it
// used while it was open (design v14 §4): its replays run on the real clock,
// and the verdict asks that CI reach them.
func stampReal(byCase map[string]models.MappedTestCase, windows []models.ScopeWindow) {
	for _, w := range windows {
		if len(w.Real) == 0 {
			continue
		}
		tc := byCase[w.Name]
		tc.Real = mergeSorted(tc.Real, realDests(w.Real))
		byCase[w.Name] = tc
	}
}

// stampGiven writes into each test's mapping the values it gave while it was
// open (models.ScopeWindow.Given), in the order it gave them: each replay
// binds the value the test gives then with the recorded one. Of a name with
// more than one window (a subtest run twice) the latest window's are kept,
// none included, as a retried test keeps its last attempt's (foldRetries):
// they pair with that attempt's calls.
func stampGiven(byCase map[string]models.MappedTestCase, windows []models.ScopeWindow) {
	last := make(map[string]int, len(windows))
	for i, w := range windows {
		if j, ok := last[w.Name]; !ok || !w.Start.Before(windows[j].Start) {
			last[w.Name] = i
		}
	}
	for name, i := range last {
		tc, ok := byCase[name]
		if !ok && len(windows[i].Given) == 0 {
			continue
		}
		tc.Given = slices.Clone(windows[i].Given)
		byCase[name] = tc
	}
}

func flowOf(windows []models.ScopeWindow, test string) string {
	flow := test
	for _, w := range windows {
		if strings.HasPrefix(test, w.Name+"/") && len(w.Name) < len(flow) {
			flow = w.Name
		}
	}
	return flow
}

func startupMocks(windows []models.ScopeWindow, mocks []capturedMock, claims []models.ScopeWindow) []models.MockEntry {
	var out []models.MockEntry
	for _, mk := range mocks {
		if mk.boot || mk.protocol || containing(windows, mk.ts) == "" {
			// Under the proven wire, the pool slice of the key it was made
			// under (design v14 §5.2); claims are none without it.
			out = append(out, models.MockEntry{Name: mk.name, Kind: mk.kind, Key: claimKeyAt(claims, mk.ts)})
		}
	}
	return out
}

func containing(windows []models.ScopeWindow, at time.Time) string {
	best := -1
	for i, w := range windows {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		if best == -1 || w.Start.After(windows[best].Start) {
			best = i
		}
	}
	if best == -1 {
		return ""
	}
	return windows[best].Name
}

// drainTrailingMocks blocks until the agent has evidently finished handing over
// mocks: no new arrival of one requested before the drain began
// (mocksSeen counts those) for mockDrainQuiet and the agent says it holds
// nothing more (clear), or the stream ended by itself. Clear is not
// delivered: a mock the agent had handed on and this client had not received
// yet is still on its way. From an agent that ends its streams on request,
// the clear is noted on round (roundEnd.endsByMarkers), and the caller then
// reads the streams to the agent's markers (endRound), so that mock is read;
// with any other agent, a stream ended once this returns loses it unseen
// (models.BacklogClear), which the quiet before the drain asks has almost
// always let through.
// It is bounded by progress, not by a deadline: an agent still handing over a
// backlog is waited for however long that takes, and so is one the user
// interrupted (Ctrl+C stops the recording, not the handing over of what was
// already captured). Calls something left running keeps making do not hold
// it. It must be called BEFORE the capture context is cancelled - see the
// call site.
//
// The user can end it: every mockDrainNotice it says what it is still doing
// (words.saving), and, once the run has been interrupted, that another
// interrupt stops it now and what that costs (words.stopNow;
// utils.StopNowNotice), and from then the next one does (StopNow). One that
// came before is not the user's answer to that: a stop sent to both sudo and
// keploy reaches keploy a second time from sudo, as the first does. A drain
// the user ends so did not get what the agent still held: it is lost (said
// with words.gaveUp).
//
// unconfirmed is true when it ended because an agent that does not hold its
// stream for the drain (docker compose stops it with the project) closed it:
// nothing then says the caller has all it captured, nor that it does not.
func (m *mockService) drainTrailingMocks(consumerDone <-chan struct{}, from time.Time, agentHolds bool, held func() (progress uint64, busy bool), arrived func() uint64, words drainWords, round *roundEnd) (lost, unconfirmed bool) {
	quiet := time.NewTimer(mockDrainQuiet)
	defer quiet.Stop()
	notice := utils.NewStopNowNotice(mockDrainNotice)
	defer notice.Withdraw()
	notices := time.NewTicker(mockDrainNotice)
	defer notices.Stop()
	savedAtStart := arrived()

	last, _ := held()
	looks := utils.BacklogLooks{Stall: mockDrainStallLooks}
	// lookedAt is arrived()'s count at the drain's latest look the agent was
	// not gone for (utils.BacklogLooks.Said).
	lookedAt := arrived()
	for {
		select {
		case <-consumerDone:
			// The agent closed the stream on its own. One that holds it for
			// this drain (agentHolds) closes it only as it goes away: it was
			// stopped, or crashed, before the drain was through. Otherwise
			// (compose: the agent goes with the project) nothing is left to
			// wait for.
			if agentHolds {
				// A second interrupt that reached the agent too ends its
				// stream as it gives the drain up: the user's stop.
				select {
				case <-utils.StopNow():
					return m.stoppedNow(words), false
				default:
					return m.agentEndedDrain(&looks, arrived() == lookedAt), false
				}
			}
			// The agent gave no word that it had handed over all it had:
			// its stream just closed. What this caller saves cannot be
			// confirmed whole: the caller says so if it saves it.
			return false, true
		case <-utils.StopNow():
			return m.stoppedNow(words), false
		case <-notices.C:
			msg := words.saving
			if notice.Next() {
				msg += "; " + words.stopNow
			}
			m.logger.Info(msg, zap.Uint64("sinceTheEnd", arrived()-savedAtStart))
		case <-quiet.C:
			// Quiet means the stream brought nothing held while the consumer
			// was free to take it: a consumer still persisting one is not
			// quiet, however long the write takes.
			now, busy := held()
			if now != last || busy {
				last = now
				quiet.Reset(mockDrainQuiet)
				continue
			}
			// Quiet is not proof: an agent whose capture can tell says whether
			// it may still hand over a mock from before the runner exited,
			// and one that does not answer may still hold one
			// (holdForBacklog).
			lookedBefore := lookedAt
			lookedAt = arrived()
			wait, gaveUp := m.holdForBacklog(from, &looks, now, round, words)
			if looks.LastGone() {
				// A look the agent was gone for says nothing of what it
				// held: what came since counts from the one before.
				lookedAt = lookedBefore
			}
			if wait {
				quiet.Reset(mockDrainQuiet)
				continue
			}
			return gaveUp, false
		}
	}
}

// drainWords is how drainTrailingMocks speaks to its caller's user: of what
// it is still doing, and of what ending it at a second interrupt costs.
type drainWords struct {
	// saving is what it is still doing, said every mockDrainNotice.
	saving string
	// stopNow, added to that once it offers to stop at the next interrupt,
	// says so and what doing so costs.
	stopNow string
	// gaveUp and nextStep are said at WARN when the user ends it so.
	gaveUp, nextStep string
	// Once the agent said "clear", the round ends on its markers (endRound):
	// endedEarly is the next step when an agent that only this client stops
	// left before them (the run fails); notTheAgent when one something else
	// stops did (said at WARN); endedHere when the streams were ended here
	// all the same, a frame on its way lost.
	endedEarly, notTheAgent, endedHere string
	// stalled and stalledNext are said at ERROR when the agent made no
	// progress through what it said it still held (holdForBacklog).
	stalled, stalledNext string
	// Once the capture has stopped, its consumers are waited for
	// (waitConsumers): writing is what they are still doing, said every
	// mockDrainNotice; notWritten is said at ERROR, with nextStep, when the
	// user ends that wait at a second interrupt.
	writing, notWritten string
	// Then the mock store writes what it still holds (closeStore):
	// storeWriting is said every mockDrainNotice while it does, and
	// storeNotWritten at ERROR when it did not write every mock.
	storeWriting, storeNotWritten string
	// casesNotWritten is said at ERROR when the test-case store did not
	// write every test case (--record-requests).
	casesNotWritten string
}

// recordDrainWords are keploy mock record's: a recording ended before it has
// every mock the agent captured is not saved over the existing set.
var recordDrainWords = drainWords{
	saving:   "Still saving the mocks the keploy agent captured before the run ended",
	stopNow:  "press Ctrl+C again to stop now: this recording is then not saved, and the existing mock set is kept",
	gaveUp:   "stopped saving the mocks the agent captured before the run ended, at a second interrupt",
	nextStep: "this recording was not saved; the existing mock set is kept. Re-record it",

	endedEarly:  "the agent was stopped or crashed before the recording had read all it had; the run ends as failed and the existing mock set is kept. Do not stop the agent separately from the recording; re-record",
	notTheAgent: "stop the recording, not the agent: the recording ends the agent's streams once it has saved what the agent captured",
	endedHere:   "a mock may be missing from this recording; if a replay of it misses a call, record it again",

	stalled:     "stopped waiting for the keploy agent to hand over the mocks it captured before the run ended: it made no progress",
	stalledNext: "the mocks the agent still held are not saved, and the run ends as failed. Re-record; if the agent was starved of CPU, give it more",

	writing:    "Still writing the mocks and test cases the keploy agent handed over to this recording",
	notWritten: "the recording was not saved: a second interrupt stopped keploy writing the mocks and test cases the agent had handed over to it",

	storeWriting:    "Still writing the recording's mocks to the mock file",
	storeNotWritten: "the recording was not saved: the mock file did not take every mock the recording captured",
	casesNotWritten: "the recording was not saved: the test-case store did not take every test case the recording captured",
}

// composeTailUnconfirmed, with its next step, is said at WARN once keploy mock
// record has saved a recording whose drain was unconfirmed
// (drainTrailingMocks): under docker compose the agent goes with the project,
// and nothing says the recording holds every mock it captured.
const (
	composeTailUnconfirmed     = "the keploy agent closed its mock stream as docker compose stopped it with the project, without saying it had handed over every mock it captured: keploy cannot confirm this recording holds them all"
	composeTailUnconfirmedNext = "the recording is saved; replay the set to check it: under --on-miss fail (the default) a call it is missing fails the replay. Or record the test command natively (keploy mock record -c \"<test command>\"), where keploy confirms it"
)

// replayDrainWords are keploy mock replay's: it collects the requests the
// agent captured, for the run's report. A run short of them is not judged
// (Replay).
var replayDrainWords = drainWords{
	saving:   "Still collecting the requests the keploy agent captured before the run ended",
	stopNow:  "press Ctrl+C again to stop now: this run then ends unjudged, as one stopped before it finished",
	gaveUp:   "stopped collecting the requests the agent captured before the run ended, at a second interrupt",
	nextStep: "this run ends unjudged, as one stopped before it finished; the last run's receipt is kept. Run it again",

	endedEarly:  "the agent was stopped or crashed before keploy had read every request it captured; the run fails as keploy's, not the suite's. Do not stop the agent separately from the replay; run it again",
	notTheAgent: "stop the replay, not the agent: the replay ends the agent's streams once it has read what the agent captured",
	endedHere:   "a request may be missing from what this run compared; if one is reported missing, run it again",

	stalled:     "stopped waiting for the keploy agent to hand over the requests it captured before the run ended: it made no progress",
	stalledNext: "the run fails as keploy's, not the suite's, short of the requests the agent still held. Run it again; if the agent was starved of CPU, give it more",

	writing:    "Still collecting the requests the keploy agent handed over",
	notWritten: "stopped collecting the requests the agent handed over, at a second interrupt",
}

// stoppedNow is drainTrailingMocks ended by the user's second interrupt
// (utils.StopNow): what the agent still held is not had, so the drain is lost.
func (m *mockService) stoppedNow(words drainWords) (lost bool) {
	m.logger.Warn(words.gaveUp, zap.String("next_step", words.nextStep))
	return true
}

// armGivenUp arms the exit code of a recording that is not saved because the
// user gave up what it still waited for (the drain, the consumers, the mock
// store's close): 128+N for the signal that stopped the run
// (utils.InterruptExitCode), as keploy mock replay exits for a run the user
// stopped before it finished, or keploy's 1 where no signal's number is known
// (a build with its own handler). keploy mock record's command returns no
// error for a run the user stopped (an interrupt is not a failure), so the
// process exits with this code: before, it exited 0, a pass, for a recording
// that was not saved. A code already armed is kept (SetExitCodeOnce).
func armGivenUp() {
	code := utils.InterruptExitCode()
	if code == 0 {
		code = utils.ExitKeployError
	}
	utils.SetExitCodeOnce(code)
}

// runFailure is why a run the user did not stop ended: the agent's container
// says so under compose; natively its death fails the run's own errgroup
// (ctx). nil: nothing failed.
func (m *mockService) runFailure(ctx context.Context) error {
	if cause := m.composeAgentFailure(); cause != nil {
		return cause
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// agentEndedDrain reports the agent ending its mock stream while the drain
// still needed it (drainTrailingMocks): what it still held is lost. The run
// fails (lost), said at ERROR, and the set it recorded is not saved over the
// existing one, when the agent itself gave a sign it held something (at the
// latest look it was not already gone for, it said so, or did not answer:
// Held), or when this client started the agent (startedAgent): such an agent
// holds its stream open until this drain is done, so one that ends it first
// went away before the run could be confirmed complete. The loss is said to be
// certain only when it said it held something and no mock from before the
// runner exited came since (noneSince). Otherwise it is said at WARN.
func (m *mockService) agentEndedDrain(looks *utils.BacklogLooks, noneSince bool) (lost bool) {
	if !looks.Held() && !startedAgent(m.instrumentation) {
		m.logger.Warn("the keploy agent ended its mock stream before it said it held nothing more from before the run ended: mocks it still held, if any, are lost",
			zap.String("next_step", "stop the recording, not the agent: the recording stops the agent once it has saved what the agent captured"))
		return false
	}
	said, answered := looks.Said()
	msg := "the keploy agent ended its mock stream before it confirmed it had handed over all the mocks it captured before the run ended: some may be lost"
	if answered && said == models.BacklogPending && noneSince {
		msg = "the keploy agent ended its mock stream while it still held mocks it captured before the run ended: those are lost"
	}
	var fields []zap.Field
	if answered {
		fields = append(fields, zap.Stringer("agent_said", said))
	}
	m.logger.Error(msg, append(fields,
		zap.String("next_step", "the agent was stopped or crashed before the recording had saved what it held; the run ends as failed and the existing mock set is kept. Do not stop the agent separately from the recording; re-record"))...)
	utils.SetExitCodeOnce(utils.ExitKeployError)
	return true
}

// startedAgent reports whether the agent client started the agent it talks to
// (AgentClient.StartedAgent): only this client stops such an agent.
func startedAgent(inst any) bool {
	s, ok := inst.(interface{ StartedAgent() bool })
	return ok && s.StartedAgent()
}

// backlogReader is an agent client that can ask the agent whether it may still
// hand over something captured before a time (AgentClient.BacklogBefore).
type backlogReader interface {
	BacklogBefore(ctx context.Context, before time.Time) (models.AgentBacklog, error)
}

// mockDrainStallLooks is how many looks in a row the drain makes at a backlog
// the agent says is pending, or does not answer about, with nothing handed
// over and no progress in the agent's capture between them, before it stops
// waiting and reports what is lost. A count, not a time: each look comes once
// the stream has been quiet for mockDrainQuiet, so the drain waits at least
// that times this on a backlog that does not move, well past the agent's own
// hang bound (a parser that holds work and makes no progress is retired,
// which is progress here).
const mockDrainStallLooks = 180

// holdForBacklog decides, for the trailing drain once the stream has gone
// quiet, whether it goes on waiting for what the agent still holds from
// before the runner exited (true) or ends (false), by the agent's answer
// (utils.BacklogLooks.Look): clear or unsupported end it; pending, or no
// answer at all, keep it going, never read as "nothing pending". frames is
// the drain's own progress so far. A wait that sees no progress for
// mockDrainStallLooks looks ends with an error that says what the agent still
// held, and the run fails (a Keploy-side exit code, never over the runner's
// own), in its caller's words. round (nil: none) notes whether the answer
// lets the round end on the agent's markers (roundEnd.clear).
func (m *mockService) holdForBacklog(before time.Time, looks *utils.BacklogLooks, frames uint64, round *roundEnd, words drainWords) (wait, gaveUp bool) {
	br, ok := m.instrumentation.(backlogReader)
	if !ok {
		return false, false
	}
	b, err := br.BacklogBefore(context.Background(), before)
	round.clear(b, err)
	wait, gaveUp = looks.Look(b, err, frames)
	if gaveUp {
		m.logger.Error(words.stalled,
			zap.Stringer("agent_says", b.Verdict),
			zap.String("waiting_on", b.WaitingOn),
			zap.Int("looks_without_progress", looks.Stalled()),
			zap.NamedError("last_look_error", err),
			zap.String("next_step", words.stalledNext))
		utils.SetExitCodeOnce(utils.ExitKeployError)
		return false, true
	}
	if wait && b.Verdict == models.BacklogCannotTell && looks.Stalled() == 1 {
		m.logger.Info("could not ask the keploy agent what it still holds from before the run ended; waiting for it", zap.Error(err))
	}
	return wait, false
}

// nothingCapturedNext is what to do about a record that captured no call.
// Under the proven switch a test's calls are captured once its harness
// reports it to keploy, so the integration is named; with the switch off the
// advice is as it was.
func nothingCapturedNext() string {
	if provenswitch.On() {
		return "confirm the tests call an external dependency (HTTP, MySQL, ...) and report themselves to keploy: in Go, make `defer keploytest.T(t)()` each test's first line (github.com/keploy/go-sdk/v3/keploytest)"
	}
	return "confirm the test command actually calls an external dependency (HTTP, MySQL, ...), and on macOS run it via a docker command"
}

// foldRetries keeps, of each top-level test that ran more than once, its
// last attempt's window, failed if any attempt failed, and returns the
// earlier attempts' windows apart, with how often each retried test ran.
func foldRetries(windows []models.ScopeWindow) ([]models.ScopeWindow, []models.ScopeWindow, map[string]int) {
	last := map[string]int{}
	failed := map[string]bool{}
	runs := map[string]int{}
	for i, w := range windows {
		if parentOf(windows, w.Name) != "" {
			continue
		}
		runs[w.Name]++
		if j, ok := last[w.Name]; !ok || !w.Start.Before(windows[j].Start) {
			last[w.Name] = i
		}
		if w.Outcome != models.ScopeOutcomePassed && w.Outcome != models.ScopeOutcomeSkipped {
			failed[w.Name] = true
		}
	}
	var kept, earlier []models.ScopeWindow
	retried := map[string]int{}
	// An earlier attempt's subtest windows go with it: a subtest window
	// that began inside an earlier attempt of its parent.
	inEarlier := func(w models.ScopeWindow) bool {
		top := parentOf(windows, w.Name)
		if top == "" || runs[top] < 2 {
			return false
		}
		for i, p := range windows {
			if p.Name == top && i != last[top] && !w.Start.Before(p.Start) && !w.Start.After(p.End) {
				return true
			}
		}
		return false
	}
	for i, w := range windows {
		if inEarlier(w) {
			earlier = append(earlier, w)
			continue
		}
		if parentOf(windows, w.Name) != "" || runs[w.Name] < 2 {
			kept = append(kept, w)
			continue
		}
		retried[w.Name] = runs[w.Name]
		if last[w.Name] != i {
			earlier = append(earlier, w)
			continue
		}
		if failed[w.Name] {
			w.Outcome = models.ScopeOutcomeFailed
		}
		kept = append(kept, w)
	}
	return kept, earlier, retried
}

// withoutRetries is mocks without those captured during an earlier attempt
// of a retried test (foldRetries).
func withoutRetries(mocks []capturedMock, earlier []models.ScopeWindow) []capturedMock {
	if len(earlier) == 0 {
		return mocks
	}
	out := make([]capturedMock, 0, len(mocks))
	for _, mk := range mocks {
		if containing(earlier, mk.ts) == "" {
			out = append(out, mk)
		}
	}
	return out
}
