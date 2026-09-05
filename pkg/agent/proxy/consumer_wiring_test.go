package proxy

import (
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The consumer contract is reached from ANOTHER REPOSITORY, through two seams
// only: a parser finds the Gate and the Recorder on its context, and whatever
// implements replay.ConsumerInstrumentation finds the same Gate through
// ConsumerGate(). Neither seam had a non-test caller before these lines
// existed, which made "deliberately inert because OSS has no parser" quietly
// become "inert because the wiring does not exist either" — an enabling tag
// that cannot be reached from the repository it is meant to enable.

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	cfg := &config.Config{}
	return New(zap.NewNop(), nil, cfg)
}

// The gate is allocated by New, is default-closed, and round-trips through
// the context carriage the parser branches use.
func TestProxyOwnsADefaultClosedConsumerGate(t *testing.T) {
	p := newTestProxy(t)

	g := p.ConsumerGate()
	if g == nil {
		t.Fatal("a proxy built by New must own a gate; a nil one makes consumer.WithGate a no-op and every parser lookup return nil")
	}
	if g.Phase() != consumer.PhaseBoot {
		t.Fatalf("phase %q at construction, want boot: arming a test's mocks is necessary but NOT sufficient for injection", g.Phase())
	}

	// What a parser sees: the SAME instance whatever implements
	// ConsumerInstrumentation will arm.
	ctx := consumer.WithGate(context.Background(), p.ConsumerGate())
	if got := consumer.GateFromContext(ctx); got != g {
		t.Fatal("the gate on the parser context must be the one ConsumerGate() hands out, or arming opens a window no parser can see")
	}

	// Default-closed: with nothing armed, delivery is refused whatever a
	// parser does.
	err := g.Deliver(context.Background(), consumerfake.Protocol, nil)
	if err == nil {
		t.Fatal("an unarmed gate must refuse delivery")
	}
}

// The recorder is nil until a test-case sink is installed, and installing one
// is what cli/provider wires. A nil recorder makes every installation a no-op,
// which is the right behaviour for an embedder with nowhere to put a minted
// test case.
func TestConsumerRecorderLifecycle(t *testing.T) {
	p := newTestProxy(t)
	ctx := context.Background()

	if p.ConsumerRecorder() != nil {
		t.Fatal("no recording session and no sink: there must be no recorder")
	}
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if p.ConsumerRecorder() != nil {
		t.Fatal("without a test-case sink a recorder would resolve windows for tests that can never reach disk")
	}
	// A nil recorder must leave the context untouched, so an HTTP-only run is
	// byte-identical.
	if consumer.WithRecorder(ctx, p.ConsumerRecorder()) != ctx {
		t.Fatal("installing a nil recorder must return the context unchanged")
	}

	tc := make(chan *models.TestCase, 8)
	p.SetConsumerTestCases(tc)
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec := p.ConsumerRecorder()
	if rec == nil {
		t.Fatal("a recording session with a sink must have a recorder for the record-branch parser context")
	}
	if got := consumer.RecorderFromContext(consumer.WithRecorder(ctx, rec)); got != rec {
		t.Fatal("the recorder on the parser context must be the session's own")
	}

	// A SECOND RECORDING SESSION GETS A FRESH RECORDER. A recorder owns
	// per-session state — the open unit, its dedup-queue job, the
	// reconciliation counters — and carrying that across would attribute one
	// recording's trailing effects to the next one's first test.
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if p.ConsumerRecorder() == rec {
		t.Fatal("a new recording session must not inherit the previous session's recorder")
	}

	// Wind-down closes it, which is what mints the recording's LAST unit.
	if err := p.SetGracefulShutdown(ctx); err != nil {
		t.Fatalf("SetGracefulShutdown: %v", err)
	}
	if p.ConsumerRecorder() != nil {
		t.Fatal("wind-down must close the recorder, not leave it open across the next session")
	}

	// Entering replay ends any recording session outright.
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Mock() itself may fail late (it writes /etc/nsswitch.conf, which an
	// unprivileged test cannot do); the recorder is closed before that, which
	// is the part under test here.
	_ = p.Mock(ctx, models.OutgoingOptions{})
	if p.ConsumerRecorder() != nil {
		t.Fatal("a record-mode recorder must not survive into a replay")
	}
}

// A recorder driven through the proxy's own seam mints a test case onto the
// channel cli/provider installs — the same channel the HTTP ingress pushes
// onto, so a consumer test reaches persistence through the unchanged path.
func TestARecorderInstalledOnTheProxySeamMintsOntoTheIngressChannel(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()

	p := newTestProxy(t)
	tcChan := make(chan *models.TestCase, 8)
	p.SetConsumerTestCases(tcChan)
	ctx := context.Background()
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// This is exactly what a parser does: pull the recorder off its context
	// and hand it the mocks it just built.
	rec := consumer.RecorderFromContext(consumer.WithRecorder(ctx, p.ConsumerRecorder()))
	if rec == nil {
		t.Fatal("no recorder on the record-branch context")
	}

	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "effect-1", Role: models.RoleEffect, ConnID: "c-2",
		ReqAt: base.Add(10 * time.Millisecond), ResAt: base.Add(20 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)},
	}))

	// Wind-down mints the last open unit.
	if err := p.SetGracefulShutdown(ctx); err != nil {
		t.Fatalf("SetGracefulShutdown: %v", err)
	}

	select {
	case tc := <-tcChan:
		if tc.Kind != models.CONSUMER {
			t.Fatalf("kind %q", tc.Kind)
		}
		if tc.ConsumerSpec == nil || len(tc.ConsumerSpec.Effects) != 1 {
			t.Fatalf("spec %+v", tc.ConsumerSpec)
		}
	default:
		t.Fatal("the recording's last unit was never minted onto the ingress channel")
	}
}

