// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/realclient"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// mappingFlushBatch is how many mappings accumulate before mappings.yaml is
	// rewritten. Each rewrite re-encodes the whole file, so writing per mapping is
	// quadratic (368 tests: 164us for the first, 19.45ms for the last, ~2.7s of
	// pure rewriting) — slow enough to back-pressure the agent's mapping stream,
	// which then DROPS what it cannot hand over. Batching keeps the consumer far
	// ahead of the stream.
	mappingFlushBatch = 32

	// mappingFlushInterval bounds how long a partial batch waits, so a long
	// recording still persists mappings as it goes instead of holding them all in
	// memory until the stream closes.
	mappingFlushInterval = 2 * time.Second

	// closedStreamGrace is how long a frame stream that has been ended may take
	// to hand over what its reader had already decoded (forwardUntilDrained).
	closedStreamGrace = 250 * time.Millisecond

	// mappingIdleGrace ends the shutdown drain once the mapping stream falls idle.
	// The agent holds the stream open for the whole session, so idleness — not EOF
	// — is what signals the tail is through. Sized well above the agent's
	// per-mapping flush latency so a slow flush is not mistaken for completion.
	mappingIdleGrace = 3 * time.Second
)

// stopTimer disarms t, draining its channel if it had already fired, so a later
// Reset starts from a clean state.
func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// drainOpts says how forwardUntilDrained treats one stream's frames.
type drainOpts[T any] struct {
	// capturedAt is when a frame was captured (zero: it does not say).
	capturedAt func(T) time.Time
	// dropped logs a frame taken off the stream that no consumer will take.
	dropped func(T)
	// closeStream ends the stream, once the drain is over.
	closeStream func()
	// afterStop, when set, is told of a frame captured after the stop,
	// undated ones included, which is then not forwarded. It is set when the
	// app keeps serving through the drain (docker compose): what it serves
	// then is not part of the recording. Without it such frames are forwarded
	// (a native app is already stopped: nothing after the stop is new).
	afterStop func(T)
	// keepUndated keeps undated frames even when afterStop is set.
	keepUndated bool
	// handing, when set, is called with each frame about to be handed to the
	// consumer, and returns what to run once it has been. It reads what it
	// needs from the frame there: a handed frame is the consumer's, which may
	// change it (InsertTestCase names an unnamed test case in place).
	handing func(T) (handed func())
	// kind names the frames in logs ("test cases", "mocks").
	kind string
	// stream is the agent stream the frames come on, and ends the
	// recording's stream end, for its end by the agent's marker (streamsEnd).
	stream models.FrameStream
	ends   *streamsEnd
}

// beforeHand runs o.handing for a frame about to be handed over, and returns
// what to run once it has been.
func (o drainOpts[T]) beforeHand(item T) (handed func()) {
	if o.handing == nil {
		return func() {}
	}
	return o.handing(item)
}

// forwardUntilDrained forwards a frame stream from the agent to its consumer,
// and on through the stop until the agent says it holds nothing more captured
// before it (which is not yet that all of it has reached this client: see
// "clear" below).
//
// The agent's parsers run behind the traffic, so at the stop a busy recording
// still has frames queued there: at 110 req/s the CLI that stopped reading at
// the stop and gave the stream one 30 s wait lost the mocks of 1,655 of 3,300
// tests. So a stop does not end the forwarding. The drain ends when the stream
// closes, when nothing will consume it (abandoned), or once the agent says it
// holds nothing more from before the stop: each time the stream has delivered
// nothing captured before the stop for mappingIdleGrace, the drain asks it
// (holdForBacklog). The agent holds its streams open for the whole session,
// and its parsers can be quiet, behind the traffic, for longer than any grace,
// so quiet alone ends a drain only with an agent that can never tell. One
// that does not answer is waited for, and a backlog that makes no progress
// look after look ends the drain loudly, the recording failed. Frames
// captured after the stop, and undated ones, say nothing about that backlog
// and do not extend it; see drainOpts.afterStop for whether they are
// forwarded. There is no bound on the total: a sink still draining is never
// cut off.
//
// A frame taken off the stream is always handed over, unless no consumer will
// ever come (abandoned), when dropped logs it. When the drain ends, closeStream
// ends the stream, and what its reader has already queued is still handed
// over before this returns; a frame the reader holds at that moment can be
// lost.
//
// The agent's "clear" is not "delivered": it says its capture, its parsers
// and its test-case queue hold nothing from before the stop, not that what
// they already handed on has reached this client. Mocks the agent's mock
// manager still buffers or has sent (its outgoing channel, the stream's
// encoder, the socket, this client's reader) are not counted. The drain asks
// only once the stream has been quiet for its bound, which such frames have
// almost always crossed by then, but a frame still on its way when the stream
// is ended is lost unseen. An end marker the agent writes on each stream once
// it has flushed what it holds, read here before the stream is ended, closes
// this; until then the drain can end early.
//
// Stop and frame times are compared across the CLI and the agent, which run on
// one host (natively, or in a local container) and so share its clock.
func forwardUntilDrained[T any](ctx context.Context, r *Recorder, stream <-chan T, out chan<- T,
	abandoned <-chan struct{}, o drainOpts[T]) {
	// An agent that ends its streams on request (streamsEnd): once it says
	// "clear", this drain reads its stream to the agent's marker (ending),
	// not until it has been quiet for a while after it closed it.
	ender, canEnd := r.instrumentation.(streamEnder)
	ends := o.ends
	canEnd = canEnd && o.stream != "" && ends != nil
	if canEnd {
		// Joined before this drain started (GetTestAndMockChans): a drain
		// that clears first cannot have the streams ended before this one
		// is there to read its marker.
		defer ends.left(r, ender, o.stream)
	}
	ending := false
	var stoppedAt time.Time
	quiet := r.frameQuietGrace()
	idle := time.NewTimer(quiet)
	defer idle.Stop()
	stopTimer(idle)
	// quietUntil is when the stream will have been quiet for the whole bound,
	// once the stop has come. Only time spent waiting on the stream counts:
	// the bound is paused while a frame is handed to a consumer that is still
	// persisting the last one (the channels hold one frame), or one slow
	// write (a 12 MB mock, a disk stall) ended the drain with the agent still
	// holding frames, and the stream's ready frame then lost a random race
	// against the timer that fired meanwhile.
	var quietUntil time.Time
	// stopCame starts the drain: the stop has come, now.
	stopCame := func() {
		stoppedAt = time.Now()
		quietUntil = stoppedAt.Add(quiet)
		resetTimer(idle, quiet)
	}
	// sinceStop counts the frames handed on since the stop. giveUp ends the
	// drain at a second interrupt, and says what it got.
	var sinceStop int
	// looks is this drain's looks at what the agent still holds from before
	// the stop, once its stream falls quiet (holdForBacklog).
	var looks backlogLooks
	// giveUp ends the drain at the user's second interrupt (utils.StopNow).
	// What the agent still held is not saved, so the recording is incomplete
	// (captureLost): it fails, as one that lost it any other way does.
	giveUp := func() {
		o.closeStream()
		r.captureLost.Store(true)
		r.logger.Warn("stopped saving what the agent captured before the stop, at a second interrupt",
			zap.String("frames", o.kind), zap.Int("savedSinceTheStop", sinceStop),
			zap.String("next_step", "what the agent still held was not saved: test cases it had captured may be missing, or saved without their mocks"))
	}
	// keep reports whether a frame taken off the stream goes on to the
	// consumer, and whether it is progress: a frame from before the stop.
	keep := func(item T) (forward, progress bool) {
		if stoppedAt.IsZero() {
			return true, false
		}
		switch at := o.capturedAt(item); {
		case at.IsZero():
			// Undated: no sign of a backlog, or undated traffic after the stop
			// would hold the drain open.
			if o.afterStop == nil || o.keepUndated {
				return true, false
			}
		case !at.After(stoppedAt):
			return true, true
		case o.afterStop == nil:
			return true, false
		}
		o.afterStop(item)
		return false, false
	}
	// mayBeHeld reports whether a frame handed on after the stop may be what
	// the agent said it held from before it: one captured before the stop, or
	// undated. Counted in r.heldFrames (agentEndedDrain).
	mayBeHeld := func(item T) bool {
		at := o.capturedAt(item)
		return at.IsZero() || !at.After(stoppedAt)
	}
	hand := func(item T, progress bool) bool {
		draining := !stoppedAt.IsZero()
		var left time.Duration
		if draining {
			left = max(time.Until(quietUntil), 0)
		}
		var again <-chan struct{}
		if draining {
			again = utils.StopNow()
		}
		onHanded := o.beforeHand(item)
		handed := false
		select {
		case out <- item:
			r.frameProgress.Add(1)
			if draining {
				sinceStop++
				if mayBeHeld(item) {
					r.heldFrames.Add(1)
				}
			}
			onHanded()
			handed = true
		case <-abandoned:
			o.dropped(item)
		case <-again:
			giveUp()
			return false
		}
		if draining {
			if progress {
				quietUntil = time.Now().Add(quiet)
			} else {
				// Not progress, but not quiet either: the bound resumes
				// where it was paused.
				quietUntil = time.Now().Add(left)
			}
			// Re-armed, and drained of a fire that came meanwhile, so the
			// next select never weighs a stale fire against a ready frame.
			resetTimer(idle, time.Until(quietUntil))
		}
		return handed
	}
	for {
		var shutdownC, againC <-chan struct{}
		var idleC <-chan time.Time
		if stoppedAt.IsZero() {
			shutdownC = ctx.Done()
		} else {
			if !ending {
				idleC = idle.C
			}
			againC = utils.StopNow()
		}
		select {
		case <-againC:
			giveUp()
			return
		case <-shutdownC:
			stopCame()
			continue
		case <-idleC:
			// Quiet is not proof: an agent whose capture can tell says whether
			// it may still hand over something from before the stop (a parser
			// behind the traffic can be quiet for longer than any grace). One
			// that does not answer may still hold something: only its "no",
			// or a stall it makes no progress through, ends the drain
			// (holdForBacklog).
			if r.holdForBacklog(stoppedAt, &looks, o.kind) {
				quietUntil = time.Now().Add(quiet)
				resetTimer(idle, quiet)
				continue
			}
			if canEnd && looks.endable {
				// "Clear", from an agent that ends its streams on request:
				// read on to its marker, which comes once every drain is
				// clear and it has put on the stream all it had. A frame
				// still on its way here is read, not lost to a grace.
				ending = true
				stopTimer(idle)
				ends.clear(r, ender, o.stream, o.closeStream)
				continue
			}
			// The agent holds nothing more from before the stop: its capture,
			// parsers and test-case queue are through it. End the stream, and
			// hand over what its reader had already queued; a frame still on
			// its way from the agent is not waited for ("clear" is not
			// "delivered", above).
			o.closeStream()
			for {
				// The reader closes the stream once its request ends; one that
				// does not is taken as done once it has nothing buffered.
				var item T
				var ok bool
				resetTimer(idle, closedStreamGrace)
				select {
				case item, ok = <-stream:
				case <-idle.C:
				}
				if !ok {
					return
				}
				stopTimer(idle)
				if forward, _ := keep(item); forward {
					onHanded := o.beforeHand(item)
					select {
					case out <- item:
						r.frameProgress.Add(1)
						if mayBeHeld(item) {
							r.heldFrames.Add(1)
						}
						onHanded()
					case <-abandoned:
						o.dropped(item)
						return
					}
				}
			}
		case <-abandoned:
			// No consumer is coming; what is still queued on the stream has
			// nowhere to go (see the MAPPINGS forwarder's note).
			return
		case item, ok := <-stream:
			marker := !ok && canEnd && ender.StreamEnded(o.stream)
			if marker || (!ok && ending && ends.failed.Load()) {
				// Read to the agent's marker (or closed here, the agent not
				// taking the request to end it): every frame it had for this
				// drain is through.
				if marker {
					r.logger.Debug(StreamEndedLog, zap.String("frames", o.kind))
				}
				return
			}
			if !ok {
				// The agent ended the stream, not this drain (which closes
				// it only to read the rest in its own loop above, or as it
				// gives up), and without its marker: it did not say it had
				// sent all it had. Judged by that, whenever it came: before
				// the stop, at it, or during its drain. Whether the stop had
				// begun is no measure of it: an agent killed or crashed
				// mid-recording, or one docker compose or kubelet stops on a
				// signal that reached it first, ends its streams before the
				// recording knows it is stopping, and an end taken for one
				// before the stop was said to be no loss at all.
				select {
				case <-utils.StopNow():
					// A second interrupt that reached the agent too ends its
					// stream as it gives the drain up: that is the user's
					// stop, and said as one, whichever this select saw first.
					giveUp()
				default:
					r.agentEndedDrain(o.kind, &looks)
				}
				return
			}
			// The stop and a frame can both be ready when this select runs,
			// and it takes either: a frame taken once the stop has come is
			// judged by it, as one taken after the stop branch would be, or
			// what the app served after the stop is recorded.
			if stoppedAt.IsZero() && ctx.Err() != nil {
				stopCame()
			}
			if forward, progress := keep(item); forward && !hand(item, progress) {
				return
			}
		}
	}
}

// testCaseCapturedAt is when a test case was complete (TestCase.CompletedAt):
// its response, else its request, HTTP or gRPC. One with neither is undated.
func testCaseCapturedAt(tc *models.TestCase) time.Time { return tc.CompletedAt() }

// resetTimer re-arms t for d, safely draining any pending fire first.
func resetTimer(t *time.Timer, d time.Duration) {
	stopTimer(t)
	t.Reset(d)
}

// mergeMockEntries unions incoming entries into existing ones by mock name,
// preserving recorded order. Mirrors the mapping store's merge: a test's mapping
// arrives in more than one piece and the later pieces are deltas, so replacing
// would delete mocks the earlier piece recorded.
func mergeMockEntries(existing, incoming []models.MockEntry) []models.MockEntry {
	if len(existing) == 0 {
		return incoming
	}
	seen := make(map[string]struct{}, len(existing))
	for _, e := range existing {
		seen[e.Name] = struct{}{}
	}
	merged := existing
	for _, e := range incoming {
		if _, dup := seen[e.Name]; dup {
			continue
		}
		seen[e.Name] = struct{}{}
		merged = append(merged, e)
	}
	return merged
}

// consumeMappings correlates each test<->mock mapping the agent streams to its
// real mock entries and persists it to mappings.yaml. It runs until mappings is
// closed by the producer, and returns only then — ctx bounds nothing here by
// design.
//
// Mappings are the last artifact a recorded test produces (the agent resolves a
// test's mock range only once that test is done), so the tail of every endpoint
// arrives while recording is already shutting down. Writes therefore run on a
// context detached from ctx: on a cancelled one the yaml store refuses every
// write and those tests vanish from mappings.yaml, which replay then reports as
// no_mocks. Detaching here rather than relying on the caller keeps that
// guarantee local to the loop that depends on it — see the persistCtx note in
// Start for the same requirement on the test-case and mock stores.
// shortPoolByTest is required, not optional: it is how the one data-loss path
// this function deliberately does not revoke stays visible. It maps a test name
// to the number of its mocks that never correlated. Totals are computed at
// teardown rather than accumulated here, because a test can also be revoked by
// the AGENT (a RevokedTests control frame this function never sees) and a
// revoked test is deleted, not shipped with a short pool.
//
// This form has no mock stream beside it: every mock's outcome is already
// published, so an entry whose mock is unknown counts as uncorrelated at once.
func (r *Recorder) consumeMappings(ctx context.Context, testSetID string, mappings <-chan models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet, revokeTest func(string), shortPoolByTest *sync.Map) error {
	return r.consumeMappingsFor(ctx, testSetID, mappings, correlationMap, asyncMockIDs, droppedMockIDs, nil, revokeTest, shortPoolByTest, nil)
}

