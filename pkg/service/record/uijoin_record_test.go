package record

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// uiJoinConf is a TestSetConfig that remembers what was written and can
// be primed with what is already on disk.
type uiJoinConf struct {
	stored  *models.TestSet
	writes  int
	readErr error
	// writeErr fails the WRITE, which is the one arm whose only signal to
	// the user is a log line -- see the diagnostics test.
	writeErr error
}

func (c *uiJoinConf) Read(context.Context, string) (*models.TestSet, error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	if c.stored == nil {
		// The real Db.Read never fails for a missing file — it returns a
		// default — and recTestSetConf in record_test.go returns a nil
		// pointer with a nil error. Both shapes reach the producer.
		return nil, nil
	}
	cp := *c.stored
	return &cp, nil
}

/*
THE FAKE GAINS THE READ SHAPE THE REAL STORE CAN PRODUCE, and the absence
of it is what let a SEV-1 through.

It is NOT a faithful model of testset.Db: ReadForUpdate here delegates to
the same Read, exactly as the real one does, so nothing in this fake
distinguishes the two. The real-store test is what covers that. What this
adds is the ability to EXPRESS one shape the old fake could not.

`uiJoinConf.Read` returned (nil, nil) or an explicit error. The real
`testset.Db.Read` returns NEITHER on a malformed config.yaml — it
returns a non-nil ZERO struct with a NIL error, because it swallows the
unmarshal error so secret hydration still has somewhere to land. So the
fake could not express the one shape that breaks a read-modify-write,
and a test named for that guard pinned a state production never
produces.

ReadForUpdate reports the malformed file instead. `readErr` now drives
THIS method, which is the one the producer calls.
*/
func (c *uiJoinConf) ReadForUpdate(ctx context.Context, id string) (*models.TestSet, error) {
	return c.Read(ctx, id)
}

func (c *uiJoinConf) Write(_ context.Context, _ string, ts *models.TestSet) error {
	if c.writeErr != nil {
		// COUNTED ANYWAY. A write that was attempted and failed is not the
		// same as one that never happened, and a test asserting `writes ==
		// 0` must not be able to confuse the two.
		c.writes++
		return c.writeErr
	}
	c.writes++
	cp := *ts
	c.stored = &cp
	return nil
}

func uiJoinRecorder(conf TestSetConfig) *Recorder {
	return &Recorder{logger: zap.NewNop(), testSetConf: conf}
}

// The environment is process-global, so every case sets all three
// variables explicitly rather than inheriting whatever ran before.
func setUIJoinEnv(t *testing.T, captureID, nonce, origins string) {
	t.Helper()
	t.Setenv(EnvUIJoinCaptureID, captureID)
	t.Setenv(EnvUIJoinNonce, nonce)
	t.Setenv(EnvUIJoinAppOrigins, origins)
}

/*
KEPLOY_APP_ORIGINS is a comma-separated list typed by a human into a shell, so
`http://a,,http://b` and trailing spaces are the ordinary shapes. An
empty entry passed through as "" would be refused by
uiJoinOriginIsWellFormed and take the WHOLE annotation down with it —
one stray comma costing the join.
*/
func TestAppOriginsSplittingToleratesTheShapesPeopleType(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,,", nil},
		{"http://a.example.com", []string{"http://a.example.com"}},
		{" http://a.example.com , http://b.example.com ",
			[]string{"http://a.example.com", "http://b.example.com"}},
		{"http://a.example.com,,http://b.example.com",
			[]string{"http://a.example.com", "http://b.example.com"}},
	} {
		if got := uiJoinOrigins(tc.raw); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("uiJoinOrigins(%q) = %#v, want %#v", tc.raw, got, tc.want)
		}
	}
}

/*
The port list is PERSISTED, so its order is part of the bytes on disk.
Go randomises map iteration: without the sort, two runs that observed
exactly the same ports write two different config.yaml files. Nothing
downstream reads the order, which is exactly why nothing downstream
would catch it.

THE INPUT THAT DISCRIMINATES: more than a handful of ports. Go's map
iteration is randomised per iteration but a 1- or 2-element map comes
back in the same order often enough that a small fixture passes by
chance; 12 entries over 50 iterations does not.
*/
func TestObservedPortsAreDeterministicAndDeduplicated(t *testing.T) {
	build := func() *uiJoinPorts {
		p := newUIJoinPorts()
		for _, port := range []uint16{9000, 3000, 8080, 5432, 7000, 1234, 4000, 6000, 2000, 8081, 3001, 9999} {
			p.observe(port)
		}
		p.observe(8080) // a repeat is one port, not two
		p.observe(0)    // never a port anything was observed on
		return p
	}
	want := []int{1234, 2000, 3000, 3001, 4000, 5432, 6000, 7000, 8080, 8081, 9000, 9999}
	for i := 0; i < 50; i++ {
		if got := build().sorted(); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: sorted() = %v, want %v", i, got, want)
		}
	}
	if got := newUIJoinPorts().sorted(); got != nil {
		t.Errorf("no observation must yield nil, got %#v", got)
	}
}

/*
Port 0 is dropped rather than stored: models.uiJoinPortInRange refuses
it, so a single unpopulated AppPort would make the whole annotation
unstorable and cost the join every port that WAS observed.
*/
func TestAPortZeroDoesNotSinkTheWholeList(t *testing.T) {
	p := newUIJoinPorts()
	p.observe(0)
	p.observe(8080)
	p.observe(0)
	if got := p.sorted(); !reflect.DeepEqual(got, []int{8080}) {
		t.Fatalf("sorted() = %v, want [8080]", got)
	}
}

// Setting one identifier and omitting the other is a mistake worth
// reporting, and requiring both here would turn it into silence.
func TestEitherIdentifierCountsAsARequest(t *testing.T) {
	setUIJoinEnv(t, "", "", "")
	if uiJoinRequested() {
		t.Error("no identifiers must not read as a request")
	}
	setUIJoinEnv(t, "  ", "", "")
	if uiJoinRequested() {
		t.Error("whitespace is not a capture id")
	}
	setUIJoinEnv(t, "cap-1", "", "")
	if !uiJoinRequested() {
		t.Error("a capture id alone is still a request")
	}
	setUIJoinEnv(t, "", "nonce-1", "")
	if !uiJoinRequested() {
		t.Error("a nonce alone is still a request")
	}
}

// The ordinary keploy recording, which must be completely untouched.
/*
THE SPELLINGS ARE THE CONTRACT, so they are asserted as literals.

Every other test in this file reaches the environment through the
EnvUIJoin* constants, so renaming one renames it in the tests too and the
package stays green. MEASURED: `KEPLOY_APP_ORIGINS` -> `APP_ORIGINS` and
`KEPLOY_UI_CAPTURE_ID` -> `KEPLOY_CAPTURE` both passed the whole package
with this test deleted.

These names are set by a human in a deploy script or by an agent driving
the recorder; a silent rename breaks every existing caller and there is
no compile error anywhere to catch it. The hint in the unstorable-
annotation Error is built from the same constants, so a rename also
quietly makes that diagnostic name variables that do not exist.

ONLY THE LITERALS. It is tempting to also assert that pkg/models/uijoin.go
mentions the spelling, on the grounds that its refusal messages document
it -- but they do not: all eight occurrences in that file are in COMMENTS,
and its refusal messages name `appOrigins[N] (authority)` rather than the
variable. Such an assertion would check another package's comment prose
and go red on an innocuous rewording. There is nothing mechanical on the
other side to check against.
*/
func TestTheEnvironmentVariableNamesAreTheOnesCallersSet(t *testing.T) {
	assert.Equal(t, "KEPLOY_UI_CAPTURE_ID", EnvUIJoinCaptureID)
	assert.Equal(t, "KEPLOY_UI_SESSION_NONCE", EnvUIJoinNonce)
	assert.Equal(t, "KEPLOY_APP_ORIGINS", EnvUIJoinAppOrigins)
}

/*
T0 IS WHEN CAPTURE BEGAN, NOT WHEN THE PROCESS DID.

pkg/models/uijoin.go asks T0WallMs to be good enough "for skew estimation
only", and the same file spends twenty-four lines
refusing a float32 BECAUSE it introduces 24 seconds of error. (Quoting
only that half on purpose: the rest of the field doc now reads "when
capture began", which is this change's own wording and cannot be its
justification.) Handing it
`sessionStart` -- the top of Start(), before instrumentation setup, agent
bring-up (AgentReadyTimeout defaults to 330s), and under docker-compose
the app's own boot as well --
charges the whole of startup to the skew, which is a larger error than
the one the model refuses.

MEASURED as a gap first: six independent ways of undoing this change,
including simply deleting the `captureStart = time.Now()` line, left the
whole package green. Nothing referenced uiJoinT0Ms, captureStart or
sessionStart anywhere in the tests, and the harness's own
`assert.GreaterOrEqual(T0, before)` passes for both values.

The arguments are DISTINCT and ordered, so a swap is caught: `captureStart`
is necessarily the later of the two.
*/
func TestT0IsTheCaptureStartRatherThanTheProcessStart(t *testing.T) {
	sessionStart := time.UnixMilli(1700000000000)
	captureStart := time.UnixMilli(1700000045000) // 45s of bring-up

	got := uiJoinT0Ms(captureStart, sessionStart)

	assert.Equal(t, captureStart.UnixMilli(), got,
		"T0 must be when capture began; %ds of startup was charged to clock skew",
		(got-sessionStart.UnixMilli())/1000)
	assert.NotEqual(t, sessionStart.UnixMilli(), got,
		"T0 is still the process start")
}