// CLOSING THE RECORDER MINTS ITS LAST OPEN UNIT, and both callers run on a
// path whose context is about to be — or has just been — cancelled: wind-down,
// and the start of the next session. Passing the live context through would
// race the mint against the very signal that triggered it, and
// Recorder.closeUnit counts a cancelled hand-off as a refusal by name — so the
// last message of every recording would be reported as CONSUMER_UNITS_LOST
// instead of persisted.
func TestWindDownStillMintsTheLastUnitWhenTheContextIsAlreadyCancelled(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()

	p := newTestProxy(t)
	tcChan := make(chan *models.TestCase, 8)
	p.SetConsumerTestCases(tcChan)

	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec := p.ConsumerRecorder()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "effect-1", Role: models.RoleEffect, ConnID: "c-2",
		ReqAt: base.Add(10 * time.Millisecond), ResAt: base.Add(20 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)},
	}))

	// The shutdown signal arrives BEFORE the recorder is closed, which is the
	// real ordering: utils/ctx.go cancels on SIGTERM and the HTTP handler then
	// calls SetGracefulShutdown.
	cancel()
	if err := p.SetGracefulShutdown(ctx); err != nil {
		t.Fatalf("SetGracefulShutdown: %v", err)
	}

	select {
	case tc := <-tcChan:
		if tc.Kind != models.CONSUMER || tc.ConsumerSpec == nil || len(tc.ConsumerSpec.Effects) != 1 {
			t.Fatalf("minted the wrong thing: %+v", tc)
		}
	default:
		t.Fatal("the last unit was dropped because the shutdown context was already cancelled")
	}
}

