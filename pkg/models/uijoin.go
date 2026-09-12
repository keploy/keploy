package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"reflect"
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

WHY IT IS SESSION-GRAIN. Six scalars on the test-set, not a field on every
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
	T0WallMs      when recording started, for skew estimation only
	T1WallMs      when it stopped; 0 while still recording
	IngressPorts  which ports the recorder was listening on, so an
	              exchange on an unobserved port is NO_INGRESS_OBSERVED
	              rather than silently unjoined
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
)

// Validate reports whether the annotation can be used for a join.
//
// Fails CLOSED on every incomplete case. A partial annotation is worse
// than none: it looks joinable and produces edges nobody can trust, and
// the failure is silent because a wrong join still returns pairs.
func (a *UIJoinAnnotation) Validate() error {
	if a == nil {
		return ErrUIJoinAbsent
	}
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
// Split out so Validate can normalize first without recursing, and so
// the storage path can validate the exact value it is about to write
// rather than an equivalent one.
func (a *UIJoinAnnotation) validateNormalized() error {
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
		// An empty port list would make every exchange look observed.
		return fmt.Errorf("%w: ingressPorts", ErrUIJoinIncomplete)
	}
	for _, p := range a.IngressPorts {
		if err := uiJoinPortInRange(int64(p)); err != nil {
			return err
		}
	}
	// ENFORCED, on the same ground as the port list. AppOrigins is what
	// lets the joiner call an exchange FOREIGN_ORIGIN; with none, it
	// cannot make that call at all and every foreign exchange reads as
	// simply missing, with nothing to explain why. "Fails closed on every
	// incomplete case" has to include this one or the sentence is false.
	if len(a.AppOrigins) == 0 {
		return fmt.Errorf("%w: appOrigins", ErrUIJoinIncomplete)
	}
	for _, o := range a.AppOrigins {
		if err := uiJoinOriginIsWellFormed(o); err != nil {
			return err
		}
	}
	return nil
}

