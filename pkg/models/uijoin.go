package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/v2/bson"
	"golang.org/x/net/idna"
)

/*
Record-time annotation that lets a browser capture be joined to this
test-set offline.

WHY THIS IS NOT A HEADER. The obvious design is to stamp a correlation id
on every request and match on it. That is unavailable here, twice over:

  - FlakyHeaders already auto-noises traceparent, tracestate, baggage,
    x-request-id and x-correlation-id, annotated "unique trace-id +
    span-id per request". Keying a join on the exact headers the matcher
    is engineered to forgive is incoherent — and at replay the browser
    mints a fresh id, so a mock keyed on one could never match.
  - Stamping a custom header on a customer's production traffic trips
    CORS preflight on every cross-origin call, breaking their app at
    install time. An observability feature must not do that.

So the join is computed OFFLINE from content hashes, and this annotation
carries only what content alone cannot supply: which capture session this
test-set belongs to, when it ran, and what the two planes agreed to
canonicalise by.

WHY IT IS SESSION-GRAIN. Eight fields on the test-set -- six scalars and
two lists, countable from the json tags below -- not a field on every
test case. Per-exchange annotation would change the on-disk test-case
format, the upload body and the matcher's view of a request. This changes
none of them: it rides in TestSet.Metadata, which is already a free-form
map persisted to keploy/<id>/config.yaml.
*/

// Metadata key under which the annotation is stored. Namespaced so it
// cannot collide with a user's own metadata.
const UIJoinMetadataKey = "io.keploy.ui-join/v1"

/*
The canonical-key specs this build can join under.

CLOSED, and checked. Both planes must have normalized request keys the
same way or the join is confidently wrong, and this field is the only
place that is detectable — so an unrecognised value has to be refused
rather than recorded. Adding an entry here is a deliberate act that says
"this build implements that spec".

The identifier is the spec's IDENTITY, not a path: spec/canonical-key.v1.json
is where it lives in the ui-capture repo, and moving the file must not
invalidate every annotation ever written.
*/
var UIJoinCanonicalKeySpecs = []string{"canonical-key.v1"}

// UIJoinKnownKeySpec reports whether this build can join under spec.
func UIJoinKnownKeySpec(spec string) bool {
	// TRIMS, though no in-package path arrives untrimmed: normalized()
	// trims CanonicalKeySpec before validateStructure reads it. It stays
	// because this function is EXPORTED — a producer asking "is this a
	// spec you know?" about a value read from a config file or an env
	// var reaches it directly, and that is the caller who has
	// whitespace. Pinned by
	// TestKnownKeySpecToleratesPaddingForExternalCallers.
	spec = strings.TrimSpace(spec)
	for _, known := range UIJoinCanonicalKeySpecs {
		if spec == known {
			return true
		}
	}
	return false
}

/*
The window a recording timestamp must fall in, in epoch milliseconds.

2020-01-01 to 2100-01-01. Wide enough that no real recording is refused,
narrow enough to catch the two mistakes that actually happen: seconds
passed where millis are expected (which lands in 1970) and a mis-decoded
read (which lands anywhere).
*/
const (
	uiJoinEpochFloorMs int64 = 1577836800000
	uiJoinEpochCeilMs  int64 = 4102444800000
)

// UIJoinSpec version. Bumped only when the join algorithm changes in a way that
// makes an older annotation unusable, so a consumer can refuse rather
// than misinterpret.
const UIJoinSpecVersion = 1

/*
UIJoinAnnotation is the server-side half of the browser/backend join.

Every field answers a question content hashing cannot:

	CaptureID     which browser capture session this test-set pairs with
	SessionNonce  proves both halves came from the SAME run, not two runs
	              of the same script — without it, replaying a scenario
	              twice produces two test-sets that both "match" one
	              browser capture
	T0WallMs      when capture began -- not when the recorder process
	              started; see uiJoinT0Ms in the producer. It excludes
	              instrumentation setup and agent bring-up always, and
	              the app's own boot only under docker-compose: for every
	              other command type instrumentation.Run starts the app
	              AFTER this stamp. For skew estimation only.
	T1WallMs      when it stopped; 0 while still recording. NOT tightened
	              the way T0WallMs is: it is taken inside the stop defer,
	              after the graceful-shutdown notice and the drain
	              groups, so it carries up to 130s of teardown from those
	              alone -- 10s of notice plus four 30s drains -- and then
	              the per-name DeleteTests loop and two telemetry calls
	              on top. (utils.Stop IS in that tail -- same defer, above
	              the notice -- but costs nothing, being a bare cancel.) The same argument for
	              tightening applies here; it has not been made yet.
	IngressPorts  the ports ingress was OBSERVED arriving on, so an
	              exchange on an unobserved port is NO_INGRESS_OBSERVED
	              rather than silently unjoined. Observed, not
	              "listening on", and the difference is the point: a
	              configured or bound-port list describes INTENT and can
	              claim a port this recording never saw, which turns
	              "nothing was listening there" into "the join is broken
	              and nothing says why". The producer takes them from
	              models.TestCase.AppPort for that reason.
	AppOrigins    the origins the app under test was served from, which is
	              what makes an exchange FOREIGN_ORIGIN instead of missing
	CanonicalKeySpec
	              the key spec BOTH planes used. A join computed under two
	              different normalizers is confidently wrong, and this is
	              the only place that can be detected.
*/
// The codec tags all spell the SAME wire names as the map
// SetUIJoinAnnotation writes, and TestTheWireKeysAreTheStructTags pins
// that. The bson tags used to be snake_case while the wire keys were
// camelCase, so anything that ever marshaled this struct to BSON would
// have produced a document this package's own reader rejects — a
// contradiction sitting one line away from the comment promising "the
// field names are the same over every codec".
type UIJoinAnnotation struct {
	SpecVersion      int      `json:"specVersion" yaml:"specVersion" bson:"specVersion"`
	CaptureID        string   `json:"captureId" yaml:"captureId" bson:"captureId"`
	SessionNonce     string   `json:"sessionNonce" yaml:"sessionNonce" bson:"sessionNonce"`
	T0WallMs         int64    `json:"t0WallMs" yaml:"t0WallMs" bson:"t0WallMs"`
	T1WallMs         int64    `json:"t1WallMs" yaml:"t1WallMs" bson:"t1WallMs"`
	IngressPorts     []int    `json:"ingressPorts" yaml:"ingressPorts" bson:"ingressPorts"`
	AppOrigins       []string `json:"appOrigins" yaml:"appOrigins" bson:"appOrigins"`
	CanonicalKeySpec string   `json:"canonicalKeySpec" yaml:"canonicalKeySpec" bson:"canonicalKeySpec"`
}

var (
	ErrUIJoinAbsent        = errors.New("test-set carries no UI join annotation")
	ErrUIJoinUnsupported   = errors.New("UI join annotation has an unsupported spec version")
	ErrUIJoinIncomplete    = errors.New("UI join annotation is missing a required field")
	ErrUIJoinNotAnnotation = errors.New("UI join metadata is not an annotation")

	/*
	 * "CORRECTLY RECORDED, CANNOT BE JOINED" IS NOT "CORRUPT".
	 *
	 * ErrUIJoinIncomplete used to carry both, and the two call for
	 * opposite responses: a missing field means a producer wrote a
	 * broken record and somebody should go find out why; an
	 * un-joinable capture means the producer did its job and the join
	 * simply is not available for this page.
	 *
	 * They were conflated in the one place it costs something. The
	 * browser half MINTS the un-joinable case on purpose — see
	 * CaptureIdentityBuilder.build in @keploy/capture-sdk, where an
	 * opaque origin (a sandboxed iframe, a data: or file: URL) yields
	 * an identity with an empty appOrigins and a comment calling that
	 * "a real answer". This package then refused to store it, so the
	 * captureId, the nonce, the timestamps and the ports — every one
	 * of them present and correct — were dropped on the floor, and
	 * the test-set read back ErrUIJoinAbsent: "carries no UI join
	 * annotation", which is the ordinary never-attempted case. The
	 * two halves of one protocol disagreed about what an empty
	 * appOrigins means, and the disagreement destroyed data.
	 *
	 * Deliberately NOT wrapping ErrUIJoinIncomplete: errors.Is must
	 * be able to tell them apart, which is the entire point.
	 */
	ErrUIJoinNotJoinable = errors.New("UI join annotation records a capture that cannot be joined")

	/*
	 * "YOU LOST A RACE" IS NOT "YOUR RECORD IS CORRUPT" EITHER.
	 *
	 * The block above argues that one sentinel carrying two meanings
	 * that call for opposite responses is worth a new error value, and
	 * the write-once refusal in FinishUIJoinAnnotation was hung on
	 * ErrUIJoinIncomplete — "missing a required field" — for an
	 * annotation that is missing nothing and is already complete. A
	 * third meaning on the same sentinel, in the commit that argued
	 * against exactly that.
	 *
	 * It matters to the obvious producer. A stop path that retries with
	 * a fresh clock reading — FinishUIJoinAnnotation(ts,
	 * time.Now().UnixMilli()) in a loop — gets a hard error on every
	 * attempt after the first, under a sentinel telling it the record is
	 * corrupt. With its own value the retry can treat it as success,
	 * which is what a retry wants: the stop time is already recorded.
	 */
	ErrUIJoinAlreadyFinished = errors.New("UI join annotation already carries a stop time")
)

// Validate reports whether the annotation can be used for a join.
//
// Fails CLOSED on every incomplete case. A partial annotation is worse
// than none: it looks joinable and produces edges nobody can trust, and
// the failure is silent because a wrong join still returns pairs.
//
// Two distinguishable refusals, because they call for opposite
// responses — ErrUIJoinIncomplete/ErrUIJoinUnsupported for a record no
// correct producer emits, ErrUIJoinNotJoinable for one that is faithful
// and simply cannot support a join. Structure is checked FIRST, so a
// record that is both broken and un-joinable reports the broken half:
// that is the one somebody has to investigate.
func (a *UIJoinAnnotation) Validate() error {
	// VALIDATES THE NORMALIZED FORM, because that is what gets stored.
	//
	// SetUIJoinAnnotation normalizes and then validates; Validate called
	// directly did not normalize. So the two exported entry points
	// disagreed about the same value:
	//
	//   a.AppOrigins = []string{"http://localhost:3000/"}
	//   a.Validate()               -> "an origin carries no path"
	//   SetUIJoinAnnotation(ts, a) -> nil, stored as ".../3000"
	//
	// A producer doing the obvious `if err := a.Validate(); err != nil {
	// bail }` before Set therefore rejected an annotation the storage
	// layer would have accepted and repaired. The disagreement was baked
	// into this package's own tests as two contradictory expectations.
	//
	// Normalizing here makes Validate answer for the value that will
	// actually exist, which is the only question worth asking.
	return a.normalized().validateNormalized()
}

// validateNormalized is Validate's body, on an already-normalized value.
//
// Split out so Validate can normalize first without recursing. Its only
// caller now is Validate: the storage path moved to validateStructure in
// the same change that introduced it — Set, Storable and Get all call
// that directly — so the clause that used to sit here, naming the
// storage path, described a caller this function had already lost.
func (a *UIJoinAnnotation) validateNormalized() error {
	if err := a.validateStructure(); err != nil {
		return err
	}
	return a.joinability()
}

