package record

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

/*
The RECORD-TIME half of the browser/backend join.

pkg/models/uijoin.go defines the annotation and every rule about what a
storable one looks like. This is the only thing in the recorder that
produces one. The two halves are joined OFFLINE, by content hashes — see
that file's header for why a correlation header is unavailable to this
design — so the annotation carries only what content alone cannot
supply.

WHY ENVIRONMENT VARIABLES. THREE of the eight fields are unknowable to
the recorder: CaptureID names the capture session this test-set pairs
with, SessionNonce proves both halves came from the SAME run rather
than two runs of the same script, and AppOrigins names the origins the
browser half was served from. Nothing the recorder can observe produces
any of them, so all three arrive from whatever starts both halves --
the origins under the spelling pkg/models/uijoin.go's own commentary
documents: strings.Split(os.Getenv("KEPLOY_APP_ORIGINS"), ","). Its
REFUSAL MESSAGES name the offending entry as `appOrigins[N]` rather than
the variable, which is the more useful half for an operator with three
origins to check.

WHY THE PORTS ARE OBSERVED, NOT CONFIGURED. IngressPorts exists so that
an exchange on a port nothing was listening on classifies
NO_INGRESS_OBSERVED rather than silently failing to join. A configured
value would describe INTENT; models.TestCase.AppPort records the port an
ingress request actually arrived on, which is the question the joiner is
asking. Observing also means the list cannot claim a port this recording
never saw — a claim that would turn "nothing was listening there" into
"the join is broken and nothing says why".

WHAT IDENTITY THIS CANNOT SUPPLY. CaptureID and SessionNonce are read
from PROCESS-GLOBAL environment at stop, so every session in one process
writes the same pair. record.go's own comment describes a DaemonSet
embedding where Start is re-entered per session; in that shape the nonce
stops proving what it exists to prove -- that both halves came from the
SAME run rather than two runs of the same script -- because two sessions
are indistinguishable. There is no per-session plumbing today: no config
field, no flag, no argument. A caller that needs it (an agent driving
several recordings in one process is the realistic one) has to set the
variables between sessions, which is not something a single process can
do safely for itself. Worth fixing before that embedding is used for
joins; stated here rather than discovered there.

WHY IT IS WRITTEN AT STOP, ONCE. validateStructure refuses an empty
IngressPorts list, on the grounds that no correct producer emits one —
and at start no ingress has been observed, so there is no storable
annotation to write yet. A recording killed before it stops therefore
leaves no annotation, which is the honest outcome: it also leaves no
finished test-set to join against.

WHY A BROKEN OPT-IN IS LOUD. Setting the capture id is an explicit
request for a join. If the annotation that request implies cannot be
stored — a malformed KEPLOY_APP_ORIGINS entry is the realistic cause — writing
nothing and continuing quietly hands back a test-set that can never
join, discovered much later by a joiner that can only say
FOREIGN_ORIGIN. The recording itself is still valid keploy data, so this
does not fail the session; it refuses to write a broken annotation and
says exactly why.
*/

// The environment variables the recorder reads to build the annotation.
//
// ALL THREE ARE KEPLOY_-PREFIXED, deliberately.
//
// `APP_ORIGINS` unprefixed is a name an application may well define for
// itself -- it reads as the
// application's own configuration, not a recorder's. keploy reads it from
// the environment it is LAUNCHED in, which is the same shell, CI job or
// pod spec that configures the app, so a value set for the application
// would have been picked up here as though it were meant for us: origins
// the user never chose to publish, landing in a published annotation.
// (The child relationship runs the other way -- keploy's environment
// flows INTO the app -- so it is the shared environment that is the
// hazard, not the spawn.) Renaming is free only while nothing depends on
// the name, which is now; every reference lives in this repository.
const (
	EnvUIJoinCaptureID  = "KEPLOY_UI_CAPTURE_ID"
	EnvUIJoinNonce      = "KEPLOY_UI_SESSION_NONCE"
	EnvUIJoinAppOrigins = "KEPLOY_APP_ORIGINS"
)

// uiJoinPorts accumulates the ports ingress was OBSERVED on.
type uiJoinPorts struct {
	mu   sync.Mutex
	seen map[int]struct{}
}

func newUIJoinPorts() *uiJoinPorts { return &uiJoinPorts{seen: map[int]struct{}{}} }