/*
THE SPAN RUNS FORWARD, and the two ends cannot be exchanged by accident.

recordUIJoinAnnotation used to take `t0Ms, t1Ms int64` and the call site
assembled them as two adjacent expressions of the same type. Swapping
them compiled and ran, and every annotation came out backwards: on a
short session the skew estimate is garbage, and on a long one
validateStructure refuses `T1WallMs < T0WallMs` and the join is silently
lost with only a Debug line.

That swap was a FLAKY catch, not an uncaught one, and the difference
matters: the harness usually completes inside one millisecond, so T0 ==
T1 and the model's ordering rule -- which needs T1 strictly greater --
has nothing to refuse, but it sometimes crosses a millisecond boundary
and fails.

NO RATE IS QUOTED -- see the note on uiJoinSpan, which is the one place
that explains why. Successive versions of this paragraph each quoted a
different figure and each corrected the last, which is how a paragraph
announcing that it will not quote a rate ended up quoting three.

IT IS NOT A COMPILE ERROR. uiJoinSpan removed the interchangeable
`t0Ms, t1Ms int64` pair, but newUIJoinSpan's own parameters are two
time.Time values, so
`newUIJoinSpan(sessionStart, captureStart)` still builds and vets clean.
MEASURED: it compiles, and
TestStart_TakesT0FromTheCaptureStartNotTheProcessStart kills it 10 runs
out of 10 -- deterministically, because slowSetupInstr.Setup spins until
the wall clock's millisecond advances, so the gap is guaranteed rather
than raced for. That test is the guard; believing the compiler was would
make it look redundant, and deleting it would silently reopen this.

What THIS test pins is the ordering decision inside newUIJoinSpan, on a
five-second SYNTHETIC gap, so it does not depend on how fast the machine
runs or on the clock's granularity. Under the swap, t0Ms is `now` rather
than captureStart and the first assertion is wrong by five full seconds.
*/
func TestTheSpanRunsForwardFromCaptureStartToNow(t *testing.T) {
	captureStart := time.Now().Add(-5 * time.Second)
	sessionStart := captureStart.Add(-45 * time.Second)

	span := newUIJoinSpan(captureStart, sessionStart)

	assert.Equal(t, captureStart.UnixMilli(), span.t0Ms,
		"T0 must be when capture began, not when the recording stopped")
	assert.Greater(t, span.t1Ms, span.t0Ms,
		"the span runs backwards; validateStructure will refuse it")
	// The gap is real, not a rounding artifact -- and it is the assertion
	// that stays true no matter how long the test itself takes.
	assert.GreaterOrEqual(t, span.t1Ms-span.t0Ms, int64(5000),
		"T1 did not advance past a captureStart five seconds in the past")
}

/*
AND THE SPAN'S T0 USES THE SAME FALLBACK the bare function does.

Otherwise newUIJoinSpan could stamp a zero T0 while uiJoinT0Ms stayed
correct, and the two tests above would both pass over a broken producer.
*/
func TestTheSpanFallsBackToTheSessionStartWhenCaptureNeverBegan(t *testing.T) {
	sessionStart := time.Now().Add(-30 * time.Second)

	span := newUIJoinSpan(time.Time{}, sessionStart)

	assert.Equal(t, sessionStart.UnixMilli(), span.t0Ms)
	assert.Greater(t, span.t1Ms, span.t0Ms)
}

/*
AND THE ZERO CASE FALLS BACK RATHER THAN WRITING A ZERO.

A session that never reached `recordingStarted = true` leaves
`captureStart` at the zero time. Writing that through would put
-62135596800000 in T0WallMs -- the zero time's UnixMilli, which is what
this path calls -- and validateStructure refuses that outright, so the
annotation would be dropped rather than merely imprecise.

Unreachable from the stop defer (no capture means no observed ports,
which returns at the no-ingress arm before T0 is read), and asserted
anyway: this function is exported to exactly one caller today, and
"unreachable" is a property of that caller rather than of this function.
*/
func TestT0FallsBackToTheSessionStartWhenCaptureNeverBegan(t *testing.T) {
	sessionStart := time.UnixMilli(1700000000000)

	got := uiJoinT0Ms(time.Time{}, sessionStart)

	assert.Equal(t, sessionStart.UnixMilli(), got)
	assert.Positive(t, got, "a zero captureStart was written through as T0")
}

/*
THE WIRING for T0, which the two unit tests above cannot reach.

They pin uiJoinT0Ms's arithmetic. They say nothing about whether the
recorder passes it the right clock -- and MEASURED, deleting
`captureStart = time.Now()` from record.go left the whole package green,
because the IsZero fallback then silently returns sessionStart, which is
exactly the value this change exists to stop using.

The gap is made observable by spinning until the wall clock's millisecond
ADVANCES inside Setup, which runs after sessionStart and before
`recordingStarted = true`. That is a wait for a CONDITION, not for a
duration: it exits the instant the clock ticks, typically in microseconds,
and cannot be flaky the way a sleep can. UnixMilli is the annotation's own
resolution, so one tick is the smallest difference the field can express.
*/
type slowSetupInstr struct {
	*fakeInstr
	// The wall clock after Setup finished. T0 must be at or after this;
	// sessionStart necessarily precedes it.
	setupDoneMs int64
}

func (s *slowSetupInstr) Setup(context.Context, string, models.SetupOptions) error {
	tick := time.Now().UnixMilli()
	for time.Now().UnixMilli() == tick {
		// Spin. Bounded by one millisecond by construction.
	}
	s.setupDoneMs = time.Now().UnixMilli()
	return nil
}

func (s *slowSetupInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

func TestStart_TakesT0FromTheCaptureStartNotTheProcessStart(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &slowSetupInstr{fakeInstr: f}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     store,
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	/*
	 * THREE SENDS, AND THE COUNT IS THE FIX.
	 *
	 * A send on f.incoming completes when the FORWARDER takes it, one
	 * hop before the consumer -- so a single send proves nothing about
	 * whether Start got past its own gate. `cancel()` could then beat
	 * the `if ctx.Err() != nil` check that runs immediately after
	 * GetTestAndMockChans returns (record.go, beside `captureStart`),
	 * and Start exited with context.Canceled before spawning a consumer
	 * at all: no ports observed, no annotation, and the failure landed
	 * on `require.NoError` below rather than on anything about T0.
	 *
	 * MEASURED at 8 runs in 400 (2%) under load, and deterministically
	 * with a 200ms sleep inserted before that gate -- which is the
	 * oracle this fix has to satisfy, because three green suite passes
	 * at 2% are a coin flip, not evidence.
	 *
	 * Two sends are not enough, and the mechanism is the point. A send
	 * on f.incoming completes when the FORWARDER takes it, not when a
	 * consumer does. incomingChan carries one slot, so the forwarder
	 * puts the first item in it and is free to take the second; that
	 * second send therefore returns with no consumer in existence. The
	 * forwarder then PARKS trying to place the second item in the full
	 * slot, which is what makes a third send block until something has
	 * actually drained it -- so when the third returns, a consumer
	 * demonstrably exists.
	 * TestStart_WritesTheUIJoinAnnotationFromObservedPorts has always
	 * sent three and has never been seen to flake.
	 */
	for _, name := range []string{"t-1", "t-2", "t-3"} {
		select {
		case f.incoming <- &models.TestCase{Name: name, Kind: models.HTTP, AppPort: 8080}:
		case <-time.After(10 * time.Second):
			t.Fatal("recorder never consumed the test case")
		}
	}

	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(mappingIdleGrace + 10*time.Second):
		t.Fatal("Start did not return")
	}

	ids, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	ts, err := store.Read(context.Background(), ids[0].Name())
	require.NoError(t, err)
	got, err := models.GetUIJoinAnnotation(ts)
	require.NoError(t, err)

	require.Positive(t, instr.setupDoneMs, "Setup never ran, so this proves nothing")
	/*
	 * WHAT THIS ASSERTION IS, AND WHAT IT IS NOT.
	 *
	 * It pins that T0 is the CAPTURE start rather than the process
	 * start: `setupDoneMs` is stamped at the end of agent setup, and
	 * `captureStart` is taken after it, so T0 must not predate it.
	 * slowSetupInstr.Setup spins until the wall clock's millisecond
	 * advances, so the margin is guaranteed rather than raced for.
	 *
	 * THIS ASSERTION HAS NEVER BEEN THE ONE THAT FAILS. The flake in this
	 * test was `require.NoError` on Start's return, 38 lines above,
	 * reporting context.Canceled because the test's single send raced
	 * Start's own gate; the three-send loop up there is the fix. Worth
	 * saying because the T0 comparison is the obvious suspect and is not
	 * the culprit.
	 */
	assert.GreaterOrEqual(t, got.T0WallMs, instr.setupDoneMs,
		"T0 predates the end of agent setup, so it is the PROCESS start, not the "+
			"capture start: %dms of bring-up is being charged to clock skew "+
			"(T0WallMs=%d setupDoneMs=%d)",
		instr.setupDoneMs-got.T0WallMs, got.T0WallMs, instr.setupDoneMs)
}