/*
validateStructure checks only what no correct producer ever emits.

This is the STORAGE gate. Everything here means the record does not
faithfully describe any capture — a field absent that the writer always
writes, a spec version this build cannot interpret, an origin string
that is not an origin. Such a record is refused rather than persisted,
because writing it creates a test-set that advertises a join it cannot
support.

Joinability is deliberately not part of it. See joinability.
*/
func (a *UIJoinAnnotation) validateStructure() error {
	/*
	 * THE ONLY NIL GUARD, and every nil reaches it.
	 *
	 * There used to be four — here, in Validate, in Storable and in
	 * Joinable — and they MUTUALLY MASKED: normalized() on a nil
	 * receiver returns nil, so Validate and Storable reach this one
	 * anyway, and Joinable delegates to Validate. Any one of them, or
	 * any three, could be deleted with the suite green, and the test
	 * written to pin them pinned none. Four unpinnable branches became
	 * one that a single mutation kills.
	 */
	if a == nil {
		return ErrUIJoinAbsent
	}
	// NOT `>`. Refusing an OLDER annotation is the stated purpose of the
	// field — a consumer must decline what it cannot interpret rather
	// than read it under the wrong rules — and `specVersion: 0`, which is
	// what a missing-then-defaulted value looks like, has to be refused
	// for the same reason.
	if a.SpecVersion != UIJoinSpecVersion {
		return fmt.Errorf("%w: found %d, expected %d", ErrUIJoinUnsupported, a.SpecVersion, UIJoinSpecVersion)
	}
	if strings.TrimSpace(a.CaptureID) == "" {
		return fmt.Errorf("%w: captureId", ErrUIJoinIncomplete)
	}
	// The nonce is what distinguishes this run from another run of the
	// same scenario. Without it two test-sets are indistinguishable to a
	// joiner and one browser capture would match both.
	if strings.TrimSpace(a.SessionNonce) == "" {
		return fmt.Errorf("%w: sessionNonce", ErrUIJoinIncomplete)
	}
	// A KNOWN SPEC, not merely a non-empty string.
	//
	// The struct doc calls this "the key spec BOTH planes used … the only
	// place [a join under two different normalizers] can be detected",
	// and then nothing compared it to anything: `canonicalKeySpec:
	// "$$$garbage$$$"` validated and was stored. A field that is the sole
	// detector of a whole class of wrong answer cannot be validated only
	// for blankness.
	if !UIJoinKnownKeySpec(a.CanonicalKeySpec) {
		return fmt.Errorf("%w: canonicalKeySpec %q is not a spec this build knows (known: %s)",
			ErrUIJoinUnsupported, a.CanonicalKeySpec,
			strings.Join(UIJoinCanonicalKeySpecs, ", "))
	}
	// A PLAUSIBLE EPOCH, not merely positive.
	//
	// IngressPorts gets a full range check on the stated ground that "an
	// out-of-range entry is what a truncated or mis-decoded read looks
	// like". `t0WallMs: 1` — 1970-01-01T00:00:00.001Z — and
	// `t0WallMs: math.MaxInt64` are exactly that, and both passed. The
	// asymmetry was unargued.
	if a.T0WallMs < uiJoinEpochFloorMs || a.T0WallMs >= uiJoinEpochCeilMs {
		return fmt.Errorf("%w: t0WallMs %d is not a plausible epoch-ms timestamp "+
			"(expected between %d and %d); a value outside this range is what a "+
			"seconds-vs-millis mix-up or a mis-decoded read looks like",
			ErrUIJoinIncomplete, a.T0WallMs, uiJoinEpochFloorMs, uiJoinEpochCeilMs)
	}
	if a.T1WallMs != 0 &&
		(a.T1WallMs < uiJoinEpochFloorMs || a.T1WallMs >= uiJoinEpochCeilMs) {
		return fmt.Errorf("%w: t1WallMs %d is not a plausible epoch-ms timestamp",
			ErrUIJoinIncomplete, a.T1WallMs)
	}
	if a.T1WallMs != 0 && a.T1WallMs < a.T0WallMs {
		return fmt.Errorf("%w: t1WallMs precedes t0WallMs", ErrUIJoinIncomplete)
	}
	if len(a.IngressPorts) == 0 {
		// STRUCTURAL, unlike an empty appOrigins, and the asymmetry needs
		// its own argument rather than the inverted one that used to sit
		// here ("would make every exchange look observed" — it is the
		// observed set, so an empty one makes every exchange look
		// UNobserved).
		//
		// The difference is the producer. appOrigins comes from a browser
		// that legitimately has no usable origin; ingressPorts comes from
		// the Go recorder, which cannot have OBSERVED zero ports and still
		// have produced a test-set. So no correct producer emits this,
		// which is what puts it on this side of the line.
		return fmt.Errorf("%w: ingressPorts", ErrUIJoinIncomplete)
	}
	for _, p := range a.IngressPorts {
		if err := uiJoinPortInRange(int64(p)); err != nil {
			return err
		}
	}
	// EVERY origin present must be an origin. An EMPTY list is not
	// checked here — see joinability, and the long note on
	// ErrUIJoinNotJoinable for why the difference is load-bearing.
	// A malformed origin is the structural case: no producer that is
	// working correctly emits one.
	for i, o := range a.AppOrigins {
		if err := uiJoinOriginIsWellFormed(i, o); err != nil {
			return err
		}
	}
	return nil
}

/*
joinability reports whether a structurally sound annotation can actually
carry a join.

An empty list is the only un-joinable shape: with no origin at all
there is nothing to attribute an exchange to, and the record is still
worth keeping because the captureId and nonce that stop two runs of a
scenario merging are present either way.

DELIBERATELY NOT "at least one http or https origin", which this
briefly required on the stated ground that "a backend exchange's origin
is always http or https, so the joiner cannot match anything else".
Both halves were invented. No joiner compares appOrigins to anything —
in @keploy/join, FOREIGN_ORIGIN is decided from a per-exchange
sameOrigin boolean the payload already carries (promote.ts), and
appOrigins is read by nothing outside the server's input schema. And
the rule was probably backwards where it bit: a Capacitor app's backend
calls carry a literal `Origin: capacitor://localhost` request header —
that is why such apps must allowlist that exact string in CORS — so the
value being declared un-attributable is the one that appears verbatim
on the recorded request.

Nobody has yet decided what the joiner compares, so the storage layer
does not decide for it. Matching the SDK is the conservative reading —
currentOrigin() drops exactly "" and "null", so every origin it keeps is
one it considers real.

NOT because the verdict is persisted; it is not. Nothing writes a
joinability answer: Set writes eight fields, uiJoinSetStoredInt64 writes
one, and this is recomputed from appOrigins on every Validate, Get and
Joinable. Changing the rule re-judges every record on disk for free, so
this is a two-way door in both directions. The one-way door was the OLD
placement — a scheme check inside validateStructure made Set refuse to
write, and that destroyed the captureId and nonce irreversibly. That is
what the move out of validateStructure fixes, and it is the whole of the
argument.

Assumes validateStructure has already passed, and both callers arrange
that: validateNormalized runs validateStructure first and then this,
and GetUIJoinAnnotation does the same in two statements so it can
return the annotation alongside this refusal. That ordering is what
makes the dual return safe. The nil case is nominally
validateStructure's, though in practice no caller reaches either
function with nil — see the note on its guard.
*/
func (a *UIJoinAnnotation) joinability() error {
	if len(a.AppOrigins) == 0 {
		return fmt.Errorf("%w: appOrigins is empty, so no exchange can be "+
			"attributed to the app under test; the capture itself is intact",
			ErrUIJoinNotJoinable)
	}
	return nil
}