// observe records one ingress port. Port 0 is dropped rather than
// stored: models.uiJoinPortInRange refuses it, on the grounds that 0
// means "the kernel picks one" and can never be a port something was
// observed on — so a test case carrying 0 is a test case whose port was
// never populated, not a test case on port zero.
// The `p == nil` arm is DEFENSIVE, NOT A GUARD, and it is unreachable:
// newUIJoinPorts never returns nil and there is exactly one PRODUCTION
// construction site (record.go; the tests add three more). It is
// removable with the package green, and that
// is expected rather than a coverage gap. Port 0, by contrast, is
// reachable and is a real decision.
func (p *uiJoinPorts) observe(port uint16) {
	if p == nil || port == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[int(port)] = struct{}{}
}

// sorted returns the observed ports in ascending order.
//
// SORTED BECAUSE THE RESULT IS PERSISTED. Go randomises map iteration,
// so an unsorted list would put a different byte sequence in
// keploy/<id>/config.yaml on every run of the same recording — two runs
// that observed exactly the same ports producing two different files.
// Nothing downstream depends on the order, which is precisely why
// nothing downstream would catch it.
func (p *uiJoinPorts) sorted() []int {
	// Unreachable, same as observe()'s. Defensive, not a guard.
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		// NIL, NOT AN EMPTY SLICE. The two marshal differently — an
		// empty list writes `ingressPorts: []` into config.yaml, which
		// asserts the recorder looked and saw none, while nil is absent.
		// validateStructure refuses both, so this changes no verdict; it
		// changes what the file on disk claims about a recording that
		// observed nothing.
		return nil
	}
	out := make([]int, 0, len(p.seen))
	for port := range p.seen {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// uiJoinEnv reads one variable, trimmed. models.UIJoinAnnotation
// normalises its own fields, but trimming here keeps what this function
// REPORTS about emptiness honest: a variable set to a single space is
// not a capture id.
func uiJoinEnv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

// uiJoinRequested reports whether the operator asked for a join at all.
//
// EITHER identifier, not both. Setting one and omitting the other is a
// mistake worth reporting — Validate names the missing field — and
// requiring both here would turn that mistake into silence.
func uiJoinRequested() bool {
	return uiJoinEnv(EnvUIJoinCaptureID) != "" || uiJoinEnv(EnvUIJoinNonce) != ""
}

// uiJoinOrigins splits KEPLOY_APP_ORIGINS on commas, dropping empty entries.
//
// An empty list is legitimate and is NOT an error here: a browser can
// genuinely have no usable origin, which is why models.joinability
// treats "no origins" as not-joinable rather than not-storable. Dropping
// empty entries means `KEPLOY_APP_ORIGINS="http://a,,"` is one origin rather
// than three, one of which would be refused.
func uiJoinOrigins(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildUIJoinAnnotation assembles the annotation from the environment
// and the observed ports. It does no validation; the caller decides what
// a refusal means.
func buildUIJoinAnnotation(span uiJoinSpan, ports []int) *models.UIJoinAnnotation {
	return &models.UIJoinAnnotation{
		SpecVersion:      models.UIJoinSpecVersion,
		CaptureID:        uiJoinEnv(EnvUIJoinCaptureID),
		SessionNonce:     uiJoinEnv(EnvUIJoinNonce),
		T0WallMs:         span.t0Ms,
		T1WallMs:         span.t1Ms,
		IngressPorts:     ports,
		AppOrigins:       uiJoinOrigins(os.Getenv(EnvUIJoinAppOrigins)),
		CanonicalKeySpec: models.UIJoinCanonicalKeySpecs[0],
	}
}

// persistUIJoinAnnotation writes the annotation onto the test-set's
// config, preserving every field models.TestSet models -- which is not
// the same as "everything already there". The store round-trips through
// models.TestSet, so an UNMODELLED top-level key in config.yaml does not
// survive; measured with `totallyUnknown: {keep: me}`, which parses and
// is then dropped.
// That is pre-existing store behaviour and not something this write
// introduces, but it is not a promise this function can make.
//
// READ-MODIFY-WRITE, because createConfigWithMetadata may already have
// written a config for this test-set carrying the operator's own
// metadata, and SetUIJoinAnnotation stores under one namespaced key
// inside that same map. Constructing a fresh models.TestSet here would
// silently drop preScript, postScript, template and every user metadata
// key.
//
// ReadForUpdate, NOT Read. Read never fails for a MISSING file -- it
// returns a default -- which makes it look safe for the first-write
// case. It returns that same default for a file that is PRESENT AND
// UNREADABLE, because it swallows an unmarshal error so secret
// hydration still has somewhere to land. Correct for a reader,
// catastrophic for a writer.
//
// Measured against the real store with one stray indent in config.yaml:
// preScript, postScript, appCommand and the operator's metadata were all
// replaced by the zero struct, and warnIfDroppingFields logged that it
// could not check rather than stopping it. That is the bug 20aae271
// fixed for --updateTemplate, re-entered through a new door.
//
// A MISSING file is still not an error, so the first-write case really
// does need no special handling — which is what the old comment was
// reaching for.
//
// REACHED BY RUNTIME ASSERTION, NOT BY WIDENING THE INTERFACE.
//
// record.TestSetConfig is EXPORTED and implemented outside this
// repository -- record.go carries the house rule twenty lines from here,
// about TestDB: "enterprise implements that interface; don't break it",
// and reaches DeleteTests by assertion for exactly this reason. An
// earlier version of this change added ReadForUpdate to the interface
// instead, which stops any narrower implementation compiling at the next
// bump. (tools.TestSetConfig and replay.TestSetConfig do declare it, so
// a store shared with those already has the method -- but that is a
// guess about a repository this one cannot see, and the house rule
// exists because the guess was wrong before.)
//
// AND IT REFUSES RATHER THAN DEGRADING, which is where this differs from
// the DeleteTests precedent. Falling back to Read is precisely the
// catastrophic path described above: it cannot distinguish a missing
// config from an unreadable one, so the fallback would overwrite the
// user's config.yaml with the zero struct. There is nothing safe to
// degrade to, so an implementation without ReadForUpdate gets no
// annotation and an error saying why.
type testSetConfigReadForUpdate interface {
	ReadForUpdate(ctx context.Context, testSetID string) (*models.TestSet, error)
}

func (r *Recorder) persistUIJoinAnnotation(ctx context.Context, testSetID string, a *models.UIJoinAnnotation) error {
	reader, ok := r.testSetConf.(testSetConfigReadForUpdate)
	if !ok {
		return fmt.Errorf("this test-set config store does not implement ReadForUpdate, " +
			"and reading through Read cannot tell a missing config.yaml from an unreadable " +
			"one -- writing the annotation would replace the file with a zero document")
	}
	ts, err := reader.ReadForUpdate(ctx, testSetID)
	if err != nil {
		// REFUSE, do not write. An annotation is worth less than the
		// config it would replace.
		return err
	}
	if ts == nil {
		ts = &models.TestSet{}
	}
	/*
	 * UNREACHABLE, and kept anyway. SetUIJoinAnnotation's only failure
	 * modes are a nil test-set (excluded one line above), a nil
	 * annotation (excluded by the caller) and a structure it refuses --
	 * and the caller has already run the identical
	 * normalized().validateStructure() via a.Storable(). So replacing
	 * this with `_ =` leaves the package green, necessarily.
	 *
	 * Kept because the alternative is a silent write of whatever
	 * SetUIJoinAnnotation did manage to do, and because the day
	 * Storable() and SetUIJoinAnnotation stop agreeing is exactly the
	 * day this matters. It is not a guard anything tests, and should
	 * not be described as one.
	 */
	if err := models.SetUIJoinAnnotation(ts, a); err != nil {
		return err
	}
	return r.testSetConf.Write(ctx, testSetID, ts)
}

/*
uiJoinStorableHint names the VARIABLE to go and look at, not the field.

THE HALF-CONFIGURED OPT-IN is the case this exists for, and it is the
documented one: setting either of KEPLOY_UI_CAPTURE_ID or
KEPLOY_UI_SESSION_NONCE switches the feature on, so setting exactly one
is a mistake a user makes by typing one export and not the other. What
they got for it was `sessionNonce` -- a field name that appears nowhere
in their shell -- plus a hint listing all three variables, including the
two that were correct. The doc claimed it named the missing variable; it
did not.

DERIVED FROM THE VALUES, not by matching on the error string. The
annotation carries what the environment supplied, so asking it which
field is blank is exact; parsing a message is a second copy of the rule
that drifts the first time the wording changes.

Falls back to the full list, because Storable also refuses for reasons no
single variable explains -- an implausible timestamp, an unknown key
spec, a malformed origin.
*/
func uiJoinStorableHint(a *models.UIJoinAnnotation) string {
	/*
	 * UNSET AND BLANK ARE DIFFERENT MISTAKES, and telling the user the
	 * wrong one sends them hunting for an export that does not exist.
	 *
	 * Typing one export and forgetting the other is the documented,
	 * likeliest way to reach this path -- setting EITHER variable
	 * switches the join on. MEASURED with only KEPLOY_UI_CAPTURE_ID set:
	 * the hint named KEPLOY_UI_SESSION_NONCE, correctly, and then said it
	 * "is set to an empty or whitespace-only value" about a variable that
	 * was not set at all.
	 *
	 * The annotation's VALUES cannot separate these -- an unset variable
	 * and a blank one both arrive as "" -- so this asks the environment
	 * directly. Reading the error string would not have helped either;
	 * only os.LookupEnv distinguishes them.
	 */
	describe := func(name, value string) string {
		if _, set := os.LookupEnv(name); !set {
			return name + " is not set"
		}
		if strings.TrimSpace(value) == "" {
			return name + " is set to an empty or whitespace-only value"
		}
		return ""
	}

	var captureID, nonce string
	if a != nil {
		captureID, nonce = a.CaptureID, a.SessionNonce
	}
	var problems []string
	if strings.TrimSpace(captureID) == "" {
		problems = append(problems, describe(EnvUIJoinCaptureID, captureID))
	}
	if strings.TrimSpace(nonce) == "" {
		problems = append(problems, describe(EnvUIJoinNonce, nonce))
	}
	if len(problems) > 0 {
		return strings.Join(problems, " and ") + "; setting either of " +
			EnvUIJoinCaptureID + " or " + EnvUIJoinNonce +
			" switches the join on, so both must carry a value"
	}
	// No single variable explains this one -- an implausible timestamp,
	// an unknown key spec, a malformed origin.
	return "check " + EnvUIJoinAppOrigins + ", " + EnvUIJoinCaptureID +
		" and " + EnvUIJoinNonce
}

// recordUIJoinAnnotation is the whole of the stop-time behaviour.
//
// Returns without doing anything when no join was requested, which is
// the ordinary keploy recording and must stay untouched.
func (r *Recorder) recordUIJoinAnnotation(ctx context.Context, testSetID string, span uiJoinSpan, ports []int, testCount int) {
	if !uiJoinRequested() {
		// The ordinary keploy recording. Nothing was asked for, so
		// nothing is owed -- and this must stay silent.
		return
	}
	/*
	 * ASKED FOR AND CANNOT BE DONE IS NOT THE SAME AS NOT ASKED FOR.
	 *
	 * These three conditions were one `||`, so a join that was REQUESTED
	 * and then could not be attempted returned with no log at any level.
	 * The header of this file promises the opposite -- WHY A BROKEN
	 * OPT-IN IS LOUD -- and that promise only held for the Storable()
	 * arm further down.
	 *
	 * Neither of these is a user error, which is exactly why they are
	 * Error and not Warn: the user did everything right, set the
	 * variables, and got no annotation. `r.testSetConf` is non-nil in OSS
	 * (cli/provider/core_service.go wires it), so this bites the
	 * enterprise/DaemonSet embedding -- the one this file already spends
	 * a paragraph worrying about, and the one least able to debug it.
	 */
	if r.testSetConf == nil {
		r.logger.Error("a UI join was requested but this recorder has no test-set config store, "+
			"so the annotation cannot be written; the recording itself is unaffected",
			zap.String("testSet", testSetID))
		return
	}
	if testSetID == "" {
		r.logger.Error("a UI join was requested but the recording produced no test-set id, " +
			"so there is nothing to annotate; the recording itself is unaffected")
		return
	}
	/*
	 * NOTHING PERSISTED, NOTHING TO JOIN -- and writing anyway breaks the
	 * next `keploy test`.
	 *
	 * The annotation is written onto keploy/<id>/config.yaml, and that
	 * write CREATES the test-set directory when nothing else has. On a
	 * disk-full recording, where every InsertTestCase fails, it can be
	 * the only writer that succeeds -- and the session then leaves
	 * behind a test-set holding a config and no test cases.
	 *
	 * THIS CLOSES ONE DOOR OF THREE, not the only one:
	 * createConfigWithMetadata (record.go, under `--metadata`) and the
	 * pcap MkdirAll (under `--capture-packets`) each create the
	 * directory independently and earlier. With either flag the phantom
	 * exists whatever this arm decides. The root cause is that record
	 * leaves an empty test-set behind at all and replay then exits 1 on
	 * it; that is not fixed here.
	 *
	 * That is not a harmless artifact. GetAllTestSetIDs enumerates it,
	 * replay classifies it TestSetStatusNoTestsToRun, and
	 * replay.go's roll-up sets `testSetResult = false` for that status --
	 * so replayRunOutcome returns EXIT 1. Every subsequent `keploy test`
	 * fails, for a reason no output explains, until someone deletes the
	 * directory by hand.
	 *
	 * The joiner has nothing to do with an empty test-set either: there
	 * is no exchange to pair with a browser capture. So a recording that
	 * persisted no test cases gets no annotation, and says so.
	 *
	 * ONE PATH REACHES HERE WHERE THE REASONING ABOVE DOES NOT APPLY: an
	 * all-revoked recording. `testCount -= deleted` can bring the count
	 * to 0 after inserts succeeded, so the directory already exists and
	 * refusing the annotation prevents no phantom -- it only costs the
	 * join. Left as-is because the alternative is annotating a test-set
	 * with no test cases, which is the state replay exits 1 on; but the
	 * refusal is doing nothing useful on that path and the Debug line is
	 * the only trace.
	 */
	if testCount <= 0 {
		r.logger.Debug("a UI join was requested but no test case was persisted, "+
			"so there is no test-set to join and no annotation is written",
			zap.String("testSet", testSetID))
		return
	}
	/*
	 * NO INGRESS OBSERVED IS NOT A BROKEN OPT-IN.
	 *
	 * validateStructure refuses an empty IngressPorts list, so this
	 * would fall into the Error arm below and blame KEPLOY_APP_ORIGINS,
	 * KEPLOY_UI_CAPTURE_ID and KEPLOY_UI_SESSION_NONCE — three variables
	 * that are, in this case, perfectly correct. The recording simply
	 * captured nothing, which keploy reports in its own right.
	 *
	 * There is no guard at the call site any more -- and there does not
	 * need to be: a diagnostic that is only right because of where it is
	 * called from is a diagnostic waiting to be wrong. This one is
	 * correct wherever it is called.
	 */
	if len(ports) == 0 {
		/*
		 * "NO PORT" IS NOT "NO INGRESS", and this arm asserted the second
		 * while only ever measuring the first.
		 *
		 * observe() DROPS port 0 -- see its comment: zero means "the
		 * kernel picks one" and can never be a port something was
		 * observed on, so a test case carrying 0 is one whose port was
		 * never populated. A recording in which every exchange carried
		 * zero therefore arrives here with an empty list, exactly like a
		 * recording nothing reached.
		 *
		 * AND ONLY THE FIRST IS REACHABLE HERE. record.go calls
		 * `uiJoinObserved.observe(testCase.AppPort)` for EVERY test
		 * case, deliberately before InsertTestCase so a failed insert
		 * still records the port -- and the testCount guard above has
		 * already returned for testCount <= 0. So reaching this line
		 * means test cases were persisted, observe() ran at least once
		 * per test case, and every AppPort it saw was zero. There is no
		 * benign reading left.
		 *
		 * Hence Error, not Debug, and unconditionally. An earlier draft
		 * of this fix guarded the Error on `testCount > 0` and kept the
		 * old Debug line beneath it -- but the guard above makes that
		 * condition always true, so the Debug branch was dead code
		 * dressed as a fallback. Debug is suppressed on a default run
		 * anyway: a user who set the variables and got no annotation had
		 * nothing to read.
		 */
		r.logger.Error("a UI join was requested and test cases were recorded, but none "+
			"carried an ingress port, so the annotation cannot say where traffic arrived "+
			"and is not written; the recording itself is unaffected",
			zap.String("testSet", testSetID),
			zap.Int("testCases", testCount))
		return
	}
	a := buildUIJoinAnnotation(span, ports)
	// STORABLE, not Joinable. An annotation with no origins is storable
	// and not joinable, and that is a state a correct producer reaches —
	// see models.ErrUIJoinNotJoinable. Refusing it here would discard the
	// capture id and nonce over a condition the joiner is designed to
	// report.
	if err := a.Storable(); err != nil {
		r.logger.Error("a UI join was requested but the annotation cannot be stored; "+
			"this test-set will not join to any browser capture",
			zap.Error(err),
			zap.String("testSet", testSetID),
			zap.String("hint", uiJoinStorableHint(a)))
		return
	}
	if err := r.persistUIJoinAnnotation(ctx, testSetID, a); err != nil {
		r.logger.Error("failed to write the UI join annotation; this test-set will not "+
			"join to any browser capture",
			zap.Error(err), zap.String("testSet", testSetID))
		return
	}
	r.logger.Info("recorded the UI join annotation",
		zap.String("testSet", testSetID),
		zap.String("captureId", a.CaptureID),
		zap.Ints("ingressPorts", a.IngressPorts),
		zap.Int("appOrigins", len(a.AppOrigins)))
}

// uiJoinSpan is the recording's wall-clock span.
//
// IT EXISTS SO THE ANNOTATION'S TWO TIMESTAMPS ARE NOT A POSITIONAL PAIR.
// They were two bare int64 parameters, `t0Ms, t1Ms`, and the call site
// assembled them as two adjacent expressions of the same type -- so
// exchanging them compiled, ran, and wrote every annotation backwards,
// and the suite caught it only SOMETIMES: the harness usually finishes
// inside a millisecond, and T0 == T1 makes validateStructure's
// `T1WallMs < T0WallMs` check pass.
//
// THE CATCH WAS TIMING-DEPENDENT, and that is the whole finding. It was
// measured several times and came out differently every time, on one
// machine within hours. Each figure got written down as a property, and
// none of them was one -- the same error as the original claim, repeated
// while correcting it. So no rate appears here: a test that catches a
// real defect on some runs and not others is a latent flaky test sitting
// on the green side, and a number would only invite a fourth.
//
// THIS IS NOT A COMPILE-TIME GUARANTEE. newUIJoinSpan takes two
// time.Time values, so `newUIJoinSpan(sessionStart, captureStart)` builds
// and vets clean -- measured. The swap is caught DETERMINISTICALLY
// instead:
// TestStart_TakesT0FromTheCaptureStartNotTheProcessStart kills it 10 out
// of 10, because slowSetupInstr.Setup spins until the wall clock advances,
// so the 1ms margin is guaranteed rather than raced for. A flaky catch
// became a reliable one; the type system is not what does it.
//
// The remaining orderable decision -- which end is which -- moved into
// newUIJoinSpan, where it is one function over known inputs and can be
// asserted without racing a clock.
type uiJoinSpan struct {
	// t0Ms is when capture began.
	t0Ms int64
	// t1Ms is when the recording finished.
	t1Ms int64
}

// newUIJoinSpan closes the span NOW, at the moment of the call.
//
// t1 is taken here rather than passed in, which is what removes the
// swappable pair from the call site: there is no second timestamp there
// to hand over in the wrong position.
func newUIJoinSpan(captureStart, sessionStart time.Time) uiJoinSpan {
	return uiJoinSpan{
		t0Ms: uiJoinT0Ms(captureStart, sessionStart),
		t1Ms: time.Now().UnixMilli(),
	}
}

// uiJoinT0Ms picks the annotation's T0.
//
// captureStart is when GetTestAndMockChans returned, i.e. when the agent
// actually began capturing. pkg/models/uijoin.go asks this field to be
// good enough "for skew estimation only", and this value excludes
// instrumentation setup and agent bring-up always, and the app's own
// boot ONLY under docker-compose: for every other command type
// instrumentation.Run starts the app AFTER this stamp. The other two
// sites that state this carry the qualifier; this one said "and the app
// booting" flatly, which is wrong for every non-compose recording —
// tens of seconds that would otherwise be charged to clock skew.
//
// It is the ZERO TIME on a session that never got that far. Falling back
// to sessionStart there rather than writing 0: a zero T0 is refused by
// validateStructure, so the annotation would be dropped entirely, and
// the recording still has a real start. In practice the fallback is
// unreachable from the stop defer — no capture means no observed ports,
// which returns at the no-ingress arm before T0 is ever read — so this
// is about the function being correct on its own inputs rather than
// about a path anything takes.
func uiJoinT0Ms(captureStart, sessionStart time.Time) int64 {
	if captureStart.IsZero() {
		return sessionStart.UnixMilli()
	}
	return captureStart.UnixMilli()
}