// consumeMappingsFor is consumeMappings beside a live mock stream, and telling
// mapped, when set, each test's mocks as its mapping resolves them: the capture
// watch charges a test with a mock of its whose container the Docker daemon did
// not answer the read of (captureWatch.mapped), whenever that mapping comes.
//
// waits holds the entries whose mock the mock loop has not reported yet
// (mappingWaits): they are applied when it does, and the function returns only
// once the mapping stream is closed AND no entry still waits — an entry whose
// mock never came is settled when the mock loop ends its stream (waits.end).
// Nothing here is bounded by a clock: on a loaded host a mock is saved long
// after the mapping that names it, and giving up on a clock is what left such
// a mock out of its test's mapping while it sat in the mocks file.
func (r *Recorder) consumeMappingsFor(ctx context.Context, testSetID string, mappings <-chan models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet, waits *mappingWaits, revokeTest func(string), shortPoolByTest *sync.Map, mapped func(test string, entries []models.MockEntry)) error {
	persistCtx := context.WithoutCancel(ctx)

	// pending batches mappings so the file is rewritten once per batch instead of
	// once per test. flushMu guards it because the ticker below flushes too.
	pending := make(map[string][]models.MockEntry, mappingFlushBatch)
	var flushMu sync.Mutex

	// Both are touched only by this loop (the ticker goroutine below flushes
	// `pending` under flushMu and never reads these), so plain maps are safe.
	revokedHere := map[string]struct{}{}

	flush := func() {
		flushMu.Lock()
		defer flushMu.Unlock()
		if len(pending) == 0 {
			return
		}
		if err := r.mappingDb.UpsertBatch(persistCtx, testSetID, pending); err != nil {
			// Deliberately not utils.LogError: it suppresses context.Canceled,
			// which is exactly the class this write used to fail with, so a lost
			// batch left no trace at all. A mapping that goes missing here is a
			// no_mocks failure at replay — it must never be silent again.
			names := make([]string, 0, len(pending))
			for tn := range pending {
				names = append(names, tn)
			}
			sort.Strings(names)
			r.logger.Error("failed to save mappings",
				zap.Strings("tests", names),
				zap.Error(err),
				zap.String("next_step", "these tests' mocks will be missing from mappings.yaml and replay will report no_mocks for them; re-record the test set"))
		}
		clear(pending)
	}

	// A partial batch must not sit in memory until the stream closes: recording
	// can run for hours, and an operator watching mappings.yaml should see it
	// grow. The ticker bounds how long a mapping stays unpersisted without
	// putting a file rewrite on the per-mapping path.
	ticker := time.NewTicker(mappingFlushInterval)
	defer ticker.Stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				flush()
			}
		}
	}()

	// apply folds what one test's entries settled as into the batch, the
	// short-pool counts and the revokes. A test's entries settle in parts — a
	// mapping's entries whose mocks were saved at once, then each held one as
	// its mock arrives — and every part comes through here.
	apply := func(testName string, res mappingResolution) {
		realMockEntries, lostMock, uncorrelated := res.entries, res.lost, res.uncorrelated

		// Persisted anyway (see addSettled), so record that this test
		// reaches replay with an incomplete mock set.
		//
		// Counted per DISTINCT TEST, not per mapping: the agent emits a test's
		// mocks as several delta mappings (see the merge note below), so a
		// per-mapping counter would report more "tests" than the set contains.
		// A revoked test is excluded — it is deleted at finalize, so it never
		// reaches replay with a short pool at all.
		if uncorrelated > 0 && !lostMock {
			if _, revoked := revokedHere[testName]; !revoked {
				prev, _ := shortPoolByTest.Load(testName)
				n, _ := prev.(int64)
				shortPoolByTest.Store(testName, n+int64(uncorrelated))
			}
		}

		// This test owned at least one mock we could not persist. Replaying it
		// with a short pool produces a guaranteed failure that looks like a
		// product regression, so revoke it the same way the agent's own
		// capacity-drop path does (RevokedTests) and let finalize delete it.
		if lostMock {
			revokedHere[testName] = struct{}{}
			// Drop whatever an EARLIER delta recorded for this test. The revoke
			// can arrive after a delta whose mock never came, and a
			// deleted test never reaches replay with a short pool at all —
			// leaving the entry in place inflates the very metric whose
			// credibility is the reason the never-arrived path is not revoked.
			// (Teardown applies the same exclusion for the agent's own revokes,
			// keyed on tests it actually managed to DELETE.)
			shortPoolByTest.Delete(testName)
			if revokeTest != nil {
				revokeTest(testName)
			}
		}

		if len(realMockEntries) == 0 {
			return
		}
		if mapped != nil {
			mapped(testName, realMockEntries)
		}

		flushMu.Lock()
		// Union, don't replace: the agent emits a test's mocks when its window
		// resolves and emits more later for mocks retroactively binned into that
		// window. Those later emissions are a DELTA — overwriting would delete the
		// mocks already recorded for the test and replay it with a short pool.
		pending[testName] = mergeMockEntries(pending[testName], realMockEntries)
		full := len(pending) >= mappingFlushBatch
		flushMu.Unlock()

		// Flush on size only. Draining the channel is the priority: every
		// millisecond spent rewriting mappings.yaml is a millisecond the agent
		// cannot hand over its next mapping, and it DROPS what it cannot hand
		// over. The ticker flushes whatever a partial batch leaves behind.
		if full {
			flush()
		}
	}

	// applyReady settles the held entries handed back since the last look:
	// their mock has an outcome now, or the mock stream ended without it.
	applyReady := func() {
		ready := waits.takeReady()
		if len(ready) == 0 {
			return
		}
		byTest := map[string]*mappingResolution{}
		var order []string
		for _, e := range ready {
			res := byTest[e.test]
			if res == nil {
				res = &mappingResolution{}
				byTest[e.test] = res
				order = append(order, e.test)
			}
			entry, fate := settleMock(e.tempID, correlationMap, asyncMockIDs, droppedMockIDs)
			r.addSettled(res, e.test, e.tempID, entry, fate, e.why, asyncMockIDs)
		}
		for _, tn := range order {
			apply(tn, *byTest[tn])
		}
	}

	// Drain the mapping stream, and the held entries as their mocks arrive,
	// until the stream is closed and nothing waits.
	in := mappings
	for in != nil || !waits.settled() {
		select {
		case mapping, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			r.frameProgress.Add(1)
			r.mappingsStreamed.Add(1)
			entries, lost, uncorrelated := r.resolveMappingEntries(mapping, correlationMap, asyncMockIDs, droppedMockIDs, waits)
			apply(mapping.TestName, mappingResolution{entries: entries, lost: lost, uncorrelated: uncorrelated})
		case <-waits.wakeCh():
			applyReady()
		}
	}

	// The stream is closed: persist whatever the last partial batch holds.
	flush()
	return nil
}

type Recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	mappingDb       MappingDb
	telemetry       Telemetry
	instrumentation Instrumentation
	testSetConf     TestSetConfig
	config          *config.Config
	hooks           RecordHooks

	// cleanups is run in LIFO order from Start's defer. Downstream
	// builds and internal subsystems register shutdown work here
	// rather than each one type-asserting its own io.Closer into the
	// store interfaces. Register with RegisterCleanup.
	cleanupMu sync.Mutex
	cleanups  []func() error

	// frameProgress counts test cases, mocks and mappings handed on from the
	// agent's streams and persisted. The stop's drains wait while it moves
	// (utils.DrainErrGroupProgress).
	frameProgress atomic.Uint64

	// frameQuiet is how long a frame stream must go without delivering
	// anything captured before the stop for its drain to ask the agent what
	// it still holds (forwardUntilDrained); zero means mappingIdleGrace. A
	// field so tests that are not about the drain need not wait it out.
	frameQuiet time.Duration
	// waitNotice is how often a stop that waits for the app ports'
	// questions says so (unreachablePortWarner.wait); zero means
	// drainProgressEvery. A field so a test need not wait it out.
	waitNotice time.Duration

	// backlogHolds counts the looks at which a frame drain kept waiting for
	// what the agent still held from before the stop (holdForBacklog):
	// progress for the stop's outer drains (drainProgress).
	backlogHolds atomic.Uint64
	// teardown is the recording's own teardown scope (Start): what keploy
	// sees its stops do -- the agent's, the app's, the daemons', the
	// compose project's down -- moves it, and it is progress for the stop's
	// drains (drainProgress). Nil before Start.
	teardown atomic.Pointer[utils.TeardownScope]
	// drainClock is the clock the stop's drains measure a stall on: nil,
	// keploy's own runnable time; a test drives its own.
	drainClock stall.Clock
	// mappingWaitsMade, when set (a test), is handed each recording's mapping
	// waits as Start makes them, before anything uses them.
	mappingWaitsMade func(*mappingWaits)
	// captureLost is set when a frame drain stopped waiting for what the
	// agent still held from before the stop, or the mock store could not
	// write mocks the recording captured (mocksNotWritten): the recording is
	// incomplete, and ends as failed.
	captureLost atomic.Bool
	// endCut, under the proven switch, is why the recording's end is cut:
	// keploy's agent went before it had handed over what it captured (its
	// stream ended without its end marker, or its drain stalled). The
	// test-set's config says so, as for a stop that killed the app (P101-b-k,
	// P101-c).
	endCut atomic.Pointer[string]
	// mocksLossSaid is set once mocksNotWritten has said the mock store
	// could not write mocks the recording captured.
	mocksLossSaid atomic.Bool
	// mockWriteStopped is set when the user's second interrupt stopped the
	// mock store's writing (closeMockStore): the cleanup does not wait for
	// it again.
	mockWriteStopped atomic.Bool
	// realUncaptured is set, under the proven switch with a mock.real list,
	// when keploy could neither capture nor relay as real a connection of
	// the recording, and realUnread when it could not read whether it did
	// (reportRealRun): the recording is incomplete, and ends as failed, as
	// for a capture loss, unless a later pass judges it (JudgeLossesLater).
	realUncaptured atomic.Bool
	realUnread     atomic.Pointer[string]
	// heldFrames counts the test cases and mocks the frame drains handed on
	// after the stop that may have been captured before it: what the agent's
	// answer to a look (holdForBacklog) is about. Mappings, which it does not
	// cover, are not counted. Whether one came since the look that said the
	// agent held something decides if a loss is certain (agentEndedDrain).
	heldFrames atomic.Uint64
	// mappingsStreamed counts the test-mock mappings the agent handed over
	// this recording. A recording made without --sync that the agent mapped
	// itself (enterprise's static dedup resolves its windows in the agent)
	// derives none from its test windows (windowMappings.write).
	mappingsStreamed atomic.Uint64
	// unsupportedBacklogOnce says once that the agent cannot tell what it
	// still holds.
	unsupportedBacklogOnce sync.Once
}

// backlogReader is an agent client that can ask the agent whether it may still
// hand over something captured before a time (AgentClient.BacklogBefore).
type backlogReader interface {
	BacklogBefore(ctx context.Context, before time.Time) (models.AgentBacklog, error)
}

// agentStopWatcher is an agent client that can be told when a stop signal has
// reached an agent something else stops, and the agent waits for this
// recording to drain its streams (AgentClient.WatchAgentStop).
type agentStopWatcher interface {
	WatchAgentStop(ctx context.Context) <-chan struct{}
}

// composeAgentEnder sees an agent docker compose stops to its end once this
// recording has drained it: its exit waited for while its teardown makes
// progress, and a wedged one killed and said (AgentClient.EndComposeAgent).
type composeAgentEnder interface {
	EndComposeAgent(release bool)
}

// drainStallLooks is how many looks in a row a drain makes at a backlog the
// agent says is pending, or does not answer about, with no frame handed over
// and no progress in the agent's capture between them, before it stops
// waiting and reports what is lost (holdForBacklog). A count, not a time: each
// look comes once the stream has been quiet for the drain's quiet bound
// (mappingIdleGrace), so the drain waits at least that times this on a
// backlog that does not move. That is well past the agent's own hang bound (a
// parser that holds work and makes no progress is retired, and a retired
// parser is progress here), so it is reached only when the agent itself can
// no longer move.
const drainStallLooks = 30

// backlogLooks is one drain's looks at what the agent still holds from before
// the stop (holdForBacklog).
type backlogLooks struct {
	utils.BacklogLooks
	// toldUnanswered: the drain has said that the agent does not answer.
	toldUnanswered bool
	// heldFrames is Recorder.heldFrames at the latest look the agent was not
	// gone for (utils.BacklogLooks.Said).
	heldFrames uint64
	// endable: the latest look's answer was "clear", from an agent that ends
	// its streams on request (models.AgentBacklog.EndMarkers).
	endable bool
}

// agentEndedDrain reports a frame stream the agent ended on its own, without
// its end marker, while this recording still read it (before the stop, or
// before its drain was done with it): the agent went away (it was killed, or
// crashed) before it said it held nothing more, and what it still held is
// lost. The recording fails (captureLost),
// said at ERROR, when the agent itself gave a sign it held something (at the
// latest look it was not already gone for, it said so, or did not answer:
// Held), or when this client started the agent (startedAgent): such an agent
// holds its streams open until this drain is done, so one that ends them
// first went away before the recording could be confirmed complete. The loss
// is said to be certain only when it said it held something and nothing it
// may have held came since. Otherwise it is said at WARN (an agent docker
// compose or kubelet stops): frames from before the stop still coming would
// not tell its backlog from this CLI's own lag in taking what it had sent.
func (r *Recorder) agentEndedDrain(kind string, looks *backlogLooks) {
	r.noteEndCut("keploy's agent ended its " + kind + " stream before it said it had handed over all it captured, so what it still held is not in the recording")
	if !looks.Held() && !startedAgent(r.instrumentation) {
		r.logger.Warn("the keploy agent ended its stream before it said it held nothing more: what it still held, if anything, is lost",
			zap.String("frames", kind),
			zap.String("next_step", "the agent was stopped before the recording had saved what it captured (docker compose stops it with the project, also when the app exits on its own; kubelet with its pod): stop the recording, not the app or the agent, and re-record if this recording's last test cases matter"))
		return
	}
	r.captureLost.Store(true)
	said, answered := looks.Said()
	msg := "the keploy agent ended its stream before it confirmed it had handed over all it captured: some of that may be lost"
	if answered && said == models.BacklogPending && r.heldFrames.Load() == looks.heldFrames {
		msg = "the keploy agent ended its stream while it still held what it captured before the stop: that is lost"
	}
	fields := []zap.Field{zap.String("frames", kind)}
	if answered {
		fields = append(fields, zap.Stringer("agent_said", said))
	}
	r.logger.Error(msg, append(fields,
		zap.String("next_step", "the agent was stopped or crashed before the recording had saved what it held; the recording ends as failed. Do not stop the agent separately from the recording; re-record"))...)
}

// mockStoreWriter is a mock store that writes in the background and is
// closed to write the rest (MockYaml in the gob format): CloseUnless stops
// waiting once stop is closed, and MocksLeft is how many it has yet to write.
type mockStoreWriter interface {
	CloseUnless(stop <-chan struct{}) error
	MocksLeft() uint64
}

// closeMockStore closes the mock store, which writes what it still holds. A
// store that writes in the background is waited for for as long as it
// writes, which a slow disk makes long: every `every` it says how
// many mocks it has left, and, once it has said so, that a second interrupt
// stops it now (the rest is then not written, a capture loss), as the stop's
// other drains do (reportDrainProgress).
func (r *Recorder) closeMockStore(store any, every time.Duration) error {
	return CloseMockStore(r.logger, store, every,
		"Still writing the recording's mocks to the mock file",
		"press Ctrl+C again to stop now: the rest is then not written, and this recording fails as incomplete",
		func() { r.mockWriteStopped.Store(true) })
}

// CloseMockStore closes store, which writes what it still holds (a store that
// writes in the background, mockStoreWriter, writes the rest). Such a store
// is waited for for as long as it writes: every `every` the wait logs still
// with how many mocks are left, and, once the run has been interrupted,
// stopNow, the offer that another interrupt stops the writing now. That
// interrupt calls stopped (when not nil) and stops it: the rest is then not
// written, and the error says how many (models.ErrMocksNotWritten).
// keploy record's stop and keploy mock replay --on-miss record use it.
func CloseMockStore(logger *zap.Logger, store any, every time.Duration, still, stopNow string, stopped func()) error {
	w, ok := store.(mockStoreWriter)
	if !ok {
		if closer, ok := store.(io.Closer); ok {
			return closer.Close()
		}
		return nil
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- w.CloseUnless(stop) }()
	notice := utils.NewStopNowNotice(every)
	defer notice.Withdraw()
	t := time.NewTicker(every)
	defer t.Stop()
	said, offering := false, false
	last := w.MocksLeft()
	for {
		select {
		case err := <-done:
			return err
		case <-notice.Interrupt():
			if stopped != nil {
				stopped()
			}
			close(stop)
			return <-done
		case <-t.C:
		}
		left := w.MocksLeft()
		wasOffering := offering
		offering = notice.Next()
		if left != last || !said || offering != wasOffering {
			msg := still
			if offering {
				msg += "; " + stopNow
			}
			logger.Info(msg, zap.Uint64("mocksLeft", left))
			said, last = true, left
		}
	}
}

// mockStoreCleanup is Start's cleanup of the mock store: it closes it, which
// writes what it still holds, unless the user stopped that writing at a
// second interrupt (closeMockStore): it is not waited for again. A store that
// writes in the background is closed as the stop closes it (closeMockStore):
// on a record drain that did not join, all it holds is written here, and the
// user is told what is left and can end it.
func (r *Recorder) mockStoreCleanup(closer io.Closer) func() error {
	return func() error {
		if r.mockWriteStopped.Load() {
			return nil
		}
		if _, ok := closer.(mockStoreWriter); ok {
			return r.closeMockStore(closer, drainProgressEvery)
		}
		return closer.Close()
	}
}

// mocksNotWritten says the mock store could not write mocks the recording
// captured (models.ErrMocksNotWritten, or any error closing it): the
// recording is incomplete, and ends as failed, as for any capture loss.
// It is said once a recording: the deferred cleanup closes the store again.
func (r *Recorder) mocksNotWritten(err error) {
	r.captureLost.Store(true)
	if !r.mocksLossSaid.CompareAndSwap(false, true) {
		r.logger.Debug("the mock store still could not write every mock", zap.Error(err))
		return
	}
	r.logger.Error("the recording is incomplete: mocks it captured were not written to the mock file",
		zap.Error(err),
		zap.String("next_step", "check the disk the recording writes to (space, speed, permissions); re-record this test set"))
}

// startedAgent reports whether the agent client started the agent it talks to
// (AgentClient.StartedAgent): only this client stops such an agent.
func startedAgent(inst any) bool {
	s, ok := inst.(interface{ StartedAgent() bool })
	return ok && s.StartedAgent()
}

// holdForBacklog decides, for a drain whose stream has gone quiet after the
// stop, whether it goes on waiting for what the agent still holds from before
// the stop (true) or ends (false). It asks the agent (AgentClient.BacklogBefore)
// and goes by its answer (utils.BacklogLooks.Look):
//
//   - clear: the agent holds nothing more from before the stop: its capture,
//     parsers and test-case queue are through it. End. What it already handed
//     on and has not reached this client yet is not counted
//     (forwardUntilDrained).
//   - unsupported: the agent can never tell (it predates the question, or its
//     capture has no watermark). End on the quiet, as before such an agent
//     could be asked; said once.
//   - pending, or cannot tell (no answer came): wait. An answer that did not
//     come is never read as "nothing pending": the agent may be starved, and
//     what it holds is lost the moment the stream is ended. A stream whose
//     agent dies ends on its own (the stream closes).
//
// A wait is bounded by progress, not time: a look that finds a frame handed
// over since the last one (on any stream), or the agent's capture further
// along, resets the count; drainStallLooks looks in a row without either end
// the wait, and the drain, with an error that says what the agent still held,
// and the recording ends as failed (captureLost). Every look that waits counts
// as progress for the stop's outer drains (drainProgress), which would
// otherwise take a drain waiting on the agent for a wedged one.
func (r *Recorder) holdForBacklog(stoppedAt time.Time, looks *backlogLooks, kind string) bool {
	br, ok := r.instrumentation.(backlogReader)
	if !ok {
		return false
	}
	if looks.Stall == 0 {
		looks.Stall = drainStallLooks
	}
	heldBefore := looks.heldFrames
	looks.heldFrames = r.heldFrames.Load()
	looks.endable = false
	b, err := br.BacklogBefore(context.Background(), stoppedAt)
	r.logger.Debug("looked at what the keploy agent still holds from before the stop",
		zap.String("frames", kind), zap.Stringer("agent_says", b.Verdict), zap.Uint64("progress", b.Progress), zap.Error(err))
	wait, gaveUp := looks.Look(b, err, r.frameProgress.Load())
	if looks.LastGone() {
		// The agent could not be reached: it is going away. This look says
		// nothing of what it held; the one before it does (agentEndedDrain).
		looks.heldFrames = heldBefore
	}
	switch {
	case gaveUp:
		r.captureLost.Store(true)
		r.logger.Error("stopped waiting for the keploy agent to hand over what it captured before the stop: it made no progress",
			zap.String("frames", kind),
			zap.Stringer("agent_says", b.Verdict),
			zap.String("waiting_on", b.WaitingOn),
			zap.Int("looks_without_progress", looks.Stalled()),
			zap.Duration("quiet_before_each_look", r.frameQuietGrace()),
			zap.NamedError("last_look_error", err),
			zap.String("next_step", "what the agent still held is not saved: test cases it had captured may be missing, or saved without their mocks, and the recording ends as failed. Re-record; if the agent was starved of CPU, give it more"))
		return false
	case !wait:
		looks.endable = b.Verdict == models.BacklogClear && b.EndMarkers
		if b.Verdict == models.BacklogUnsupported {
			r.unsupportedBacklogOnce.Do(func() {
				r.logger.Info("the keploy agent cannot say whether it still holds what it captured before the stop; each stream ends once it has been quiet for a while",
					zap.Duration("quiet", r.frameQuietGrace()),
					zap.String("next_step", "use an agent of the same version as this CLI: one that can tell holds the streams open until its parsers are through"))
			})
		}
		return false
	}
	if b.Verdict == models.BacklogCannotTell && !looks.toldUnanswered {
		looks.toldUnanswered = true
		r.logger.Info("could not ask the keploy agent what it still holds from before the stop; waiting for it",
			zap.String("frames", kind), zap.Error(err))
	}
	r.backlogHolds.Add(1)
	return true
}