func TestNoRequestWritesNothing(t *testing.T) {
	setUIJoinEnv(t, "", "", "")
	conf := &uiJoinConf{}
	uiJoinRecorder(conf).recordUIJoinAnnotation(context.Background(), "ts-1", uiJoinSpan{t0Ms: 1, t1Ms: 2}, []int{8080}, 1)
	if conf.writes != 0 {
		t.Fatalf("a recording that asked for no join wrote %d times", conf.writes)
	}
}

// The whole point: what is written reads back as the same annotation.
func TestTheAnnotationRoundTripsThroughTheTestSetConfig(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"http://localhost:3000, http://app.example.com")
	conf := &uiJoinConf{}
	const t0, t1 = int64(1700000000000), int64(1700000001500)

	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: t0, t1Ms: t1}, []int{8080, 9090}, 1)

	if conf.writes != 1 {
		t.Fatalf("want exactly one write, got %d", conf.writes)
	}
	got, err := models.GetUIJoinAnnotation(conf.stored)
	if err != nil {
		t.Fatalf("the annotation just written cannot be read back: %v", err)
	}
	if got.CaptureID != "cap-0000000000000001" || got.SessionNonce != "nonce-0000000000000001" {
		t.Errorf("identifiers did not survive: %+v", got)
	}
	if got.T0WallMs != t0 || got.T1WallMs != t1 {
		t.Errorf("times did not survive: %+v", got)
	}
	if !reflect.DeepEqual(got.IngressPorts, []int{8080, 9090}) {
		t.Errorf("ports did not survive: %v", got.IngressPorts)
	}
	if len(got.AppOrigins) != 2 {
		t.Errorf("origins did not survive: %v", got.AppOrigins)
	}
	if got.CanonicalKeySpec != models.UIJoinCanonicalKeySpecs[0] {
		t.Errorf("key spec did not survive: %q", got.CanonicalKeySpec)
	}
	// Finished, so a joiner may use it rather than treating it as a run
	// still in progress.
	if !got.Complete() {
		t.Error("an annotation written at stop must carry an end time")
	}
}

/*
READ-MODIFY-WRITE, and this is the test that says so.

createConfigWithMetadata may already have written a config carrying the
operator's own metadata, and SetUIJoinAnnotation stores under one
namespaced key inside that same map. Building a fresh models.TestSet
here would silently drop preScript, postScript, template and every user
metadata key — the recording would still look fine and the loss would
surface as a missing template on the next replay.
*/
func TestTheOperatorsOwnConfigSurvives(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", "http://localhost:3000")
	conf := &uiJoinConf{stored: &models.TestSet{
		PreScript:  "echo pre",
		PostScript: "echo post",
		AppCommand: "npm start",
		Template:   map[string]interface{}{"tpl": "v"},
		Metadata:   map[string]interface{}{"name": "checkout", "ticket": "KEP-1"},
	}}

	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

	if conf.stored.PreScript != "echo pre" || conf.stored.PostScript != "echo post" {
		t.Errorf("scripts were dropped: %+v", conf.stored)
	}
	if conf.stored.AppCommand != "npm start" {
		t.Errorf("appCommand was dropped: %+v", conf.stored)
	}
	if conf.stored.Template["tpl"] != "v" {
		t.Errorf("template was dropped: %+v", conf.stored.Template)
	}
	if conf.stored.Metadata["name"] != "checkout" || conf.stored.Metadata["ticket"] != "KEP-1" {
		t.Errorf("user metadata was dropped: %+v", conf.stored.Metadata)
	}
	if _, err := models.GetUIJoinAnnotation(conf.stored); err != nil {
		t.Errorf("the annotation did not land beside the user's metadata: %v", err)
	}
}

/*
A BROKEN OPT-IN WRITES NOTHING, and leaves what was there alone.

A malformed KEPLOY_APP_ORIGINS entry is the realistic cause. Writing a partial
or invalid annotation would hand back a test-set whose join fails much
later, in a joiner that can only say FOREIGN_ORIGIN; clobbering the
existing config on the way would lose the operator's metadata too.
*/
func TestABrokenOptInWritesNothing(t *testing.T) {
	/*
	 * WHAT THIS DOES NOT PIN, said rather than left to be assumed.
	 *
	 * `writes == 0` holds whether or not the arm this test is named for
	 * exists: SetUIJoinAnnotation re-runs the identical
	 * normalized().validateStructure() and refuses the same annotation
	 * independently, so deleting the guard leaves this green. What it
	 * proves is the OBSERVABLE a user depends on -- nothing is written
	 * over their config.yaml -- which is worth pinning on its own.
	 *
	 * The arm itself is pinned by
	 * TestTheDiagnosticsAreTheGuardsOwnObservable, which asserts the
	 * diagnostic each guard produces. Stated here because a test that
	 * reads as arm coverage and is not is how a guard gets deleted with
	 * the suite green.
	 */

	for name, origins := range map[string]string{
		"a path":         "http://a.example.com/v1",
		"a query":        "http://a.example.com?x=1",
		"no scheme":      "a.example.com",
		"an empty label": "http://a..example.com",
		"a wildcard":     "http://*.example.com",
		"credentials":    "http://admin:hunter2@a.example.com",
	} {
		t.Run(name, func(t *testing.T) {
			setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", origins)
			before := &models.TestSet{Metadata: map[string]interface{}{"name": "checkout"}}
			conf := &uiJoinConf{stored: before}

			uiJoinRecorder(conf).recordUIJoinAnnotation(
				context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

			if conf.writes != 0 {
				t.Fatalf("a refused annotation was written anyway (%d writes)", conf.writes)
			}
			if conf.stored.Metadata["name"] != "checkout" {
				t.Errorf("the operator's config was clobbered: %+v", conf.stored)
			}
		})
	}
}

/*
NO PORTS IS A REFUSAL, not an empty list on disk.

validateStructure refuses an empty IngressPorts on the grounds that no
correct producer emits one. A recording that observed no ingress has
nothing to join, and writing an annotation that claims otherwise is the
failure this whole file exists to avoid.
*/
func TestNoObservedPortsIsRefusedRatherThanStored(t *testing.T) {
	/*
	 * WHAT THIS DOES NOT PIN, said rather than left to be assumed.
	 *
	 * `writes == 0` holds whether or not the arm this test is named for
	 * exists: SetUIJoinAnnotation re-runs the identical
	 * normalized().validateStructure() and refuses the same annotation
	 * independently, so deleting the guard leaves this green. What it
	 * proves is the OBSERVABLE a user depends on -- nothing is written
	 * over their config.yaml -- which is worth pinning on its own.
	 *
	 * The arm itself is pinned by
	 * TestTheDiagnosticsAreTheGuardsOwnObservable, which asserts the
	 * diagnostic each guard produces. Stated here because a test that
	 * reads as arm coverage and is not is how a guard gets deleted with
	 * the suite green.
	 */

	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", "http://localhost:3000")
	conf := &uiJoinConf{}
	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, nil, 1)
	if conf.writes != 0 {
		t.Fatalf("an annotation with no ingress ports was stored")
	}
}