/*
uiJoinOriginIsWellFormed rejects anything that is not SHAPED like an
origin — scheme + host [+ port], and nothing else.

Not "is not a web origin": the scheme allowlist that used to be here was
DELETED — not relocated, and joinability has no replacement for it — so
capacitor://, ionic://, chrome-extension:// and whatever the next
webview calls itself are accepted. What is checked is the SHAPE, which
no correct producer gets wrong whatever the scheme.

The port list gets a range check because "an out-of-range entry is what a
truncated or mis-decoded read looks like", and the same argument applies
here with the same consequence: a joiner comparing an exchange origin
against "not a url at all" classifies EVERY exchange FOREIGN_ORIGIN, with
nothing anywhere to explain why. Non-empty was half the rule.

An origin is scheme + host [+ port] and nothing else. A trailing path or
query means the producer sent a URL where an origin was asked for, and
comparisons against it would silently never match.
*/
func uiJoinOriginIsWellFormed(index int, o string) error {
	// No separate blank check: url.Parse("") yields no scheme and no
	// host, so the check below already rejects it. A second branch that
	// only changes the wording is a branch no test can distinguish.
	/*
	 * PARSED EXACTLY AS GIVEN, and it used to be trimmed first.
	 *
	 * The trim was dead — every validateStructure call site runs
	 * normalized() first, and normalizeOrigin trims every origin — but
	 * dead is not harmless here: it made the string this function
	 * PARSES differ from the string every message below QUOTES, and from
	 * the one the redaction was handed. An origin with surrounding
	 * whitespace is not an origin; refusing it is both more honest and
	 * the only way these messages name the value that failed. (They used
	 * to render it with %q; they now render uiJoinNamed's output with
	 * %s. That is a DIFFERENT value — uiJoinNamed returns an index plus
	 * a possibly-absent authority, not the origin — so the trim argument
	 * above now rests on the index naming the entry, not on the message
	 * quoting it.)
	 */
	u, err := url.Parse(o)
	/*
	 * ONE DISPLAY STRING FOR EVERY MESSAGE BELOW.
	 *
	 * These go into the recorder's log, and the documented way origins
	 * arrive is strings.Split(os.Getenv("KEPLOY_APP_ORIGINS"), ","), so a
	 * credentialed URL is the expected shape rather than an exotic one —
	 * `KEPLOY_APP_ORIGINS="http://user:pass@api.internal/v1"` most of all.
	 *
	 * Redacting at the one arm that refuses credentials fixed the one
	 * shape that never reaches the others: every arm ABOVE it — path,
	 * query, fragment, wildcard, DNS shape, empty port, and the parse
	 * error — formatted the raw value and printed the password verbatim.
	 * Thirteen sites, one fixed. Computing the display string once is
	 * necessary and NOT sufficient: a single computation that is a switch
	 * with a fall-through to the raw entry still prints the whole string
	 * for any origin url.Parse rejects. What makes the leak impossible is
	 * that uiJoinDisplay has no branch that can return input it has not
	 * proved safe — see its docstring.
	 */
	/*
	 * THREE STATES, and the third one leaked.
	 *
	 * `err == nil && u.User == nil` — parse SUCCEEDED but Go populated no
	 * userinfo because there was no //-authority — fell through with the
	 * raw string, and it is the only state that reaches the "no scheme
	 * and host" arm. `http:/admin:hunter2@api.internal/v1` (one slash
	 * short of the example this redaction was written for) printed the
	 * password.
	 *
	 * So the fallback is textual whenever the string carries an at-sign
	 * at all, whether or not url.Parse made sense of it. Over-redacting a
	 * string this validator is about to refuse costs an operator nothing.
	 */
	/*
	 * ONE SAFE RENDERING, COMPUTED ONCE.
	 *
	 * This used to be a switch that masked parts of the raw entry and
	 * fell through to the raw entry itself when no arm matched — so an
	 * origin that merely failed to parse, with no '@' in it, was printed
	 * whole, and printed TWICE: once as the display and again inside the
	 * parse reason. `http://api.internal/v1?token=SECRET` plus a stray
	 * 0x7f, or a `%zz` escape, was enough.
	 *
	 * uiJoinDisplay inverts that: it prints only an authority it can
	 * prove, and nothing at all otherwise.
	 *
	 * WHAT THAT IS AND IS NOT WORTH. The raw origin is not passed to
	 * uiJoinDisplay or uiJoinNamed — they take the parsed *url.URL — and
	 * no fmt.Errorf below references `o`. That is NOT the same as
	 * "structurally impossible to leak", and the difference matters:
	 *
	 *   - `parseErr` is a *url.Error, and url.Error.URL IS the raw
	 *     origin. Six lines inside uiJoinDisplay can reach it without
	 *     changing a signature.
	 *   - Both leaks found after that claim was written flowed through
	 *     u.Host, which is derived from `o`. No grep for `o` could have
	 *     seen either.
	 *
	 * So the property is real but narrow: it removes the ACCIDENTAL
	 * fall-through, not the class. What actually holds the line is the
	 * pair of refusals in uiJoinDisplay — no authority without a HOST,
	 * and no authority when an at-sign survived it — and the tests that
	 * pin them. (Not "without a parse": url.Parse returns (nil, err), so
	 * such a check is subsumed by the nil check and no mutation of it is
	 * observable.)
	 */
	named := uiJoinNamed(index, u)
	if err != nil {
		return fmt.Errorf("%w: %s is not a URL: %s",
			ErrUIJoinIncomplete, named, uiJoinParseReason(err))
	}
	if u.Scheme == "" || u.Host == "" {
		// Phrased without a trailing parenthetical, because `named` may
		// already carry one: "appOrigins[0] (http://x), which is not an
		// origin (no scheme and host)" put two of them in one sentence.
		// NEITHER, OR EITHER. The condition is `Scheme == "" || Host ==
		// ""`, so this one sentence covers three states and is literally
		// true of only one. "ZQXTOK:pw@evil://api.internal/x" parses
		// with Scheme "zqxtok" and Host "" — net/url reads everything
		// after the first colon as Opaque — and is told it carries no
		// scheme, which is false and sends the reader looking at the
		// wrong half. Naming the requirement rather than asserting an
		// absence is true of all three.
		return fmt.Errorf("%w: %s is not an origin: an origin needs both a scheme and a host",
			ErrUIJoinIncomplete, named)
	}
	/*
	 * THE SCHEME IS NOT CHECKED HERE. It is a joinability question, not
	 * a structural one, and putting it here destroyed real captures.
	 *
	 * currentOrigin() in @keploy/capture-sdk filters exactly two values,
	 * "" and "null"; every other location.origin is passed through, and
	 * the TS server accepts it. So capacitor://localhost (Capacitor on
	 * iOS), ionic://localhost (WKWebView), chrome-extension://<id> and
	 * app://bundle (Electron) all arrive from correct producers
	 * describing real apps people record. Refusing them as malformed
	 * discarded the captureId and nonce — the same data loss
	 * ErrUIJoinNotJoinable exists to stop, reached by a different origin
	 * shape.
	 *
	 * Whether any of them can attribute an exchange is a question for
	 * joinability, and the answer there is currently "yes, like any
	 * other origin" — see that function, which refuses only an EMPTY
	 * list. In particular there is no rule here that a backend exchange
	 * always has an http(s) origin, nor that joinability requires one.
	 */
	// Each part named separately, so a test can show which rule rejected
	// what. Collapsed into a single condition, most of these were
	// unreachable and the message named whichever arm happened to win.
	switch {
	case strings.HasSuffix(u.Host, ":"):
		/*
		 * A TRAILING COLON WITH NO PORT, and it goes FIRST.
		 *
		 * url.Parse accepts "http://a:" and u.Port() returns "", so the
		 * whole port-canonicalisation path is skipped and the string
		 * would be stored verbatim — where it can never match a browser's
		 * "http://a". The same failure the rest of this function exists
		 * to prevent, reached by the one shape that looks portless.
		 *
		 * It has to be the first host rule because nothing else can reach
		 * it: when Host ends in a colon, normalizeOrigin computes the
		 * canonical host but ASSIGNS neither branch — the port arm needs
		 * a non-empty port, the no-port arm is guarded against exactly
		 * this — so u.Host keeps the raw spelling and arrives non-ASCII.
		 * (The IDNA conversion does run; its result is discarded.)
		 * Ordered after the non-ASCII case this could never win, and the
		 * truncated-write shape it exists to name was reported as an
		 * unresolvable host instead.
		 *
		 * This used to be duplicated: a live switch case and a dead `if`
		 * after the switch, same condition, different wording, with the
		 * explanation attached to the unreachable one.
		 */
		return fmt.Errorf("%w: %s, whose port is empty; "+
			"that is what a truncated write looks like", ErrUIJoinIncomplete, named)
	case u.Path != "":
		return fmt.Errorf("%w: %s followed by a path; an origin carries no path",
			ErrUIJoinIncomplete, named)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("%w: %s followed by a query; an origin carries no query",
			ErrUIJoinIncomplete, named)
	case u.Fragment != "":
		return fmt.Errorf("%w: %s followed by a fragment; an origin carries no fragment",
			ErrUIJoinIncomplete, named)
	case strings.Contains(u.Hostname(), "*"):
		/*
		 * A WILDCARD, which is a CORS allowlist entry and never an origin.
		 *
		 * `*` is a non-empty label under 63 octets, so isResolvableDNSName
		 * waved `https://*.example.com` through and isASCII had nothing to
		 * say; it was stored verbatim. No browser's location.origin is ever
		 * a wildcard, so every exchange would then classify FOREIGN_ORIGIN
		 * with nothing anywhere to explain why — the exact silent-empty-join
		 * this function exists to prevent.
		 *
		 * It reaches here by the path the tests call THE PATH THAT MATTERS:
		 * a hand-written config.yaml, filled in by a human copying from the
		 * server's CORS configuration, which is where wildcards live.
		 */
		return fmt.Errorf("%w: %s; a wildcard is a CORS "+
			"allowlist entry, not an origin — no browser ever reports one, so "+
			"nothing could ever match it. List each origin the app is served "+
			"from", ErrUIJoinIncomplete, named)
	case strings.Contains(u.Hostname(), "%"):
		// An IPv6 ZONE INDEX (`[fe80::1%25eth0]`) is a link-local scope
		// identifier that names an interface on one machine. No
		// location.origin ever carries one, so an annotation holding one
		// is unjoinable by construction — the same class as the trailing
		// dot and the IPv4-mapped literal canonicalHost was written for,
		// and it must be refused rather than canonicalised, because there
		// is no form of it a browser would produce. A percent sign in a
		// host is otherwise an escape, which is malformed here too.
		return fmt.Errorf("%w: %s; a host carrying %% is either "+
			"an IPv6 zone index or a percent-escape, and a browser origin is neither",
			ErrUIJoinIncomplete, named)
	// NOT canonicalHost(u.Hostname()). This rule has to inspect the bytes
	// that reach disk, and canonicalHost is not idempotent —
	// trimTrailingDot is a single TrimSuffix — so validating one
	// canonicalisation pass BEYOND the stored value let
	// `http://a.example.com....` be accepted and STORED as
	// `http://a.example.com..`, an empty trailing DNS label, the exact
	// spelling trimTrailingDot exists to keep off disk. normalizeOrigin
	// now calls canonicalHost exactly once, so `u.Hostname()` here is the
	// stored spelling.
	case !isResolvableDNSName(u.Hostname()):
		// THE SHAPE DNS REQUIRES, applied to every host by the same rule.
		//
		// The relaxed IDNA profile turns off VerifyDNSLength along with
		// STD3, so it will happily produce an empty label (`пример..рф`)
		// or a 136-byte A-label from a 120-character one. Neither can be
		// resolved, so neither can ever be a browser's location.origin.
		//
		// Checked on the CANONICAL form and for ASCII hosts too. An
		// earlier version only refused the non-ASCII ones, so a 64-octet
		// label was rejected when some other label in the host happened
		// to be non-ASCII and accepted when the whole host was ASCII —
		// the same "depends on an unrelated label" asymmetry that made
		// the strict IDNA profile wrong here.
		return fmt.Errorf("%w: %s, whose host is not a "+
			"resolvable DNS name (an empty label, a label over 63 octets, or a "+
			"name over 253)", ErrUIJoinIncomplete, named)
	case !isASCII(u.Hostname()):
		// LAST AMONG THE HOST CHECKS, deliberately. Ordered first, it
		// pre-empted the more specific ones: `http://пример.рф:` — an
		// empty port, which is the truncated-write shape the trailing
		// colon check exists to name — was reported as "whose host is
		// not a name this can canonicalise", which is true of nothing
		// the producer did wrong.
		//
		// A host that reaches here is one the browser's own IDNA profile
		// could not convert either (see uiJoinIDNA). canonicalHost
		// leaves it as Unicode precisely so it arrives here, because
		// url.String() would percent-encode it into a spelling no
		// browser produces — and storing that makes every exchange
		// FOREIGN_ORIGIN with nothing to explain why.
		return fmt.Errorf("%w: %s, whose host is not a domain "+
			"name any browser can resolve (it fails the same IDNA rules a URL bar "+
			"applies); a browser origin is always ASCII once resolved",
			ErrUIJoinIncomplete, named)
	case u.User != nil:
		// REDACTED, because this string goes into the recorder's log and
		// the stated way origins arrive is
		// strings.Split(os.Getenv("KEPLOY_APP_ORIGINS"), ","), which is exactly
		// where a credentialed URL comes from. Echoing %q verbatim put
		// `http://admin:hunter2@host` — password and all — into the log
		// of an error about there being credentials.
		return fmt.Errorf("%w: %s; an origin carries no credentials",
			ErrUIJoinIncomplete, named)
	}
	// The PORT gets the same rigour ingressPorts gets. The const block
	// above argues that port 0 "can never be a value something was
	// observed on" and that an out-of-range entry is what a mis-decoded
	// read looks like; both are as true in an origin as in a port list.
	//
	// ONE ARM, because there is only one failure. url.URL.Port() is
	// digits-only by construction — validOptionalPort refuses anything
	// else, and url.Parse refuses the whole URL before this line runs
	// (`http://h.example.com:pw` is `invalid port ":pw" after host`). So
	// Atoi cannot fail here on a non-number. The only way it fails is
	// RANGE — `:99999999999999999999` — and calling that "port is not a
	// number" told the operator something plainly false about a value
	// that is nothing but digits. Out of range is what it is.
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || uiJoinPortInRange(int64(n)) != nil {
			return fmt.Errorf("%w: %s, whose port is outside %d-%d",
				ErrUIJoinIncomplete, named, minIngressPort, maxIngressPort)
		}
	}
	return nil
}

// Ingress ports are TCP ports ingress was OBSERVED arriving on -- not
// ports the recorder bound, and the field doc above spends seven lines
// on why that distinction is the point. Either way 0 is as wrong as
// 70000: port 0 means "the kernel picks one" and can never be a value
// something was observed on. An out-of-range entry is what a truncated
// or mis-decoded read looks like — ports read back as [0 0] from a
// codec this package mishandled would otherwise validate, and every
// exchange would then classify NO_INGRESS_OBSERVED with no error
// anywhere to explain it.
const (
	minIngressPort = 1
	maxIngressPort = 65535
)

func uiJoinPortInRange(p int64) error {
	if p < minIngressPort || p > maxIngressPort {
		return fmt.Errorf("%w: ingressPorts contains %d, outside %d-%d",
			ErrUIJoinIncomplete, p, minIngressPort, maxIngressPort)
	}
	return nil
}

/*
normalized returns a copy with the surrounding whitespace removed and the
origin hosts lowercased.

Hosts are case-insensitive, so "http://App.Example.COM" and
"http://app.example.com" are the same origin — but a joiner comparing
strings is not case-insensitive, and storing whichever spelling the
producer happened to send makes the comparison a coin flip.

(The paragraph above documents the unexported `normalized`; it sits here
because that is where the explanation belongs, and the blank line below
keeps godoc from gluing it onto `Normalized`'s own doc — which it did,
so the exported function's published documentation opened with a
paragraph about a different function.)
*/

/*
Normalized returns the canonical form of this annotation — the exact
value SetUIJoinAnnotation will store.

EXPORTED FOR THE PRODUCER, WHICH NOW EXISTS.
pkg/service/record/uijoin_record.go writes the annotation at stop, via
SetUIJoinAnnotation. `Normalized` itself is still uncalled outside
tests — it is part of the contract this package publishes, not a claim
that something is using it.

Set deliberately does not mutate its argument, so a caller that built an
annotation from, say, strings.Split(os.Getenv("KEPLOY_APP_ORIGINS"), ",") keeps
the padded strings in memory while the trimmed ones go to disk:

	b.CaptureID = "  cap_pad  "
	SetUIJoinAnnotation(ts, b)   // stores "cap_pad"
	b.CaptureID                  // still "  cap_pad  "

The producer's in-memory join key and the key on disk are then different
strings, which is the coin flip normalizeOrigin exists to eliminate,
relocated from the store into the caller. A producer that intends to use
the values it just stored should hold on to this instead:

	a = a.Normalized()
	if err := models.SetUIJoinAnnotation(ts, a); err != nil { ... }
*/
func (a *UIJoinAnnotation) Normalized() *UIJoinAnnotation {
	return a.normalized()
}

func (a *UIJoinAnnotation) normalized() *UIJoinAnnotation {
	if a == nil {
		return nil
	}
	out := *a
	out.CaptureID = strings.TrimSpace(a.CaptureID)
	out.SessionNonce = strings.TrimSpace(a.SessionNonce)
	out.CanonicalKeySpec = strings.TrimSpace(a.CanonicalKeySpec)
	out.IngressPorts = append([]int(nil), a.IngressPorts...)
	out.AppOrigins = make([]string, 0, len(a.AppOrigins))
	for _, o := range a.AppOrigins {
		out.AppOrigins = append(out.AppOrigins, normalizeOrigin(o))
	}
	return &out
}

// normalizeOrigin trims, lowercases the host, and drops a trailing
// slash. `http://localhost:3000/` names the same origin as
// `http://localhost:3000` and used to be rejected outright, so a
// hand-written config.yaml with a trailing slash failed the whole
// annotation.
func normalizeOrigin(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return trimmed
	}
	// Only the host. url.Parse already lowercases the scheme, so doing it
	// again was dead code behind a comment that claimed otherwise.
	u.Host = strings.ToLower(u.Host)
	// A FULLY-QUALIFIED TRAILING DOT NAMES THE SAME HOST.
	// `http://a.example.com.` is legal, url.Parse accepts it, and a
	// browser's location.origin never produces it — so it was stored
	// verbatim and every exchange classified FOREIGN_ORIGIN with nothing
	// to explain why. This function goes to real lengths over `:080` vs
	// `:80` and then stopped short of the host itself.
	/*
	 * ONE canonicalHost CALL PER ORIGIN, and that is the whole point.
	 *
	 * canonicalHost is not idempotent — trimTrailingDot is a single
	 * TrimSuffix — so when it was applied a varying number of times per
	 * branch, the VERDICT depended on the port:
	 *
	 *     http://a.example.com..        stored   (2 trims: here + below)
	 *     http://a.example.com..:80     stored   (2: the default-port
	 *                                             branch elides the port,
	 *                                             so the block below
	 *                                             re-entered)
	 *     http://a.example.com..:443    stored   (2: same, for https)
	 *     http://a.example.com..:8080   REFUSED  (1: only the port branch)
	 *
	 * Same host, same empty trailing label, opposite answers — and a Set
	 * refusal destroys the captureId, nonce, timestamps and ports this
	 * file spends sixty lines arguing must never be dropped. It is also
	 * the asymmetry this file condemns twice in its own prose: "the SAME
	 * label was accepted or refused depending on whether some other label
	 * in the host happened to be non-ASCII."
	 *
	 * Computing the host ONCE makes the verdict uniform across every port
	 * path, which is what canonicalHost's docstring already promised:
	 * refusing `a.example.com..` is the honest answer, and it now happens
	 * on all four.
	 */
	host := canonicalHost(u.Hostname())
	if port := u.Port(); port != "" {
		// `:80`, `:080` and `:0080` are one origin written three ways,
		// and a browser's location.origin elides the default port
		// entirely — so `http://app.example.com:80` from a hand-written
		// config could never match the browser half.
		trimmedPort := strings.TrimLeft(port, "0")
		if trimmedPort == "" {
			trimmedPort = "0"
		}
		def := (u.Scheme == "http" && trimmedPort == "80") ||
			(u.Scheme == "https" && trimmedPort == "443")
		if def {
			u.Host = host
		} else {
			u.Host = host + ":" + trimmedPort
		}
	} else if !strings.HasSuffix(u.Host, ":") {
		// THE NO-PORT ARM, and it is not redundant with the branch above:
		// that one only runs when a port is present, so an IPv4-mapped
		// literal or a trailing-dot host written without a port would
		// otherwise never be canonicalised at all.
		//
		// The trailing-colon guard keeps `http://localhost:` malformed.
		// It has no port and a Host of "localhost:"; rewriting it from
		// Hostname() would silently repair it into a valid origin, and an
		// empty port is refused on purpose — it is what a truncated write
		// looks like.
		u.Host = host
	}
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}