// drainProgress is what the stop's drains wait on while it moves: the frames
// handed on, every look at which a frame drain decided to keep waiting for
// the agent (holdForBacklog), which bounds its own wait, and every movement
// keploy saw of the recording's stops (Recorder.teardown).
func (r *Recorder) drainProgress() uint64 {
	return r.frameProgress.Load() + r.backlogHolds.Load() + r.teardown.Load().Moves()
}

// teardownDrainStall bounds the drains of the recording's stop that wait on
// its teardown moving (drainProgress): how long, in time keploy could run,
// it may make no progress.
const teardownDrainStall = 30 * time.Second

// drainProgressEvery is how often a stop that is still saving what the agent
// captured says so.
const drainProgressEvery = 5 * time.Second

// reportDrainProgress says, every `every` while the stop's drain is saving
// frames, how many it has saved, and that a second interrupt ends it. The
// drain has no fixed bound (forwardUntilDrained), so a user waiting on it must
// be able to see that it is moving, and to end it. It returns the stop.
//
// It says so once the stop has offered to end at the next interrupt
// (utils.StopNowNotice): at the first notice when an interrupt stopped the
// recording, and a whole notice period after the user's interrupt was first
// seen when the stop began on its own (the app exited, a record timer). An
// interrupt before that is not the user's answer to it: a stop sent to both
// sudo and keploy (`sudo pkill -f keploy`, as CI stops a recording) reaches
// keploy a second time from sudo, as the first does, and taken for a second
// Ctrl+C it gave the drain up at its start. A notice is given at the first
// tick, whenever the drain moves, and at the tick that makes the offer, so the
// user is told as soon as it is.
func (r *Recorder) reportDrainProgress(every time.Duration) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	start := r.frameProgress.Load()
	notice := utils.NewStopNowNotice(every)
	go func() {
		defer close(exited)
		defer notice.Withdraw()
		t := time.NewTicker(every)
		defer t.Stop()
		last, lastHolds := start, r.backlogHolds.Load()
		said, offering := false, false
		for {
			select {
			case <-done:
				return
			case <-utils.StopNow():
				return
			case <-t.C:
			}
			now, holds := r.frameProgress.Load(), r.backlogHolds.Load()
			wasOffering := offering
			offering = notice.Next()
			if now != last || holds != lastHolds || !said || offering != wasOffering {
				msg := "Still saving what Keploy captured before the stop"
				if now == last && holds != lastHolds {
					msg = "Waiting for the keploy agent to finish parsing what it captured before the stop (its parsers are behind the traffic)"
				}
				if offering {
					msg += "; press Ctrl+C again to stop now: the rest is then not saved, and this recording fails as incomplete"
				}
				r.logger.Info(msg, zap.Uint64("savedSinceTheStop", now-start))
				said = true
			}
			last, lastHolds = now, holds
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// noticeEvery is waitNotice, or drainProgressEvery.
func (r *Recorder) noticeEvery() time.Duration {
	if r.waitNotice > 0 {
		return r.waitNotice
	}
	return drainProgressEvery
}

func (r *Recorder) frameQuietGrace() time.Duration {
	if r.frameQuiet > 0 {
		return r.frameQuiet
	}
	return mappingIdleGrace
}

// RegisterCleanup appends a shutdown callback that Recorder.Start's
// defer will drain in LIFO order. Thread-safe. Callbacks should be
// idempotent — Recorder may be restarted in some flows.
func (r *Recorder) RegisterCleanup(fn func() error) {
	if fn == nil {
		return
	}
	r.cleanupMu.Lock()
	r.cleanups = append(r.cleanups, fn)
	r.cleanupMu.Unlock()
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, mappingDB MappingDb, telemetry Telemetry, instrumentation Instrumentation, testSetConf TestSetConfig, hooks RecordHooks, config *config.Config) Service {
	if hooks == nil {
		hooks = BaseRecordHooks{}
	}
	return &Recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		testSetConf:     testSetConf,
		config:          config,
		hooks:           hooks,
	}
}

// afterRecordingComplete invokes the end-of-recording hook (issue #1867). It is
// best-effort: the recording is already saved, so neither an error NOR a panic
// from the hook may propagate — a panic here would otherwise unwind the teardown
// defer and skip the deferred-orphan-revoke that runs right after this call. The
// hook does data-driven work (the Basic-Auth re-key), so a panic is plausible;
// recover, log, and let teardown continue.
func (r *Recorder) afterRecordingComplete(ctx context.Context, testSetID string) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("AfterRecordingComplete hook panicked; recording is saved but the post-record pass did not finish. Check your RecordHooks implementation.",
				zap.Any("panic", rec), zap.String("testSetID", testSetID))
		}
	}()
	// The recorder ctx is already cancelled on the normal SIGINT stop of an
	// interactive recording (the teardown defer above runs under a cancelled
	// ctx); RunAfterRecordingHook gives the hook a context without that
	// cancellation, and without a deadline: the pass runs in full however
	// loaded the machine is, and a second interrupt, once offered, stops it.
	if hookErr := RunAfterRecordingHook(ctx, r.logger, r.hooks, &RecordingCompleteContext{
		TestSetID: testSetID,
		Path:      r.config.Path,
	}, r.noticeEvery()); hookErr != nil {
		r.logger.Error("AfterRecordingComplete hook failed; recording is saved but a post-record pass may be incomplete. Check your RecordHooks implementation.",
			zap.Error(hookErr), zap.String("testSetID", testSetID))
	}
}

// SetRecordHooks replaces the current hooks. Mirrors SetTestHooks on the Replayer.
func (r *Recorder) SetRecordHooks(hooks RecordHooks) {
	if hooks != nil {
		r.hooks = hooks
	}
}

// maxLiveDroppedMockIDs bounds droppedMockSet. Every dropped mock is meant to
// be consumed by the one mapping that references it, but a mock nothing ever
// maps — a background egress whose owning test never completes — has no
// consumer, so an unbounded set would grow for the life of the recording.
//
// Past the cap we stop tracking, and further drops lose their revoke: a
// mapping entry already held for one settles at once, and one whose mapping
// comes after the drop waits until the mock stream ends; either way it settles
// as a mock that was never saved, so its test is counted as recorded with an
// incomplete mock set instead of revoked. That is the deliberate trade — a
// recording with 10k dropped mocks is systematically broken and the teardown
// Warn plus the mocks-dropped telemetry still report the true scale — but it is
// not free, so the cap is set well above any plausible healthy session.
//
// A package-level var, not a const, only so a test can shrink it. Never
// reassigned in production code.
var maxLiveDroppedMockIDs = 10000

// droppedMockSet is the set of mock tempIDs the recorder FAILED to persist, so
// the mapping consumer revokes the test that owns one instead of replaying it
// with a short mock pool.
//
// A mock a RecordHook asked us not to persist (a hook-collapsed async poll
// cycle) is not here: it is no loss, and its skippedMockMarker in asyncMockIDs
// is what settles a mapping entry naming it.
//
// take() removes the ID as it reports it: entries are consumed exactly once,
// which is what keeps the set from growing across a multi-day recording.
type droppedMockSet struct {
	ids  sync.Map // map[string]struct{}
	live atomic.Int64
}

// add records a mock the recorder FAILED to persist. It reports whether the ID
// was tracked; false means the set is at capacity and the owning test will NOT
// be revoked.
func (d *droppedMockSet) add(tempID string) bool {
	if d.live.Load() >= int64(maxLiveDroppedMockIDs) {
		return false
	}
	if _, loaded := d.ids.LoadOrStore(tempID, struct{}{}); !loaded {
		d.live.Add(1)
	}
	return true
}

// take reports whether tempID is in the set, and removes it.
func (d *droppedMockSet) take(tempID string) bool {
	if _, ok := d.ids.LoadAndDelete(tempID); !ok {
		return false
	}
	d.live.Add(-1)
	return true
}

// skippedMockMarker tags an asyncMockIDs entry that exists ONLY because a
// RecordHook asked us not to persist the mock (a collapsed async poll cycle).
// Ordinary async-egress mocks are stored as struct{}{}.
//
// The distinction does two things. A skip is the mock's outcome — it will never
// be saved — so a mapping entry naming it settles at once (settleMock) rather
// than waiting for a save that never comes. And the entry can be freed:
// asyncMockIDs is otherwise never pruned, and skips are the highest-frequency
// event in a poll-heavy recording — at ~105 bytes per entry, a single 100ms
// lane would retain hundreds of MB for the life of the session, which is an
// OOM risk in the DaemonSet embedding where Start is re-entered per session.
//
// The release is best-effort, not a bound: it happens when a mapping consumes
// the tempID, so a run with mappings disabled (--disable-mapping), or a skipped
// mock no mapping ever references, retains its entry for the whole session.
// Those runs are back to the pre-marker growth; the marker helps the common
// case, it does not cap the worst one.
type skippedMock struct{}

var skippedMockMarker = skippedMock{}

// markSkippedMock records a mock a RecordHook asked us not to persist.
//
// Extracted so the marker choice is directly assertable. It is the load-bearing
// line: storing a plain struct{}{} here instead of skippedMockMarker would leave
// a mapping entry naming the mock waiting for a save that never comes, until
// the mock stream ends — and the entry would never be released again.
//
// It cannot be gated on mock.IsAsync(): AsyncRecorder sets Skip and returns
// BEFORE assigning Spec.Async, so a collapsed cycle reports IsAsync()==false and
// the guard would be a no-op.
func markSkippedMock(asyncMockIDs *sync.Map, tempID string) {
	asyncMockIDs.Store(tempID, skippedMockMarker)
}

// releaseSkippedMock frees a SKIP-marked asyncMockIDs entry once a mapping has
// consumed it. Ordinary async entries are deliberately left in place.
//
// The asymmetry is not fussiness. A real async mock IS in correlationMap, so if
// a later mapping referenced a pruned entry it would resolve and be appended —
// leaking a background egress into a test's mock pool. A skipped mock was never
// persisted, so a later mapping referencing a pruned entry simply never
// correlates: no revoke, no leak, at worst one counter tick at the end.
func releaseSkippedMock(asyncMockIDs *sync.Map, tempID string, v any) {
	if _, skipped := v.(skippedMock); skipped {
		asyncMockIDs.Delete(tempID)
	}
}

// settleMock reports what the mock loop has published, so far, of the mock
// tempID names, and consumes it. fateUnknown means nothing yet.
//
// It EXCLUDES async-egress mocks (those in asyncMockIDs): they are served at
// replay by the async engine from the complete corpus, so they must never be
// recorded as a testcase's per-test mock — even when a background egress's
// timestamp happened to fall inside the testcase's request window (the agent's
// mapper bins purely by timestamp and has no async awareness). The mock loop
// stores the async marker before it attempts the insert, so once the tempID is
// saved or dropped its async status is decided: that is when it is read.
func settleMock(tempID string, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet) (models.MockEntry, mockFate) {
	if val, ok := correlationMap.LoadAndDelete(tempID); ok {
		if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
			releaseSkippedMock(asyncMockIDs, tempID, v)
			return models.MockEntry{}, fateExcluded
		}
		return val.(models.MockEntry), fateMapped
	}
	if droppedMockIDs.take(tempID) {
		// A dropped ASYNC mock is no short pool for anybody: revoking on it
		// would delete the healthy test that merely overlapped it in time.
		if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
			releaseSkippedMock(asyncMockIDs, tempID, v)
			return models.MockEntry{}, fateExcluded
		}
		return models.MockEntry{}, fateLost
	}
	// A skip is an outcome of its own: the mock is never saved.
	if v, ok := asyncMockIDs.Load(tempID); ok {
		if _, skipped := v.(skippedMock); skipped {
			asyncMockIDs.Delete(tempID)
			return models.MockEntry{}, fateExcluded
		}
	}
	return models.MockEntry{}, fateUnknown
}

// mappingResolution is what a test's mapping entries settled as.
type mappingResolution struct {
	entries []models.MockEntry
	// lost: the test owned a mock we could not persist; it is revoked.
	lost bool
	// uncorrelated counts the mocks of the test that never arrived.
	uncorrelated int
}

// addSettled folds one settled entry into res. An entry still fateUnknown
// here has no mock for its test, and why says why (lossReason).
func (r *Recorder) addSettled(res *mappingResolution, test, tempID string, entry models.MockEntry, fate mockFate, why lossReason, asyncMockIDs *sync.Map) {
	if fate == fateUnknown {
		// An async mock that never correlates is not an incomplete mock set
		// for anyone — async-egress mocks are never part of a per-test
		// mapping — so only a tempID still unclassified is real loss.
		if v, isAsync := asyncMockIDs.Load(tempID); isAsync {
			releaseSkippedMock(asyncMockIDs, tempID, v)
			fate = fateExcluded
		}
	}
	switch fate {
	case fateMapped:
		res.entries = append(res.entries, entry)
	case fateLost:
		res.lost = true
	case fateUnknown:
		// Deliberately NOT treated as a drop. A drop is a fact — we know the
		// mock was abandoned. A mock the stream never delivered is not known
		// to be lost by this recorder, so the test is still persisted, with a
		// short pool, and counted: the count is reported at teardown and in
		// telemetry.
		res.uncorrelated++
		if why == reasonRevoked {
			// The test is deleted at the recording's end (and counted short
			// there only if its delete fails): no loss to report per mock.
			r.logger.Debug("not waiting for a revoked test's mock",
				zap.String("test", test),
				zap.String("tempMockID", tempID))
			return
		}
		r.logger.Error("Failed to correlate mock mapping",
			zap.String("test", test),
			zap.String("tempMockID", tempID),
			zap.String("next_step", lossNextStep(why)))
	}
}

// lossNextStep is what a "Failed to correlate" line says of why.
func lossNextStep(why lossReason) string {
	switch why {
	case reasonTaken:
		return "another mapping entry naming the same mock took it first (a mock is mapped to one entry), so this test is recorded without it; re-record the test set"
	case reasonUntrackedDrop:
		return fmt.Sprintf("the recorder failed to save this mock while %d dropped mocks were already tracked, so its test is recorded without it instead of revoked; re-record the test set", maxLiveDroppedMockIDs)
	case reasonCapFull:
		return fmt.Sprintf("%d mapping entries were already waiting for their mocks, so this one was not held; re-record the test set", maxHeldMappingEntries)
	case reasonAbandoned:
		return "the recording's teardown gave the mock loop up before it said what became of this mock; re-record the test set"
	case reasonConsumerGivenUp:
		return "the mock loop said what became of this mock, but the recording's teardown gave the mapping consumer up before it applied it, so this test is recorded without it; re-record the test set"
	default:
		return "the agent named this mock in the test's mapping but the mock stream ended with no mock left for it: the agent never sent it, or another mapping entry naming the same mock took it (a mock is mapped to one entry); re-record the test set"
	}
}

// resolveMappingEntries settles a test's mapping entries against what the mock
// loop has published (settleMock). An entry whose mock has no outcome yet is
// held in waits and handed back when it does (or when the mock stream ends);
// such an entry is in none of the results. With nil waits the mock stream is
// taken as over: an entry with no outcome counts as uncorrelated at once.
func (r *Recorder) resolveMappingEntries(mapping models.TestMockMapping, correlationMap, asyncMockIDs *sync.Map, droppedMockIDs *droppedMockSet, waits *mappingWaits) ([]models.MockEntry, bool, int) {
	var res mappingResolution
	for _, tempID := range mapping.MockIDs {
		c := waits.claim(mapping.TestName, tempID, func() (models.MockEntry, mockFate) {
			return settleMock(tempID, correlationMap, asyncMockIDs, droppedMockIDs)
		})
		if c.held {
			continue
		}
		if c.capFirst {
			r.logger.Warn("too many mapping entries are waiting for their mocks: entries past the cap are not held, and each test that names one is recorded without that mock",
				zap.Int("maxHeldMappingEntries", maxHeldMappingEntries),
				zap.String("test", mapping.TestName),
				zap.String("next_step", "the agent is naming mocks it does not send; re-record the test set and report it"))
		}
		r.addSettled(&res, mapping.TestName, tempID, c.entry, c.fate, c.why, asyncMockIDs)
	}
	return res.entries, res.lost, res.uncorrelated
}

// GetRecordHooks returns the current hooks.
func (r *Recorder) GetRecordHooks() RecordHooks {
	return r.hooks
}

// ComposeAgentFailureReader is an optional Instrumentation extension for a
// docker compose recording: whether the keploy agent, a service in the
// project, is what stopped it, as keploy's own failure, or nil.
// AgentClient implements it (keploy mock reads the same).
type ComposeAgentFailureReader interface {
	ComposeAgentFailure() error
}

// ComposeEndReader is an optional Instrumentation extension for a docker
// compose recording under way: what ended the project when the app's own end
// does not stand -- the keploy agent's crash after keploy saw it ready (the
// app stopped over it, or never started), or the project stopping before the
// app ever started though compose reported 0 -- or nil. appExit is the app's
// exit as keploy got it. AgentClient implements it.
type ComposeEndReader interface {
	ComposeEndAfterReady(appExit int) error
}

// composeEnd is what ended a compose recording when the app's own end does
// not stand (ComposeEndReader), or nil.
func (r *Recorder) composeEnd(appExit int) error {
	reader, ok := r.instrumentation.(ComposeEndReader)
	if !ok {
		return nil
	}
	return reader.ComposeEndAfterReady(appExit)
}

// composeAgentFailure is the agent's own failure when it stopped the compose
// project, or nil.
func (r *Recorder) composeAgentFailure() error {
	reader, ok := r.instrumentation.(ComposeAgentFailureReader)
	if !ok {
		return nil
	}
	return reader.ComposeAgentFailure()
}

// appStoppedBySignal reports whether the app ended on SIGTERM or SIGINT
// (exit 143 or 130: `docker stop`, `kill` or `kill -INT` on the app). That is
// how an app is stopped, so it ends the recording normally, with exit 0, as
// an app that exited 0 does. Any other end -- a crash, the app's own non-zero
// code, SIGKILL or SIGSEGV (137, 139) -- fails the recording with that code
// (recordExitCode). Under docker compose a run the keploy agent's crash
// ended is keploy's failure instead, whatever the app's code (composeEnd).
func appStoppedBySignal(appErr models.AppError) bool {
	return appErr.AppErrorType == models.ErrUnExpected &&
		(appErr.ExitCode == 128+int(syscall.SIGTERM) || appErr.ExitCode == 128+int(syscall.SIGINT))
}

// recordExitCode is the exit code a recording that failed with err ends the
// process with. An app that ended with a code of its own (a non-zero exit, or
// 128+N for signal N; SIGTERM and SIGINT are a stop, appStoppedBySignal)
// gives that code, as `keploy mock` mirrors its runner's (utils/exitcodes.go:
// the wrapped command's own code, verbatim); anything else is a Keploy-side
// failure, with the code err is tagged with.
func recordExitCode(err error) int {
	var appErr models.AppError
	if errors.As(err, &appErr) && appErr.ExitCode > 0 &&
		(appErr.AppErrorType == models.ErrUnExpected || appErr.AppErrorType == models.ErrCommandError) {
		return appErr.ExitCode
	}
	return utils.ExitCodeFor(err)
}