/*
NO ORIGINS IS STORABLE, and that difference is load-bearing.

A browser can genuinely have no usable origin — capture-sdk filters ""
and "null" — so models.joinability treats "no origins" as NOT JOINABLE
while validateStructure still accepts it. Refusing it here would discard
the capture id and nonce over a condition the joiner is designed to
report, which is the data loss ErrUIJoinNotJoinable exists to prevent.
*/
func TestAnOriginlessCaptureStillRecordsItsIdentifiers(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", "")
	conf := &uiJoinConf{}
	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
	if conf.writes != 1 {
		t.Fatalf("want one write, got %d", conf.writes)
	}
	/*
	 * READING IT BACK REPORTS NOT-JOINABLE, and that is the contract —
	 * the read does not succeed here, and should not.
	 *
	 * The distinction is the whole point of the two errors. Absent means
	 * nothing was written; NotJoinable means a producer did its job and
	 * the join is unavailable for this capture. Getting NotJoinable here
	 * is therefore positive evidence that the annotation was stored —
	 * had recordUIJoinAnnotation refused, this would be Absent.
	 */
	_, err := models.GetUIJoinAnnotation(conf.stored)
	if !errors.Is(err, models.ErrUIJoinNotJoinable) {
		t.Fatalf("want ErrUIJoinNotJoinable, got %v", err)
	}
	if errors.Is(err, models.ErrUIJoinAbsent) {
		t.Fatal("nothing was written: the identifiers were discarded")
	}
	/*
	 * ...and the identifiers survived into the stored record. Read off
	 * the metadata map rather than through Get, because Get is exactly
	 * what refuses this annotation — asserting through it would only
	 * re-test the refusal.
	 */
	raw, ok := conf.stored.Metadata[models.UIJoinMetadataKey].(map[string]interface{})
	if !ok {
		t.Fatalf("no annotation under %s: %#v", models.UIJoinMetadataKey, conf.stored.Metadata)
	}
	if raw["captureId"] != "cap-0000000000000001" {
		t.Errorf("the capture id was discarded over a non-joinable origin list: %#v", raw["captureId"])
	}
	if raw["sessionNonce"] != "nonce-0000000000000001" {
		t.Errorf("the session nonce was discarded: %#v", raw["sessionNonce"])
	}
}

/*
THE GUARDS THAT COST NOTHING TO WRITE AND EVERYTHING TO LEAVE UNTESTED.

recordUIJoinAnnotation bails out on three conditions before doing any
work. Each is a real state — the recorder is constructed with a nil
testSetConf in some embeddings, and GetNextTestSetID can fail and leave
the id empty on the path the stop defer still runs — and none of them
was exercised. A guard nothing reaches is indistinguishable from a guard
that is wrong.
*/
func TestTheBailOutsAreReachedRatherThanAssumed(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", "http://localhost:3000")

	t.Run("no test-set config", func(t *testing.T) {
		/*
		 * "MUST NOT PANIC" IS AN ASSERTION, so it is written down.
		 *
		 * This subtest used to have no assertion at all, on the grounds
		 * that the guard's whole point is not panicking. But an
		 * unrecovered nil-interface dereference aborts the test BINARY:
		 * this subtest would report nothing, and every test scheduled
		 * after it in the package would never run. A failure that hides
		 * other failures is worse than the one it is reporting, and a
		 * reviewer reading the file could not tell the difference
		 * between "asserts nothing" and "asserts it returned".
		 */
		r := &Recorder{logger: zap.NewNop(), testSetConf: nil}
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the nil testSetConf guard did not hold: %v", p)
			}
		}()
		r.recordUIJoinAnnotation(context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
	})

	t.Run("no test-set id", func(t *testing.T) {
		conf := &uiJoinConf{}
		uiJoinRecorder(conf).recordUIJoinAnnotation(context.Background(), "", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
		if conf.writes != 0 {
			t.Fatalf("wrote an annotation for an unnamed test-set (%d writes)", conf.writes)
		}
	})

	t.Run("a requested join that cannot be attempted says so", func(t *testing.T) {
		/*
		 * THREE STATES COLLAPSED INTO ONE SILENT RETURN.
		 *
		 * The guard was a single `!uiJoinRequested() || r.testSetConf ==
		 * nil || testSetID == ""`. "The operator did not ask for a join"
		 * and "the operator asked and this recorder cannot do it" are
		 * opposite conditions, and the second exited with no log at any
		 * level -- while this file's header promises WHY A BROKEN OPT-IN
		 * IS LOUD. That promise only held for the Storable() arm.
		 *
		 * r.testSetConf is non-nil in OSS (cli/provider wires it), so
		 * the population this bites is the enterprise/DaemonSet
		 * embedding the header already spends a paragraph worrying
		 * about -- and the one least able to diagnose silence.
		 *
		 * BOTH ARMS, and the negative control below, because a constant
		 * satisfies any one of them alone.
		 */
		setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
			"https://app.example.com")

		t.Run("no config store", func(t *testing.T) {
			/*
			 * THE MESSAGE, NOT THE COUNT.
			 *
			 * Asserting only "one Error was logged" does not
			 * discriminate this arm: MEASURED, deleting the guard lets
			 * execution fall through to persistUIJoinAnnotation, whose
			 * type assertion on a NIL interface yields ok == false
			 * (no panic) and returns an error -- so exactly one Error
			 * is still logged, at the same level, and the mutant
			 * survives. A test that passes for the wrong reason is
			 * worse than no test, because it reports coverage.
			 */
			logger, logs := newObserved()
			r := &Recorder{logger: logger, testSetConf: nil}
			r.recordUIJoinAnnotation(context.Background(), "ts-1",
				uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			require.Len(t, errs, 1,
				"a requested join that cannot be written must not return silently")
			assert.Contains(t, errs[0].Message, "no test-set config store",
				"the diagnostic must name THIS condition; the fall-through "+
					"failure further down logs an Error too, so the count alone "+
					"does not tell the two apart")
		})

		t.Run("no test-set id", func(t *testing.T) {
			logger, logs := newObserved()
			conf := &uiJoinConf{}
			r := uiJoinRecorder(conf)
			r.logger = logger
			r.recordUIJoinAnnotation(context.Background(), "",
				uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
			assert.Equal(t, 0, conf.writes)
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			require.Len(t, errs, 1,
				"a requested join with nothing to annotate must not return silently")
			// Same reasoning as the sibling above: name the condition.
			assert.Contains(t, errs[0].Message, "produced no test-set id",
				"the diagnostic must name THIS condition")
		})
	})

	t.Run("an ordinary recording stays silent", func(t *testing.T) {
		/*
		 * THE NEGATIVE CONTROL, and the reason the arms above are split
		 * rather than merged into one loud guard. The overwhelming
		 * majority of keploy recordings never ask for a join; logging at
		 * Error for them would be noise on every run, and the first
		 * thing anyone would do is turn it off.
		 */
		t.Setenv(EnvUIJoinCaptureID, "")
		t.Setenv(EnvUIJoinNonce, "")
		logger, logs := newObserved()
		r := &Recorder{logger: logger, testSetConf: nil}
		r.recordUIJoinAnnotation(context.Background(), "",
			uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, nil, 0)
		assert.Empty(t, logs.All(),
			"a recording that asked for no join must produce no output at any level")
	})

	t.Run("the config cannot be read", func(t *testing.T) {
		// A read failure must not become a write that clobbers whatever
		// is on disk with a config built from nothing.
		conf := &uiJoinConf{readErr: errors.New("permission denied")}
		uiJoinRecorder(conf).recordUIJoinAnnotation(context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
		if conf.writes != 0 {
			t.Fatalf("wrote after a failed read (%d writes)", conf.writes)
		}
	})
}

/*
THE EXISTING CONFIG SURVIVES, and nothing checked that it did.

testSetConf.Write marshals the WHOLE models.TestSet over config.yaml, so
a producer that builds a fresh one deletes every field it forgets.
That is not hypothetical: `keploy test --updateTemplate` did exactly
this — erasing `metadata:` AND `appCommand:` — and the loss was
invisible, because a test-set without either is the ordinary case. It is
why Db.Write carries warnIfDroppingFields, and a warning is not a
refusal.

MEASURED: before this test existed, replacing the read with a fresh
`&models.TestSet{}` left the whole package green. The annotation is
written under one namespaced key inside Metadata, so the operator's own
`--metadata` entries share that map and are the first thing lost.
*/
func TestTheAnnotationDoesNotEraseTheConfigItJoins(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	conf := &uiJoinConf{stored: &models.TestSet{
		PreScript:  "echo before",
		PostScript: "echo after",
		Template:   map[string]interface{}{"token": "t"},
		// map[string]interface{} — the same map the annotation is
		// written into, under its own namespaced key. That sharing is
		// exactly why the operator's entries are the most likely thing
		// to lose to a fresh-struct write.
		Metadata: map[string]interface{}{
			"team":    "checkout",
			"release": "2026.09",
		},
	}}
	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

	if conf.writes != 1 {
		t.Fatalf("want one write, got %d", conf.writes)
	}
	if conf.stored.PreScript != "echo before" {
		t.Errorf("preScript was erased: %q", conf.stored.PreScript)
	}
	if conf.stored.PostScript != "echo after" {
		t.Errorf("postScript was erased: %q", conf.stored.PostScript)
	}
	if conf.stored.Template["token"] != "t" {
		t.Errorf("template was erased: %#v", conf.stored.Template)
	}
	// THE OPERATOR'S OWN METADATA, which shares the map the annotation
	// is written into and is therefore the most likely thing to lose.
	if conf.stored.Metadata["team"] != "checkout" ||
		conf.stored.Metadata["release"] != "2026.09" {
		t.Errorf("user metadata was erased: %#v", conf.stored.Metadata)
	}
	// ...and the annotation really did land alongside it.
	if _, err := models.GetUIJoinAnnotation(conf.stored); err != nil {
		t.Errorf("the annotation was not stored: %v", err)
	}
}

/*
A BROKEN OPT-IN IS LOUD, AND WRITES NOTHING.

Setting the capture id is an explicit request for a join. If the
annotation that request implies cannot be stored — a malformed
KEPLOY_APP_ORIGINS entry is the realistic cause, and pkg/models/uijoin.go
refuses a dozen shapes of one — then writing it anyway hands back a
test-set carrying an annotation the reader will reject, discovered much
later by a joiner that can only say FOREIGN_ORIGIN.

WHAT THIS TEST ACTUALLY PINS: that nothing is WRITTEN. It asserts
`writes == 0`, and a re-measurement says that is all it pins — deleting
the `a.Storable()` refusal is killed by
TestTheDiagnosticsAreTheGuardsOwnObservable's malformed-KEPLOY_APP_ORIGINS
subtest, not by this one, because SetUIJoinAnnotation runs the identical
normalized().validateStructure() and refuses independently. So
`writes == 0` holds either way.

THIS TEST IS NOT WHAT HOLDS THE `a.Storable()` REFUSAL. Asserting
`writes` pins the wrong observable -- the test two below says so in its
own header -- and the diagnostic is what actually holds the behaviour.
The producer's header documents it under "WHY A BROKEN OPT-IN IS LOUD".

The recording itself stays valid keploy data either way — this is a
refusal to write a broken annotation, not a failed session.
*/
func TestAnUnstorableAnnotationIsRefusedRatherThanWritten(t *testing.T) {
	for _, tc := range []struct{ name, origins string }{
		// Each of these is refused by validateStructure for its own
		// reason, and every one is a plausible typo in a deploy script.
		{"an origin with a path", "https://app.example.com/v1"},
		{"an origin with a query", "https://app.example.com?a=1"},
		{"a bare host, no scheme", "app.example.com"},
		{"a wildcard", "https://*.example.com"},
		{"an empty port", "https://app.example.com:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001", tc.origins)
			conf := &uiJoinConf{}
			uiJoinRecorder(conf).recordUIJoinAnnotation(
				context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
			if conf.writes != 0 {
				t.Fatalf("an unstorable annotation was written (%d writes): %#v",
					conf.writes, conf.stored)
			}
			if conf.stored != nil {
				t.Errorf("nothing should have been stored, got %#v", conf.stored)
			}
		})
	}

	// THE NEGATIVE CONTROL. A refusal that fired on everything would
	// satisfy every row above while never writing an annotation at all.
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")
	conf := &uiJoinConf{}
	uiJoinRecorder(conf).recordUIJoinAnnotation(
		context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)
	if conf.writes != 1 {
		t.Fatalf("a storable annotation must still be written, got %d writes", conf.writes)
	}
}

/*
AGAINST THE REAL STORE, because the fake cannot produce the shape that
breaks a read-modify-write.

testset.Db.Read swallows an unmarshal error and returns a non-nil ZERO
struct with a NIL error — correct for a reader, because secret hydration
still needs somewhere to land, and catastrophic for a writer, because
Write replaces the whole document. One stray indent in a hand-edited
config.yaml and a read-modify-write over Read puts the zero struct on
disk: preScript, postScript, appCommand, template and every user
metadata key gone. warnIfDroppingFields does not stop it either — it
reads the same zero value and logs that it could not check.

That bug was fixed once, for `keploy test --updateTemplate`, which is
why ReadForUpdate exists. Adding a second read-modify-write on Read
re-entered it through a new door, and the fake-based tests could not see
it because they answer with (nil, nil) or an explicit error — neither of
which is what production returns.
*/
func TestAMalformedConfigIsNotOverwrittenByTheAnnotation(t *testing.T) {
	dir := t.TempDir()
	logger := zap.NewNop()
	store := testset.New[*models.TestSet](logger, dir)

	const id = "test-set-0"
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id, "config.yaml")
	// A config a human edited and got slightly wrong. Its CONTENT is the
	// point: every line here is something the annotation must not cost.
	malformed := "preScript: echo before\n" +
		"postScript: echo after\n" +
		"  appCommand: npm start\n" + // <- the stray indent
		"metadata:\n" +
		"  team: checkout\n"
	if err := os.WriteFile(path, []byte(malformed), 0o644); err != nil {
		t.Fatal(err)
	}

	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")
	r := uiJoinRecorder(store)
	r.recordUIJoinAnnotation(
		context.Background(), id, uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config.yaml is gone entirely: %v", err)
	}
	if string(after) != malformed {
		t.Errorf("a malformed config was REPLACED rather than left alone.\n"+
			"before:\n%s\nafter:\n%s", malformed, string(after))
	}
}