/*
uiJoinOriginIsWellFormed rejects anything that is not a web origin.

The port list gets a range check because "an out-of-range entry is what a
truncated or mis-decoded read looks like", and the same argument applies
here with the same consequence: a joiner comparing an exchange origin
against "not a url at all" classifies EVERY exchange FOREIGN_ORIGIN, with
nothing anywhere to explain why. Non-empty was half the rule.

An origin is scheme + host [+ port] and nothing else. A trailing path or
query means the producer sent a URL where an origin was asked for, and
comparisons against it would silently never match.
*/
func uiJoinOriginIsWellFormed(o string) error {
	// No separate blank check: url.Parse("") yields no scheme and no
	// host, so the check below already rejects it. A second branch that
	// only changes the wording is a branch no test can distinguish.
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil {
		return fmt.Errorf("%w: appOrigins contains %q, which is not a URL: %v",
			ErrUIJoinIncomplete, o, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%w: appOrigins contains %q, which is not an origin (no scheme and host)",
			ErrUIJoinIncomplete, o)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: appOrigins contains %q; an app origin is http or https",
			ErrUIJoinIncomplete, o)
	}
	// Each part named separately, so a test can show which rule rejected
	// what. As one four-way branch, only two of the four were reachable.
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
		 * it: when Host ends in a colon, normalizeOrigin skips both
		 * canonicalHost branches — one needs a non-empty port, the other
		 * is guarded against exactly this — so the host never reaches
		 * IDNA and arrives non-ASCII. Ordered after the non-ASCII case it
		 * could never win, and the truncated-write shape this exists to
		 * name was reported as an unresolvable host instead.
		 *
		 * This used to be duplicated: a live switch case and a dead `if`
		 * after the switch, same condition, different wording, with the
		 * explanation attached to the unreachable one.
		 */
		return fmt.Errorf("%w: appOrigins contains %q, whose port is empty; "+
			"that is what a truncated write looks like", ErrUIJoinIncomplete, o)
	case u.Path != "":
		return fmt.Errorf("%w: appOrigins contains %q; an origin carries no path",
			ErrUIJoinIncomplete, o)
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("%w: appOrigins contains %q; an origin carries no query",
			ErrUIJoinIncomplete, o)
	case u.Fragment != "":
		return fmt.Errorf("%w: appOrigins contains %q; an origin carries no fragment",
			ErrUIJoinIncomplete, o)
	case strings.Contains(u.Hostname(), "%"):
		// An IPv6 ZONE INDEX (`[fe80::1%25eth0]`) is a link-local scope
		// identifier that names an interface on one machine. No
		// location.origin ever carries one, so an annotation holding one
		// is unjoinable by construction — the same class as the trailing
		// dot and the IPv4-mapped literal canonicalHost was written for,
		// and it must be refused rather than canonicalised, because there
		// is no form of it a browser would produce. A percent sign in a
		// host is otherwise an escape, which is malformed here too.
		return fmt.Errorf("%w: appOrigins contains %q; a host carrying %% is either "+
			"an IPv6 zone index or a percent-escape, and a browser origin is neither",
			ErrUIJoinIncomplete, o)
	case !isResolvableDNSName(canonicalHost(u.Hostname())):
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
		return fmt.Errorf("%w: appOrigins contains %q, whose host is not a "+
			"resolvable DNS name (an empty label, a label over 63 octets, or a "+
			"name over 253)", ErrUIJoinIncomplete, o)
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
		return fmt.Errorf("%w: appOrigins contains %q, whose host is not a domain "+
			"name any browser can resolve (it fails the same IDNA rules a URL bar "+
			"applies); a browser origin is always ASCII once resolved",
			ErrUIJoinIncomplete, o)
	case u.User != nil:
		return fmt.Errorf("%w: appOrigins contains %q; an origin carries no credentials",
			ErrUIJoinIncomplete, o)
	}
	// The PORT gets the same rigour ingressPorts gets. The const block
	// above argues that port 0 "can never be a value something was
	// observed on" and that an out-of-range entry is what a mis-decoded
	// read looks like; both are as true in an origin as in a port list.
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return fmt.Errorf("%w: appOrigins contains %q, whose port is not a number",
				ErrUIJoinIncomplete, o)
		}
		if err := uiJoinPortInRange(int64(n)); err != nil {
			return fmt.Errorf("%w: appOrigins contains %q, whose port is outside %d-%d",
				ErrUIJoinIncomplete, o, minIngressPort, maxIngressPort)
		}
	}
	return nil
}

// Ingress ports are TCP ports the recorder bound, so 0 is as wrong as
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
*/
/*
Normalized returns the canonical form of this annotation — the exact
value SetUIJoinAnnotation will store.

EXPORTED FOR THE PRODUCER, WHICH DOES NOT EXIST YET. Nothing in this
repo calls it — the same is true of the annotation as a whole, which has
no writer on either side. It is part of the contract this package
publishes, not a claim that something is using it.

Set deliberately does not mutate its argument, so a caller that built an
annotation from, say, strings.Split(os.Getenv("APP_ORIGINS"), ",") keeps
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
	u.Host = trimTrailingDot(u.Host)
	// THE PORT TOO. `:80`, `:080` and `:0080` are one origin written
	// three ways, and a browser's location.origin elides the default
	// port entirely — so `http://app.example.com:80` from a hand-written
	// config could never match the browser half. The comment above says
	// storing whichever spelling the producer sent makes the comparison
	// a coin flip; the port was the half still being flipped.
	if port := u.Port(); port != "" {
		trimmedPort := strings.TrimLeft(port, "0")
		if trimmedPort == "" {
			trimmedPort = "0"
		}
		host := canonicalHost(u.Hostname())
		def := (u.Scheme == "http" && trimmedPort == "80") ||
			(u.Scheme == "https" && trimmedPort == "443")
		if def {
			u.Host = host
		} else {
			u.Host = host + ":" + trimmedPort
		}
	}
	if u.Path == "/" {
		u.Path = ""
	}
	if u.Port() == "" && !strings.HasSuffix(u.Host, ":") {
		// An IPv4-mapped literal with no port never reached the branch
		// above, which only runs when a port is present.
		//
		// The trailing-colon guard keeps `http://localhost:` malformed.
		// It has no port and a Host of "localhost:"; rewriting it from
		// Hostname() would silently repair it into a valid origin, and an
		// empty port is refused on purpose — it is what a truncated write
		// looks like.
		u.Host = canonicalHost(u.Hostname())
	}
	return u.String()
}

/*
canonicalHost is the one spelling of a host this package stores.

IDEMPOTENT, and that is the point: both branches above call it, and an
earlier version had each doing its own bracket-wrapping, so an IPv6
literal that went through both came out as `[[::1]]`. Takes a host with
no port, in either bracketed or bare form.
*/
func canonicalHost(hostname string) string {
	host := unmapIPv4(trimTrailingDot(strings.Trim(hostname, "[]")))
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return toASCIIHost(host)
}

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