// Start records until it is stopped or the recording fails, and it ends a
// failed recording with a non-zero exit code itself (see the stop defer
// below): every caller of it, keploy's own `record` and the downstream ones,
// logs its error and goes on, and the process exit code is the only thing CI
// reads.
func (r *Recorder) Start(ctx context.Context) (startErr error) {

	r.logger.Debug("Starting Keploy recording... Please wait.")
	// The recording's own teardown scope (Recorder.teardown).
	ctx, teardown := utils.NewTeardownScope(ctx)
	r.teardown.Store(teardown)
	// A mock.real keploy cannot read is refused before anything runs (proven
	// switch only).
	if err := realclient.Check(r.mockReal()); err != nil {
		utils.LogError(r.logger, err, "keploy cannot read keploy.yml mock.real")
		// A recording that cannot start fails the process (the stop
		// defer below arms the code for every later failure).
		utils.SetExitCodeOnce(recordExitCode(err))
		return err
	}
	// A recorder can be started again: this recording has not lost anything
	// yet.
	r.captureLost.Store(false)
	r.mocksLossSaid.Store(false)
	r.mockWriteStopped.Store(false)
	r.heldFrames.Store(0)
	r.mappingsStreamed.Store(0)

	sessionStart := time.Now()

	// Auto-register mockDB.Close if it implements io.Closer. MockYaml
	// implements Close unconditionally: in gob mode it drains the
	// async writer and flushes the file; in yaml mode it is a no-op
	// (gobStop is nil, so Close returns nil immediately). Registered
	// via RegisterCleanup so it drains in LIFO order alongside any
	// other subsystems the caller has registered (telemetry flush,
	// mapping-DB sync, etc.).
	if closer, ok := r.mockDB.(io.Closer); ok {
		r.RegisterCleanup(r.mockStoreCleanup(closer))
	}

	// Drain all registered cleanups in LIFO order on return. Errors
	// are logged but do not abort subsequent cleanups — each subsystem
	// gets its flush regardless of what came before.
	defer func() {
		r.cleanupMu.Lock()
		cleanups := r.cleanups
		r.cleanups = nil
		r.cleanupMu.Unlock()
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				if errors.Is(err, models.ErrMocksNotWritten) {
					// The summary is said: the run fails all the same.
					r.mocksNotWritten(err)
					utils.SetExitCodeOnce(utils.ExitKeployError)
					continue
				}
				// A cleanup error usually means a mock-file flush or
				// telemetry-drain returned non-nil — the session data
				// is still on disk (the writer buffers drain before
				// Close returns err on inner failures), but the tail
				// batch may be incomplete. Log at Error so the
				// operator sees it on exit summary; include cleanup
				// index so `record cleanup N failed` is actionable
				// against the known RegisterCleanup call sites.
				r.logger.Error("record cleanup returned an error; inspect the mock file for a truncated tail and check disk space/permissions at the recording output directory",
					zap.Int("cleanupIndex", i),
					zap.Error(err))
			}
		}
	}()

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	// persistCtx is the context the test-case and mock stores are written on,
	// detached from ctx on purpose. (consumeMappings makes the same guarantee for
	// mappings.yaml itself.)
	//
	// Capture and persistence are driven by different contexts, and the gap
	// between them is where recorded data used to disappear. The agent streams on
	// reqCtx (see below), which is WithoutCancel'd and so survives SIGINT — it
	// stops only at reqCtxCancel() in teardown, AFTER the user app has been
	// stopped and drained. The consumer goroutines below, however, are children of
	// ctx, which cancels the instant SIGINT lands. So for the whole teardown
	// window (a NotifyGracefulShutdown of up to 10s, then an app drain of up to
	// 30s) the agent kept handing us test cases, mocks and mappings while every
	// store refused to write them: the yaml path returns ctx.Err() on a cancelled
	// ctx, and the callers below treated that as "shutting down, skip" — silently.
	//
	// The damage was worst for mappings, which are emitted LAST (the agent can
	// only resolve a test's mock range once that test is done), so the tail of
	// every endpoint landed squarely in that window: mappings.yaml lost those
	// tests and replay reported "no_mocks" (candidates: 0) for exactly them. A
	// dropped mock insert compounded it by also skipping its correlationMap entry,
	// stranding the mapping that referenced it even when the mapping did arrive.
	//
	// Writes must therefore outlive cancellation. This does NOT risk a hung
	// teardown: the consumers are `range` loops that end when the agent closes its
	// streams (at reqCtxCancel), each write is a local file op, and Start's
	// teardown still bounds the whole group (DrainErrGroupProgress).
	persistCtx := context.WithoutCancel(ctx)

	/*
	 * THE PORTS INGRESS WAS OBSERVED ON, for the UI join annotation.
	 *
	 * Declared here rather than inside the consumer loop because the
	 * annotation is written from the stop defer below, which needs the
	 * whole session's observations — and because the loop runs in its
	 * own goroutine, so the collector is the thing that carries them
	 * across. It is safe for concurrent use because it holds a mutex;
	 * the goroutine is the reason it NEEDS to be safe, not the reason it
	 * is.
	 *
	 * Costs an ordinary recording AT MOST one mutex and one map insert
	 * per test case -- observe() consults nothing about whether a join
	 * was requested, but it does return before the mutex when AppPort is
	 * 0 -- plus one sorted() at stop, which takes the mutex and, on a
	 * recording that saw ingress, allocates and sorts, and one
	 * context.WithTimeout plus its cancel. The defer then returns
	 * immediately because no join was requested.
	 */
	uiJoinObserved := newUIJoinPorts()
	// Its questions run beside the recording; the stop waits for those still
	// out once its drains are over (the record drain's inserts start the last
	// of them).
	unreachablePorts := newUnreachablePortWarner(r.logger, r.instrumentation, r.config)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	setupErrGrp, _ := errgroup.WithContext(ctx)
	setupCtx := context.WithoutCancel(ctx)
	setupCtx, setupCtxCancel := context.WithCancel(setupCtx)
	setupCtx = context.WithValue(setupCtx, models.ErrGroupKey, setupErrGrp)

	// A Ctrl+C while the agent is still being set up cancels the setup
	// (setupCtx), at once. Once it is set up, the agent records until the
	// stop has drained what it captured before the stop: setupCtx is the
	// agent's lifetime (its process, or its container, stops when it ends),
	// and the stop defer cancels it after the frame drains
	// (forwardUntilDrained), not as the interrupt lands. Cancelled then, the
	// agent closed the streams the drains were reading, and what its parsers
	// still held from before the stop was lost with it. The interrupt and the
	// agent being up are decided together (utils.SetupGate).
	setupGate := utils.NewSetupGate(ctx, setupCtxCancel)

	reqErrGrp, _ := errgroup.WithContext(ctx)
	reqCtx := context.WithoutCancel(ctx)
	reqCtx, reqCtxCancel := context.WithCancel(reqCtx)
	reqCtx = context.WithValue(reqCtx, models.ErrGroupKey, reqErrGrp)

	var stopReason string
	// defining all the channels and variables required for the record
	var runAppError models.AppError
	// stopKilled is why keploy's stop of the app at the recording's end had
	// to kill it, under the proven switch (models.AppError.StopKilled): set
	// by the app's runner, read by the stop once the runner is drained.
	var stopKilled atomic.Pointer[string]
	var appErrChan = make(chan models.AppError, 1)
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var newTestSetID string
	var err error
	var testCount = 0
	var mockCountMap = make(map[string]int)
	// droppedMockCount / droppedMockKinds are the missing half of mockCountMap,
	// which only ever counts successes. A mock whose own payload cannot be
	// encoded is now skipped rather than fatal (see the ErrMockEncode branch in
	// the consumer below), and a skipped mock must never be silent — these feed
	// the teardown log and the RecordedTestSuite telemetry.
	var droppedMockCount = 0
	// shortPoolByTest maps a test name to how many of its mocks never
	// correlated, i.e. tests persisted with an INCOMPLETE mock set (see the
	// fateUnknown branch in addSettled). Unlike a dropped mock this does
	// not revoke the test, so without a total the loss shows up only as
	// scattered per-occurrence log lines. Kept per-test rather than
	// pre-aggregated so the teardown block can subtract EVERY revoke path — the
	// agent's RevokedTests control frame revokes tests consumeMappings never
	// sees, and a revoked test is deleted rather than shipped short.
	var shortPoolByTest sync.Map
	// correlationMap, asyncMockIDs, droppedMockIDs and mappingHolds are what
	// the mock loop publishes of each mock for the mapping consumer; declared
	// here so the teardown can settle what a mock loop it gave up still holds.
	var correlationMap sync.Map
	// asyncMockIDs holds tempIDs of async mocks (Spec.Async set); resolveMappingEntries uses it
	// to keep async-egress mocks out of the per-test mappings (see its doc).
	var asyncMockIDs sync.Map
	// droppedMockIDs holds tempIDs of mocks the recorder gave up persisting,
	// so the mapping consumer revokes the owning test rather than replay it
	// with a short mock pool.
	var droppedMockIDs droppedMockSet
	// mappingHolds holds the mapping entries whose mock the mock loop has not
	// reported yet (see mappingWaits); the mock loop reports each outcome to
	// it, and ends it with its stream.
	mappingHolds := newMappingWaits()
	if r.mappingWaitsMade != nil {
		r.mappingWaitsMade(mappingHolds)
	}
	var droppedMockKinds = make(map[string]int)
	// mockCountMap is written by the mock-consumer goroutine and read at
	// teardown for telemetry. Guard both with a mutex so that if teardown
	// ever runs while the consumer is still draining (e.g. a force-shutdown),
	// it can't trigger a concurrent map read/write. correlationMap beside it
	// is already a sync.Map.
	var mockCountMapMu sync.Mutex
	domainSet := telemetry.NewDomainSet()
	var recordingStarted bool
	// capWatch checks, under the proven switch, each saved test case against
	// what the agent could not capture (capture_loss.go); nil until the agent
	// is capturing, and then its methods are no-ops with the switch off.
	var capWatch *captureWatch
	/*
	 * WHEN CAPTURE ACTUALLY BEGAN, which is not when Start was entered.
	 *
	 * This is T0WallMs for the UI join annotation, which
	 * pkg/models/uijoin.go asks to be good enough "for skew estimation
	 * only" -- the part of that field doc that makes startup time an
	 * ERROR rather than a matter of taste. Deliberately not quoting the
	 * rest of it. `sessionStart` is stamped at the
	 * top of Start(), before instrumentation setup and agent bring-up
	 * (as long as the agent's start makes progress) — tens of seconds in a
	 * docker-compose recording, where the user's app boot falls inside
	 * that window too. (For every other command type instrumentation.Run
	 * starts the app AFTER this stamp, so app boot is not excluded;
	 * pkg/models/uijoin.go's field doc says so.) Handing `sessionStart`
	 * to a skew estimate puts the whole of startup into the skew, in a
	 * model that spends twenty-four lines refusing a float32 BECAUSE it
	 * introduces 24 seconds of error. The recorder knows the better
	 * value; this is it.
	 *
	 * `sessionStart` keeps its own job (telemetry duration, which really
	 * is the whole session).
	 */
	var captureStart time.Time

	// Deferred-orphan revoke bookkeeping. revokedNames collects TC names the
	// agent signalled via Kind=RevokedTests control frames (their owned mock
	// was capacity-dropped AFTER the TC streamed); insertedNames records which
	// TCs actually persisted. At finalize we delete the intersection so a
	// revoke that raced ahead of its TC's persistence is still caught. Both are
	// written only by the mock/TC consumer goroutines and read at teardown
	// AFTER those goroutines drain, but the mutexes are cheap insurance against
	// a force-shutdown teardown racing an in-flight consumer.
	revokedNames := map[string]struct{}{}
	var revokedMu sync.Mutex
	insertedNames := map[string]struct{}{}
	var insertedMu sync.Mutex
	// Under the proven switch a recording made without --sync writes its
	// mappings.yaml at the end, derived from its test windows; nil otherwise.
	windowMaps := r.newWindowMappings()
	// The moment the recording is stopped (a stop signal, the record timer,
	// a caller's cancel each end ctx first) is when the application's
	// shutdown begins: what it records from then on is the shutdown's
	// (MockEntry.AtStop).
	if windowMaps != nil {
		go func() {
			<-ctx.Done()
			windowMaps.noteStop(time.Now())
		}()
	}

	// Bounded drains: a goroutine wedged under contention that ignores its
	// cancel() must not hang teardown forever (which would swallow SIGINT and
	// keep the process alive until an external SIGKILL). See utils.DrainErrGroup.
	//
	// The frame streams drain through the stop until what the agent held at
	// it is through (forwardUntilDrained): wait for them while they make
	// progress, however long that takes, and give up only on a stall.
	// It reports whether it joined every frame drain: one it gave up on, a
	// stall, may still hold its stream to the agent open.
	drainFrames := func() (joined bool) {
		reqCtxCancel()
		defer r.reportDrainProgress(drainProgressEvery)()
		err, timedOut := utils.DrainErrGroupProgressOn(r.drainClock, r.logger, "record-req", reqErrGrp, teardownDrainStall, r.drainProgress)
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop request processing")
		}
		if timedOut {
			r.noteEndCut("keploy's drain of what the agent captured made no progress at the recording's end, so keploy gave it up, and what the agent still held is not in the recording")
		}
		return !timedOut
	}
	// Under docker compose the agent is a service of the app's own stack
	// (keploy injects it), so taking the stack down takes the agent, and
	// everything it has not handed over yet, with it: at 110 req/s for 30 s
	// that was the mocks of 1,655 of 3,300 tests. So the agent is drained
	// first, once (composeDrain): by the stop, before it takes the stack down
	// (the app keeps running meanwhile, but what it serves after the stop is
	// not recorded: forwardUntilDrained), or earlier, when the agent says
	// compose is stopping it (agentGoing: the app exited on its own, or a
	// container of its project did, and compose stops the project). The agent
	// then waits for the drain, and goes once its streams close: what it
	// could not capture (mock.real), and what it lost after the last test
	// case, are read first, while it is still up, as of an agent this client
	// started. Its exit is then seen to (composeAgentEnder): it goes once the
	// streams close, and one wedged in its stop is killed, rather than held
	// for compose's grace, which waits on it. After a drain the stop began,
	// the reads come with the stack still up.
	var composeDrain sync.Once
	drainComposeAgent := func(agentGoing bool) {
		composeDrain.Do(func() {
			r.logger.Info("Waiting for the keploy agent to hand over what it captured before the stop...")
			if agentGoing {
				if recordingStarted {
					r.reportRealRun(ctx)
					capWatch.afterTestCases(ctx)
				}
				joined := drainFrames()
				capWatch.waitForReads() // while the agent is up
				if e, ok := r.instrumentation.(composeAgentEnder); ok {
					// A drain given up on a stall may hold its stream open,
					// and the agent waits for that stream: it is told to go.
					e.EndComposeAgent(!joined)
				}
				return
			}
			drainFrames()
			if recordingStarted {
				r.reportRealRun(ctx)
				capWatch.afterTestCases(ctx)
				capWatch.waitForReads() // before the stack, and the agent with it, goes
			}
		})
	}

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
		default:
			// Start is ending on its own: nothing stopped it (a signal to
			// keploy, the record timer, or a caller cancelling ctx each end
			// ctx first), so an error it returns is the recording failing --
			// an agent that could not start, an environment that could not be
			// set up, a store that refused a write, an app that died. It used
			// to end the process with exit 0: `keploy record` logged the error
			// and returned nil, so a CI job whose agent never started went
			// green with nothing recorded (#4772). The code is armed here,
			// where Start still knows it was not stopped, because the Stop
			// below ends ctx and no caller can tell afterwards. A code the
			// failure armed already (the agent's own, 3 or 6) is kept.
			//
			// "Not stopped" is read from ctx alone, so a stop signal that
			// reaches the app as well as keploy (a service manager that
			// signals the whole control group) can end the app before keploy
			// has ended ctx, and is then taken for the app's own end: a stop
			// when the app died of SIGTERM or SIGINT (appStoppedBySignal), a
			// failure when it exited with another code (a handler's own, or
			// SIGKILL's 137).
			if startErr != nil {
				utils.SetExitCodeOnce(recordExitCode(startErr))
			}
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}

		r.logger.Info("Stopping Keploy recording...")

		// Notify the agent that we are shutting down gracefully
		// This will cause connection errors to be logged as debug instead of error.
		// Bound it: an up-but-unresponsive agent (still booting under contention)
		// must not block teardown — the path SIGINT takes — indefinitely.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
			r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
		}
		notifyCancel()
		// Pcap + keylog flow over long-lived HTTP streams started in
		// Start() right after the agent's broadcaster came up. The
		// streams unwind on their own when the agent closes the
		// response or the recorder context is cancelled — there is
		// no on-disk file on the agent and no fetch step here.

		// Under docker compose the agent is drained first (drainComposeAgent),
		// before the stack it is a service of is taken down.
		composeAgent := r.config.CommandType == string(utils.DockerCompose)
		if composeAgent {
			// Under the switch the app's stop is keploy's own and leaves the
			// agent up (P101-c): the app is stopped first, so what it does as
			// it stops is recorded with the rest, and the drain then takes
			// it. Its stop says, in the app's run, whether it had to kill.
			if s, ok := r.instrumentation.(composeAppStopper); ok && provenswitch.On() {
				// An app that exited on its own needs no stop.
				if _, err := s.StopComposeApp(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, models.ErrAppExitedOnItsOwn) {
					r.logger.Warn("keploy could not stop the application's container before saving what the agent captured", zap.Error(err))
				}
			}
			drainComposeAgent(false)
		}
		windowMaps.noteStop(time.Now())
		runAppCtxCancel()
		// Waited for while the stop moves (drainProgress): the app's way
		// out, what the agent captures of it, not a fixed time.
		if err, _ := utils.DrainErrGroupProgressOn(r.drainClock, r.logger, "record-app", runAppErrGrp, teardownDrainStall, r.drainProgress); err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}
		if !composeAgent {
			// Read before the frames drain: the agent stays up while this
			// recording holds its streams open, even when the stop signal
			// reached it too, and the app has stopped making calls.
			if recordingStarted {
				r.reportRealRun(ctx)
				capWatch.afterTestCases(ctx)
			}
			drainFrames()
			capWatch.waitForReads() // before the agent stops
		}
		// The stop had to kill the app, or the agent went before it had
		// handed over what it captured: the recording's end is cut, and the
		// recording says so.
		why := stopKilled.Load()
		if why == nil {
			why = r.endCut.Load()
		}
		if why != nil && recordingStarted && newTestSetID != "" {
			r.markStopKilled(context.WithoutCancel(ctx), newTestSetID, *why)
		}

		setupCtxCancel()
		if err, _ := utils.DrainErrGroupProgressOn(r.drainClock, r.logger, "record-setup", setupErrGrp, teardownDrainStall, r.drainProgress); err != nil {
			utils.LogError(r.logger, err, "failed to stop setup execution, that covers init container")
		}

		// recordDrainedCleanly gates the end-of-recording hook below. It stays true
		// only if this drain actually joined every persist goroutine; on the
		// timeout path a mock/test-case writer may still be in flight (see below).
		recordDrainedCleanly := true
		if err, timedOut := utils.DrainErrGroupProgressOn(r.drainClock, r.logger, "record", errGrp, teardownDrainStall, r.drainProgress); err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		} else if timedOut {
			recordDrainedCleanly = false
		}
		// A drain that gave the mock loop up (wedged in a save or a hook) left
		// the mapping entries it would have settled held; one that gave the
		// mapping consumer up (wedged in a write) left those handed back to it
		// unapplied. Neither is counted by anyone else: take them here, before
		// the totals, as the tests' incomplete mock sets they are; the totals
		// subtract a deleted test as they do for every other entry. After a
		// clean drain there are none.
		abandonedReady, abandonedHeld := mappingHolds.abandon()
		foldAbandoned := func(e heldEntry, unapplied lossReason) {
			entry, fate := settleMock(e.tempID, &correlationMap, &asyncMockIDs, &droppedMockIDs)
			why := e.why
			if fate == fateMapped || fate == fateLost {
				// Its outcome came, but the consumer will not apply it now:
				// the test is recorded without it.
				fate = fateUnknown
				if why != reasonRevoked {
					why = unapplied
				}
			}
			var res mappingResolution
			r.addSettled(&res, e.test, e.tempID, entry, fate, why, &asyncMockIDs)
			if res.uncorrelated > 0 {
				prev, _ := shortPoolByTest.Load(e.test)
				n, _ := prev.(int64)
				shortPoolByTest.Store(e.test, n+int64(res.uncorrelated))
			}
		}
		// A handed-back entry with no mock keeps why it was handed back (the
		// stream ended without it, another entry took it, an untracked drop);
		// one whose mock came was the consumer's to apply.
		for _, e := range abandonedReady {
			foldAbandoned(e, reasonConsumerGivenUp)
		}
		for _, e := range abandonedHeld {
			foldAbandoned(e, reasonAbandoned)
		}
		// Every drain is over: nothing still selects on utils.StopNow, which
		// a press at this wait's offer closes, but what a drain abandoned on
		// a stall (already counted as cut). The record drain joined the
		// test-case consumer that asks the questions, if one ever ran (a
		// recording that ended before it captured has none), or gave it up:
		// the wait asks no more (it seals the warner), and what those still
		// out find is said before the session's summary.
		unreachablePorts.wait(r.noticeEvery())

		// Every mock is now handed to the mock store: a store that writes
		// in the background (the gob writer) writes the rest here, before
		// the end-of-recording hook reads the mock file and before the
		// summary says whether the recording is complete. One it could not
		// write is a capture loss. On a drain that did not join, a writer
		// may still hand it mocks, and the deferred cleanup closes it.
		// agentLost is what the drains lost, before any loss of the store's
		// own (the summary's reason names both).
		agentLost := r.captureLost.Load()
		storeWrote := true // false once the mock store said it could not write every mock
		if recordDrainedCleanly {
			if err := r.closeMockStore(r.mockDB, drainProgressEvery); err != nil {
				r.mocksNotWritten(err)
				storeWrote = false
			}
		}

		// End-of-recording hook (issue #1867 Basic-Auth re-key): every test case
		// and mock is now drained to disk, so a cross-artifact pass can correlate
		// them. Best-effort — a failure never invalidates the recording.
		//
		// Gated on a CLEAN drain. DrainErrGroupStatus reports timedOut when a
		// persist goroutine ignored cancellation and may still be appending to the
		// test-set files (the insert goroutines write on persistCtx =
		// WithoutCancel, so they do NOT stop on the teardown cancel). The hook's
		// consumer rewrites a whole mock file (read → rewrite → atomic rename), so
		// a mock appended between its read and its rename would be silently dropped
		// into a valid-but-short file. Skipping the pass on the timeout path costs
		// one recording's post-record work; running it risks a truncated mock set.
		if recordingStarted && newTestSetID != "" {
			switch {
			case !recordDrainedCleanly:
				r.logger.Warn("skipping the end-of-recording hook: the record drain timed out, so a mock or test-case write may still be in flight; the post-record pass is skipped to avoid racing an unfinished write. The recording itself is saved.",
					zap.String("testSetID", newTestSetID))
			case !storeWrote:
				r.logger.Warn("skipping the end-of-recording hook: the mock store could not write every mock, so the mock file is unfinished",
					zap.String("testSetID", newTestSetID))
			default:
				r.afterRecordingComplete(ctx, newTestSetID)
			}
		}

		// Deferred-orphan revoke: delete TCs whose owned mock was capacity-dropped
		// AFTER the TC streamed (the agent signalled them live via RevokedTests
		// control frames). Applied here — after all inserts drained — so a revoke
		// that raced ahead of its TC's persistence is still caught by the intersect.
		// Snapshot both sets under their OWN locks before intersecting. On the
		// DrainErrGroup TIMEOUT path a wedged consumer goroutine can still be
		// writing these maps (DrainErrGroup returns after its timeout WITHOUT
		// joining a goroutine that ignores cancellation — see utils/drain.go),
		// so an unguarded read here could hit a fatal "concurrent map read and
		// map write". The two maps are never locked together elsewhere, so
		// snapshot each independently (lock, copy, unlock) — no nesting, no
		// ordering hazard.
		revokedMu.Lock()
		revokedSnap := make([]string, 0, len(revokedNames))
		for n := range revokedNames {
			revokedSnap = append(revokedSnap, n)
		}
		revokedMu.Unlock()

		// Names whose delete actually SUCCEEDED. A revoke is not always applied:
		// DeleteTests is reached by runtime assertion because it is deliberately
		// not on the record TestDB interface, and an individual delete can fail and
		// warn. A test that stays in the set IS shipped with a short mock pool, so
		// only a genuinely deleted one may be excluded from the totals below.
		deletedSet := map[string]struct{}{}

		var toDelete []string
		insertedMu.Lock()
		for _, n := range revokedSnap {
			if _, ok := insertedNames[n]; ok {
				toDelete = append(toDelete, n)
			}
		}
		insertedMu.Unlock()
		if len(toDelete) > 0 {
			// DeleteTests is on the concrete *testdb.TestYaml but NOT on the record
			// TestDB interface (enterprise implements that interface; don't break it).
			// Reach it by runtime assertion; degrade to a warning if unavailable.
			if td, ok := r.testDB.(interface {
				DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
			}); ok {
				deleted := 0
				for _, n := range toDelete {
					if err := td.DeleteTests(ctx, newTestSetID, []string{n}); err != nil {
						r.logger.Warn("deferred-orphan revoke: failed to delete a capacity-orphaned test case; leaving it in the set",
							zap.String("testSetID", newTestSetID), zap.String("testCase", n), zap.Error(err))
						continue
					}
					deletedSet[n] = struct{}{}
					deleted++
				}
				if deleted > 0 {
					// Guard under insertedMu so this decrement can't race the
					// (possibly still-live, on the drain-timeout path) testCount++
					// in the TC-insert loop, which now runs under the same lock.
					insertedMu.Lock()
					testCount -= deleted
					insertedMu.Unlock()
					r.logger.Info("deferred-orphan revoke: removed capacity-orphaned test cases whose mock was dropped after streaming",
						zap.Int("revoked", deleted), zap.String("testSetID", newTestSetID))
				}
			} else {
				r.logger.Warn("deferred-orphan revoke: testDB does not support DeleteTests; leaving orphaned test cases in the set",
					zap.Int("count", len(toDelete)))
			}
		}

		// The test cases the set keeps are final: map them (a no-op unless
		// this recording derives its mappings from its test windows).
		if recordingStarted && newTestSetID != "" {
			windowMaps.writeFor(persistCtx, r.logger, r.mappingDb, newTestSetID, deletedSet, recordDrainedCleanly, r.mappingsStreamed.Load(), capWatch.mapped)
		}

		/*
		 * testCount, READ ONCE AND UNDER THE LOCK, for every consumer in
		 * this defer.
		 *
		 * There were three readers and only the annotation's was guarded
		 * -- the two telemetry calls below took it bare. On the
		 * DRAIN-TIMEOUT path DrainErrGroup returns without joining a
		 * wedged consumer, so the `testCount++` in the TC-insert loop can
		 * still be live while this defer runs: exactly the hazard the
		 * revokedNames/insertedNames note above documents, on a plain int
		 * instead of a map, where the race detector has nothing to trip
		 * on until it does.
		 *
		 * Reading once also makes the three consumers agree. Three
		 * separate reads of a value another goroutine may still be
		 * incrementing can report three different totals for one
		 * recording -- telemetry saying N, the session summary N+1, and
		 * the annotation refusing at 0.
		 *
		 * THE POSITION IS WHAT MAKES IT CORRECT. It has to sit BELOW the
		 * deferred-orphan revoke's `testCount -= deleted` and above every
		 * reader: above the decrement, telemetry over-reports revoked
		 * tests, and on an all-revoked recording the `testCount <= 0` arm
		 * stops firing and writes an annotation onto a test-set with no
		 * test cases -- the phantom that arm exists to prevent.
		 *
		 * PINNED by TestStart_AgentRevokedTestIsNotCountedAsShortPool,
		 * which records one test, has the agent revoke it, and asserts
		 * BOTH telemetry counts are 0. Measured: moving this read above
		 * the decrement makes both report 1. It was unpinned until
		 * recTelemetry stopped discarding the count argument -- a fake
		 * that throws a value away makes every test using it blind to
		 * that value.
		 *
		 * It is also STALER than the three bare reads it replaced, on
		 * one path: on the drain-timeout path a still-live `testCount++`
		 * between here and the annotation is now excluded from all
		 * three. That is the trade -- consistency over freshness, on a
		 * path where the count is arbitrary anyway -- and it is a cost,
		 * not a free win.
		 */
		insertedMu.Lock()
		testCountSnapshot := testCount
		insertedMu.Unlock()

		totalMocks := 0
		if recordingStarted {
			mockCountMapMu.Lock()
			mockCountSnapshot := make(map[string]int, len(mockCountMap))
			for k, v := range mockCountMap {
				mockCountSnapshot[k] = v
			}
			// Snapshot the drop counters under the same lock that guards them.
			droppedSnapshot := droppedMockCount
			droppedKindSnapshot := make(map[string]int, len(droppedMockKinds))
			for k, v := range droppedMockKinds {
				droppedKindSnapshot[k] = v
			}
			mockCountMapMu.Unlock()
			// Say it on the way out too, so an operator sees the holes even with
			// telemetry disabled.
			// Totalled here, not accumulated as we went, so every revoke path is
			// accounted for: revokedNames holds both consumeMappings' own revokes
			// and the agent's RevokedTests frames. Subtract on DELETED, not
			// merely revoked — a revoke that could not be applied leaves the test
			// in the set, still short, so it must stay in the count.
			shortPoolTests, shortPoolMocks := 0, 0
			shortPoolByTest.Range(func(k, v any) bool {
				name, _ := k.(string)
				if _, gone := deletedSet[name]; gone {
					return true
				}
				shortPoolTests++
				n, _ := v.(int64)
				shortPoolMocks += int(n)
				return true
			})
			if shortPoolTests > 0 {
				r.logger.Warn("recording finished with test cases whose mock set is INCOMPLETE: some mocks never correlated to their mapping, and unlike a dropped mock this does not revoke the test — these will fail at replay",
					zap.Int("testsWithShortMockPool", shortPoolTests),
					zap.Int("mocksUncorrelated", shortPoolMocks),
					zap.String("testSetID", newTestSetID),
					zap.String("next_step", "ensure the mapping store is enabled; each test's \"Failed to correlate mock mapping\" line says why its mock is missing; re-record the test set if this count is non-trivial"))
			}
			if droppedSnapshot > 0 {
				r.logger.Warn("recording finished with mocks dropped; the tests that owned them were revoked rather than recorded with a short mock pool",
					zap.Int("mocksDropped", droppedSnapshot),
					zap.Any("byKind", droppedKindSnapshot),
					zap.String("testSetID", newTestSetID),
					zap.String("next_step", "check byKind: an unencodable payload is a parser/encoder bug worth reporting; drops during shutdown are usually I/O and re-recording recovers them"))
			}
			suiteMeta := map[string]interface{}{
				"host-domains": domainSet.ToSlice(),
			}
			// Report skipped mocks alongside the recorded ones. mockCountSnapshot
			// counts only successes, so without this a recording with holes is
			// indistinguishable from a clean one in telemetry.
			if droppedSnapshot > 0 {
				suiteMeta["mocks-dropped"] = droppedSnapshot
				suiteMeta["mocks-dropped-by-kind"] = droppedKindSnapshot
			}
			// Gated separately from the drops. The motivating case for these two
			// is a mock the agent names in a mapping but never sends, with a
			// perfectly healthy encoder — zero drops — which is exactly the recording the
			// drop gate excludes, so folding them in there would leave the
			// frequency invisible in the one case they were added to measure.
			if shortPoolTests > 0 {
				suiteMeta["tests-short-pool"] = shortPoolTests
				suiteMeta["mocks-uncorrelated"] = shortPoolMocks
			}
			r.telemetry.RecordedTestSuite(newTestSetID, testCountSnapshot, mockCountSnapshot, suiteMeta)
			for _, c := range mockCountSnapshot {
				totalMocks += c
			}
		}
		// Emit the session summary on every graceful stop: either recording
		// actually started, OR an error path set a stopReason before frames
		// were established (setup/agent-not-ready/testset-lookup failures that
		// previously emitted nothing — the highest-signal "stuck" cases).
		// "completed" for a clean exit / user Ctrl+C, "aborted" when a
		// stopReason was set; stop_reason carries the categorized cause.
		// A drain that stopped waiting for what the agent still held from
		// before the stop (holdForBacklog) left the recording incomplete:
		// said once more here, where the run ends, and the run fails.
		agentHeldBack := r.captureLost.Load()
		if agentHeldBack {
			r.logger.Error("the recording may be incomplete: Keploy could not confirm it saved all the keploy agent captured before the stop",
				zap.String("testSetID", newTestSetID),
				zap.String("next_step", "the lines above say why (the agent made no progress, went away before it said it had handed everything over, a second interrupt stopped the saving, or the mock store could not write every mock); re-record this test set"))
			utils.SetExitCodeOnce(utils.ExitKeployError)
		}
		// Under the proven switch: test cases recorded while the agent could
		// not capture every call, or that could not be checked, and
		// connections to a mock.real destination neither captured nor
		// relayed, fail the run, unless a later pass judges each test case
		// (JudgeLossesLater).
		fail, lossy := r.finishCapture(capWatch, newTestSetID)
		if fail {
			utils.SetExitCodeOnce(utils.ExitKeployError)
		}
		if recordingStarted || stopReason != "" {
			status, reason := sessionEnd(stopReason, agentHeldBack, lossy, storeWrote, agentLost)
			r.telemetry.RecordSessionCompleted(int64(testCountSnapshot), int64(totalMocks), time.Since(sessionStart).Milliseconds(), status, reason)
		}
		/*
		 * THE UI JOIN ANNOTATION, written once, at stop.
		 *
		 * Here and not at start, for two reasons that both come from
		 * pkg/models/uijoin.go. T1WallMs is the session's end and is not
		 * knowable before it; and validateStructure refuses an empty
		 * IngressPorts list, so at start — with no ingress observed yet —
		 * there is no storable annotation to write.
		 *
		 * ON persistCtx, NOT ctx. This defer runs during teardown, so on
		 * Ctrl+C `ctx` is already cancelled and the write would fail for
		 * the one reason that has nothing to do with the data. persistCtx
		 * is the same detached context the test-case and mock stores use,
		 * and for the same reason.
		 *
		 * NOT GUARDED ON recordingStarted, deliberately. Such a guard
		 * would be redundant rather than wrong: a session that never
		 * began has nothing to annotate, and that is already handled one
		 * layer down — `recordingStarted = true` is set before the
		 * consumer goroutine that calls observe() is ever spawned, so
		 * recordingStarted == false implies both that sorted() is nil
		 * and that nothing was persisted.
		 *
		 * WHICH ARM FIRES DEPENDS ON WHY THE RUN STOPPED, and an earlier
		 * version of this note said it was always the testCount one and
		 * always Debug. That is no longer true and was the justification
		 * for deleting the guard, so it is worth restating exactly. If
		 * GetNextTestSetID failed, newTestSetID is "" and the
		 * `testSetID == ""` arm fires -- at ERROR, because a join that
		 * was REQUESTED and cannot be attempted must not return
		 * silently. Only when a test-set id exists and nothing was
		 * persisted does the testCount arm fire, at Debug.
		 *
		 * The guard stays deleted anyway, and on a better reason than
		 * the one it had: what it would suppress is a truthful Error
		 * about a join the operator asked for and did not get. It could
		 * be deleted with the whole package green, and inverting it to
		 * `!recordingStarted` DID turn the harness test red -- so the
		 * direction was pinned and only the removal survived.
		 */
		/*
		 * A DEADLINE, AND EXACTLY AS MUCH AS THAT BUYS.
		 *
		 * persistCtx is context.WithoutCancel(ctx), which strips the
		 * deadline along with the cancellation -- that is what it is for,
		 * so a write survives SIGINT. This call then sits after every
		 * DrainErrGroup budget, on the path SIGINT takes, and does a
		 * read-modify-write of config.yaml with no bound of its own.
		 *
		 * WHAT THE 10s ACTUALLY REACHES, traced rather than assumed:
		 * ctxReader.Read selects on ctx.Done() between chunks, and
		 * CreateFileF checks ctx.Err() in its not-exist branch. That is
		 * all. ctxWriter.Write (pkg/platform/yaml/yaml.go) is a plain
		 * retry loop that never looks at the context; MkdirAll, OpenFile,
		 * Sync, Chmod and Rename are blocking syscalls a Go context
		 * cannot interrupt; and warnIfDroppingFields re-reads with
		 * context.WithoutCancel, deliberately discarding this deadline
		 * inside the very write it is meant to bound.
		 *
		 * SO THIS DOES NOT MAKE TEARDOWN SAFE ON A WEDGED MOUNT. An
		 * NFS/overlay volume in D-state still hangs here, deadline or
		 * not, and it is not "bounded like every other operation in this
		 * defer": the drains bound because DrainErrGroup returns WITHOUT
		 * its goroutine, which a deadline over synchronous file I/O has
		 * no equivalent for. Nor is it the only unbounded one -- the
		 * per-name DeleteTests loop earlier in this defer is equally
		 * unbounded.
		 *
		 * What it does buy is the slow-but-responsive disk: the read
		 * returns DeadlineExceeded, persistUIJoinAnnotation propagates
		 * it, and the operator gets an Error line instead of an
		 * indefinite wait. The write is temp-file + Rename, so a
		 * cut-off write cannot corrupt config.yaml. Losing the
		 * annotation there is the right trade -- the recording is
		 * already on disk.
		 *
		 * AND NO TEST PINS THE BOUND. Measured: removing it entirely
		 * survives the package 10 runs out of 10 (shrinking it to 1ns is
		 * caught, so the context is genuinely propagated -- the duration
		 * is not). Said here rather than left to be found.
		 */
		annotateCtx, annotateCancel := context.WithTimeout(persistCtx, 10*time.Second)
		r.recordUIJoinAnnotation(
			annotateCtx,
			newTestSetID,
			// One argument where there were two interchangeable int64s,
			// and T1 is stamped inside newUIJoinSpan -- see uiJoinSpan.
			// NOT a compile-time guarantee: these two are both time.Time
			// and swapping them still builds. It is caught by
			// TestStart_TakesT0FromTheCaptureStartNotTheProcessStart,
			// measured 10 kills out of 10.
			newUIJoinSpan(captureStart, sessionStart),
			uiJoinObserved.sorted(),
			// The one guarded snapshot, taken at the top of this defer
			// and shared by all three consumers -- this annotation and
			// the two telemetry calls beside it, which read
			// testCountSnapshot rather than the live counter for the
			// same reason. Reading the counter once is what makes the
			// three agree; an inline read here could race the
			// still-live consumer on the drain-timeout path and size the
			// annotation from a different number than the telemetry.
			//
			// NOT COVERED, said rather than left to be found: replacing
			// this with a bare `testCount` read is a MEASURED survivor.
			// The snapshot's POSITION is pinned (moving it above the
			// revoke block is caught); the snapshot itself is not.
			testCountSnapshot,
		)
		annotateCancel()
		if s, ok := r.telemetry.(interface{ Shutdown() }); ok {
			s.Shutdown()
		}
	}()

	// appErrChan is intentionally NOT closed by Start. The app-runner goroutine
	// spawned in runAppErrGrp (the DockerCompose branch at ~308 and the
	// non-compose branch at ~518) sends the app's exit error on it and can
	// still be running when Start returns during shutdown: on SIGTERM the app
	// exits with "signal: terminated" (not ErrCtxCanceled), so the goroutine
	// reaches its `appErrChan <- runAppError` send. Closing the channel here
	// raced that send and panicked with "send on closed channel" (seen in the
	// pulsar-basetopic teardown). The sole consumer is a single receive in the
	// select below that does not depend on a close, and the size-1 buffer
	// absorbs the lone send (the two sender branches are mutually exclusive),
	// so leaving the channel open to be GC'd is correct and race-free.
	// insertTestErrChan / insertMockErrChan are NOT closed either, for exactly
	// the reason spelled out for appErrChan above. Their producer is the
	// consumer goroutine spawned below, which can still be mid-send when Start
	// returns during teardown; closing here raced that send and panicked with
	// "send on closed channel", taking down the whole control plane in the
	// DaemonSet embedding. The sole consumer is a single receive in the select
	// below that does not depend on a close, and the size-10 buffers absorb any
	// in-flight send, so leaving them open to be GC'd is correct and race-free.

	newTestSetID, err = r.GetNextTestSetID(ctx)
	if err != nil {
		stopReason = "failed to get new test-set id"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	// Create config.yaml if metadata is provided
	if r.config.Record.Metadata != "" && r.testSetConf != nil {
		r.createConfigWithMetadata(ctx, newTestSetID)
	}

	//checking for context cancellation as we don't want to start the instrumentation if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	passPortsUint := config.GetByPassPorts(r.config)
	passPortsUint32 := make([]uint32, len(passPortsUint)) // slice type of uint32
	for i, port := range passPortsUint {
		passPortsUint32[i] = uint32(port)
	}

	memoryLimit := uint64(0)
	if r.config.CommandType == string(utils.DockerRun) || r.config.CommandType == string(utils.DockerCompose) {
		memoryLimit = r.config.Record.MemoryLimit
	}

	// Instrument will setup the environment and start the hooks and proxy
	setupOpts := models.SetupOptions{Container: r.config.ContainerName, DockerDelay: r.config.BuildDelay, Mode: models.MODE_RECORD, CommandType: r.config.CommandType, EnableTesting: false, GlobalPassthrough: r.config.Record.GlobalPassthrough, DisableHandshakeHold: r.config.Record.DisableHandshakeHold, CapturePackets: r.config.Record.CapturePackets, OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept, ChannelBindingShim: r.config.Record.ChannelBindingShim, UpstreamTLSVerify: r.config.Record.UpstreamTLS.Verify, UpstreamTLSCACert: r.config.Record.UpstreamTLS.CACert, BuildDelay: r.config.BuildDelay, PassThroughPorts: passPortsUint, MemoryLimit: memoryLimit, ConfigPath: r.config.ConfigPath, EnableSampling: r.config.Record.EnableSampling, RecordBufferMaxMemoryPerConn: r.config.Record.RecordBuffer.MaxMemoryPerConnection, RecordBufferQueueSize: r.config.Record.RecordBuffer.QueueSize, RecordBufferConsumerStallGrace: r.config.Record.RecordBuffer.ConsumerStallGrace, RecordBufferHalfCloseGrace: r.config.Record.RecordBuffer.HalfCloseGrace}
	// Retry only a stalled agent bring-up (pkg.ErrAgentNotReady) with a fresh agent.
	err = pkg.RetryAgentSetup(setupCtx, r.logger, func(c context.Context, _ int) error {
		return r.instrumentation.Setup(c, r.config.Command, setupOpts)
	})
	if err == nil && !setupGate.Up() {
		// The interrupt came first, as the setup finished, and stopped the
		// agent: an interrupted setup.
		err = context.Cause(ctx)
	}
	// Otherwise the agent is up: from here a Ctrl+C no longer stops it; the
	// stop defer does, once the drains are done.

	if err != nil {
		// If context was cancelled (user pressed Ctrl+C), return gracefully without error
		if ctx.Err() != nil {
			return nil
		}
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		// Wrapped, so the cause is named where the run's error is printed
		// and the exit code its tag calls for (utils.ExitCodeFor) survives.
		return fmt.Errorf("%s: %w", stopReason, err)
	}

	r.logger.Debug("Command type:", zap.String("commandType", r.config.CommandType))

	if r.config.CommandType == string(utils.DockerCompose) {

		r.logger.Info("Waiting for keploy-agent to be ready for docker compose...", zap.String("Agent-uri", r.config.Agent.AgentURI))

		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			noteStopKilled(&stopKilled, runAppError)
			if (runAppError.AppErrorType == models.ErrCtxCanceled || runAppError == models.AppError{}) {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})

		// The wait follows the agent's start, not a clock: it lasts while the
		// start makes progress (and while the engine has not run the agent's
		// container yet: compose starts it in its own time), and ends once
		// the start fails or stops making progress (pkg.WatchAgentStart). A
		// fixed budget gave up on a start a loaded host ran slowly, and
		// waited out the whole of it for one that had stopped.
		agentCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		agentReadyCh := make(chan bool, 1)
		go pkg.AgentHealthTicker(agentCtx, r.logger, r.config.Agent.AgentURI, agentReadyCh, 1*time.Second)
		agentStart := pkg.WatchAgentStart(agentCtx, r.instrumentation)

		select {
		case <-ctx.Done():
			// A stop (Ctrl+C) is no agent that never got ready, and no
			// failure.
			return ctx.Err()
		case err := <-agentStart:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			stopReason = "the keploy-agent docker compose started did not come up"
			utils.LogError(r.logger, err, stopReason)
			return fmt.Errorf("%s: %w", stopReason, err)
		case appErr := <-appErrChan:
			// The project stopped before the agent was ready. When the agent
			// is what stopped -- it could not start: no tracefs where Docker
			// runs, a kernel that refused it eBPF -- compose aborts the
			// project over it ("dependency failed to start"), and its own
			// exit says why, with its code (as `keploy mock` reads it).
			// Otherwise the project died on its own (a bad compose file, a
			// failed build, a port in use), with its code. Either way, now:
			// not after the agent's whole readiness budget, as a "not ready
			// in time" that names neither.
			if err := r.composeAgentFailure(); err != nil {
				stopReason = "the keploy agent stopped the compose project before it was ready"
				utils.LogError(r.logger, err, stopReason)
				return fmt.Errorf("%s: %w", stopReason, err)
			}
			if appStoppedBySignal(appErr) {
				stopReason = "the compose project was stopped (SIGTERM or SIGINT) before the keploy-agent was ready, hence stopping keploy"
				r.logger.Info(stopReason, zap.Int("app_exit_code", appErr.ExitCode))
				return nil
			}
			stopReason = "the compose project stopped while keploy was waiting for the keploy-agent to come up"
			utils.LogError(r.logger, appErr, stopReason)
			return fmt.Errorf("%s: %w", stopReason, appErr)
		case ready, ok := <-agentReadyCh:
			// The ticker closes its channel, unready, only once ctx ends.
			if !ok || !ready {
				return ctx.Err()
			}
		}
		// The health check answered: the watch of the agent's start ends.
		cancel()
	}

	r.logger.Debug("Agent is ready. Starting to fetch test cases and mocks...")

	// A stop before the agent's streams open finds nothing captured: end here
	// rather than open them only to drain them.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Under docker compose the agent is stopped with the app's project, and
	// this client learns of that only from the agent: its streams ask the
	// agent to say so (models.WithAgentStopNotice), and agentStopping closes
	// when it does.
	streamCtx := reqCtx
	var agentStopping <-chan struct{}
	if w, ok := r.instrumentation.(agentStopWatcher); ok && r.config.CommandType == string(utils.DockerCompose) {
		streamCtx = models.WithAgentStopNotice(reqCtx)
		agentStopping = w.WatchAgentStop(reqCtx)
	}
	// fetching test cases and mocks from the application and inserting them into the database
	frames, err := r.GetTestAndMockChans(streamCtx)
	if err != nil {
		stopReason = "failed to get data frames"
		utils.LogError(r.logger, err, stopReason)
		if ctx.Err() == context.Canceled {
			return err
		}
		return fmt.Errorf("%s: %w", stopReason, err)
	}
	recordingStarted = true
	// Stamped HERE, beside the flag, because they mean the same thing:
	// GetTestAndMockChans has returned, so the agent is capturing.
	captureStart = time.Now()
	// What the agent could not capture is counted from here: the agent is
	// capturing, and a native app has not started.
	capWatch = watchCaptureLosses(ctx, r.logger, r.instrumentation)
	if agentStopping != nil {
		// Compose is stopping the agent (the app exited on its own, or a
		// container of its project did), and the agent waits for this drain
		// before it goes: drain it now, not once compose has taken it down
		// with the project, which it waits on the agent to do.
		go func() {
			select {
			case <-agentStopping:
				r.logger.Info("docker compose is stopping the keploy agent with the app's project; saving what it captured before it goes")
				drainComposeAgent(true)
			case <-reqCtx.Done():
			}
		}()
	}

	/*
	 * THE FORWARDERS NOW OUTLIVE THIS FUNCTION, so every path out of it has
	 * to tell them whether a consumer is coming.
	 *
	 * GetTestAndMockChans has spawned UP TO three forwarders on reqCtx
	 * (WithoutCancel), and they keep pulling from the agent regardless of
	 * what happens to ctx.
	 *
	 * "Up to", because it has two `err == nil` returns of its own, both on
	 * the shutdown path: the one taken when GetIncoming fails with ctx
	 * already done has spawned NONE and closes all three channels, and
	 * the GetOutgoing equivalent has spawned ONE (the incoming forwarder)
	 * and sets handedOff itself. This defer is correct on all three
	 * counts -- it abandons whatever exists -- but an earlier version of
	 * this note said "three" flatly, which is only true of a full
	 * success, and someone reasoning from it about the shutdown paths
	 * would be reasoning about goroutines that were never started.
	 *
	 * The consumers that drain them are spawned three
	 * hundred lines below, and between here and there is exactly ONE
	 * return: the agent's refusal of this keploy's proven switch. On that
	 * path the forwarders are alive with nobody reading, and each one ends
	 * by handing over the item it has already taken from the agent. That
	 * hand-over used to park forever: a 30s DrainErrGroup timeout on Ctrl+C
	 * and a goroutine held for the process lifetime, re-leaked per session
	 * in the DaemonSet embedding. MEASURED deterministically on all three.
	 *
	 * A defer rather than a call beside each return, because the failure
	 * mode of getting this wrong is a hang rather than a compile error, and
	 * a new error return added later would reintroduce it silently. The flag
	 * is set only once every consumer is running, so it says exactly what it
	 * is named for: is anyone going to read these channels.
	 */
	consumersStarted := false
	defer func() {
		if consumersStarted {
			return
		}
		/*
		 * NIL-GUARDED, because a missing Abandon must degrade to the leak
		 * this defer exists to prevent -- not to a panic in teardown.
		 *
		 * GetTestAndMockChans sets Abandon on every err == nil return, so
		 * this is unreachable today, and the suite cannot reach it either
		 * -- MEASURED: removing this nil guard AND `Abandon:` from an
		 * early return leaves the package green. The guard is kept for
		 * the cost of being wrong about a construction site, not because
		 * a test defends it; saying so is more useful than a measurement
		 * that was never taken.
		 */
		if frames.Abandon != nil {
			frames.Abandon()
		}
	}()

	// A stop that comes from here on (ctx done) finds the agent capturing,
	// and goes the way of any stop: through the consumers below, and the
	// stop's drain of what the agent captured before it. Returning here, as
	// this once did, abandoned the agent's streams; under docker compose,
	// whose stack (the agent in it) is up by now, the stop then took the
	// stack down with all the agent held -- lost, said by no one. A busy
	// host stretches the window between the agent's streams opening and the
	// consumers running.

	// Kick off the pcap + keylog streams. GetTestAndMockChans above
	// has already triggered Proxy.Record() on the agent, which
	// installs the broadcaster — so the stream subscribes against a
	// live capture. The goroutine ends when ctx is cancelled
	// (recording stop) or when the agent's HTTP server closes the
	// response. Any error is logged but never fails the recording.
	if r.config.Record.CapturePackets {
		destDir := filepath.Join(r.config.Path, newTestSetID)
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			r.logger.Warn("failed to create test-set dir for pcap stream; continuing without pcap",
				zap.String("destDir", destDir), zap.Error(err))
		} else {
			errGrp, _ := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
			if errGrp != nil {
				errGrp.Go(func() error {
					if err := r.instrumentation.StreamPcapArtifacts(ctx, destDir); err != nil {
						utils.LogError(r.logger, err, "pcap stream ended with error",
							zap.String("destDir", destDir))
					}
					return nil
				})
			}
		}
	}

	// Not once the recording is stopping: the app the agent would release
	// is being stopped.
	if r.config.CommandType == string(utils.DockerCompose) && ctx.Err() == nil {

		r.logger.Debug("Making keploy-agent ready for docker compose...")

		err := r.instrumentation.MakeAgentReadyForDockerCompose(ctx)
		if errors.Is(err, provenswitch.ErrAgentSwitch) {
			// The agent and this keploy are on different proven paths:
			// recording against it would capture under the wrong rules.
			stopReason = "keploy's agent does not have this keploy's proven switch"
			return err
		}
		if err != nil {
			utils.LogError(r.logger, err, "Failed to make the request to make agent ready for the docker compose")
		}
	}

	if ctx.Err() == nil {
		r.logger.Info("Keploy agent is ready to record test cases and mocks.")
	}

	r.mockDB.ResetCounterID() // Reset mock ID counter for each recording session
	errGrp.Go(func() error {
		for testCase := range frames.Incoming {
			r.frameProgress.Add(1)
			// Skip curl generation for either form data requests or large body (>1MB)
			if len(testCase.HTTPReq.Body) <= 1*1024*1024 && len(testCase.HTTPReq.Form) == 0 {
				testCase.Curl = pkg.MakeCurlCommand(testCase.HTTPReq)
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromTestCase(testCase))
			if hookErr := r.hooks.BeforeTestCaseInsert(ctx, &TestCaseContext{
				TestCase: testCase, TestSetID: newTestSetID,
			}); hookErr != nil {
				r.logger.Error("BeforeTestCaseInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("testCaseName", testCase.Name))
			}
			/*
			 * OBSERVED HERE, at the one point ingress is known to have
			 * ARRIVED -- which is BEFORE the insert, not after it.
			 *
			 * IngressPorts exists so the offline joiner can say
			 * NO_INGRESS_OBSERVED for an exchange on a port nothing was
			 * listening on, rather than failing to join with no reason.
			 * That answer is only honest if the list records ports
			 * ingress ACTUALLY arrived on — a configured value would
			 * describe intent, and could claim a port this recording
			 * never saw.
			 *
			 * Before the insert, deliberately: a test case that fails to
			 * persist still tells us which port ingress came in on, and
			 * the port list is about what the recorder observed, not
			 * about what the store accepted.
			 *
			 * PINNED BOTH WAYS, BY TWO TESTS -- and it takes two.
			 *
			 * Moving this call into the `err == nil` branch below is the
			 * disk-full shape, where every insert errors and the mutant
			 * loses the join entirely:
			 * TestAPortIsObservedEvenWhenItsTestCaseFailsToPersist fails
			 * on it.
			 *
			 * Moving it after the whole if/else is only visible when the
			 * error branch takes its `continue`, which needs ctx already
			 * cancelled. That test runs on context.Background(), so
			 * execution falls through past the if/else either way and the
			 * mutant SURVIVED it 10 runs out of 10.
			 * TestAPortIsObservedWhenTheInsertFailsDuringShutdown is the
			 * missing half: it holds the insert open, cancels, then lets
			 * it fail, so the `continue` really is taken.
			 */
			uiJoinObserved.observe(testCase.AppPort)
			err := r.testDB.InsertTestCase(persistCtx, testCase, newTestSetID, true)
			// After the insert, which is what names an auto-named test case.
			unreachablePorts.check(ctx, testCase)
			if err != nil {
				windowMaps.addLostTest(testCase)
				if ctx.Err() != nil {
					// Once shutdown has begun nothing reads insertTestErrChan:
					// Start's select has already returned, and this loop keeps
					// draining for the whole app-drain window. Log instead of
					// dropping the error on the floor: with persistCtx the write
					// no longer fails merely because we are shutting down, so an
					// error here is real and the operator needs it.
					utils.LogError(r.logger, err, "failed to record test case during shutdown",
						zap.String("testCaseName", testCase.Name),
						zap.String("testSetID", newTestSetID))
					continue
				}
				// Non-blocking for the same reason as the mock loop's sibling
				// send below: on a disk-full EVERY insert errors here, and this
				// loop keeps draining long after Start's one-shot select has
				// taken the first error. A blocking send would park forever once
				// the buffer filled, wedging this goroutine and the forwarder
				// feeding frames.Incoming, timing out DrainErrGroup and leaking
				// both past teardown. The buffer guarantees the first error —
				// the only one acted on — still lands.
				select {
				case insertTestErrChan <- err:
				default:
				}
			} else {
				// testCount and insertedNames are both TC-insert state; write
				// them under one insertedMu section so the finalize revoke block
				// (which reads insertedNames and adjusts testCount, possibly
				// while this goroutine is still live on the drain-timeout path)
				// can't race them.
				insertedMu.Lock()
				testCount++
				insertedNames[testCase.Name] = struct{}{}
				insertedMu.Unlock()
				windowMaps.addTest(testCase)
				capWatch.afterTestCase(ctx, testCase.Name)
				r.telemetry.RecordedTestAndMocks()
				if hookErr := r.hooks.AfterTestCaseInsert(ctx, &TestCaseContext{
					TestCase: testCase, TestSetID: newTestSetID,
				}); hookErr != nil {
					r.logger.Error("AfterTestCaseInsert hook failed; test case was recorded successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("testCaseName", testCase.Name))
				}
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		// The document the MockDB writes for each mock, for AfterMockInsert.
		insertedDoc := NewInsertedMockDoc(persistCtx, r.hooks)
		// No mock comes after this loop: a mapping entry still waiting for
		// one is settled as never arrived.
		defer mappingHolds.end()
		// dropMock publishes that the recorder failed to save tempID's mock,
		// then tells the waits. It reports whether the drop is tracked (its
		// test revocable): past maxLiveDroppedMockIDs it is not, and an entry
		// held for it is settled as lost for that reason.
		dropMock := func(tempID string) bool {
			if droppedMockIDs.add(tempID) {
				mappingHolds.arrived(tempID)
				return true
			}
			mappingHolds.droppedUntracked(tempID)
			return false
		}
		for mock := range frames.Outgoing {
			r.frameProgress.Add(1)
			// Deferred-orphan revoke: a reserved-Kind control frame, NOT a mock.
			// Divert it into the revoke set (applied at finalize) BEFORE any
			// domain-extraction / hook / InsertMock / correlation work — it
			// carries no traffic and must never be persisted.
			if mock.GetKind() == string(models.RevokedTests) {
				var revoked []string
				if mock.Spec.Metadata != nil {
					for _, n := range strings.Split(mock.Spec.Metadata["revoked_tests"], ",") {
						if n = strings.TrimSpace(n); n != "" {
							revokedMu.Lock()
							revokedNames[n] = struct{}{}
							revokedMu.Unlock()
							revoked = append(revoked, n)
						}
					}
				}
				// A revoked test is deleted at the end: its mapping still names
				// the mock the agent dropped and never sends, so nothing of it
				// may wait for that mock (and count toward the waits' cap).
				mappingHolds.revoke(revoked)
				continue
			}
			domainSet.AddAll(telemetry.ExtractDomainsFromMock(mock))
			tempID := mock.Name
			mockCtx := &MockContext{Mock: mock, TestSetID: newTestSetID}
			if hookErr := r.hooks.BeforeMockInsert(ctx, mockCtx); hookErr != nil {
				r.logger.Error("BeforeMockInsert hook failed; recording will continue but hook side-effects may be missing. Check your RecordHooks implementation.",
					zap.Error(hookErr),
					zap.String("testSetID", newTestSetID),
					zap.String("mockName", mock.Name),
					zap.String("mockKind", mock.GetKind()))
			}
			if mockCtx.Skip {
				// A hook asked to drop this mock (collapsed async poll no-change
				// cycle). Do not persist, map, or correlate it.
				//
				// The agent does not know we skipped it, so its tempID is still
				// in the mapping it streams. Skip means "never persisted", so
				// this tempID must never appear in a per-test mapping and must
				// never be read as loss: the skip marker says so, and is the
				// outcome a mapping entry waiting for this mock is settled on.
				// It cannot be gated on mock.IsAsync(): AsyncRecorder sets
				// info.Skip and returns BEFORE it assigns Spec.Async, so a
				// collapsed cycle reports IsAsync()==false. Gating on it would
				// make this whole guard a no-op.
				markSkippedMock(&asyncMockIDs, tempID)
				mappingHolds.arrived(tempID)
				continue
			}
			// The AsyncRecorder hook sets Spec.Async in BeforeMockInsert above;
			// remember it so the mapping goroutine below never per-test maps it.
			if mock.IsAsync() {
				asyncMockIDs.Store(tempID, struct{}{})
			}
			encoded, encodedFormat, err := insertedDoc.Insert(r.mockDB, mock, newTestSetID)
			if err != nil {
				if ctx.Err() != nil {
					// See the sibling note on the test-case insert: insertMockErrChan
					// is no longer safe to report through once teardown has begun. A
					// skipped mock also skips its correlationMap entry below, which
					// strands the mapping that references it — so this must be loud.
					//
					// Bookkeep EVERY error class here, not just ErrMockEncode: past
					// this point the mock is lost whatever the cause, and the
					// consequences are identical — the owning test would otherwise be
					// persisted with a short pool, invisible to both the teardown
					// Warn and the telemetry. Nothing is being made non-fatal: teardown is already
					// under way.
					mockCountMapMu.Lock()
					droppedMockCount++
					droppedMockKinds[mock.GetKind()]++
					mockCountMapMu.Unlock()
					dropMock(tempID)
					utils.LogError(r.logger, err, "failed to record mock during shutdown",
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()),
						zap.String("testSetID", newTestSetID))
					continue
				}
				if errors.Is(err, models.ErrMockEncode) {
					// THIS mock's payload cannot be encoded — an unsupported
					// kind or a value the encoder cannot represent. Unlike a
					// disk-full or storage-gone error it says nothing about the
					// next mock, so killing the session throws away everything
					// recorded so far: one gzip response body ended a 46-hour
					// production recording exactly this way. Drop the one mock
					// and keep recording.
					mockCountMapMu.Lock()
					droppedMockCount++
					droppedMockKinds[mock.GetKind()]++
					dropped := droppedMockCount
					mockCountMapMu.Unlock()
					// Tell the mapping consumer so it revokes the owning test
					// instead of recording it short.
					tracked := dropMock(tempID)
					// Loud on the first, then sampled: a recording that quietly
					// grows holes is worse than one that dies, so this must stay
					// visible without drowning the log on a systematic failure.
					if dropped == 1 || dropped%100 == 0 {
						utils.LogError(r.logger, err, "dropping ONE unencodable mock and CONTINUING to record; the test that owns it is revoked, not recorded short",
							zap.String("mockName", mock.Name),
							zap.String("mockKind", mock.GetKind()),
							zap.String("testSetID", newTestSetID),
							zap.Int("mocksDroppedSoFar", dropped),
							zap.Bool("ownerRevocable", tracked))
					}
					continue
				}
				// Environmental (I/O, storage gone): every later mock fails too,
				// so stopping is still correct — but stopping is not instant.
				// Start's select takes the first error and teardown then runs
				// for the whole app-drain window with this loop still draining,
				// so errors 2..N land here too. Bookkeep them exactly like the
				// shutdown branch: the mock is lost whatever the cause, and
				// without this the teardown Warn and the mocks-dropped telemetry
				// undercount while every owning test is persisted with a short
				// pool instead of revoked.
				mockCountMapMu.Lock()
				droppedMockCount++
				droppedMockKinds[mock.GetKind()]++
				dropped := droppedMockCount
				mockCountMapMu.Unlock()
				dropMock(tempID)
				if dropped == 1 || dropped%100 == 0 {
					utils.LogError(r.logger, err, "failed to record mock (environmental); stopping the session",
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()),
						zap.String("testSetID", newTestSetID),
						zap.Int("mocksDroppedSoFar", dropped))
				}
				// The send must not block: once the buffer filled, a blocking
				// send would wedge this goroutine forever — which in turn wedges
				// the forwarder feeding frames.Outgoing, times out both
				// DrainErrGroup budgets, and leaks the pair past teardown (it
				// matters in the DaemonSet embedding, where Start is re-entered
				// per session). Only the FIRST error is acted on anyway; the
				// buffer guarantees it lands.
				select {
				case insertMockErrChan <- err:
				default:
				}
			} else {
				if hookErr := r.hooks.AfterMockInsert(ctx, &MockContext{
					Mock: mock, TestSetID: newTestSetID,
					Encoded: encoded, EncodedFormat: encodedFormat,
				}); hookErr != nil {
					r.logger.Error("AfterMockInsert hook failed; mock was inserted successfully but post-insert hook side-effects may be missing. Check your RecordHooks implementation.",
						zap.Error(hookErr),
						zap.String("testSetID", newTestSetID),
						zap.String("mockName", mock.Name),
						zap.String("mockKind", mock.GetKind()))
				}
				// Before the mock is published to its mappings: a mapping
				// that resolves it finds what its stamp said.
				capWatch.saved(mock)
				if tempID != "" && mock.Name != "" {
					correlationMap.Store(tempID, models.MockEntry{
						Name:             mock.Name,
						Kind:             string(mock.GetKind()),
						Timestamp:        mock.Spec.ReqTimestampMock.Unix(),
						ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
						ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
					})
					// Published first, then told: a mapping entry held for it
					// settles on what it now finds.
					mappingHolds.arrived(tempID)
				}
				windowMaps.addMock(mock)
				mockCountMapMu.Lock()
				mockCountMap[mock.GetKind()]++
				mockCountMapMu.Unlock()
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	errGrp.Go(func() error {
		return r.consumeMappingsFor(ctx, newTestSetID, frames.Mappings, &correlationMap, &asyncMockIDs, &droppedMockIDs, mappingHolds,
			func(testName string) {
				// Reuse the deferred-orphan revoke path the agent's own
				// capacity-drop uses: finalize deletes these test cases instead
				// of letting them reach replay without their mocks.
				revokedMu.Lock()
				revokedNames[testName] = struct{}{}
				revokedMu.Unlock()
				mappingHolds.revoke([]string{testName})
			}, &shortPoolByTest, capWatch.mapped)
	})

	// Every frame channel now has a reader, so the shutdown hand-over can no
	// longer be abandoned. Nothing may return between GetTestAndMockChans and
	// this line without the defer above firing.
	consumersStarted = true

	// Not once the recording is stopping: an app started now would only be
	// stopped again.
	if r.config.CommandType != string(utils.DockerCompose) && ctx.Err() == nil {
		runAppErrGrp.Go(func() error {
			runAppError = r.instrumentation.Run(runAppCtx, models.RunOptions{})
			noteStopKilled(&stopKilled, runAppError)
			if runAppError.AppErrorType == models.ErrCtxCanceled {
				return nil
			}
			appErrChan <- runAppError
			return nil
		})
	}

	// setting a timer for recording
	if r.config.Record.RecordTimer != 0 && ctx.Err() == nil {
		errGrp.Go(func() error {
			r.logger.Info("Setting a timer of " + r.config.Record.RecordTimer.String() + " for recording")
			timer := time.After(r.config.Record.RecordTimer)
			select {
			case <-timer:
				r.logger.Info("Time up! Stopping keploy")
				err := utils.Stop(r.logger, "Time up! Stopping keploy")
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	// Waiting for the error to occur in any of the go routines
	select {
	case appErr := <-appErrChan:
		// Under compose the agent is a service in the project: when it
		// crashes, compose stops the app over it (143 or 137), or never
		// starts it (and may report 0), and the run is the agent's failure,
		// keploy's, whatever the app's code. A project that stopped before
		// the app ever started is no clean end either, though compose
		// reports 0. An app that ran and ended with a code of its own (0,
		// 42) keeps it, even if the agent crashed after that.
		if err := r.composeEnd(appErr.ExitCode); err != nil {
			stopReason = "the compose project stopped after the keploy-agent was ready"
			utils.LogError(r.logger, err, stopReason)
			return fmt.Errorf("%s: %w", stopReason, err)
		}
		if appStoppedBySignal(appErr) {
			stopReason = "the user application was stopped (SIGTERM or SIGINT), hence stopping keploy"
			r.logger.Info(stopReason, zap.Int("app_exit_code", appErr.ExitCode))
			return nil
		}
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "error in running the user application, hence stopping keploy"
		case models.ErrUnExpected:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
		case models.ErrInternal:
			stopReason = "internal error occurred while hooking into the application, hence stopping keploy"
		case models.ErrAppStopped:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
			r.logger.Info(stopReason, zap.Error(appErr))
			return nil
		case models.ErrCtxCanceled:
			return nil
		case models.ErrTestBinStopped:
			stopReason = "keploy test mode binary stopped, hence stopping keploy"
			return nil
		default:
			stopReason = "unknown error received from application, hence stopping keploy"
		}
		err = appErr

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf("%s: %w", stopReason, err)
}

func (r *Recorder) GetTestAndMockChans(ctx context.Context) (FrameChan, error) {
	// This recording's streams end by the agent's markers when it can
	// (stream_end.go): its token names them to the agent.
	ends := &streamsEnd{token: rand.Text()}
	// This recording drains the agent's streams after its stop and closes
	// them before it stops the agent: an agent the client started holds a
	// stop signal it gets meanwhile for them (models.DrainsAfterStopHeader).
	ctx = models.WithDrainsAfterStop(ctx)
	ctx = models.WithStreamEndToken(ctx, ends.token)

	incomingOpts := models.IncomingOptions{
		Filters: r.config.Record.Filters,
	}

	// Create channels to receive incoming and outgoing data.
	//
	// THE ONE SLOT IS A CONVENIENCE, NOT A GUARANTEE. It is tempting to
	// reason that the handover fires at most once per forwarder, so one slot
	// is exactly sufficient; it does not follow. The slot can ALREADY be full
	// from an ordinary send that no consumer has taken yet, and the handover
	// below then parks on a full buffer exactly as it would on an unbuffered
	// one. Measured, with nothing consuming: mappingChan (unbuffered) wedges
	// on the first item, these two on the second.
	//
	// What actually stops the wedge is `abandoned` — see FrameChan.Abandon.
	// Nothing consuming is a reachable state rather than a hypothetical:
	// these forwarders run on reqCtx (WithoutCancel) and keep pulling from
	// the agent, while Start can return between creating them and spawning
	// the consumers. With the fix in place Abandon reaches them FIRST --
	// Start registers its abandon defer after the stop defer, so LIFO runs
	// it before reqCtxCancel(). Before the fix that cancellation was what
	// drove all three into the handover, with nobody left to receive it.
	incomingChan := make(chan *models.TestCase, 1)
	outgoingChan := make(chan *models.Mock, 1)
	mappingChan := make(chan models.TestMockMapping)

	// Closed by FrameChan.Abandon. Idempotent via sync.Once because Start
	// defers it unconditionally and the error paths below return a FrameChan
	// that carries it too.
	abandoned := make(chan struct{})
	var abandonOnce sync.Once
	abandon := func() { abandonOnce.Do(func() { close(abandoned) }) }

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return FrameChan{}, fmt.Errorf("failed to get error group from context")
	}

	// Both frame streams stay open through the stop until what the agent held
	// at it is through (forwardUntilDrained), so they are opened on contexts
	// that only their forwarders end. framesDrained closes when both have
	// ended: the mapping drain waits for it (see MAPPINGS).
	framesDrained := make(chan struct{})
	// Under docker compose the app runs on while the agent drains (Start's
	// stop): what it serves after the stop is not recorded. postStopTests
	// names the test cases left out for it, and recordedTests those handed to
	// the consumer, so that the mappings of the rest are left out too. Both
	// are kept only under compose: recordedTests holds a name (tens of bytes)
	// per recorded test case for the whole recording, the size of the test
	// set's own index.
	appRunsOn := r.config.CommandType == string(utils.DockerCompose)
	var postStopTests, recordedTests sync.Map
	var framesLeft atomic.Int32
	framesLeft.Store(2)
	frameDrained := func() {
		if framesLeft.Add(-1) == 0 {
			close(framesDrained)
		}
	}

	// INCOMING
	tcCtx, tcCancel := context.WithCancel(context.WithoutCancel(ctx))
	incomingStream, err := r.instrumentation.GetIncoming(tcCtx, incomingOpts)
	if err != nil {
		tcCancel()
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting incoming test cases")
			/*
			 * ALL THREE CLOSED, and mappingChan is the one that was
			 * missed.
			 *
			 * These early returns hand back a FrameChan the caller ranges
			 * over. A channel that is never closed and never written is
			 * the same as no channel at all: `for m := range mappings`
			 * blocks forever with no ctx escape. Start spawns
			 * consumeMappings unconditionally, so it parked here, and
			 * teardown waited out the whole DrainErrGroup budget before
			 * giving up on it.
			 *
			 * NOT A SHUTDOWN-ONLY PATH, which is what made it worth
			 * fixing rather than documenting: utils.IsShutdownError
			 * matches "connection refused", so an agent socket that is
			 * merely not up yet lands here with ctx perfectly live.
			 * MEASURED on that shape: Start returned in 30.0s and the
			 * mapping consumer plus its flush ticker leaked for the
			 * process lifetime.
			 */
			close(incomingChan)
			close(outgoingChan)
			close(mappingChan)
			return FrameChan{
				Incoming: incomingChan,
				Outgoing: outgoingChan,
				Mappings: mappingChan,
				Abandon:  abandon,
			}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get incoming test cases: %w", err)
	}

	ends.join(models.TestCaseStream)
	g.Go(func() error {
		defer close(incomingChan)
		defer frameDrained()
		defer tcCancel()
		opts := drainOpts[*models.TestCase]{
			capturedAt: testCaseCapturedAt,
			dropped: func(tc *models.TestCase) {
				r.logger.Warn("dropped a test case: recording stopped before any consumer started, so there is nothing left to persist it",
					zap.String("testCaseName", tc.Name),
					zap.String("next_step", "this test case was captured but not written; re-record the test set if you need it"))
			},
			closeStream: tcCancel,
			kind:        "test cases",
			stream:      models.TestCaseStream,
			ends:        ends,
		}
		if appRunsOn {
			// A test case the app served after the stop is not part of the
			// recording: its mocks may come after the drain has ended, and
			// saved without them it would fail replay. An undated one
			// (testCaseCapturedAt) cannot be told from one, and is left out
			// too. Neither is its mapping (MAPPINGS).
			opts.afterStop = func(tc *models.TestCase) {
				postStopTests.Store(tc.Name, struct{}{})
				r.logger.Debug("not recording a test case served after the stop", zap.String("testCaseName", tc.Name))
			}
			// Keyed by the name the test case is handed over with: the
			// agent's, which its mappings carry. Read before the hand-off,
			// since the consumer's insert names an unnamed test case in place.
			opts.handing = func(tc *models.TestCase) func() {
				name := tc.Name
				return func() { recordedTests.Store(name, struct{}{}) }
			}
		}
		forwardUntilDrained(ctx, r, incomingStream, incomingChan, abandoned, opts)
		return nil
	})

	/*
	 * FROM HERE ON A FORWARDER IS ALREADY RUNNING, so no error return may
	 * leave without telling it.
	 *
	 * The incoming forwarder is spawned just above, on reqCtx
	 * (WithoutCancel). Every `return FrameChan{}, err` below therefore
	 * hands the caller a ZERO FrameChan -- nil Abandon -- and Start bails
	 * at its `if err != nil` before it can register its own abandon
	 * defer. The forwarder is then unabandonable: it takes an item, parks
	 * on the hand-over, and nothing can release it.
	 *
	 * MEASURED on the non-shutdown GetOutgoing error with ctx live:
	 * Start returned in 30.028s -- the full DrainErrGroup budget -- with
	 * the goroutine leaked for the process lifetime. Byte-for-byte the
	 * SEV-1 the rest of this file's guards exist to remove, behind a
	 * different door.
	 *
	 * A DEFER WITH A FLAG rather than a call beside each return, for the
	 * same reason Start uses one: there are two such returns today, the
	 * failure mode of missing one is a hang rather than a compile error,
	 * and a third added later would reintroduce it silently.
	 */
	handedOff := false
	defer func() {
		if !handedOff {
			abandon()
		}
	}()

	// OUTGOING
	// Create a cancelable child that we always cancel when ctx is done.
	mockCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var tlsPrivateKey string
	if r.config.Record.TLSPrivateKeyPath != "" {
		keyBytes, err := os.ReadFile(r.config.Record.TLSPrivateKeyPath)
		if err != nil {
			r.logger.Error("failed to read tls private key", zap.Error(err))
			cancel()
			return FrameChan{}, err
		}
		tlsPrivateKey = string(keyBytes)
	}

	// Under the proven switch the agent is handed the run's mock.real first
	// (design v14 §4): its destinations are reached for real, never
	// recorded, and each test case lists those its request used.
	if err := realclient.Push(mockCtx, r.logger, r.instrumentation, r.mockReal()); err != nil {
		cancel()
		frameDrained() // the mapping forwarder is not started on these returns
		return FrameChan{}, fmt.Errorf("failed to hand keploy's agent mock.real: %w", err)
	}

	outgoingStream, err := r.instrumentation.GetOutgoing(mockCtx, models.OutgoingOptions{
		Rules:                     r.config.BypassRules,
		MongoPassword:             r.config.Test.MongoPassword,
		TLSPrivateKey:             tlsPrivateKey,
		CapturePackets:            r.config.Record.CapturePackets,
		OpportunisticTLSIntercept: r.config.Record.OpportunisticTLSIntercept,
		// Only the boolean travels on this request; the *x509.CertPool cannot
		// (see models.OutgoingOptions.UpstreamTLSRootCAs). The agent resolves
		// record.upstreamTls.caCert on its own filesystem — the path precedence
		// at proxy.New, the pool itself lazily on the first record session —
		// and stamps it onto these options in Proxy.Record.
		UpstreamTLSVerify:         r.config.Record.UpstreamTLS.Verify,
		MysqlPorts:                r.config.MysqlPorts,
		DisableMysqlAutoDetect:    r.config.DisableMysqlAutoDetect,
		DisableMysqlEndpointDrift: r.config.DisableMysqlEndpointDrift,
		PassThroughPorts:          r.config.Record.PassThroughPorts,
		PassThroughHosts:          r.config.Record.PassThroughHosts,
		// Advertise that this CLI understands the reserved Kind=RevokedTests
		// control frame: it diverts such a frame into a revoke set and deletes
		// those deferred-orphan test cases at finalize instead of persisting it
		// as a mock. The agent emits revoke frames ONLY when this is true.
		SupportsDroppedRevoke: true,
	})
	if err != nil {

		cancel()
		frameDrained() // the mapping forwarder is not started on these returns
		if ctx.Err() != nil || utils.IsShutdownError(err) {
			r.logger.Debug("Context cancelled or shutdown error while getting outgoing mocks")
			// Close outgoingChan to prevent callers from hanging.
			// incomingChan is closed by the forwarder started above when
			// ctx is done. mappingChan has no producer on this path at
			// all -- see the note on the incoming early return for what
			// ranging over it costs.
			close(outgoingChan)
			close(mappingChan)
			handedOff = true
			return FrameChan{
				Incoming: incomingChan,
				Outgoing: outgoingChan,
				Mappings: mappingChan,
				Abandon:  abandon,
			}, nil
		}
		return FrameChan{}, fmt.Errorf("failed to get outgoing mocks: %w", err)
	}
	ends.join(models.MockStream)
	g.Go(func() error {
		defer close(outgoingChan)
		defer frameDrained()
		defer cancel()
		opts := drainOpts[*models.Mock]{
			capturedAt: func(m *models.Mock) time.Time { return m.Spec.ReqTimestampMock },
			dropped: func(m *models.Mock) {
				r.logger.Warn("dropped a mock: recording stopped before any consumer started, so there is nothing left to persist it",
					zap.String("mockName", m.Name),
					zap.String("next_step", "this mock was captured but not written; re-record the test set if you need it"))
			},
			closeStream: cancel,
			kind:        "mocks",
			stream:      models.MockStream,
			ends:        ends,
		}
		if appRunsOn {
			// Traffic the app serves after the stop is not recorded: a mock
			// requested after it belongs to a test case that is not (a recorded
			// one ended before the stop, and so did its mocks). An undated
			// mock is kept: at worst it is a mock no test case uses.
			opts.afterStop = func(*models.Mock) {}
			opts.keepUndated = true
		}
		forwardUntilDrained(ctx, r, outgoingStream, outgoingChan, abandoned, opts)
		return nil
	})

	// MAPPINGS
	g.Go(func() error {
		defer close(mappingChan)

		// A mapping is the LAST artifact a recorded test produces: it is derived,
		// and the agent can only emit it once that test's mock range has resolved.
		// So when recording stops, a burst for the most recently recorded tests is
		// still queued inside the agent. Tearing the stream down the instant ctx was
		// done dropped that burst SILENTLY — the entries never reach the consumer, so
		// not even a write error is logged. mappings.yaml then omitted the tail of
		// every endpoint, and replay reported "no_mocks" (candidates: 0) for exactly
		// those tests, because resolveMockSets picks mapping-based selection per test
		// SET and has no per-test fallback for a test that is missing.
		//
		// It is tempting to assume the tail has already been flushed by now because
		// teardown stops the app first. That is only true for the NATIVE path: under
		// docker-compose the agent runs in the app's stack, so teardown drains the
		// frame streams BEFORE it takes the stack down (see Start's stop defer), and
		// reqCtxCancel therefore fires within milliseconds of SIGINT — with the
		// agent's queue still full. Docker-compose is what most e2e lanes and many
		// users run, and it is where this bug was measured: 27 of 342 tests lost, all
		// tails.
		//
		// So on shutdown keep the stream open and keep draining, for as long as
		// mappings keep coming: the stream ends when the agent closes it, or once
		// it has delivered nothing for mappingIdleGrace. Time spent handing a
		// mapping to a consumer that is still persisting is not quiet: a sink
		// still draining is never cut off.
		//
		// The tail is the mappings of the tests whose mocks were queued at the
		// stop, and the agent emits a test's mapping only once its mocks are in.
		// So the quiet bound does not start until the test-case and mock drains
		// have ended (framesDrained): those last as long as the agent keeps
		// delivering what it held at the stop, and a quiet mapping stream
		// meanwhile means the agent is still busy with mocks, not that the tail
		// is through.
		mapCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()

		// lastMapping is when the stream last delivered a mapping, or a
		// hand-over to the consumer ended (UnixNano); handing is set while one
		// is under way.
		var lastMapping atomic.Int64
		var handing atomic.Bool
		drained := make(chan struct{})
		defer close(drained)
		// markerDecided carries, once the frame drains have ended, whether
		// the stream ends at the agent's marker (streamsEnd) or on quiet.
		markerDecided := make(chan bool, 1)
		ender, _ := r.instrumentation.(streamEnder)
		go func() {
			// The bound for a stream that is not being read below: an older
			// agent, which establishes its mapping stream only with the first
			// mapping, leaves GetMappings blocked in its request.
			// The loop below applies the same bound to a stream it reads.
			select {
			case <-drained:
				return // finished before shutdown — nothing to wait for
			case <-ctx.Done():
			}
			select {
			case <-drained:
				return
			case <-framesDrained:
			}
			// An agent that took the request to end its streams ends this one
			// at its marker, once every mapping it owed from before the stop
			// is on it (its "clear" counted them): no quiet decides it.
			byMarker := ends.endsByMarker(drained)
			markerDecided <- byMarker
			if byMarker {
				return
			}
			lastMapping.CompareAndSwap(0, time.Now().UnixNano())
			quiet := time.NewTimer(mappingIdleGrace)
			defer quiet.Stop()
			for {
				select {
				case <-drained:
					return
				case <-quiet.C:
				}
				idle := time.Since(time.Unix(0, max(lastMapping.Load(), 0)))
				if !handing.Load() && idle >= mappingIdleGrace {
					r.logger.Debug("mapping stream quiet after the stop; ending its drain", zap.Duration("quiet", idle))
					cancel()
					return
				}
				quiet.Reset(max(mappingIdleGrace-idle, mappingIdleGrace/10))
			}
		}()

		// Call the new AgentClient method
		ch, err := r.instrumentation.GetMappings(mapCtx, incomingOpts)
		if err != nil {
			// An agent with no mapping stream (models.ErrNoMappingStream,
			// said by its client): the recording goes on without mappings.
			if ctx.Err() != nil || utils.IsShutdownError(err) || errors.Is(err, models.ErrNoMappingStream) {
				return nil
			}
			return fmt.Errorf("failed to get mappings: %w", err)
		}

		// idle is armed only once shutdown has begun and the test-case and mock
		// drains have ended. While recording is live, gaps between mappings are
		// normal and must never end the stream; once the tail's mocks are in, a gap
		// long enough means the agent has nothing left to flush (it holds the
		// stream open for the whole session, so it will not close it).
		idle := time.NewTimer(mappingIdleGrace)
		defer idle.Stop()
		stopTimer(idle)

		// lastSeen is when the stream last delivered, or shutdown began: the
		// idle bound runs from it, so a stream that went quiet while the frame
		// drains were still ending is not waited on again for a whole grace.
		framesDone := false
		// decided and byMarker: whether the stream ends at the agent's
		// marker, once that is known (markerDecided).
		decided, byMarker := false, false
		var lastSeen time.Time
		for {
			// While recording is live, wake on ctx.Done(); once shutdown began,
			// on the frame drains ending; once they have, on how the stream
			// ends: at the agent's marker (and a second interrupt), or on the
			// idle timer. Arming an already-closed channel would spin the loop.
			var idleC <-chan time.Time
			var shutdownC, framesC, againC <-chan struct{}
			var decidedC <-chan bool
			if ctx.Err() != nil && lastSeen.IsZero() {
				lastSeen = time.Now() // shutdown began before this loop saw it
			}
			switch {
			case ctx.Err() == nil:
				shutdownC = ctx.Done()
			case !framesDone:
				framesC = framesDrained
			case !decided:
				decidedC = markerDecided
			case byMarker:
				againC = utils.StopNow()
			default:
				rest := mappingIdleGrace - time.Since(lastSeen)
				if rest <= 0 {
					return nil // quiet for the whole grace already: the tail is through
				}
				resetTimer(idle, rest)
				idleC = idle.C
			}

			select {
			case <-shutdownC:
				// Shutdown began: re-loop to wait for the frame drains.
				continue
			case <-framesC:
				// The mocks queued at the stop are in: re-loop to learn how
				// the stream ends, and to arm the idle timer, for a whole
				// grace from here, since the agent emits a test's mapping only
				// after its mocks, if it ends on quiet.
				framesDone = true
				lastSeen = time.Now()
				continue
			case d := <-decidedC:
				decided, byMarker = true, d
				continue
			case <-againC:
				// The user's second interrupt gives the drain up: what the
				// frame drains said (they gave up too).
				cancel()
				return nil
			case <-abandoned:
				/*
				 * NOBODY IS COMING, so there is nothing to drain FOR.
				 *
				 * `abandoned` was wired only into the two SEND selects
				 * below, never here -- so an abandoned mapping forwarder
				 * ignored it and sat out the full mappingIdleGrace before
				 * noticing the stream had gone quiet. MEASURED: a fixed
				 * 3s added to every abandoned teardown, inside the
				 * record-req drain, and 8 of the wedge table's 10
				 * subtests paying it at 3s each.
				 *
				 * Returning here drops nothing a consumer could have
				 * received: an item already taken off the stream is
				 * handled by the hand-over below, which logs what it
				 * drops. Items still queued on `ch` ARE dropped, and
				 * without a log line -- where the pre-fix path logged
				 * one. That is the right trade only because this arm is
				 * reached when the consumers are already gone, so no
				 * reader exists for them; it is not "nothing".
				 */
				return nil
			case <-mapCtx.Done():
				// Hard cap reached, or the stream was torn down.
				return nil
			case <-idleC:
				// Draining and nothing for mappingIdleGrace: the tail is through.
				return nil
			case m, ok := <-ch:
				if !ok && ender != nil && ender.StreamEnded(models.MappingStream) {
					// The agent ended it at its marker, every mapping it owed
					// from before the stop on it.
					r.logger.Debug(StreamEndedLog, zap.String("frames", "mappings"))
					return nil
				}
				if !ok {
					// Not ended here (mapCtx): the agent ended it, without
					// its marker, before this recording was done with it
					// (whenever that came: see forwardUntilDrained), and the
					// mappings it still held are lost.
					if mapCtx.Err() == nil {
						r.noteEndCut("keploy's agent ended its mapping stream before the recording had saved its mappings, so those it still held are not in the recording")
						r.logger.Warn("the keploy agent ended its mapping stream before the recording had saved its mappings: those it still held, if any, are lost",
							zap.String("next_step", "the agent was stopped before the recording had saved its mappings (docker compose stops it with the project, also when the app exits on its own; kubelet with its pod): stop the recording, not the app or the agent. A test left without its mapping replays with no mocks"))
					}
					return nil
				}
				if appRunsOn && ctx.Err() != nil {
					// A mapping of a test case served after the stop is not
					// recorded. Once the test-case drain is over, every test
					// case recorded has been handed on, so a mapping of any
					// other one is left out.
					_, after := postStopTests.Load(m.TestName)
					_, recorded := recordedTests.Load(m.TestName)
					if after || (framesDone && !recorded) {
						continue
					}
				}
				lastSeen = time.Now()
				lastMapping.Store(lastSeen.UnixNano())
				handing.Store(true)
				handed := func() {
					// The hand-over is not quiet: a consumer still persisting
					// holds it, and the quiet bound runs from its end.
					lastSeen = time.Now()
					lastMapping.Store(lastSeen.UnixNano())
					handing.Store(false)
				}
				select {
				case <-mapCtx.Done():
					// Hand off the mapping we already took off the stream before
					// unwinding — cancellation is not a licence to drop data we are
					// holding. Both cases are ready once mapCtx is done, so the
					// runtime picks at random and this would otherwise lose the last
					// in-flight mapping about half the time. Mirrors the outgoing
					// producer above.
					//
					// THE CONSUMER ONLY RANGES IF Start GOT FAR ENOUGH TO
					// SPAWN IT, which is not something this goroutine can
					// assume. The channel is UNBUFFERED, so with no consumer
					// this send wedged on the very FIRST mapping — measured.
					/*
					 * THIS ARM IS DEFENSIVE, AND NOT COVERED BY A TEST --
					 * said plainly rather than left for the next reviewer
					 * to measure.
					 *
					 * Reaching it needs mapCtx cancelled while `abandoned`
					 * is still open, with an item in hand. mapCtx is
					 * cancelled only once the stream has been quiet for
					 * mappingIdleGrace after the frame drains ended, and not
					 * during a hand-over, while Abandon fires the moment
					 * Start returns -- which on the no-consumer path (the
					 * agent's refusal of this keploy's proven switch) is
					 * immediately. So the outer arm below wins
					 * every time, and the wedge table cannot drive this one
					 * without a state the current ordering cannot produce.
					 *
					 * Kept because "the ordering makes it unreachable" is
					 * a property of two other functions, not of this one,
					 * and the cost of being wrong about that is a
					 * permanent goroutine leak. MEASURED: deleting it
					 * leaves the suite green.
					 */
					select {
					case mappingChan <- m:
					case <-abandoned:
						r.logger.Warn("dropped an in-flight test-mock mapping on shutdown: recording stopped before any consumer started",
							zap.String("testName", m.TestName),
							zap.String("next_step", "mappings.yaml may be missing this test; re-record the test set if you need it"))
					}
					handed()
					return ctx.Err()
				case mappingChan <- m:
					handed()
				case <-abandoned:
					// See the incoming forwarder. This one matters most: mapCtx
					// is not cancelled while mappings keep coming, so without
					// this arm an abandoned mapping forwarder holds the
					// errgroup for as long as the agent sends.
					r.logger.Warn("dropped a test-mock mapping: recording stopped before any consumer started",
						zap.String("testName", m.TestName),
						zap.String("next_step", "mappings.yaml may be missing this test; re-record the test set if you need it"))
					return nil
				}
			}
		}
	})

	handedOff = true
	return FrameChan{
		Incoming: incomingChan,
		Outgoing: outgoingChan,
		Mappings: mappingChan,
		Abandon:  abandon,
	}, nil

}

func (r *Recorder) RunApplication(ctx context.Context, appID uint64, opts models.RunOptions) models.AppError {
	return r.instrumentation.Run(ctx, opts)
}

func (r *Recorder) GetNextTestSetID(ctx context.Context) (string, error) {
	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get test set IDs: %w", err)
	}

	if r.config.Record.Metadata == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}
	r.config.Record.Metadata = utils.TrimSpaces(r.config.Record.Metadata)
	meta, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil || meta == nil {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	nameVal, ok := meta["name"]
	requestedName, isStr := nameVal.(string)
	if !ok || !isStr || requestedName == "" {
		return pkg.NextID(testSetIDs, models.TestSetPattern), nil
	}

	existingIDs := make(map[string]struct{}, len(testSetIDs))
	for _, id := range testSetIDs {
		existingIDs[id] = struct{}{}
	}

	if _, occupied := existingIDs[requestedName]; !occupied {
		return requestedName, nil
	}

	var highestSuffix int
	namePrefix := requestedName + "-"
	for id := range existingIDs {
		if !strings.HasPrefix(id, namePrefix) {
			continue
		}
		suffixPart := id[len(namePrefix):]
		if n, err := strconv.Atoi(suffixPart); err == nil && n > highestSuffix {
			highestSuffix = n
		}
	}

	newSuffix := highestSuffix + 1
	assignedName := fmt.Sprintf("%s-%d", requestedName, newSuffix)

	r.logger.Info(fmt.Sprintf(
		"Test set name '%s' already exists, using '%s' instead. You can change this name if you want.",
		requestedName, assignedName,
	))

	return assignedName, nil
}

func (r *Recorder) createConfigWithMetadata(ctx context.Context, testSetID string) {
	// Parse metadata from the config
	metadata, err := utils.ParseMetadata(r.config.Record.Metadata)
	if err != nil {
		utils.LogError(r.logger, err, "failed to parse metadata", zap.String("metadata", r.config.Record.Metadata))
		return
	}
	testSet := &models.TestSet{
		PreScript:  "",
		PostScript: "",
		Template:   make(map[string]interface{}),
		Metadata:   metadata,
	}

	err = r.testSetConf.Write(ctx, testSetID, testSet)
	if err != nil {
		utils.LogError(r.logger, err, "Failed to create test-set config file with metadata", zap.String("testSet", testSetID))
		return
	}

	r.logger.Info("Created test-set config file with metadata")
}