/*
...AND THE ORDINARY PATH STILL WRITES, against the same real store.

The negative control for the test above: a guard that refused every
write would satisfy it and silently turn the whole feature off.
*/
func TestAWellFormedConfigStillReceivesTheAnnotation(t *testing.T) {
	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)

	const id = "test-set-0"
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")
	if err := store.Write(context.Background(), id, &models.TestSet{
		PreScript: "echo before",
		Metadata:  map[string]interface{}{"team": "checkout"},
	}); err != nil {
		t.Fatal(err)
	}

	uiJoinRecorder(store).recordUIJoinAnnotation(
		context.Background(), id, uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

	got, err := store.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.PreScript != "echo before" {
		t.Errorf("preScript was erased: %q", got.PreScript)
	}
	if got.Metadata["team"] != "checkout" {
		t.Errorf("user metadata was erased: %#v", got.Metadata)
	}
	if _, err := models.GetUIJoinAnnotation(got); err != nil {
		t.Errorf("the annotation was not stored: %v", err)
	}
}

/*
THE INTEGRATION, DRIVEN THROUGH Start() — because four of its pieces
were removable with the whole package green.

A review swept the producer by mutation and found the unit-level guards
all pinned and the WIRING pinned by nothing: deleting the
`uiJoinObserved.observe(...)` call, replacing `if recordingStarted` with
`if true`, swapping `persistCtx` for `ctx`, and swapping the T0/T1
arguments each left the suite passing. Deleting the observe call is the
whole feature silently dead — no ports observed means every annotation
is refused, and the only trace is a log line.

Three of those four are pinned here. The fourth, `if recordingStarted`,
turned out to be dead rather than untested and is gone; record.go says
why at the call site.

The harness for this already existed. `fakeInstr` + `blockingInstr` drive
real test cases through `Start()`, from seven functions named TestStart_*
and from several that are not, named for what they are about rather than
for what they drive.

THE LIST OF EXCEPTIONS IS GONE, on the instruction the list itself
carried: "if this list ever needs a third exception, delete it and stop
enumerating." It named two and there were three -- the insert-fails,
insert-fails-during-shutdown and no-phantom-test-set cases -- so the
enumeration had already drifted from the thing it enumerated, which is
the whole reason that instruction was written. `grep -l blockingInstr`
answers the question and cannot go stale.

The store is the REAL testset.Db, not the fake, for the reason the
malformed-config test above gives.
*/
func TestStart_WritesTheUIJoinAnnotationFromObservedPorts(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: &blockingInstr{f},
		testSetConf:     store,
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	before := time.Now().UnixMilli()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	// Two ports, one of them twice: the annotation must carry the SET,
	// sorted, and must carry ports it actually saw.
	for _, tc := range []*models.TestCase{
		{Name: "t-1", Kind: models.HTTP, AppPort: 8080},
		{Name: "t-2", Kind: models.HTTP, AppPort: 9090},
		{Name: "t-3", Kind: models.HTTP, AppPort: 8080},
	} {
		select {
		case f.incoming <- tc:
		case <-time.After(10 * time.Second):
			t.Fatal("recorder never consumed the test case")
		}
	}

	/*
	 * CANCEL, not a graceful close — this is the Ctrl+C path, and it is
	 * what makes persistCtx load-bearing. Db.Write goes through a
	 * ctxWriter that honours cancellation, so writing on `ctx` here
	 * fails for the one reason that has nothing to do with the data.
	 */
	cancel()
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(mappingIdleGrace + 10*time.Second):
		t.Fatal("Start did not return")
	}
	after := time.Now().UnixMilli()

	ids, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, ids, 1, "exactly one test-set should have been written")
	ts, err := store.Read(context.Background(), ids[0].Name())
	require.NoError(t, err)

	got, err := models.GetUIJoinAnnotation(ts)
	require.NoError(t, err, "no annotation was written for a requested join")

	// THE PORTS WERE OBSERVED. Deleting the observe() call leaves this
	// empty, the annotation unstorable, and the feature silently off.
	assert.Equal(t, []int{8080, 9090}, got.IngressPorts)
	assert.Equal(t, "cap-0000000000000001", got.CaptureID)

	/*
	 * BOTH STAMPS INSIDE THE RUN. These are bounds against a clock read
	 * outside Start, and they hold however long the session takes.
	 *
	 * THERE IS NO `assert.Less(T0, T1)` HERE, and there was. It needed a
	 * `time.Sleep(5 * time.Millisecond)` above it to pass, because this
	 * harness can finish inside one millisecond and then T0 == T1 —
	 * so the assertion failed on CORRECT code. The comment justifying
	 * the sleep had it backwards: it claimed the assertion "would pass
	 * on the bug" without the wait, when in fact the wait existed to
	 * stop it failing without one. A wall-clock assertion propped up by
	 * a sleep is exactly the timing-dependent test this repo forbids.
	 *
	 * It also bought nothing, though not for the reason given when it
	 * was removed. That claim -- that swapping the two arguments at the
	 * call site was "pinned STRUCTURALLY" by validateStructure refusing
	 * `T1 < T0` -- was FALSE, and it cited a test name that does not
	 * exist. MEASURED: the swap survived the whole package ten runs out
	 * of ten. The harness finishes inside one millisecond, so T0 == T1,
	 * and the model's rule needs T1 strictly greater before it refuses
	 * anything. In isolation the catch was intermittent --
	 * a coin flip on crossing a millisecond boundary.
	 *
	 * There are no two arguments to swap now: see uiJoinSpan. The
	 * ordering that remains is pinned by
	 * TestTheSpanRunsForwardFromCaptureStartToNow, on a five-second
	 * synthetic gap, verified red 10 runs out of 10.
	 */
	assert.GreaterOrEqual(t, got.T0WallMs, before)
	assert.LessOrEqual(t, got.T1WallMs, after)
}