/*
canonicalHost is the one spelling of a host this package stores.

BRACKET-TOLERANT, and the tolerance lives ONE CALL DOWN. An IPv6
literal arriving with brackets comes back with exactly one pair rather
than `[[::1]]`, because unmapIPv4 does its own `strings.Trim(host,
"[]")` before net.ParseIP and returns the bare canonical form.

There used to be a second `strings.Trim(hostname, "[]")` here, defended
by a paragraph claiming the function was "SAFE TO CALL TWICE". Both
halves were wrong. The claim is false — the function is not idempotent
at all, see the next paragraph — and the guard was fully subsumed by
unmapIPv4's own Trim: removing it left every test in the package green,
including the one written specifically to pin it. Dead code under a
false justification reads as coverage while testing nothing, so it is
gone and TestCanonicalHostDoesNotDoubleBrackets now pins the BEHAVIOUR
wherever it is implemented.

(Strictly, the two differ for a bracketed NON-IP host such as `[foo]`,
which the removed Trim would have unwrapped and unmapIPv4 leaves alone.
No such value reaches here: url.Parse refuses it, and the one caller
passes url.URL.Hostname(), which returns no brackets at all.)

Takes a host with no port.

NOT IDEMPOTENT, and this used to claim it was. trimTrailingDot is a
single TrimSuffix, so `a.b.com..` loses exactly one dot per call.

That is why normalizeOrigin calls this ONCE — `host := canonicalHost(...)`
consumed by both the port and the no-port branch — and why
validateStructure checks `u.Hostname()` rather than
canonicalHost(u.Hostname()). While the call count varied by branch, the
VERDICT varied with it: `http://a.example.com..` was stored on the
no-port, `:80` and `:443` paths and refused on `:8080`, because those
paths applied one trim or two. Making the trim a loop would paper over
it; one call plus refusing the input is the honest answer, because no
browser produces either spelling.
*/
func canonicalHost(hostname string) string {
	host := unmapIPv4(trimTrailingDot(hostname))
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return toASCIIHost(host)
}

/*
The IDNA profile a BROWSER uses, not the strictest one available.

`idna.Lookup` enforces STD3 and CheckHyphens; the WHATWG URL Standard,
which is what computes `location.origin`, sets both off. The difference
is not academic — these are hosts a browser resolves and reports, and
this package was refusing them outright:

	пример_x.рф   -> xn--_x-mlcluqhd.xn--p1ai    (underscore)
	-пример.рф    -> xn----jtbiqngd.xn--p1ai     (leading hyphen)
	пример-.рф    -> xn----itbiqngd.xn--p1ai     (trailing hyphen)
	ab--cd.рф     -> ab--cd.xn--p1ai             (-- in positions 3-4)

The last is the clearest tell that the strict profile was wrong here:
`ab--cd.example.com` is all-ASCII and takes the fast path, so the SAME
label was accepted or refused depending on whether some other label in
the host happened to be non-ASCII.

The whole stated purpose of this function is to produce what the browser
reports. It has to use the browser's rules.
*/
var uiJoinIDNA = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.Transitional(false),
	// Both OFF, matching WHATWG. See above.
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
)

/*
toASCIIHost converts an internationalised name to its A-label form.

`http://ПРИМЕР.РФ` is a legitimate origin, and url.Parse hands it back
percent-encoded — which is a spelling no browser produces. A browser's
location.origin reports `http://xn--e1afmkfd.xn--p1ai`, so the stored
value could never string-match and every exchange classified
FOREIGN_ORIGIN with nothing to explain why. That is the identical failure
unmapIPv4 and trimTrailingDot were added for, in the far more common
case.

Left ALONE on failure rather than mangled: a host this cannot convert is
then caught by uiJoinOriginIsWellFormed, which is the right place to
refuse it. Repairing it into something plausible is what must not happen.
*/
func toASCIIHost(host string) string {
	if isASCII(host) {
		// A FAST PATH, and only that. It is not about percent handling:
		// x/net/idna has none, and url.Parse has already rejected or
		// decoded every host escape before this runs. ToASCII returns an
		// ASCII host unchanged, so this skips work rather than changing
		// an answer.
		return host
	}
	ascii, err := uiJoinIDNA.ToASCII(host)
	if err != nil {
		return host
	}
	return ascii
}