// detachedRecorderCtx IS PINNED DIRECTLY BECAUSE THE INTEGRATION TEST ABOVE
// CAN NO LONGER SEE IT.
//
// closeUnit now tries a NON-BLOCKING send before falling into the blocking
// select — that is what removed the ctx.Done()/send coin flip that made
// CONSUMER_UNITS_LOST fire at random on a Ctrl-C. The side effect is that with
// room on the ingress channel the hand-off succeeds whatever the context says,
// so `detachedRecorderCtx` could be replaced with `return ctx` and
// TestWindDownStillMintsTheLastUnitWhenTheContextIsAlreadyCancelled stayed
// green — the function that exists to protect the last unit of every recording
// became untested by the fix that made its test deterministic.
//
// It still matters, and the test below this one shows where: when persistence
// is back-pressured the blocking select IS reached, and a live cancelled
// context loses the unit there.
func TestDetachedRecorderCtxKeepsTheValuesAndDropsTheCancellation(t *testing.T) {
	type key struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))
	cancel()

	d := detachedRecorderCtx(ctx)
	select {
	case <-d.Done():
		t.Fatal("detachedRecorderCtx returned a context that is already done; closing a recorder MINTS its last open unit, and both callers run on a path whose context has just been cancelled")
	default:
	}
	if d.Err() != nil {
		t.Fatalf("Err() = %v, want nil", d.Err())
	}
	if got := d.Value(key{}); got != "kept" {
		t.Fatalf("the detached context lost its values: %v", got)
	}
	if got := detachedRecorderCtx(nil); got == nil {
		t.Fatal("a nil context must be tolerated: this is reached from HTTP handlers and embedder code")
	}
}

// THE CASE WHERE THE DETACHED CONTEXT IS STILL THE ONLY THING SAVING THE UNIT.
//
// closeUnit's non-blocking send wins whenever the ingress channel has room. It
// does not when persistence is back-pressured — a full channel is the ordinary
// state of a recording whose writer is slower than its worker — and then the
// blocking select is entered with both a cancelled context and a full channel.
// With the live context that select has exactly one ready case, ctx.Done(), so
// the unit is refused CONSUMER_UNITS_LOST deterministically; with the detached
// one it waits for the writer and the unit is persisted.
//
// The channel is deliberately left FULL across the whole shutdown call: that
// makes the assertion "SetGracefulShutdown has not returned" a fact about the
// blocking send rather than a race with a drainer.
func TestABackPressuredIngressChannelDoesNotLoseTheLastUnit(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()

	p := newTestProxy(t)
	tcChan := make(chan *models.TestCase, 1)
	tcChan <- &models.TestCase{Name: "already-queued"} // the writer is behind
	p.SetConsumerTestCases(tcChan)

	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec := p.ConsumerRecorder()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "effect-1", Role: models.RoleEffect, ConnID: "c-2",
		ReqAt: base.Add(10 * time.Millisecond), ResAt: base.Add(20 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)},
	}))

	cancel()
	returned := make(chan error, 1)
	go func() { returned <- p.SetGracefulShutdown(ctx) }()

	// The channel is full, so the mint MUST still be blocked on the hand-off.
	// A shutdown that has already returned gave the unit up.
	select {
	case err := <-returned:
		t.Fatalf("SetGracefulShutdown returned (%v) while the ingress channel was still full: the last unit was dropped instead of waiting for persistence", err)
	case <-time.After(200 * time.Millisecond):
	}

	// The writer catches up.
	if first := <-tcChan; first.Name != "already-queued" {
		t.Fatalf("drained the wrong test case first: %q", first.Name)
	}
	select {
	case tc := <-tcChan:
		if tc.Kind != models.CONSUMER || tc.ConsumerSpec == nil || len(tc.ConsumerSpec.Effects) != 1 {
			t.Fatalf("minted the wrong thing: %+v", tc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the last unit never reached persistence")
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("SetGracefulShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetGracefulShutdown never returned after the channel drained")
	}
	if got := p.ConsumerRecordingReport(); got.UnitsPersisted != 1 || got.Degraded() {
		t.Fatalf("the reconciliation must record the unit as persisted, got %+v", got)
	}
}

// THE PROXY PUBLISHES THE RECONCILIATION, which is what lets `keploy record`
// FAIL a degraded recording instead of exiting 0.
//
// reportConsumerRecording used to only call utils.LogError, i.e. write one zap
// line into the AGENT's log — a different process from the record command in
// the ordinary CLI deployment — so nothing could ever move an exit code.
// Gutting the whole function left `go test ./pkg/...` green.
func TestADegradedConsumerRecordingIsPublishedForTheRecordCommand(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()

	p := newTestProxy(t)
	p.SetConsumerTestCases(make(chan *models.TestCase, 8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := p.ConsumerRecordingReport(); got.Observed() {
		t.Fatalf("a session that has just started has nothing to report: %+v", got)
	}

	// The worker produced while no unit was open: those records belong to no
	// test, so replay has nothing to compare them against.
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	p.ConsumerRecorder().OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "effect-1", Role: models.RoleEffect, ConnID: "c-2",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("produce", "order-events", "o-1", `{"a":1}`)},
	}))

	if err := p.SetGracefulShutdown(ctx); err != nil {
		t.Fatalf("SetGracefulShutdown: %v", err)
	}

	report := p.ConsumerRecordingReport()
	if !report.Observed() {
		t.Fatalf("the reconciliation was never published: %+v", report)
	}
	if !report.Degraded() {
		t.Fatalf("a recording with orphan effect records must not be replayed as-is: %+v", report)
	}
	if report.OrphanEffects != 1 {
		t.Fatalf("orphan effect records = %d, want 1", report.OrphanEffects)
	}
}