/*
A PORT IS OBSERVED WHEN INGRESS ARRIVES ON IT, not when the store
accepts the test case.

record.go calls uiJoinObserved.observe() BEFORE InsertTestCase. Moving it
into the `err == nil` branch loses the port of any test case that failed
to persist -- so a recording that lost one exchange to a transient write
error would annotate a port list missing the very port that exchange
arrived on, and the joiner would report NO_INGRESS_OBSERVED for it.

Two test cases on two ports, one of which fails to persist: the
annotation must still carry BOTH ports.
*/
func TestAPortIsObservedEvenWhenItsTestCaseFailsToPersist(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{
		logger: zap.NewNop(),
		// "t-lost" never lands; "t-kept" does.
		testDB:          &recTestDB{failNames: map[string]bool{"t-lost": true}},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: &blockingInstr{f},
		testSetConf:     store,
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	/*
	 * NOT cancellable, deliberately. See the note at the close() below:
	 * the failing insert is what ends this recording, and holding a
	 * cancel func here is what lets a test race Start's own gate.
	 */
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	/*
	 * THE FAILING INSERT GOES LAST, and the order is the test, not
	 * decoration.
	 *
	 * `t-lost` is the insert that errors, and an insert error is what
	 * `Start`'s terminating select consumes: the consumer pushes it into
	 * insertTestErrChan, Start returns, the stop defer runs
	 * reqCtxCancel(), the incoming forwarder returns and closes
	 * incomingChan. Sending `t-lost` FIRST therefore raced teardown
	 * against the second send, and when teardown won there was no reader
	 * left -- the send blocked for the full 10s and the test failed with
	 * "recorder never consumed the test case".
	 *
	 * MEASURED: with `t-lost` first, inserting a 200ms gap before the
	 * second send fails 10 out of 10 with exactly that message. It also
	 * failed a clean `go test ./...` on a loaded machine, and produced a
	 * false KILL inside an unrelated mutation sweep -- a flaky test does
	 * not just cost a re-run, it corrupts every measurement taken through
	 * it.
	 *
	 * Sending the surviving case first means Start cannot begin teardown
	 * until both are in. No sleep, no retry, no timeout bump: the
	 * ordering removes the race rather than widening the window it needs.
	 */
	for _, tc := range []*models.TestCase{
		{Name: "t-kept", Kind: models.HTTP, AppPort: 9090},
		{Name: "t-lost", Kind: models.HTTP, AppPort: 8080},
	} {
		select {
		case f.incoming <- tc:
		case <-time.After(10 * time.Second):
			t.Fatal("recorder never consumed the test case")
		}
	}

	/*
	 * NO cancel(), AND THAT IS THE FIX.
	 *
	 * The failing insert is what terminates Start -- the consumer pushes
	 * it into insertTestErrChan and Start's terminating select returns --
	 * so this test never needed to cancel anything.
	 *
	 * Cancelling anyway raced Start's own gate. A send on f.incoming
	 * completes when the FORWARDER takes it, one hop before the consumer,
	 * so `cancel()` could beat the `if ctx.Err() != nil` check that runs
	 * just after GetTestAndMockChans returns. Start then exited with
	 * context.Canceled before spawning a consumer at all, the forwarder
	 * wedged on its unconditional hand-off, and this test failed at
	 * "Start did not return" after the full 30s drain. MEASURED at 1 run
	 * in 200, and deterministically with a 200ms sleep before that gate.
	 *
	 * Reordering the sends removed a DIFFERENT race on the
	 * same line and left this one. TestNoTestCasePersistedLeavesNoPhantom
	 * TestSet stays clean precisely because it does not cancel either.
	 */
	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case err := <-done:
		/*
		 * Start ENDS ON THE INSERT ERROR, and that is the point: it
		 * reports "error while inserting test case into db, hence
		 * stopping keploy" rather than context.Canceled. A cancellation
		 * here would mean the test had raced Start's own gate instead of
		 * exercising the insert-failure path it is named for.
		 */
		require.Error(t, err, "Start should end on the insert error")
		require.NotErrorIs(t, err, context.Canceled,
			"Start ended on cancellation, so this test raced its gate rather than "+
				"exercising the insert-failure path")
	case <-time.After(mappingIdleGrace + 10*time.Second):
		t.Fatal("Start did not return")
	}

	ids, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, ids, 1, "exactly one test-set should have been written")
	ts, err := store.Read(context.Background(), ids[0].Name())
	require.NoError(t, err)

	got, err := models.GetUIJoinAnnotation(ts)
	require.NoError(t, err)
	assert.Equal(t, []int{8080, 9090}, got.IngressPorts,
		"the port of the test case that failed to persist was lost")
}

/*
gatedFailTestDB fails one named test case, and only once the test says so.

It exists because the shutdown branch cannot be reached by sending after a
cancel: once ctx is done the incoming forwarder is free to take its
ctx.Done() arm and return, so a post-cancel send races the forwarder and
blocks. Holding the INSERT instead puts the ordering entirely in the
test's hands -- the case is already inside the consumer when shutdown
begins, which is exactly the production shape (SIGINT lands while the tail
is still being written).
*/
type gatedFailTestDB struct {
	recTestDB
	failName string
	// Closed by the test when the gated insert should be allowed to fail.
	release chan struct{}
	// Closed by the DB when the gated insert has been entered, so the test
	// knows the consumer is parked inside it rather than still upstream.
	entered chan struct{}

	once sync.Once
}

func (d *gatedFailTestDB) InsertTestCase(ctx context.Context, tc *models.TestCase, setID string, b bool) error {
	if tc.Name != d.failName {
		return d.recTestDB.InsertTestCase(ctx, tc, setID, b)
	}
	d.once.Do(func() { close(d.entered) })
	<-d.release
	return errors.New("no space left on device")
}

/*
THE SHUTDOWN HALF OF THE ORDERING, and the direction that was NOT pinned.

TestAPortIsObservedEvenWhenItsTestCaseFailsToPersist above covers moving
observe() INTO the `err == nil` branch. It cannot cover moving it after
the whole if/else, because it runs on context.Background(): ctx.Err() is
always nil, the `continue` in the error branch is never taken, and
execution falls through past the if/else either way. MEASURED: that mutant
survived 10 runs out of 10, while a comment beside the call claimed it was
killed 10 out of 10.

This is that case. The insert fails WHILE ctx is cancelled, so the error
branch takes its `continue` -- and anything placed after the if/else is
skipped for exactly the test case whose port is under test.

Why it matters in production rather than only to a mutant: SIGINT lands
while the agent is still handing over the tail, so a test case that fails
to persist during shutdown is the ordinary case, not an exotic one. Its
port is still something the recorder OBSERVED, and the annotation is a
record of what ingress was seen -- not of what the store accepted.
*/
func TestAPortIsObservedWhenTheInsertFailsDuringShutdown(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	testDB := &gatedFailTestDB{
		failName: "t-shutdown-lost",
		release:  make(chan struct{}),
		entered:  make(chan struct{}),
	}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          testDB,
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: &blockingInstr{f},
		testSetConf:     store,
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	/*
	 * RELEASED ON EVERY EXIT, not only the happy one. Any t.Fatal below
	 * leaves the consumer parked inside the gated insert, which holds
	 * Start and its errgroup for the rest of the binary's run -- so one
	 * failing test would take unrelated ones with it. sync.Once because
	 * the happy path closes it explicitly and closing twice panics.
	 */
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(testDB.release) }) }
	defer release()

	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	for _, tc := range []*models.TestCase{
		{Name: "t-kept", Kind: models.HTTP, AppPort: 9090},
		{Name: "t-shutdown-lost", Kind: models.HTTP, AppPort: 7070},
	} {
		select {
		case f.incoming <- tc:
		case <-time.After(10 * time.Second):
			t.Fatal("recorder never consumed the test case")
		}
	}

	// The consumer is now parked inside the gated insert, which means it is
	// past the observe() call site and Start is long past its gate. Both
	// facts are what make the cancel below deterministic rather than a race.
	select {
	case <-testDB.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the consumer never reached the failing insert")
	}

	// NOW shut down, then let the insert fail. This is the ordering the
	// whole test exists for: the error is returned with ctx already
	// cancelled, so the consumer takes the `continue` rather than the
	// insertTestErrChan send.
	cancel()
	release()

	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	select {
	case <-done:
		// The error Start ends with is not the subject here -- cancellation
		// and the insert error are both legitimate outcomes of this ordering,
		// and asserting one of them would be asserting on the race between
		// them. What must hold is the annotation below.
	case <-time.After(mappingIdleGrace + 30*time.Second):
		t.Fatal("Start did not return")
	}

	ids, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, ids, 1, "exactly one test-set should have been written")
	ts, err := store.Read(context.Background(), ids[0].Name())
	require.NoError(t, err)

	got, err := models.GetUIJoinAnnotation(ts)
	require.NoError(t, err)
	assert.Equal(t, []int{7070, 9090}, got.IngressPorts,
		"the port of a test case whose insert failed DURING SHUTDOWN was lost; "+
			"observe() must run before the insert, not after the if/else that "+
			"`continue`s on this path")
}