/*
isResolvableDNSName applies the length rules IDNA's relaxed profile drops.

Not a general hostname validator — the profile has already decided what
characters are allowed. This is only the shape DNS itself requires: no
empty label, no label over 63 octets, no name over 253.
*/
func isResolvableDNSName(name string) bool {
	// NO TRAILING-DOT TRIM, and therefore no local to hold one.
	// normalizeOrigin has already removed the one legitimate
	// fully-qualified dot by the time anything reaches here, so the
	// TrimSuffix that used to sit here only ever laundered a SECOND dot —
	// turning an empty trailing label into a name this function then
	// called resolvable. The empty-label rule below is meant to be
	// absolute, and now is: it refuses `a..b` in the middle and `a.b..`
	// at the end by the same test.
	//
	// There is no emptiness check either. Split("", ".") yields [""], so
	// an empty name always reaches the empty-label arm below and is
	// refused there — a leading `name == "" ||` could not change the
	// answer for any input, and was unreachable rather than untested.
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// trimTrailingDot drops the root label from a fully-qualified name.
//
// APPLIED TO EVERY HOST, IPv6 literals included — the comment here used
// to say it was not, two lines above the body comment explaining that
// the guard which would have made that true was deliberately removed.
// It is harmless rather than correct-by-design: `http://[::1.]` is
// refused by url.Parse ("IPv4 field must have at least one digit")
// before canonicalHost ever runs, so no bracketed host with a trailing
// dot reaches this function to be silently repaired.
func trimTrailingDot(host string) string {
	// NO GUARD AT ALL, because both halves were tautological. A string
	// cannot end in both "]" and ".", so HasSuffix(host, "]") fired only
	// when !HasSuffix(host, ".") already had — and the survivor was no
	// better: TrimSuffix already returns the string unchanged when the
	// suffix is absent, so `if !HasSuffix { return host }` was the same
	// answer by a longer route. Removing one half and congratulating the
	// code on it is what the earlier comment here did.
	return strings.TrimSuffix(host, ".")
}

/*
unmapIPv4 rewrites an IPv4-mapped IPv6 literal as its dotted form.

`http://[::ffff:127.0.0.1]` and `http://127.0.0.1` are the same address,
and a browser only ever reports the second. Stored as the first, every
exchange classifies FOREIGN_ORIGIN — the failure
uiJoinOriginIsWellFormed's docstring says it exists to prevent, reached
by a spelling it accepts.
*/
func unmapIPv4(host string) string {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	// RFC 5952 FOR IPv6 TOO. This had the canonical form already computed
	// and threw it away — `return host` handed back whatever the producer
	// wrote. A browser's location.origin always reports the compressed,
	// minimised form:
	//
	//   written  http://[2001:0db8:85a3:0000:0000:8a2e:0370:7334]
	//   browser  http://[2001:db8:85a3::8a2e:370:7334]
	//   stored   the first one, which can never match the second
	//
	// Same failure unmapIPv4 and trimTrailingDot exist to prevent, for
	// the one address family neither covered. net.IP.String() implements
	// RFC 5952 and is idempotent.
	return ip.String()
}

// Complete reports whether recording finished. An annotation with no end
// time belongs to a capture still in progress, and joining against one is
// a race the joiner should decline rather than resolve.
func (a *UIJoinAnnotation) Complete() bool {
	// The nil guard is load-bearing: `a, err := GetUIJoinAnnotation(ts)`
	// returns a nil annotation on every error path EXCEPT the
	// not-joinable one, and a caller that checks a.Complete() before
	// err panics without it.
	return a != nil && a.T1WallMs > 0
}

/*
Storable reports whether SetUIJoinAnnotation will persist this
annotation — and is the pre-flight a producer wants, NOT Validate.

Validate additionally asks "can this carry a join?", and answers no for
an intact capture with no attributable origin. A producer that wrote the
documented-obvious `if err := a.Validate(); err != nil { bail }` before
Set would therefore skip the one record this package went to trouble to
rescue, and the test-set would read back ErrUIJoinAbsent — the original
bug, restored by the fix for it. That divergence is precisely what the
note on Validate and TestValidateAgreesWithSet exist to prevent, so the
question Set actually asks is exported rather than left unaskable.

Set returns this error for every annotation it refuses, so pre-flighting
is optional; it is here for a producer that must decide before it has a
test-set to write to. The one difference is the nil annotation, which
Set reports as "nil annotation" rather than ErrUIJoinAbsent — Set is
being handed a nil argument, which is the caller's bug, not a test-set
that carries nothing.
*/
func (a *UIJoinAnnotation) Storable() error {
	return a.normalized().validateStructure()
}

// Joinable reports whether this annotation can carry a join.
//
// The question a consumer actually has, asked directly rather than by
// matching on an error. False for a nil annotation, for a structurally
// broken one, and for an intact capture with no app origin — a caller
// deciding whether to join does not care which, and a caller deciding
// whether to alert uses errors.Is on Validate instead.
func (a *UIJoinAnnotation) Joinable() bool {
	return a.Validate() == nil
}

/*
SetUIJoinAnnotation stores the annotation on a test-set.

FOR THE RECORD-TIME PRODUCER, four constraints worth knowing before you
wire this up:

  - It CANNOT be called at t0 with placeholders. A real CaptureID and
    SessionNonce are required and both come from the browser, so the
    recorder must wait for that handshake before the first write. The
    "T1WallMs = 0 while recording" state therefore only reaches disk for
    a capture whose browser attached before recording started; for any
    other, the annotation is written once, at stop, already complete.
  - AppOrigins may be EMPTY and the annotation is still stored. That is
    the sandboxed-iframe / file: case the browser half mints on purpose;
    the result reads back with Joinable() false. Only a structurally
    broken annotation is refused — see validateStructure.
  - Do NOT hand-roll the stop-time update as GetUIJoinAnnotation →
    set T1WallMs → this function. That read-modify-write REPLACES the
    whole map, so a key written by a newer producer under the same
    specVersion is silently dropped; and Get returns a non-nil error for
    an intact-but-un-joinable capture, so the obvious
    `if err != nil { return err }` aborts and T1WallMs is never written
    on exactly the capture this package went to trouble to keep. Use
    FinishUIJoinAnnotation, which does neither.
  - ts.Metadata is a bare map and this mutates it in place. Record runs
    concurrently; a caller serializing the config on another goroutine
    gets a data race, not an error. Callers own that synchronization.

The value is written as a map rather than the struct so the field names
are the same over every codec. TestSet.Metadata is
map[string]interface{}; a struct in it marshals differently per codec,
and the two planes would then disagree about what the keys are called.

The map shape is not free, though, and the claim that it "round-trips
identically" was wrong for a long time: the Go VALUE each codec hands
back differs even when the field names agree. See the reader section
below, which is what makes the round trip actually hold, and
TestReadAcceptsEveryDocumentShapeACodecProduces, which executes it for
each shape rather than asserting it in a comment.
*/
func SetUIJoinAnnotation(ts *TestSet, a *UIJoinAnnotation) error {
	if ts == nil {
		return errors.New("nil test-set")
	}
	if a == nil {
		// Not ErrUIJoinAbsent: that reads "the test-set carries no
		// annotation", which blames the test-set for a nil argument.
		return errors.New("nil annotation")
	}
	// NORMALIZE, do not merely validate the trimmed form.
	//
	// Validate has always compared strings.TrimSpace(x) — an explicit
	// acknowledgement that whitespace arrives, from a captureId read with
	// a trailing newline or origins from
	// strings.Split(os.Getenv("KEPLOY_APP_ORIGINS"), ","). It then stored the
	// padded value verbatim, so " http://localhost:3000" validated and
	// was written, and a joiner comparing an exchange origin against it
	// would silently never match — the exact failure
	// uiJoinOriginIsWellFormed exists to prevent. For CaptureID the
	// padding corrupts the join key itself.
	normalized := a.normalized()
	// STRUCTURE, not joinability. Refuse to persist a record that
	// describes no capture correctly — writing it would create a
	// test-set advertising a join it cannot support. An intact capture
	// that merely cannot be joined IS persisted: dropping it discards a
	// captureId and nonce that are present and correct, and those are
	// what stop two runs of one scenario merging. See
	// ErrUIJoinNotJoinable.
	if err := normalized.validateStructure(); err != nil {
		return err
	}
	a = normalized
	if ts.Metadata == nil {
		ts.Metadata = map[string]interface{}{}
	}
	ts.Metadata[UIJoinMetadataKey] = map[string]interface{}{
		"specVersion":      a.SpecVersion,
		"captureId":        a.CaptureID,
		"sessionNonce":     a.SessionNonce,
		"t0WallMs":         a.T0WallMs,
		"t1WallMs":         a.T1WallMs,
		"ingressPorts":     uiJoinInts(a.IngressPorts),
		"appOrigins":       uiJoinStrings(a.AppOrigins),
		"canonicalKeySpec": a.CanonicalKeySpec,
	}
	return nil
}

/*
FinishUIJoinAnnotation stamps the stop time on an annotation already on
the test-set.

THE NARROW HELPER Set's doc asks for, and the only correct way to close
out a recording. The hand-rolled read-modify-write it replaces has two
independent faults:

  - GetUIJoinAnnotation returns a non-nil error for an intact capture
    with no attributable origin, so the obvious early return skips the
    update and leaves Complete() false forever — on precisely the
    capture ErrUIJoinNotJoinable exists to preserve. Joining against an
    unfinished annotation is a race the joiner declines, so that capture
    becomes permanently unusable.
  - Set REPLACES the whole metadata map, dropping any key a newer
    producer wrote under the same specVersion.

This edits the stored map in place: it touches t1WallMs and nothing
else, so an unknown key written by a newer producer survives.

Refuses to finish what it cannot read as an annotation, and refuses a
t1 that precedes t0 or is not a plausible epoch-ms timestamp.

WRITTEN ONCE, as behaviour and not merely as a rationale: a second call
carrying the SAME t1 succeeds and is a no-op, and a second call carrying
a different but OTHERWISE VALID t1 returns ErrUIJoinAlreadyFinished
naming both values and changes nothing. A producer retrying its stop
path can treat ErrUIJoinAlreadyFinished as success.

"Otherwise valid" is load-bearing. The argument checks run first, so a
second call whose t1 is not a plausible epoch-ms timestamp, or precedes
t0, is refused as a bad ARGUMENT — ErrUIJoinIncomplete — and not as a
lost race. Without that ordering a producer told to swallow
ErrUIJoinAlreadyFinished would swallow a broken clock instead.

THE RULE IS THIS FUNCTION'S, NOT THE PACKAGE'S. SetUIJoinAnnotation
replaces the whole annotation unconditionally, t1WallMs included, so a
producer that calls Set again after a Finish silently wins — "two
producers that disagree get an error" holds only when both go through
here. That asymmetry is deliberate: Set's job is to write the
annotation, and refusing to let it replace a complete one would make a
bad record unrepairable. It is stated because the alternative is a
promise this package only half keeps.

ts.Metadata is mutated in place; callers own synchronization, as with
Set.
*/
func FinishUIJoinAnnotation(ts *TestSet, t1WallMs int64) error {
	// A NIL TEST-SET IS THE CALLER'S BUG, and says so rather than
	// borrowing ErrUIJoinAbsent, which means "this test-set carries no
	// annotation" and would blame the test-set for not being passed one.
	// Unreachable from inside the package and pinned from outside it by
	// TestTheExportedSurfaceIsNilSafe, same as Set's.
	if ts == nil {
		return errors.New("nil test-set")
	}
	// Deliberately tolerates ErrUIJoinNotJoinable: an un-joinable capture
	// still has a stop time, and refusing to record it is the bug. No nil
	// guard on `a` after this: Get never returns (nil, nil), and every
	// other error path already returned on the line above.
	a, err := GetUIJoinAnnotation(ts)
	if err != nil && !errors.Is(err, ErrUIJoinNotJoinable) {
		return err
	}
	if t1WallMs < uiJoinEpochFloorMs || t1WallMs >= uiJoinEpochCeilMs {
		return fmt.Errorf("%w: t1WallMs %d is not a plausible epoch-ms timestamp",
			ErrUIJoinIncomplete, t1WallMs)
	}
	if t1WallMs < a.T0WallMs {
		return fmt.Errorf("%w: t1WallMs precedes t0WallMs", ErrUIJoinIncomplete)
	}
	// WRITTEN ONCE, which is what the doc above promises and what nothing
	// enforced: Finish silently moved an existing stop time, including
	// backwards. A recording stops once; a second, different answer means
	// two producers disagree, and picking the later caller is not a
	// resolution. Re-stamping the SAME value is idempotent and allowed,
	// so a retried stop is not an error.
	if a.T1WallMs != 0 && a.T1WallMs != t1WallMs {
		return fmt.Errorf("%w: already finished at t1WallMs %d, refusing to "+
			"move it to %d", ErrUIJoinAlreadyFinished, a.T1WallMs, t1WallMs)
	}
	// IN PLACE, into the document that is already there — not Set, which
	// rebuilds it from the struct and drops what this build cannot name.
	return uiJoinSetStoredInt64(ts, "t1WallMs", t1WallMs)
}

/*
uiJoinSetStoredInt64 writes one key into the stored annotation, whatever
codec shape it arrived in.

EVERY SHAPE GetUIJoinAnnotation ACCEPTS, because anything less makes
Finish refuse a document the reader just read — and the consequence is
exactly what Finish exists to prevent: Complete() stays false forever
and the joiner declines the capture.

Asserting map[string]interface{} and refusing the rest would be wrong:
nothing here re-encodes anything. bson.M IS
map[string]interface{} under a different name and takes the identical
in-place assignment; map[interface{}]interface{} takes it with a string
key; bson.D is a slice, so the matching element is replaced by index,
which preserves both order and every other entry.
*/
func uiJoinSetStoredInt64(ts *TestSet, key string, v int64) error {
	switch m := ts.Metadata[UIJoinMetadataKey].(type) {
	case map[string]interface{}:
		m[key] = v
		return nil
	case bson.M:
		m[key] = v
		return nil
	case map[interface{}]interface{}:
		m[key] = v
		return nil
	case bson.D:
		for i := range m {
			if m[i].Key == key {
				// By index, into the shared backing array, so the value
				// stored on ts.Metadata sees it without reassignment.
				m[i].Value = v
				return nil
			}
		}
		// Absent: appending may reallocate, so the result has to go back.
		ts.Metadata[UIJoinMetadataKey] = append(m, bson.E{Key: key, Value: v})
		return nil
	default:
		// DEFENCE IN DEPTH, and unreachable today: the only caller writes
		// after GetUIJoinAnnotation succeeded, and uiJoinStringMap accepts
		// exactly the four shapes above. Kept rather than deleted because
		// the two lists are in different functions and a shape added to
		// the reader without a case here must fail loudly instead of
		// silently not writing. Said plainly, because a branch no test can
		// reach and no comment explains is the kind this file deletes.
		return fmt.Errorf("%w: the stored annotation is a %T, which this build "+
			"cannot update in place", ErrUIJoinNotAnnotation,
			ts.Metadata[UIJoinMetadataKey])
	}
}

/*
GetUIJoinAnnotation reads the annotation back.

Returns ErrUIJoinAbsent when there is none — which is the ordinary case
for every test-set recorded without a browser alongside, and is not an
error condition for the caller to log loudly.

RETURNS A VALUE AND AN ERROR TOGETHER on exactly one path. An intact
capture that cannot be joined — no app origin, the sandboxed-iframe and
file: case the browser half mints on purpose — comes back as a non-nil
annotation AND ErrUIJoinNotJoinable. A caller checking only err still
fails closed, as it did before; a caller that needs the capture's
identity reads the annotation anyway. Every other error path returns a
nil annotation, so `a.Complete()` before the err check is still a panic
on those and is still the wrong order.
*/
func GetUIJoinAnnotation(ts *TestSet) (*UIJoinAnnotation, error) {
	// `ts.Metadata == nil` is not checked: indexing a nil map is legal and
	// yields ok == false, so the lookup below returns the same
	// ErrUIJoinAbsent. Another half that could not change the answer.
	if ts == nil {
		return nil, ErrUIJoinAbsent
	}
	raw, ok := ts.Metadata[UIJoinMetadataKey]
	if !ok {
		return nil, ErrUIJoinAbsent
	}
	m, why := uiJoinStringMap(raw)
	// BRANCH ON THE REASON, not on the map. A typed-nil map —
	// `map[string]interface{}(nil)` or `bson.M(nil)` — is a legitimate
	// empty document AND satisfies `m == nil`, so it produced
	// "UI join metadata is not an annotation: ": a dangling colon with no
	// explanation, which is the misdirecting error the reasons were added
	// to kill, reintroduced by the new signature. A nil bson.D went
	// through make(), came back non-nil, and reported the far better
	// "missing a required field: specVersion".
	if why != "" {
		// THE REASON, not just the sentinel. A duplicate key and a value
		// that is genuinely not a document both used to come back as
		// "UI join metadata is not an annotation: stored as bson.D",
		// which told a producer emitting a repeated captureId that its
		// metadata was the wrong shape — sending it to look in the wrong
		// place entirely.
		return nil, fmt.Errorf("%w: %s", ErrUIJoinNotAnnotation, why)
	}

	r := uiJoinRead{m: m}

	// Absence is reported as absence. Reading a missing key as 0 and
	// letting Validate describe it produced "unsupported spec version:
	// found 0, expected 1" for a truncated write — an error that actively
	// misdirects, since the version is not wrong, it is not there.
	if _, ok := r.present("specVersion"); !ok {
		return nil, fmt.Errorf("%w: specVersion", ErrUIJoinIncomplete)
	}
	// SAME RULE FOR appOrigins, for the same reason and at more cost to
	// get wrong. Set always writes the key — `appOrigins: []` for the
	// sandboxed-iframe case — so a document MISSING it is a truncated
	// write or a hand-edit, not a page with no usable origin. Read as a
	// nil slice it became ErrUIJoinNotJoinable, whose message says "the
	// capture itself is intact": a corrupt record reported as benign, in
	// the one class this reader is most careful about elsewhere.
	//
	// Absent and explicitly-empty are both already on the wire and
	// r.present tells them apart, so this needs no spec bump.
	if _, ok := r.present("appOrigins"); !ok {
		return nil, fmt.Errorf("%w: appOrigins", ErrUIJoinIncomplete)
	}
	// Through asInt32, NOT asInt64 then a conversion. int is 32 bits on a
	// 32-bit build, so int(1<<32 + 1) is 1 there and a nonsense version
	// would read back as the supported one.
	//
	// The range failure is ErrUIJoinNotAnnotation — the READER could not
	// read the value — and not ErrUIJoinUnsupported, which is what
	// Validate returns for a version it read fine and does not support.
	// Sharing one sentinel made the two indistinguishable on a 64-bit
	// host, so a test could not tell whether this guard or Validate had
	// rejected the value, and removing the guard entirely left the suite
	// green. Same fix as the ports path, for the same reason.
	specVersion := r.narrow32("specVersion")

	a := &UIJoinAnnotation{
		SpecVersion:      int(specVersion),
		CaptureID:        r.str("captureId"),
		SessionNonce:     r.str("sessionNonce"),
		T0WallMs:         r.num("t0WallMs"),
		T1WallMs:         r.num("t1WallMs"),
		IngressPorts:     r.ports("ingressPorts"),
		AppOrigins:       r.strs("appOrigins"),
		CanonicalKeySpec: r.str("canonicalKeySpec"),
	}
	if r.err != nil {
		// A field that is present but unreadable is corruption, and is
		// reported as such rather than read as a zero that Validate would
		// then describe as merely "missing".
		return nil, r.err
	}
	// NORMALIZED ON READ TOO. Set normalizes, and a hand-written
	// config.yaml never goes through Set — which is the only case
	// normalizeOrigin's own comment names. So the read path still had the
	// whole bug: `appOrigins: ["http://localhost:3000/"]` rejected the
	// entire annotation, `captureId: "  cap_01HZY  "` read the padding
	// back into the join key, and a host's case was whatever the file
	// happened to say.
	a = a.normalized()
	if err := a.validateStructure(); err != nil {
		// A stored annotation that no longer describes a capture is
		// reported as such rather than returned half-usable.
		return nil, err
	}
	// THE ONE PATH THAT RETURNS BOTH A VALUE AND AN ERROR.
	//
	// A caller doing the ordinary `if err != nil { return }` still fails
	// closed, which is the right default and the behaviour this had
	// before. A caller that wants the capture's IDENTITY — the captureId
	// and nonce that stop two runs of one scenario merging — takes the
	// annotation anyway, gated on errors.Is(err, ErrUIJoinNotJoinable).
	//
	// Returning (nil, err) here is what discarded that identity, and
	// returning (a, nil) would let a caller that checks only err go on
	// to join against an empty origin list and get a silent empty
	// result. Both halves are needed; neither alone is safe.
	if err := a.joinability(); err != nil {
		return a, err
	}
	return a, nil
}

/* ------------------------------------------------------------------ */
/*  Codec-tolerant readers                                            */
/* ------------------------------------------------------------------ */
/*
 * The same document arrives in a different Go shape from every codec on
 * the path: map[string]interface{} from JSON, map[interface{}]interface{}
 * from some YAML decoders, and an ORDERED bson.D — never a map — from the
 * mongo driver, whose lists are bson.A rather than []interface{}. Numbers
 * arrive as int, int32, int64, float64 or json.Number depending on the
 * decoder and on whether the caller enabled UseNumber.
 *
 * A join that works over one codec and silently reads zero over another
 * is the defect that only shows up in production, so nothing here reads a
 * value it does not understand. The numeric and string conversions are
 * the package's existing asInt64/asString rather than a private fork of
 * them: those already cover json.Number, every int and uint width, and
 * reject NaN, +-Inf and out-of-range floats instead of letting Go's
 * implementation-defined conversion land them on math.MinInt64.
 */

// uiJoinRead reads fields out of a decoded annotation, remembering the
// first failure.
//
// ABSENT is not the same as UNREADABLE. A key that is not there reads as
// the zero value and no error: that is a half-written annotation, and
// Validate reports it as incomplete with the field name. A key that is
// present but holds the wrong type is corruption, and is reported rather
// than silently read as 0 or "" — which is how ingressPorts decoded from
// json.Number came back as [0 0], passed Validate, and made every
// exchange classify NO_INGRESS_OBSERVED with nothing anywhere to explain
// why.
type uiJoinRead struct {
	m   map[string]interface{}
	err error
}

func (r *uiJoinRead) fail(key string, err error) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: %s: %w", ErrUIJoinNotAnnotation, key, err)
	}
}