func toASCIIHost(host string) string {
	if isASCII(host) {
		// Fast path, and the only path for every host in practice.
		// Percent-decoding first would be wrong here: an escape in an
		// ASCII host is not an IDN, it is a malformed host.
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
	trimmed := strings.TrimSuffix(name, ".")
	if trimmed == "" || len(trimmed) > 253 {
		return false
	}
	for _, label := range strings.Split(trimmed, ".") {
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
// Not applied to an IPv6 literal, where the brackets are the delimiter
// and a trailing dot would be malformed rather than canonical.
func trimTrailingDot(host string) string {
	if strings.HasSuffix(host, "]") || !strings.HasSuffix(host, ".") {
		return host
	}
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
	// returns a nil annotation on every error path, and a caller that
	// checks a.Complete() before err panics without it.
	return a != nil && a.T1WallMs > 0
}

/*
SetUIJoinAnnotation stores the annotation on a test-set.

FOR THE RECORD-TIME PRODUCER, three constraints worth knowing before you
wire this up:

  - It CANNOT be called at t0 with placeholders. Validate requires a real
    CaptureID, SessionNonce and at least one AppOrigin, all of which come
    from the browser, so the recorder must wait for that handshake before
    the first write. The "T1WallMs = 0 while recording" state therefore
    only reaches disk for a capture whose browser attached before
    recording started; for any other, the annotation is written once, at
    stop, already complete.
  - Updating T1WallMs at stop is a read-modify-write through
    GetUIJoinAnnotation and this function, and this function REPLACES the
    whole map. A key written by a newer producer under the same
    specVersion is silently dropped by an older stop-time update. A
    narrow finish-only helper would be cheaper and forward-safe.
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
	// strings.Split(os.Getenv("APP_ORIGINS"), ","). It then stored the
	// padded value verbatim, so " http://localhost:3000" validated and
	// was written, and a joiner comparing an exchange origin against it
	// would silently never match — the exact failure
	// uiJoinOriginIsWellFormed exists to prevent. For CaptureID the
	// padding corrupts the join key itself.
	normalized := a.normalized()
	if err := normalized.validateNormalized(); err != nil {
		// Refuse to persist an unusable annotation. Writing it would
		// create a test-set that advertises a join it cannot support.
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
GetUIJoinAnnotation reads the annotation back.

Returns ErrUIJoinAbsent when there is none — which is the ordinary case
for every test-set recorded without a browser alongside, and is not an
error condition for the caller to log loudly.
*/
func GetUIJoinAnnotation(ts *TestSet) (*UIJoinAnnotation, error) {
	if ts == nil || ts.Metadata == nil {
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
	if err := a.validateNormalized(); err != nil {
		// A stored annotation that no longer validates is reported as
		// such rather than returned half-usable.
		return nil, err
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
// path produce. Anything else — bson.Raw, for one — fails closed, and
// the caller reports it with the concrete type it saw.
// uiJoinStringMap returns the document, or nil and the reason there is
// none — phrased for whoever wrote the value, not for this package.
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