/*
A RECORDING THAT PERSISTED NOTHING LEAVES NO TEST-SET BEHIND.

The annotation write CREATES keploy/<id>/config.yaml, and on a disk-full
recording -- every InsertTestCase failing -- it can be the only writer
that succeeds. NOT the only one in general: createConfigWithMetadata
(under --metadata) and the pcap MkdirAll (under --capture-packets) each
create the directory independently and earlier, so under either flag the
phantom exists whatever this arm decides. The producer's own comment says
so; an unhedged "the only writer" here contradicted it in the same commit.

The leftover is not "a spurious artifact rather than a wrong answer".

Measured, it is a wrong answer: replay classifies an empty test-set
TestSetStatusNoTestsToRun, replay.go's roll-up sets `testSetResult =
false` for that status, and replayRunOutcome turns that into EXIT 1. So
the leftover directory makes every subsequent `keploy test` fail, with
nothing in the output explaining why, until a human deletes it.

There is nothing to join either: a test-set with no test cases has no
exchange to pair with a browser capture.
*/
func TestNoTestCasePersistedLeavesNoPhantomTestSet(t *testing.T) {
	setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
		"https://app.example.com")

	dir := t.TempDir()
	store := testset.New[*models.TestSet](zap.NewNop(), dir)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{insertErr: errors.New("no space left on device")},
		mockDB:          &recMockDB{unencodable: map[string]bool{}},
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: &blockingInstr{f},
		testSetConf:     store,
		hooks:           BaseRecordHooks{},
		config:          &config.Config{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	select {
	case f.incoming <- &models.TestCase{Name: "t-1", Kind: models.HTTP, AppPort: 8080}:
	case <-time.After(10 * time.Second):
		t.Fatal("recorder never consumed the test case")
	}

	// Start returns on the insert error by itself; NOT cancelling is the
	// point, so this is the error path rather than the Ctrl+C path.
	select {
	case err := <-done:
		require.Error(t, err, "a failing insert should surface")
	case <-time.After(mappingIdleGrace + 10*time.Second):
		t.Fatal("Start did not return")
	}

	close(f.outgoing)
	close(f.incoming)
	close(f.mappings)

	ids, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, ids,
		"a recording that persisted no test case left a test-set behind; "+
			"replay reports it NoTestsToRun and every later `keploy test` exits 1")
}

// newObserved returns a logger and the record of what was written to it.
//
// FILE-LEVEL, not a closure inside one test: the diagnostics this file
// asserts are produced by guards in two different functions, and a
// helper scoped to one of them is how the other's guards ended up with
// no observable at all.
//
// DebugLevel so a test can assert something was logged QUIETLY as well
// as loudly -- an ordinary recording that asked for no join must produce
// nothing at any level, which a higher threshold could not distinguish
// from a Debug line nobody sees.
func newObserved() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(core), logs
}

func TestTheDiagnosticsAreTheGuardsOwnObservable(t *testing.T) {

	t.Run("a malformed KEPLOY_APP_ORIGINS names the variables to check", func(t *testing.T) {
		setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
			"https://app.example.com/v1") // a path — refused by validateStructure
		logger, logs := newObserved()
		conf := &uiJoinConf{}
		r := uiJoinRecorder(conf)
		r.logger = logger
		r.recordUIJoinAnnotation(
			context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

		require.Equal(t, 0, conf.writes, "an unstorable annotation was written")
		entries := logs.FilterLevelExact(zapcore.ErrorLevel).All()
		require.Len(t, entries, 1, "the operator was told nothing: %v", logs.All())
		var hint string
		for _, f := range entries[0].Context {
			if f.Key == "hint" {
				hint = f.String
			}
		}
		// The three variables the operator can actually act on.
		assert.Contains(t, hint, EnvUIJoinAppOrigins)
		assert.Contains(t, hint, EnvUIJoinCaptureID)
		assert.Contains(t, hint, EnvUIJoinNonce)
	})

	t.Run("test cases with no ingress port are reported, loudly", func(t *testing.T) {
		/*
		 * "NO PORT" IS NOT "NO INGRESS", and this subtest used to pin the
		 * opposite.
		 *
		 * It passed testCount=1 with nil ports and asserted SILENCE, on
		 * the stated ground that "the recording simply captured
		 * nothing". Its own fixture contradicts that: testCount=1 means
		 * one test case WAS persisted.
		 *
		 * And the state it describes is not reachable. record.go calls
		 * `uiJoinObserved.observe(testCase.AppPort)` for EVERY test case,
		 * deliberately before InsertTestCase so even a failed insert
		 * records the port; and the testCount <= 0 guard has already
		 * returned by the time this arm runs. So arriving here means
		 * test cases were persisted and every AppPort observed was zero
		 * -- which observe() drops, because zero means "the kernel picks
		 * one" and can never be a port something arrived on.
		 *
		 * That is a fault in what the hooks populated, it produces no
		 * annotation, and before this it was a Debug line -- suppressed
		 * on a default run. A user who set the variables correctly and
		 * got nothing had nothing to read.
		 */
		setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
			"https://app.example.com")
		logger, logs := newObserved()
		conf := &uiJoinConf{}
		r := uiJoinRecorder(conf)
		r.logger = logger
		// nil ports with test cases persisted: every AppPort was zero.
		r.recordUIJoinAnnotation(
			context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, nil, 1)

		require.Equal(t, 0, conf.writes,
			"a recording with no observed port has no storable annotation")
		errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
		require.Len(t, errs, 1,
			"a requested join that produced nothing must say so at a level a default run prints")
		/*
		 * AND IT MUST NOT BLAME THE OPERATOR. The environment here is
		 * correct -- all three variables set and valid -- so a message
		 * naming them sends the reader to check something that is fine.
		 * The distinction is the whole point of the arm.
		 */
		msg := errs[0].Message
		assert.Contains(t, msg, "ingress port",
			"the diagnostic must name what was missing")
		assert.NotContains(t, msg, EnvUIJoinCaptureID,
			"the variables are correct; naming them sends the reader to the wrong place")
		assert.NotContains(t, msg, EnvUIJoinAppOrigins,
			"the variables are correct; naming them sends the reader to the wrong place")
	})

	t.Run("a recording that persisted nothing says so, quietly", func(t *testing.T) {
		/*
		 * THE testCount ARM -- first of the two DATA arms, though not the
		 * first check in the function -- and it had no test.
		 *
		 * It was added with the file's own standard stated at the top of
		 * this function -- the diagnostics are the guards' own
		 * observable -- and then not met: MEASURED, raising this line
		 * from Debug to Error survived, and deleting it survived. Its
		 * sibling no-ingress arm one line below is killed by the
		 * subtest above. On a join that was legitimately requested and
		 * produced nothing, this line is the only signal there is.
		 *
		 * PORTS ARE SUPPLIED HERE on purpose, so the no-ingress arm
		 * cannot fire and this subtest cannot pass on the wrong arm's
		 * behaviour.
		 */
		setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
			"https://app.example.com")
		logger, logs := newObserved()
		conf := &uiJoinConf{}
		r := uiJoinRecorder(conf)
		r.logger = logger
		// testCount 0: every InsertTestCase failed, or every test case
		// was revoked. Ingress WAS observed.
		r.recordUIJoinAnnotation(
			context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 0)

		require.Equal(t, 0, conf.writes,
			"an annotation was written onto a test-set with no test cases")

		// DEBUG, and only Debug. An Error here blames an operator whose
		// environment is correct; an Info claims an annotation that was
		// not written.
		assert.Empty(t, logs.FilterLevelExact(zapcore.ErrorLevel).All(),
			"a recording that persisted nothing was reported as a broken join: %v", logs.All())
		assert.Empty(t, logs.FilterLevelExact(zapcore.InfoLevel).All(),
			"a refusal was reported as a recorded annotation: %v", logs.All())

		debugs := logs.FilterLevelExact(zapcore.DebugLevel).All()
		require.Len(t, debugs, 1,
			"the one signal this arm produces is missing: %v", logs.All())
		// The CAUSE, not just any Debug line -- otherwise the
		// no-ingress arm's message would satisfy this too.
		assert.Contains(t, debugs[0].Message, "no test case was persisted")
	})

	t.Run("a write that fails says so, and never claims success", func(t *testing.T) {
		/*
		 * THE FOURTH MEMBER of this family, and the only one that
		 * reports an actual I/O failure.
		 *
		 * The other three are about not blaming the user for something
		 * that is fine. This one is the reverse: the annotation could
		 * not be written, the recording itself is unaffected and still
		 * succeeds, so the ONLY signal that this test-set will never
		 * join to a browser capture is this log line.
		 *
		 * MEASURED: swallowing the Write error -- `_ = r.testSetConf
		 * .Write(...)` then `return nil` -- left the whole package
		 * green AND made the producer log Info "recorded the UI join
		 * annotation" over a write that did not happen. That is a
		 * broken thing reporting green in the one arm where the log is
		 * the entire user-visible contract, so the absence of the Info
		 * is asserted here as firmly as the presence of the Error.
		 */
		setUIJoinEnv(t, "cap-0000000000000001", "nonce-0000000000000001",
			"https://app.example.com")
		logger, logs := newObserved()
		conf := &uiJoinConf{writeErr: errors.New("no space left on device")}
		r := uiJoinRecorder(conf)
		r.logger = logger
		r.recordUIJoinAnnotation(
			context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

		// The write was ATTEMPTED -- otherwise this would pass against a
		// producer that refused before ever reaching the store, which is
		// a different arm with a different message.
		require.Equal(t, 1, conf.writes, "the write was never attempted")

		errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
		require.Len(t, errs, 1, "the operator was not told the write failed: %v", logs.All())
		assert.Contains(t, errs[0].Message, "will not")

		assert.Empty(t, logs.FilterLevelExact(zapcore.InfoLevel).All(),
			"a failed write was reported as a recorded annotation: %v", logs.All())
	})

	t.Run("an ordinary recording that asked for no join is silent", func(t *testing.T) {
		/*
		 * THE BLAST-RADIUS CASE, and the one the other two do not
		 * reach.
		 *
		 * `!uiJoinRequested()` was removable with the whole package
		 * green: TestNoRequestWritesNothing still passed, because
		 * Storable() refuses the empty capture id and so nothing is
		 * written either way. What actually changes is the diagnostic —
		 * with the guard gone, EVERY keploy recording that ever
		 * observed an ingress port logs at ERROR that "a UI join was
		 * requested but the annotation cannot be stored", naming three
		 * environment variables the user has never heard of. That is
		 * 100% of ordinary recordings, not an aborted minority.
		 *
		 * `writes` is therefore NOT the observable here, and asserting
		 * it is what let the guard look covered. The observable is that
		 * the producer says nothing AT ALL: no Error, and no Debug
		 * either, because the no-ingress arm above must not swallow
		 * this case and report it as a join that found no ingress.
		 */
		setUIJoinEnv(t, "", "", "")
		logger, logs := newObserved()
		conf := &uiJoinConf{}
		r := uiJoinRecorder(conf)
		r.logger = logger
		// Ports WERE observed: this is a recording that worked.
		r.recordUIJoinAnnotation(
			context.Background(), "ts-1", uiJoinSpan{t0Ms: 1700000000000, t1Ms: 1700000001000}, []int{8080}, 1)

		require.Equal(t, 0, conf.writes)
		assert.Empty(t, logs.All(),
			"an ordinary recording, which asked for no join, was told its join is broken: %v",
			logs.All())
	})
}