func (r *uiJoinRead) present(key string) (interface{}, bool) {
	v, ok := r.m[key]
	return v, ok && v != nil
}

func (r *uiJoinRead) str(key string) string {
	v, ok := r.present(key)
	if !ok {
		return ""
	}
	s, err := asString(v)
	if err != nil {
		r.fail(key, err)
		return ""
	}
	return s
}

/*
integral rejects a value that is not a whole number.

WHY THIS EXISTS. asInt64/asInt32/asUint16 route through floatToInt64,
which TRUNCATES TOWARD ZERO for any in-range float. Those semantics were
chosen for a pgtype cell fixture, where a lossy read is acceptable; here
they are not. Reusing them silently produced:

	t0WallMs:     1789000000000.7  ->  1789000000000   (no error)
	ingressPorts: [8080.9]         ->  [8080]          (no error)
	specVersion:  1.9              ->  1               (VALIDATES as the
	                                                    supported version)

The package header says "nothing here reads a value it does not
understand". It did; it just landed on a plausible number instead of
zero, which is strictly worse than landing on zero — a zero is visible.

Worse still, the two documented JSON paths disagreed about the SAME
bytes. `json.Unmarshal` yields float64 and truncated; a `UseNumber`
decoder yields json.Number and failed ParseInt. One decoder accepted the
document with a mutated timestamp, the other called it corruption. A
browser producer sending performance.timeOrigin — a sub-millisecond
DOMHighResTimeStamp — emits exactly this.

Checked BEFORE the conversion rather than after, because after the
truncation there is nothing left to compare against.
*/
// The key is NOT named here: uiJoinRead.fail already prefixes it, and
// prefixing it again produced "t0WallMs: t0WallMs: ... is not a whole
// number". No other helper on this path names its own key.
func integral(v interface{}) (interface{}, error) {
	switch n := v.(type) {
	case float32:
		/*
		 * float32 IS REFUSED OUTRIGHT, not checked for integrality.
		 *
		 * "Integral" is not the property that matters for a float32 — it
		 * has 24 bits of mantissa, so every large magnitude is integral
		 * AND wrong. float32(1.789e12) passes a Trunc check and converts
		 * to 1789000024064: 24 seconds of skew, comfortably inside the
		 * epoch window, and a 60-second capture whose t0 and t1 both
		 * round to the same float32 reads as zero duration without
		 * tripping T1 < T0. That is "landed on a plausible number
		 * instead of zero" happening INSIDE the guard written to stop it.
		 *
		 * No decoder on this path produces a float32, so nothing
		 * legitimate is refused; a hand-built Go map is the only way to
		 * get one here, and it should use int64.
		 */
		// NO FIELD-SPECIFIC WORDING. This arm is shared by num, narrow32
		// and ports, and it used to say "an epoch-millisecond value" —
		// so a bad port reported "ingressPorts: 1.5 ... cannot represent
		// an epoch-millisecond value exactly", which is about a field
		// the producer was not writing. fail() prefixes the real one.
		return nil, fmt.Errorf("%v arrived as a float32, which has too few "+
			"significant digits to hold this value exactly; use an integer type", n)
	case float64:
		if n != math.Trunc(n) {
			return nil, fmt.Errorf("%v is not a whole number", n)
		}
	case json.Number:
		/*
		 * INTEGRAL, not "spelled as an integer".
		 *
		 * n.Int64() is ParseInt, which refuses any exponent form — so
		 * `1.789e12` was rejected here while the SAME BYTES through
		 * json.Unmarshal arrived as float64(1.789e12), passed the Trunc
		 * check, and were accepted. That is precisely the two-decoders-
		 * disagree defect this function's own docstring claims to have
		 * closed; the test for it only ever tried a fractional value,
		 * where both paths happen to reject.
		 *
		 * So: try ParseInt, and on failure fall back to the float and ask
		 * whether it is a whole number. A value either is a whole number
		 * or is not, whichever way it was written down.
		 */
		if _, err := n.Int64(); err != nil {
			f, ferr := n.Float64()
			if ferr != nil || f != math.Trunc(f) {
				return nil, fmt.Errorf("%s is not a whole number", n.String())
			}
			// NORMALIZED, not merely permitted. asInt64 reads a
			// json.Number with ParseInt too, so accepting the exponent
			// form here and passing it through unchanged just moved the
			// refusal one line down and reported it as a strconv error.
			// The whole point is that the same number reads the same way
			// whichever decoder produced it.
			//
			// THROUGH floatToInt64, NOT int64(f). Converting an
			// out-of-range float directly is implementation-defined, and
			// on amd64 `1e19`, `1e308` and `Inf` all land on
			// math.MinInt64 — indistinguishable from a real value.
			// Trunc(Inf) == Inf, so the check above waves Inf through.
			// The range check downstream then reported "t0WallMs
			// -9223372036854775808 is not a plausible epoch-ms
			// timestamp": a number the producer never wrote, invented by
			// the guard. The float32 arm reasons about exactly this
			// hazard; this arm was the one that did not.
			narrowed, nerr := floatToInt64(f)
			if nerr != nil {
				// The value ONCE. Wrapping floatToInt64's error printed it
				// twice in two spellings — "1e19 is not a value this can
				// read: value 1e+19 out of range for int64" — which reads
				// like two different numbers, and led with a vague
				// sentence the second half said better.
				return nil, fmt.Errorf("%s is out of range for this field, "+
					"which holds a 64-bit integer", n.String())
			}
			return narrowed, nil
		}
	case string:
		/*
		 * A QUOTED NUMBER MUST BE A PLAIN INTEGER.
		 *
		 * asInt64 reads a numeric string with ParseInt, which accepts no
		 * exponent and no fraction — so anything else would pass this
		 * check and then fail downstream with a strconv error instead of
		 * a sentence about the annotation. Refused here, in this
		 * package's terms, and refused for the same reason either way.
		 *
		 * This is deliberately stricter than the json.Number and float64
		 * arms, which do accept `1.789e12`: those are NUMBERS whose
		 * decoder chose a representation, and the same bytes must not
		 * read differently through two decoders. A quoted value is a
		 * string the producer chose to write that way, and the supported
		 * spelling for it has always been a plain integer.
		 */
		if strings.ContainsAny(n, ".eE") {
			// ALWAYS TRUE for anything carrying `.`, `e` or `E`: ParseInt base
			// 10 accepts only an optional sign and digits. Kept as the parse
			// rather than a bare refusal because the parse is what decides,
			// and writing `if true` here would hide which rule applies if the
			// base or bit size ever changes.
			if _, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err != nil {
				// The remedy does NOT say "send it unquoted". This
				// reader's own docs note that several codecs on this path
				// quote numbers, and a codec that quotes by fmt.Sprint-ing
				// a float64 emits "1.789e+12" — so blaming the producer
				// for a choice its codec made would send them looking in
				// the wrong place.
				return nil, fmt.Errorf("%q is not a whole number written as a "+
					"plain integer; this field takes an integer, quoted or not",
					n)
			}
		}
	}
	return v, nil
}

// num reads a numeric field.
//
// A numeric STRING is accepted ("8080" reads as 8080): asInt64 parses
// one, and several codecs on this path quote numbers. A non-numeric
// string is corruption and is reported.
func (r *uiJoinRead) num(key string) int64 {
	v, ok := r.present(key)
	if !ok {
		return 0
	}
	v, err := integral(v)
	if err != nil {
		r.fail(key, err)
		return 0
	}
	n, err := asInt64(v)
	if err != nil {
		r.fail(key, err)
		return 0
	}
	return n
}

// narrow32 reads a numeric field that must fit in an int32, so the
// int(...) conversion at the call site is lossless on every platform.
func (r *uiJoinRead) narrow32(key string) int32 {
	v, ok := r.present(key)
	if !ok {
		return 0
	}
	v, err := integral(v)
	if err != nil {
		r.fail(key, err)
		return 0
	}
	n, err := asInt32(v)
	if err != nil {
		r.fail(key, err)
		return 0
	}
	return n
}

func (r *uiJoinRead) list(key string) ([]interface{}, bool) {
	v, ok := r.present(key)
	if !ok {
		return nil, false
	}
	items, ok := uiJoinSlice(v)
	if !ok {
		r.fail(key, fmt.Errorf("expected a list of values, got %T", v))
		return nil, false
	}
	return items, true
}

func (r *uiJoinRead) ports(key string) []int {
	items, ok := r.list(key)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		// asUint16, NOT asInt64 then a cast. It range-checks before
		// narrowing, so uint16->int below is lossless on every platform
		// — int is 32 bits on a 32-bit build, where narrowing an
		// unchecked int64 would wrap 1<<32+8080 back into a
		// plausible-looking 8080.
		//
		// So the UPPER bound is enforced twice, here and in Validate,
		// and deliberately: this one makes the narrowing safe, and a
		// reader that rejects 70000 is what distinguishes "the stored
		// value was unreadable" from "the value read fine and is not a
		// usable port". Only the 1-vs-0 LOWER bound is uniquely
		// Validate's.
		item, err := integral(item)
		if err != nil {
			r.fail(key, err)
			return nil
		}
		n, err := asUint16(item)
		if err != nil {
			r.fail(key, err)
			return nil
		}
		out = append(out, int(n))
	}
	return out
}

func (r *uiJoinRead) strs(key string) []string {
	items, ok := r.list(key)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, err := asString(item)
		if err != nil {
			r.fail(key, err)
			return nil
		}
		out = append(out, s)
	}
	return out
}