// AND THE DETACHED CONTEXT IS WHAT COVERS BACK-PRESSURE.
//
// closeUnit tries a non-blocking hand-off first, so a cancelled context can
// never lose a unit the channel could take (the recorder package pins that
// with twenty units). What is left for the detached context is the case the
// non-blocking send cannot solve: the test-case channel is FULL at the moment
// of shutdown — the recorder's reader is draining, or the persistence
// goroutine is behind — so the hand-off has to WAIT. With the live shutdown
// context that wait is decided by ctx.Done() and the last unit of the
// recording is refused CONSUMER_UNITS_LOST; detached, it waits for the reader
// and is persisted.
//
// The test fills the channel, starts the shutdown, waits until the recorder
// has reached its hand-off (UnitsPersisted is incremented immediately before
// it), and only then drains one slot.
func TestAFullChannelAtShutdownWaitsForTheReaderRatherThanLosingTheUnit(t *testing.T) {
	unregister := consumerfake.Register()
	defer unregister()

	p := newTestProxy(t)
	tcChan := make(chan *models.TestCase, 1)
	tcChan <- &models.TestCase{Kind: models.HTTP, Name: "already-queued"}
	p.SetConsumerTestCases(tcChan)

	ctx, cancel := context.WithCancel(context.Background())
	if err := p.Record(ctx, make(chan *models.Mock, 1), models.OutgoingOptions{}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	rec := p.ConsumerRecorder()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "trigger-1", Role: models.RoleTrigger, ConnID: "c-1",
		ReqAt: base, ResAt: base.Add(5 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("fetch", "orders", "o-1", `{"orderId":"o-1"}`)},
	}))
	rec.OnMock(ctx, consumerfake.Mock(consumerfake.MockOptions{
		Name: "effect-1", Role: models.RoleEffect, ConnID: "c-2",
		ReqAt: base.Add(10 * time.Millisecond), ResAt: base.Add(20 * time.Millisecond),
		Views: []models.EffectView{consumerfake.View("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)},
	}))

	cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- p.SetGracefulShutdown(ctx) }()

	// Wait until closeUnit has counted the unit as persisted, which happens
	// immediately before the hand-off, then give it the moment it needs to
	// reach the send. Bounded: a hang here is a failure, not a delay.
	deadline := time.Now().Add(5 * time.Second)
	for rec.Stats().UnitsPersisted == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the recorder never reached its hand-off")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := <-tcChan; got.Name != "already-queued" {
		t.Fatalf("drained the wrong entry first: %+v", got)
	}
	select {
	case tc := <-tcChan:
		if tc.Kind != models.CONSUMER || tc.ConsumerSpec == nil {
			t.Fatalf("minted the wrong thing: %+v", tc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the last unit was dropped because the shutdown context was already cancelled and the channel was momentarily full")
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("SetGracefulShutdown: %v", err)
	}
	if lost := rec.Stats().UnitsLost(); lost != 0 {
		t.Fatalf("%d unit(s) reported lost; a unit that reached the reader is not lost", lost)
	}
}

// ---------------------------------------------------------------------------
// The wiring itself, read from the source.
//
// The behaviour above proves the seams work; it cannot prove handleConnection
// USES them, and handleConnection is not reachable from a unit test (it needs
// a live socket pair, a session, a destination and a matched parser). This
// pins the two installation lines the way depresult_wiring_test.go pins the
// dependency writer's call sites.
// ---------------------------------------------------------------------------

func TestParserContextsCarryTheConsumerSeams(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}

	var calls []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var sb strings.Builder
		if err := printer.Fprint(&sb, fset, call); err != nil {
			return true
		}
		calls = append(calls, sb.String())
		return true
	})

	want := []struct {
		expr string
		why  string
		// prefix pins only the head of the call. Use it where the invariant is
		// which CONTEXT a call is handed rather than its full argument list:
		// pinning the whole literal makes an unrelated upstream change to a
		// later argument look like a consumer regression, which is how a pin
		// gets deleted instead of fixed.
		prefix bool
	}{
		{
			expr: "consumer.WithGate(testCtx, p.ConsumerGate())",
			why: "without it a protocol parser in MODE_TEST calls consumer.GateFromContext and gets nil, " +
				"so it can neither take an armed trigger nor synthesize an empty poll response",
		},
		{
			expr: "consumer.WithRecorder(parserCtx, p.ConsumerRecorder())",
			why: "without it a protocol parser in MODE_RECORD calls consumer.RecorderFromContext and gets nil, " +
				"so no consumer unit is ever opened and mappings.yaml comes out empty for the whole recording. " +
				"It must be assigned back onto parserCtx BEFORE the generic/non-generic split, not onto a " +
				"branch-local recordCtx: in OSS http, mysql and generic all report IsV2, so a per-branch " +
				"install reaches the legacy path only — and a consumer parser is exactly the kind of parser " +
				"that is written against the V2 supervisor surface",
		},
		{
			expr:   "matchedParser.MockOutgoing(testCtx,",
			prefix: true,
			why:    "the parser must be handed the context the gate was installed on, not the one before it",
		},
		{
			expr:   "matchedParser.RecordOutgoing(parserCtx,",
			prefix: true,
			why:    "the parser must be handed the context the recorder was installed on, not the one before it",
		},
		{
			expr: "p.recordViaSupervisor(parserCtx, srcConn, dstConn, matchedParser, parserType, rule.MC, parserErrGrp, logger, clientConnID, destConnID, outgoingOpts)",
			why: "the V2 record path must receive the SAME parserCtx the recorder was installed on. Every " +
				"parser OSS ships takes this branch, so a recorder installed only on the legacy branch is " +
				"invisible to all of them",
		},
		{
			expr:   "p.recordViaSupervisor(parserCtx, srcConn, dstConn, genericParser,",
			prefix: true,
			why: "generic.IsV2() is unconditionally true, so THIS is the generic record path — the " +
				"genericParser.RecordOutgoing below it is reachable only on a build with GENERIC " +
				"unregistered. An unrecognised protocol's egress is still egress a consumer worker " +
				"can make while handling a message",
		},
		{
			expr: "p.installConsumerEgressObserver()",
			why: "ConsumerSpec.SideEffects counts calls of ANOTHER protocol family made while a unit is " +
				"open, and those mocks come from parsers that are not consumer-aware. Without this " +
				"registration on the syncMock manager the count is structurally always zero, and " +
				"Recorder.closeUnit then refuses every healthy consume-and-write recording as " +
				"CONSUMER_NO_OBSERVABLE_EFFECT",
		},
	}
	for _, w := range want {
		found := false
		for _, c := range calls {
			if c == w.expr || (w.prefix && strings.HasPrefix(c, w.expr)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("pkg/agent/proxy/proxy.go no longer contains the call %s\n  why it matters: %s", w.expr, w.why)
		}
	}
}