/*
A HALF-CONFIGURED OPT-IN MUST NAME THE VARIABLE, NOT THE FIELD.

Setting either of KEPLOY_UI_CAPTURE_ID or KEPLOY_UI_SESSION_NONCE
switches the join on, so setting exactly one is the mistake a user makes
by typing one export and forgetting the other -- and the documented
behaviour is that the annotation is refused "with a diagnostic naming the
missing variable".

It was not. The error carried `sessionNonce`, a field name that appears
nowhere in the user's shell, and a hint listing all three variables
including the two that were correct. The doc asserted a behaviour the
code did not have.
*/
func TestStorableHintNamesTheEnvironmentVariable(t *testing.T) {
	base := func() *models.UIJoinAnnotation {
		return &models.UIJoinAnnotation{
			SpecVersion:      models.UIJoinSpecVersion,
			CaptureID:        "cap-1",
			SessionNonce:     "nonce-1",
			T0WallMs:         1700000000000,
			T1WallMs:         1700000060000,
			IngressPorts:     []int{8080},
			AppOrigins:       []string{"https://app.example.com"},
			CanonicalKeySpec: "canonical-key.v1",
		}
	}

	noNonce := base()
	noNonce.SessionNonce = "  "
	hint := uiJoinStorableHint(noNonce)
	if !strings.Contains(hint, EnvUIJoinNonce) {
		t.Errorf("the hint does not name %s: %q", EnvUIJoinNonce, hint)
	}
	if strings.Contains(hint, EnvUIJoinAppOrigins) {
		t.Errorf("the hint sends the user to %s, which is not the problem: %q",
			EnvUIJoinAppOrigins, hint)
	}

	/*
	 * BOTH HALVES, and the negative one is what makes it a test.
	 *
	 * The fallback message ALSO contains KEPLOY_UI_CAPTURE_ID, so a bare
	 * Contains check cannot tell the targeted hint from the
	 * check-all-three one -- MEASURED: deleting the captureId branch
	 * entirely left this assertion green. The nonce case above has the
	 * discriminating NotContains; this one did not.
	 */
	noCapture := base()
	noCapture.CaptureID = ""
	hCapture := uiJoinStorableHint(noCapture)
	if !strings.Contains(hCapture, EnvUIJoinCaptureID) {
		t.Errorf("the hint does not name %s: %q", EnvUIJoinCaptureID, hCapture)
	}
	if strings.Contains(hCapture, EnvUIJoinAppOrigins) {
		t.Errorf("the hint sends the user to %s, which is not the problem: %q",
			EnvUIJoinAppOrigins, hCapture)
	}

	/*
	 * UNSET IS NOT BLANK, and this is the case a user actually reaches:
	 * one export typed, the other forgotten. MEASURED before the fix,
	 * with only KEPLOY_UI_CAPTURE_ID exported: the hint named
	 * KEPLOY_UI_SESSION_NONCE correctly and then said it "is set to an
	 * empty or whitespace-only value" about a variable that was not set
	 * at all -- sending the reader to look for an export that does not
	 * exist.
	 *
	 * t.Setenv cannot express this; the variable has to be absent from
	 * the environment, which is why this case unsets it explicitly.
	 */
	t.Setenv(EnvUIJoinCaptureID, "cap-1")
	if err := os.Unsetenv(EnvUIJoinNonce); err != nil {
		t.Fatalf("unset %s: %v", EnvUIJoinNonce, err)
	}
	unsetNonce := base()
	unsetNonce.SessionNonce = ""
	hUnset := uiJoinStorableHint(unsetNonce)
	if !strings.Contains(hUnset, EnvUIJoinNonce+" is not set") {
		t.Errorf("an UNSET variable was reported as blank: %q", hUnset)
	}

	// And the genuinely-blank spelling still says so.
	t.Setenv(EnvUIJoinNonce, "   ")
	blankNonce := base()
	blankNonce.SessionNonce = "   "
	hBlank := uiJoinStorableHint(blankNonce)
	if !strings.Contains(hBlank, "empty or whitespace-only") {
		t.Errorf("a BLANK variable was not reported as blank: %q", hBlank)
	}

	/*
	 * AND IT FALLS BACK. Storable refuses for reasons no single variable
	 * explains -- an implausible timestamp here -- and a hint that named
	 * one variable for those would send the reader to a value that is
	 * perfectly correct.
	 */
	badClock := base()
	badClock.T0WallMs = 1700000000 // seconds, not milliseconds
	full := uiJoinStorableHint(badClock)
	for _, v := range []string{EnvUIJoinAppOrigins, EnvUIJoinCaptureID, EnvUIJoinNonce} {
		if !strings.Contains(full, v) {
			t.Errorf("the fallback hint omits %s: %q", v, full)
		}
	}
}