// uiJoinStringMap normalizes the document shapes the decoders on this
// path produce, and returns the document, or nil and the reason there
// is none — phrased for whoever wrote the value, not for this package.
// Anything else — bson.Raw, for one — fails closed, with the default arm
// naming the concrete type it saw.
//
// One paragraph, not two: these were stacked with no blank line, so the
// published doc introduced the function twice and the first heading
// still said the CALLER formats the type, which the signature change
// that added the reason string moved into this function.
func uiJoinStringMap(v interface{}) (map[string]interface{}, string) {
	switch m := v.(type) {
	case map[string]interface{}:
		return m, ""
	case bson.M:
		// A DEFINED type over map[string]interface{}, so the case above
		// does not match it.
		return map[string]interface{}(m), ""
	case bson.D:
		// What the mongo driver actually hands back for a nested
		// document: an ordered slice of key/value pairs, never a map.
		//
		/*
		 * A REPEATED KEY IS REFUSED, not collapsed.
		 *
		 * bson.D is an ordered list that legitimately permits duplicates,
		 * and flattening it into a map made the last one win silently —
		 * so a document carrying `captureId` twice resolved to whichever
		 * came second, on the field whose entire job is to be a join key.
		 *
		 * THIS IS THE ONLY CODEC WHERE IT IS DETECTABLE. encoding/json
		 * collapses duplicates into map[string]interface{} before this
		 * function is ever called — `{"captureId":"a","captureId":"b"}`
		 * arrives as a one-entry map — and so does gopkg.in/yaml.v3. An
		 * earlier comment here claimed "the same shape reaches here from
		 * JSON", which is simply untrue: the JSON path cannot be hardened
		 * from this function, and saying otherwise implies a protection
		 * that does not exist.
		 */
		out := make(map[string]interface{}, len(m))
		for _, e := range m {
			if _, dup := out[e.Key]; dup {
				return nil, fmt.Sprintf(
					"the stored document carries %q more than once, so which "+
						"value it means is undefined; write each field once",
					e.Key)
			}
			out[e.Key] = e.Value
		}
		return out, ""
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Sprintf(
					"the stored document has a %T key, and every field name here "+
						"is a string", k)
			}
			if _, dup := out[ks]; dup {
				return nil, fmt.Sprintf(
					"the stored document carries %q more than once, so which "+
						"value it means is undefined; write each field once", ks)
			}
			out[ks] = val
		}
		return out, ""
	default:
		return nil, fmt.Sprintf("stored as %T, which is not a document", v)
	}
}

// uiJoinSlice normalizes any list shape to []interface{}.
//
// bson.A and []int are both defined slice types that a []interface{}
// type assertion misses, and enumerating them is how the next codec gets
// missed too, so anything of slice kind is accepted and its elements are
// converted individually by the caller.
func uiJoinSlice(v interface{}) ([]interface{}, bool) {
	if items, ok := v.([]interface{}); ok {
		return items, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil, false
	}
	// REFUSE byte slices, even though they have slice Kind. Every non-zero
	// byte is a valid TCP port, so a binary blob landing on ingressPorts
	// would decode into plausible numbers with no error anywhere — the
	// [0 0] failure this reader exists to prevent, wearing a different
	// shape.
	//
	// Tested on the ELEMENT KIND rather than by listing types: a type
	// switch on `[]byte, json.RawMessage` matched neither `type Blob
	// []byte` nor any future alias, and having to name json.RawMessage
	// separately was the evidence that enumeration does not hold.
	switch rv.Type().Elem().Kind() {
	case reflect.Uint8, reflect.Int8:
		// Both byte widths. Every non-zero byte is a valid TCP port, so a
		// binary blob would decode into plausible numbers with no error
		// anywhere — and testing only the unsigned kind left the signed
		// twin, which is the same hazard with a different tag.
		return nil, false
	}
	out := make([]interface{}, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

func uiJoinInts(in []int) []interface{} {
	out := make([]interface{}, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func uiJoinStrings(in []string) []interface{} {
	out := make([]interface{}, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

/*
uiJoinDisplay is the ONE rendering of an origin this file ever prints.

ALLOWLIST, NOT MASKING, and that inversion is the whole of the fix.

Every previous version asked "what in this string is secret?" and
printed the rest. Each round closed the shape it had been shown and left
the next one standing:

  - masking the userinfo PASSWORD left the username, so
    `https://<token>@host` — how git, npm and curl credential helpers all
    spell a token — printed it in full;
  - masking the whole userinfo span left everything after the last '@',
    so `?token=...` was echoed;
  - showing the parsed AUTHORITY closed that, but only on the arm where
    the parse SUCCEEDED: an entry that merely failed to parse fell
    through the switch and was printed raw, query string included. One
    stray control byte — a 0x7f, or a `%zz` escape — was enough. (An
    earlier version of this line said a trailing NEWLINE was enough. It
    is not: normalized() trims the value before the validator sees it,
    and a test written from that false claim was vacuous because of it.)

So this asks the opposite question: what can be PROVEN safe? Only the
authority, and only when the stdlib actually parsed one. A string that
does not parse has no provable authority, so nothing from it is printed
at all — the caller names the entry by its INDEX, which is what an
operator needs to find the line in KEPLOY_APP_ORIGINS anyway.

Returns "" when nothing is safe to show.

WHAT THIS DELIBERATELY DOES PRINT: the authority — scheme, host and
port. A secret placed in the HOST position is therefore echoed. That is
not an oversight: the authority is the only thing that identifies which
entry was refused, and refusing to print it would make every message
unactionable. The rule this file enforces is that no CREDENTIAL is
echoed — userinfo, and anything after the authority. A secret in the
host is a secret in the name of the machine, and the recorder has to be
able to say which machine.

THE SCHEME IS ECHOED TOO, and the argument above does not cover it. An
earlier version of this paragraph listed the scheme among the deliberate
echoes and then defended only the host, so `TOKEN://api.internal` prints
`TOKEN`. It is accepted because the scheme allowlist was deliberately
removed — capacitor://, ionic://, chrome-extension:// and app:// are
real origins from real producers — and net/url restricts a scheme to
alphanumerics plus `+-.`, so it cannot carry userinfo, an at-sign or a
percent. A secret spelled as a scheme is a secret the operator invented;
naming it is the same trade as naming the host.

NOT AN IPv6 ZONE IDENTIFIER, which that paragraph also used to list. A
host carrying '%' is never rendered at all now — see the rule in
uiJoinDisplay — so `http://[fe80::1%25TOKEN]` shows only its index.
*/
/*
uiJoinCarriesAtSign reports whether s holds an at-sign at ANY encoding
depth.

MATCHING A MEANING, NOT A SPELLING, and the first version matched a
spelling. It tested for the literal "@" or the literal "%40", which is
exactly one decode deep — and net/url unescapes a host exactly once. So
`%2540` arrived as `%40` and was caught, while `%252540` arrived as
`%2540`, in which the four characters "%40" simply do not occur, and the
token went to the log. That was leak #9, one layer above leak #8, in the
clause written for leak #8.

Two encoding layers is not exotic: the comment defending the old check
offered "a CI variable, a compose interpolation, a proxy config" as the
source of the first one, and any two of those give the second.

FAIL SHUT ON EXHAUSTION. The loop is bounded because a crafted string
can keep producing new output; running out of depth means the value
could not be resolved, which is not the same as "no at-sign", so it
withholds.

A decode that makes NO PROGRESS is different: it means every remaining
'%' is a literal, which is the ordinary IPv6 zone id (`[fe80::1%eth0]`),
so the loop stops and reports false. Reasoning from an ERROR instead of
from progress is what produced leak #10 — an error says SOME '%' is not
an escape and nothing about the others, while url.PathUnescape throws
away the decode of all of them.
*/
func uiJoinCarriesAtSign(s string) bool {
	for i := 0; i < uiJoinUnescapeRounds; i++ {
		/*
		 * "@" ALONE, because "%40" is redundant here and redundant
		 * code in this file has to justify itself.
		 *
		 * A level-0 "%40" test was added to make this strictly
		 * dominate the literal check it replaced. It does dominate —
		 * but not because of that test: uiJoinUnescapeOnce decodes
		 * "%40" to "@" unconditionally (the `%` is followed by two hex
		 * digits, so no input reaches the next round with it intact),
		 * and the following iteration catches it. Deleting the "%40"
		 * clause left the whole package green, which is the file's own
		 * definition of a branch that should not be there.
		 *
		 * TestTheFixpointDominatesTheLiteralCheck asserts the
		 * containment directly, so the property is pinned by a test
		 * rather than by a duplicated condition.
		 */
		if strings.Contains(s, "@") {
			return true
		}
		next := uiJoinUnescapeOnce(s)
		if next == s {
			return false
		}
		s = next
	}
	return true
}

// uiJoinUnescapeRounds bounds the fixpoint. Eight is arbitrary but
// generous: two encoding layers is the realistic ceiling for a value
// that passed through a CI variable and a compose interpolation, and
// exhaustion fails shut, so the cost of the bound being too low is a
// withheld authority rather than a leak.
const uiJoinUnescapeRounds = 8

/*
uiJoinUnescapeOnce decodes every VALID %XX in s and leaves an invalid
'%' exactly where it is.

NOT url.PathUnescape, AND THAT DISTINCTION IS LEAK #10. PathUnescape is
all-or-nothing: one malformed '%' anywhere aborts the decode of the
WHOLE string, including well-formed escapes elsewhere in it. The first
fixpoint read that abort as "no at-sign" and printed the authority, so
`https://ghp_TOKEN%2540api.internal%25` — a credentialed host with one
stray '%' after it — leaked the token, and any host carrying an IPv6
zone identifier got the at-sign check disabled outright, because the
zone's '%' is not an escape.

That made the "fix" for leak #9 WEAKER than the literal check it
replaced on three inputs. Decoding per-escape removes the coupling: a
'%' that is not an escape stops nothing but itself.
*/
func uiJoinUnescapeOnce(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) &&
			uiJoinIsHex(s[i+1]) && uiJoinIsHex(s[i+2]) {
			/*
			 * NO ERROR BRANCH, and there was one.
			 *
			 * The condition above has already established that the two
			 * bytes are hex digits, so ParseUint's argument is exactly
			 * two hex characters — at most 0xff, which fits bitSize 8.
			 * It cannot fail. The `if ... err == nil` this replaced
			 * therefore had a fall-through no input could reach, and a
			 * reader had to reconstruct this proof to know the raw '%'
			 * was not also written on some path.
			 *
			 * Same standard as the u.Opaque and level-0 "%40" entries
			 * this file removed and then wrote paragraphs about; it
			 * survived here because the dead branch was an error path,
			 * which reads as prudence rather than as dead code.
			 */
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			b.WriteByte(byte(v))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func uiJoinIsHex(c byte) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F')
}

// parseErr IS NOT A PARAMETER. It was, and it was never read: the branch
// that would have consulted it (`parseErr != nil`) is subsumed by
// `u == nil`, because url.Parse returns (nil, err). The note at the call
// site cites url.Error.URL as a live leak vector reachable from inside
// this function — which is an argument for not having the error in scope
// at all, not for keeping it unread.
func uiJoinDisplay(u *url.URL) string {
	// `parseErr != nil` is NOT tested here: url.Parse returns (nil, err),
	// so it is subsumed by `u == nil` and no mutation of it is
	// observable. It was an unpinnable branch of exactly the kind the
	// validateStructure nil-guard note argues against, and the paragraph
	// at the call site briefly advertised it as half of a load-bearing
	// pair. One condition, one mutation that kills it.
	if u == nil || u.Host == "" {
		return ""
	}
	/*
	 * BUILT BY HAND, not url.URL.String().
	 *
	 * String() percent-escapes a non-ASCII host, so `http://пример.рф`
	 * came back as `http://%D0%BF%D1%80%D0%B8%D0%BC%D0%B5%D1%80.%D1%80%D1%84`
	 * — a spelling the operator cannot match against their own
	 * KEPLOY_APP_ORIGINS line by eye, and one that the rule twenty lines below
	 * refuses for carrying a '%'.
	 *
	 * NOT "as written", though: normalizeOrigin lowercases and
	 * IDNA-converts the host before the
	 * validator runs, so on the public path u.Host holds the
	 * CANONICALISED spelling: an operator who wrote `http://пример..рф`
	 * is shown `http://xn--e1afmkfd..xn--p1ai`. Building by hand still
	 * beats url.URL.String(), which would percent-escape on top of that
	 * — but the eye-matching claim holds only for a host IDNA left
	 * alone, and saying so is better than the next reader discovering
	 * it.
	 */
	/*
	 * WITHHOLD THE AUTHORITY WHEN AN AT-SIGN SURVIVED IT.
	 *
	 * net/url ends the authority at the first '/', '?' or '#' — BEFORE
	 * it looks for '@'. So a credential containing any of those three is
	 * classified as the HOST, and u.User is nil:
	 *
	 *   https://ghp_ABCDEF/@api.internal   Host="ghp_ABCDEF"  User=nil
	 *   http://admin:1234/@api.internal    Host="admin:1234"  User=nil
	 *   https://TOKENxyz?@api.internal     Host="TOKENxyz"    User=nil
	 *
	 * All three are credentials and all three were printed in full. The
	 * u.User guard cannot help: it fires only when net/url has ALREADY
	 * decided the value is userinfo, which is exactly the case that was
	 * never dangerous. A base64 secret contains '/' about half the time,
	 * and this file's own docstrings call `https://<token>@host` the
	 * canonical shape to protect.
	 *
	 * THERE IS NO SYNTACTIC WAY TO TELL THEM APART from a benign path:
	 * `https://TOKEN/@host` and `http://a.example.com/pa@th` parse
	 * identically — same nil User, same host-shaped Host, same at-sign
	 * past the authority. So this fails closed on both. An at-sign
	 * anywhere past the authority means the authority is not PROVABLY a
	 * host, and the entry is named by index alone.
	 *
	 * UNCONDITIONAL, and it was briefly gated on `u.User == nil`. That
	 * gate made the guard WEAKER the more credential markers the input
	 * carried: `https://ghp_SECRET/@api.internal` withheld, while
	 * `https://x@ghp_SECRET/@api.internal` — one character longer, and
	 * more obviously a credential — set u.User, skipped the guard, and
	 * printed `(https://xxxxx@ghp_secret)`. The reasoning behind the
	 * gate was that a populated u.User means net/url located the
	 * userinfo correctly, so the host is trustworthy; that is false
	 * whenever a SECOND at-sign sits past the authority, which is
	 * exactly the shape here. Nothing pinned it when it shipped —
	 * deleting the gate left the whole package green — and the three
	 * `userinfo and a …` rows in TestAnAtSignPastTheAuthorityWithholdsIt
	 * were added to close that, so it is pinned now.
	 *
	 * Read off the PARSED fields, never the raw origin, so the raw
	 * string stays out of this function's scope.
	 */
	/*
	 * u.Host IS IN THIS LIST, and leaving it out was the eighth leak.
	 *
	 * The guard scanned every field EXCEPT the one the whole rule is
	 * about. net/url rejects a bare `%40` in an authority, but it passes
	 * `%25` through — so `https://ghp_TOKEN%2540api.internal`, which is
	 * `https://ghp_TOKEN%40api.internal` with one ordinary layer of URL
	 * encoding on it (a CI variable, a compose interpolation, a proxy
	 * config), parses to Host="ghp_TOKEN%40api.internal" with a nil User
	 * and SIX EMPTY scanned fields. The guard could not fire, and the
	 * token went to the log.
	 *
	 * A literal '@' can never appear in u.Host — net/url would have made
	 * it userinfo — so for the host it is the `%40` clause that does the
	 * work. An IPv6 zone id is unaffected: `[fe80::1%25eth0]` becomes
	 * `[fe80::1%eth0]`, which carries a '%' but not a '%40'.
	 */
	/*
	 * A HOST CARRYING '%' IS NEVER PRINTED, and this rule is what ends
	 * the encoding class rather than patching its next spelling.
	 *
	 * Leaks #8, #9 and #10 were three encodings of one shape: a
	 * credential that net/url classified as the HOST, smuggled past an
	 * at-sign check by `%2540`, `%252540`, and a stray `%` that aborted
	 * the decode. Each fix chased the spelling and the next spelling
	 * arrived.
	 *
	 * But validateStructure ALREADY refuses every host containing '%' —
	 * "a host carrying % is either an IPv6 zone index or a
	 * percent-escape, and a browser origin is neither". So the display
	 * has nothing to gain by rendering one: the entry is refused either
	 * way, and the index names it. Printing it was pure leak surface.
	 *
	 * WHY THE TWO CHECKS CANNOT DRIFT APART, which the first version of
	 * this paragraph asserted without saying. That arm tests
	 * u.Hostname() and this rule tests u.Host — different accessors —
	 * so "it is already refused" is only true because the two can
	 * differ ONLY over the surrounding brackets of an IPv6 literal and
	 * a trailing :port, and neither can carry a '%': validOptionalPort
	 * is digits-only and the brackets are literals. A change that made
	 * the port tolerant, or that stopped stripping brackets, would
	 * break the coupling silently. TestAPercentBearingHostIsNeverRendered
	 * pins the behaviour; this paragraph is the reason to re-derive it
	 * if either accessor changes.
	 *
	 * This is the positive form of the question the whole function is
	 * supposed to ask — "is this provably a host?" — applied to the
	 * host itself rather than to the shapes a credential might take.
	 */
	if strings.Contains(u.Host, "%") {
		return ""
	}
	/*
	 * AND THE AT-SIGN SCAN STAYS, for a different shape it still owns:
	 * `https://TOKEN/@host` puts the credential in a '%'-free Host and
	 * the at-sign in the PATH, so the host rule above cannot see it.
	 * The fixpoint covers the encoded forms of that signal in fields
	 * net/url does not decode — RawQuery most of all, since Path
	 * arrives already decoded.
	 *
	 * AND IT IS LOAD-BEARING ON THE PUBLIC PATH, which it briefly was
	 * not. When every depth case put its encoding in the HOST, the '%'
	 * rule above returned first and the fixpoint was exercised only by
	 * direct unit calls — a review measured that swapping the whole
	 * thing for the pre-leak-#9 literal check left every public-path
	 * test green. Query-position rows were added for that reason, and
	 * each of the two properties is now pinned through Storable():
	 *
	 *   - the ITERATION, by TestAnEncodedAtSignIsCaughtAtEveryDepth's
	 *     "query, depth 2/3/4" rows, which fail if the fixpoint is
	 *     replaced by a single literal scan for "@" and "%40".
	 *
	 *   - the LENIENT per-escape decode, by
	 *     TestAStrayPercentDoesNotDisableTheAtSignCheck's
	 *     "query, stray percent" and "query, stray percent interior"
	 *     rows, which fail if leak #10's all-or-nothing url.PathUnescape
	 *     is reinstated. The depth rows do NOT fail under that swap —
	 *     they carry no stray '%', so the decode succeeds and the
	 *     iteration still reaches the at-sign.
	 *
	 * NAMED, NOT COUNTED. A count like "fails nine subtests" is a trap
	 * here: `--- FAIL` lines include a parent line per test, and the
	 * figure moves with which variant of the mutation is applied. A
	 * count is a measurement of the suite as it stood on the day, and
	 * the suite is
	 * edited more often than this comment; a named row is a claim the
	 * reader can check in one command.
	 */
	/*
	 * u.RawPath AND u.RawFragment ARE NOT IN THIS LIST, and they were.
	 *
	 * net/url sets RawPath only when it is a valid encoding of Path
	 * (setPath assigns it only if escape(Path) differs from the input,
	 * having already checked unescape(input) == Path), so Path is
	 * exactly the decode of RawPath — and decoding can introduce an
	 * at-sign, never remove one. So every at-sign FOUND in RawPath is
	 * found in Path too. RawFragment stands the same way to Fragment.
	 *
	 * THE TWO FIELDS DO DIVERGE, and a fuzz run finding no counterexample
	 * is not evidence that they do not — here is one:
	 *
	 *   http://h.example.com/%2525252525252525%2f
	 *     RawPath "/%2525252525252525%2f" -> carries = true
	 *     Path    "/%25252525252525/"     -> carries = false
	 *
	 * The argument above covers only uiJoinCarriesAtSign's AT-SIGN exit.
	 * It has a second true exit — exhausting uiJoinUnescapeRounds — and
	 * Path begins one decode deeper than RawPath, so a value nested
	 * just deep enough exhausts from one and terminates from the other.
	 * A non-canonical escape somewhere else in the path (the %2f here)
	 * is what makes net/url populate RawPath at all, and it is trivial
	 * to place. The fuzz missed it because its inputs were six tokens
	 * long and this needs eight levels of nesting: a fuzz that finds
	 * nothing has told you about its own alphabet, not about the
	 * property.
	 *
	 * THE DIVERGENCE IS ONE-DIRECTIONAL, which is why the deletion is
	 * still right. Exhaustion means WITHHOLD, so the only thing scanning
	 * RawPath could add is withholding an authority that Path alone
	 * would render. It can never be the reason a credential is printed.
	 * Removing the entries removes over-withholding, never
	 * under-withholding — and deleting either left the whole package
	 * green, which is this file's own definition of a branch that should
	 * not be here.
	 *
	 * RawQuery STAYS. There is no decoded counterpart to it: Query()
	 * returns a map, and nothing else in this struct carries the raw
	 * query string, so it is the only reader of that field's at-signs.
	 *
	 * Same standard as the u.Opaque entry below. Two of the five entries
	 * in a list written to be exhaustive were dominated by their
	 * neighbours, which is what a list assembled by naming fields rather
	 * than by asking what each one can decide looks like.
	 */
	/*
	 * u.Opaque IS NOT IN THIS LIST, and it was.
	 *
	 * net/url sets Opaque only when what follows the scheme does NOT
	 * begin with "//" — and in that shape Host is "", so the guard
	 * above has already returned. The two are mutually exclusive, so
	 * the entry was unreachable: deleting it left the whole package
	 * green, which is this file's own definition of a branch that
	 * should not be there. It is stated rather than silently dropped
	 * because the same standard is applied twice elsewhere in this
	 * file and was not applied here.
	 */
	for _, part := range []string{
		u.Path, u.RawQuery, u.Fragment,
	} {
		if uiJoinCarriesAtSign(part) {
			return ""
		}
	}

	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(u.Scheme)
		b.WriteString("://")
	}
	if u.User != nil {
		// WHOLE, not url.URL.Redacted(). Redacted() rewrites userinfo
		// only when a password is present — `ru.User.Password()` has to
		// report ok — so a username-only credential survived it intact.
		b.WriteString("xxxxx@")
	}
	b.WriteString(u.Host)
	return b.String()
}

// uiJoinNamed renders one KEPLOY_APP_ORIGINS entry for a message: its index,
// plus the authority when there provably is one.
//
// The index is not decoration. When nothing about the entry is safe to
// print it is the ONLY thing that tells an operator which of their
// origins was refused.
func uiJoinNamed(index int, u *url.URL) string {
	if disp := uiJoinDisplay(u); disp != "" {
		return fmt.Sprintf("appOrigins[%d] (%s)", index, disp)
	}
	return fmt.Sprintf("appOrigins[%d]", index)
}

// uiJoinQuotedSpan matches one Go-quoted span, escapes included. Used to
// strip input echoes out of an error's own text; see
// uiJoinParseReason.
var uiJoinQuotedSpan = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// uiJoinParseReason keeps url.Parse's own message usable without
// echoing the credentials it quotes back.
//
// NOT A TEXTUAL SUBSTITUTION, and it used to be. url.Error renders as
// `%s %q: %s`, so the URL inside that message is GO-QUOTED while the
// value handed in is not — and what url.Parse actually refuses is very
// nearly the set of things %q escapes. A control character is the
// clearest case: `http://admin:hunter2@api.internal/` + 0x7f reaches
// the message as the four characters `\x7f` where the input holds one
// byte, so strings.ReplaceAll found nothing to replace and handed back
// the password verbatim — inside the single message that exists to hide
// it. The same miss happened whenever the parse call had trimmed its
// input and the redaction had not.
//
// url.Error carries the URL as a FIELD, so that half of the message is
// rebuilt rather than searched. Nothing has to match for it to hold.
//
// BUT url.Error HAS THREE FIELDS, and rebuilding from two of them was a
// REGRESSION against the textual version it replaced. `ue.Err` is the
// stdlib's own reason, and for a host/port problem it is
// fmt.Errorf("invalid port %q after host", colonPort) — where colonPort
// is raw input. So `http://admin:hunter2/@api.internal` produced
// `invalid port ":hunter2" after host`, the whole password, inside the
// one message that exists to hide it. A `/` in the password is not an
// exotic input: a base64 password contains a slash, and a slash makes
// url.Parse read the password as a port. The
// old ReplaceAll caught that case and missed only the %q-escaped one;
// the rewrite traded a wide net for a narrow certainty and lost ground.
//
// SO THE REASON IS SCRUBBED OF EVERY QUOTED SPAN. The stdlib embeds the
// offending fragment with %q and states the diagnosis in the words
// around it, so the unquoted text is the part worth keeping and the
// quoted part is an echo of input this function has already decided it
// must not print. `invalid port ":hunter2" after host` becomes
// `invalid port "..." after host`, which still says what is wrong;
// `net/url: invalid control character in URL` carries no quotes and is
// passed through untouched.
func uiJoinParseReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Err == nil {
			return "unspecified parse failure"
		}
		raw := ue.Err.Error()
		/*
		 * BALANCE IS COUNTED ON THE ORIGINAL, not inferred from the
		 * scrubbed copy.
		 *
		 * The previous form scrubbed first, stripped the literal
		 * `"..."` placeholders, and looked for a survivor — so a reason
		 * of the shape `"..."SECRET` stripped to `SECRET`, found no
		 * quote, and was returned whole. Counting unescaped quotes in
		 * the input cannot be fooled that way, and does not depend on
		 * the placeholder being unforgeable.
		 */
		quotes := 0
		for i := 0; i < len(raw); i++ {
			if raw[i] == '\\' {
				i++
				continue
			}
			if raw[i] == '"' {
				quotes++
			}
		}
		if quotes%2 != 0 {
			return "(reason withheld: its quoting is unbalanced, so no part " +
				"of it can be shown to be free of input)"
		}
		scrubbed := uiJoinQuotedSpan.ReplaceAllString(raw, `"..."`)
		return scrubbed
	}
	// UNREACHABLE from this package — url.Parse's only error return is
	// *url.Error. Fail-shut rather than convenient: the alternative is
	// echoing the text of an error we cannot take apart, and a reason we
	// cannot prove is credential-free is worth less than the password it
	// might carry.
	return fmt.Sprintf("(reason withheld: a %T cannot be redacted)", err)
}
