package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

func validAnnotation() *UIJoinAnnotation {
	return &UIJoinAnnotation{
		SpecVersion:      UIJoinSpecVersion,
		CaptureID:        "cap_01HZY",
		SessionNonce:     "n_7f3a9c",
		T0WallMs:         1789000000000,
		T1WallMs:         1789000060000,
		IngressPorts:     []int{8080, 8443},
		AppOrigins:       []string{"http://localhost:3000"},
		CanonicalKeySpec: "canonical-key.v1",
	}
}

/* ------------------------------------------------------------------ */
/*  Validation fails closed                                            */
/* ------------------------------------------------------------------ */

func TestValidateAcceptsACompleteAnnotation(t *testing.T) {
	if err := validAnnotation().Validate(); err != nil {
		t.Fatalf("expected a complete annotation to validate, got %v", err)
	}
}

func TestValidateRejectsEveryIncompleteForm(t *testing.T) {
	// A partial annotation is worse than none: it looks joinable and
	// produces edges nobody can trust, and the failure is silent because a
	// wrong join still returns pairs.
	cases := map[string]func(*UIJoinAnnotation){
		"no captureId":        func(a *UIJoinAnnotation) { a.CaptureID = "" },
		"blank captureId":     func(a *UIJoinAnnotation) { a.CaptureID = "   " },
		"no sessionNonce":     func(a *UIJoinAnnotation) { a.SessionNonce = "" },
		"no canonicalKeySpec": func(a *UIJoinAnnotation) { a.CanonicalKeySpec = "" },
		"no t0":               func(a *UIJoinAnnotation) { a.T0WallMs = 0 },
		"negative t0":         func(a *UIJoinAnnotation) { a.T0WallMs = -1 },
		"t1 before t0":        func(a *UIJoinAnnotation) { a.T1WallMs = a.T0WallMs - 1 },
		"no ingress ports":    func(a *UIJoinAnnotation) { a.IngressPorts = nil },
		"empty ingress ports": func(a *UIJoinAnnotation) { a.IngressPorts = []int{} },
		// A WHITESPACE origin is the structural case, not the
		// un-joinable one: it normalizes to "" and no producer working
		// correctly emits it. The genuinely empty list is
		// TestAnUnjoinableCaptureIsRecordedNotDiscarded.
		"blank app origin": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"  "} },
		// A joiner comparing an exchange origin against any of these
		// classifies EVERY exchange FOREIGN_ORIGIN, with nothing to say
		// why — the same silent-wrong-answer the port range check exists
		// to prevent.
		"origin is not a URL": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"not a url at all"} },
		// Bare "*" is refused for having no scheme, which made this case
		// read as wildcard coverage it never provided. The wildcard rule
		// has its own test; this one keeps the no-scheme shape it
		// actually exercises.
		"origin is a bare asterisk": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"*"} },
		"origin has no scheme":      func(a *UIJoinAnnotation) { a.AppOrigins = []string{"localhost:3000"} },
		// NAMED FOR THE RULE THAT REFUSES IT. There is no scheme check —
		// the allowlist was deleted on purpose — so this is refused for
		// having no host, like the case below it. The old name implied
		// dangerous schemes were rejected as such; they are not
		// (`javascript://evil.com` is storable), and a name implying
		// coverage that does not exist is how the wildcard hole hid.
		"opaque url has no host": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"javascript:alert(1)"} },
		// Isolates the HOST check: the scheme is fine, there is no host.
		"origin has no host":     func(a *UIJoinAnnotation) { a.AppOrigins = []string{"http://"} },
		"origin carries a path":  func(a *UIJoinAnnotation) { a.AppOrigins = []string{"http://a/path"} },
		"origin carries a query": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"http://a?q=1"} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			mutate(a)
			if err := a.Validate(); err == nil {
				t.Fatalf("expected %s to be rejected, it validated", name)
			}
		})
	}
}

func TestValidateAcceptsEveryWellFormedOrigin(t *testing.T) {
	// The other half of the rule: a legitimate origin must not be
	// rejected, whatever it is called. `date.example.com` and
	// `b3.example.com` are ordinary host labels that an over-eager header
	// scanner used to flag as trace dependencies.
	for _, origin := range []string{
		"http://localhost:3000", "https://app.example.com",
		"https://app.example.com:8443", "https://date.example.com",
		"https://b3.example.com", "http://127.0.0.1:5173",
	} {
		a := validAnnotation()
		a.AppOrigins = []string{origin}
		if err := a.Validate(); err != nil {
			t.Errorf("%q is a well-formed origin, rejected: %v", origin, err)
		}
		ts := &TestSet{}
		if err := SetUIJoinAnnotation(ts, a); err != nil {
			t.Errorf("%q could not be stored: %v", origin, err)
			continue
		}
		if refs := flakyHeaderRefs(ts.Metadata[UIJoinMetadataKey]); len(refs) != 0 {
			t.Errorf("%q flagged as a header dependency: %v", origin, refs)
		}
	}
}

func TestStoredValuesAreNormalized(t *testing.T) {
	// Validate has always compared the TRIMMED form — an explicit
	// acknowledgement that whitespace arrives — and then stored the
	// padded value verbatim. A joiner comparing an exchange origin
	// against " http://localhost:3000" silently never matches, and for
	// captureId the padding corrupts the join key itself.
	a := &UIJoinAnnotation{
		SpecVersion:      UIJoinSpecVersion,
		CaptureID:        "  cap_01HZY\n",
		SessionNonce:     " n_7f3a9c ",
		T0WallMs:         1789000000000,
		T1WallMs:         1789000060000,
		IngressPorts:     []int{8080},
		AppOrigins:       []string{" http://App.Example.COM:3000/ "},
		CanonicalKeySpec: " canonical-key.v1 ",
	}
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := GetUIJoinAnnotation(ts)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CaptureID != "cap_01HZY" {
		t.Errorf("captureId stored as %q", got.CaptureID)
	}
	if got.SessionNonce != "n_7f3a9c" {
		t.Errorf("sessionNonce stored as %q", got.SessionNonce)
	}
	if got.CanonicalKeySpec != "canonical-key.v1" {
		t.Errorf("canonicalKeySpec stored as %q", got.CanonicalKeySpec)
	}
	// Hosts are case-insensitive and a trailing slash names the same
	// origin, but a joiner comparing strings is neither.
	if len(got.AppOrigins) != 1 || got.AppOrigins[0] != "http://app.example.com:3000" {
		t.Errorf("origin stored as %q", got.AppOrigins)
	}
	// The caller's own struct must not be mutated underneath it.
	if a.CaptureID != "  cap_01HZY\n" {
		t.Errorf("the caller's annotation was modified in place")
	}
}

func TestOriginPortsAreCanonicalized(t *testing.T) {
	// `:80`, `:080` and `:0080` are ONE origin written three ways, and a
	// browser's location.origin elides the default port entirely — so
	// `http://app.example.com:80` from a hand-written config could never
	// match the browser half. The whole port block had no test at all.
	for raw, want := range map[string]string{
		"http://a.example.com:80":    "http://a.example.com",
		"http://a.example.com:080":   "http://a.example.com",
		"http://a.example.com:0080":  "http://a.example.com",
		"https://a.example.com:443":  "https://a.example.com",
		"https://a.example.com:0443": "https://a.example.com",
		"http://a.example.com:8080":  "http://a.example.com:8080",
		"http://a.example.com:08080": "http://a.example.com:8080",
		"https://a.example.com:443x": "https://a.example.com:443x",
		// The default port for the OTHER scheme is not a default here.
		"http://a.example.com:443": "http://a.example.com:443",
		"https://a.example.com:80": "https://a.example.com:80",
		// IPv6 has to keep its brackets, or the host stops parsing.
		"http://[::1]:080":           "http://[::1]",
		"http://[2001:db8::1]:08080": "http://[2001:db8::1]:8080",
		"http://[::1]:3000":          "http://[::1]:3000",
	} {
		t.Run(raw, func(t *testing.T) {
			got := normalizeOrigin(raw)
			if got != want {
				t.Errorf("normalizeOrigin(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestAnEmptyPortIsNotAnOrigin(t *testing.T) {
	// `http://a:` has no port for u.Port() to report, so the whole
	// canonicalization block is skipped and it was stored verbatim —
	// where it can never match a browser's `http://a`.
	a := validAnnotation()
	a.AppOrigins = []string{"http://a.example.com:"}
	if err := a.Validate(); err == nil {
		t.Fatal("a trailing colon with no port must not validate")
	}
}

func TestOriginRulesAreEachReachable(t *testing.T) {
	// As one four-way branch only two of the four were reachable by any
	// test, so two of the rules were asserted by nothing.
	for name, origin := range map[string]string{
		"a path":                    "http://a.example.com/path",
		"a query":                   "http://a.example.com?q=1",
		"a forced query":            "http://a.example.com?",
		"a fragment":                "http://a.example.com#frag",
		"credentials":               "http://user:pass@a.example.com",
		"port zero":                 "http://a.example.com:0",
		"a port past the TCP range": "http://a.example.com:99999",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			if err := a.Validate(); err == nil {
				t.Fatalf("%q validated", origin)
			}
		})
	}
}

func TestAHandWrittenConfigIsNormalizedOnRead(t *testing.T) {
	// THE PATH THAT MATTERS. Set normalizes, and a hand-written
	// config.yaml never goes through Set — so every normalization test
	// that round-trips Set->Get was blind to the read path, which still
	// had the whole bug.
	for name, tc := range map[string]struct {
		doc   map[string]interface{}
		check func(*testing.T, *UIJoinAnnotation)
	}{
		"a trailing slash does not reject the annotation": {
			doc: map[string]interface{}{"appOrigins": []interface{}{"http://localhost:3000/"}},
			check: func(t *testing.T, a *UIJoinAnnotation) {
				if a.AppOrigins[0] != "http://localhost:3000" {
					t.Errorf("origin read as %q", a.AppOrigins[0])
				}
			},
		},
		"a host's case is normalized": {
			doc: map[string]interface{}{"appOrigins": []interface{}{"http://App.Example.COM:3000"}},
			check: func(t *testing.T, a *UIJoinAnnotation) {
				if a.AppOrigins[0] != "http://app.example.com:3000" {
					t.Errorf("origin read as %q", a.AppOrigins[0])
				}
			},
		},
		"padding does not reach the join key": {
			doc: map[string]interface{}{"captureId": "  cap_01HZY  "},
			check: func(t *testing.T, a *UIJoinAnnotation) {
				if a.CaptureID != "cap_01HZY" {
					t.Errorf("captureId read as %q", a.CaptureID)
				}
			},
		},
		"a padded nonce is trimmed": {
			doc: map[string]interface{}{"sessionNonce": " n_7f3a9c "},
			check: func(t *testing.T, a *UIJoinAnnotation) {
				if a.SessionNonce != "n_7f3a9c" {
					t.Errorf("sessionNonce read as %q", a.SessionNonce)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := map[string]interface{}{
				"specVersion": UIJoinSpecVersion, "captureId": "cap_01HZY",
				"sessionNonce": "n_7f3a9c", "t0WallMs": int64(1789000000000),
				"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
				"appOrigins":       []interface{}{"http://localhost:3000"},
				"canonicalKeySpec": "canonical-key.v1",
			}
			for k, v := range tc.doc {
				doc[k] = v
			}
			got, err := GetUIJoinAnnotation(&TestSet{
				Metadata: map[string]interface{}{UIJoinMetadataKey: doc},
			})
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			tc.check(t, got)
		})
	}
}

func TestValidateRejectsAnOlderSpecVersion(t *testing.T) {
	// Refusing an older annotation is the stated purpose of the field,
	// and `specVersion: 0` — what a missing-then-defaulted value looks
	// like — has to be refused for the same reason. Only the +1 direction
	// was tested, so `!=` could become `>` unnoticed.
	for _, version := range []int{0, UIJoinSpecVersion - 1} {
		a := validAnnotation()
		a.SpecVersion = version
		if err := a.Validate(); !errors.Is(err, ErrUIJoinUnsupported) {
			t.Errorf("specVersion %d: expected ErrUIJoinUnsupported, got %v", version, err)
		}
	}
}

func TestSetRefusesNilArguments(t *testing.T) {
	// Both guards carry a justification comment and had no coverage; a
	// nil annotation becoming a silent no-op success is the worse of the
	// two, since the caller believes it persisted something.
	if err := SetUIJoinAnnotation(nil, validAnnotation()); err == nil {
		t.Error("a nil test-set must be refused")
	}
	ts := &TestSet{}
	err := SetUIJoinAnnotation(ts, nil)
	if err == nil {
		t.Error("a nil annotation must be refused, not silently succeed")
	}
	// And NOT as ErrUIJoinAbsent — "the test-set carries no annotation"
	// blames the test-set for a nil ARGUMENT, and it is the one error
	// callers are told to swallow, so the refusal would read as routine.
	if errors.Is(err, ErrUIJoinAbsent) {
		t.Errorf("a nil argument must not be reported as an absent annotation: %v", err)
	}
	if _, ok := ts.Metadata[UIJoinMetadataKey]; ok {
		t.Error("a refused annotation must not be written")
	}
}

func TestCompleteIsSafeOnTheErrorPath(t *testing.T) {
	// GetUIJoinAnnotation returns a nil annotation on every error path
	// EXCEPT ErrUIJoinNotJoinable (an intact capture with no attributable
	// origin, returned alongside its refusal), and a caller that checks
	// a.Complete() before err would panic on any of the others.
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: "not a map"}}
	got, err := GetUIJoinAnnotation(ts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got.Complete() {
		t.Fatal("a nil annotation must not report complete")
	}
}

func TestValidateRejectsAnUnknownSpecVersion(t *testing.T) {
	// A consumer must refuse an annotation it cannot interpret rather than
	// read it under the wrong rules.
	a := validAnnotation()
	a.SpecVersion = UIJoinSpecVersion + 1
	err := a.Validate()
	if !errors.Is(err, ErrUIJoinUnsupported) {
		t.Fatalf("expected ErrUIJoinUnsupported, got %v", err)
	}
}

func TestNilAnnotationIsAbsentNotValid(t *testing.T) {
	var a *UIJoinAnnotation
	if !errors.Is(a.Validate(), ErrUIJoinAbsent) {
		t.Fatal("a nil annotation must report absent, never valid")
	}
}

func TestCompleteDistinguishesAnInProgressCapture(t *testing.T) {
	// Joining against a capture still being written is a race the joiner
	// should decline rather than resolve.
	a := validAnnotation()
	if !a.Complete() {
		t.Fatal("a finished capture must report complete")
	}
	a.T1WallMs = 0
	if a.Complete() {
		t.Fatal("a capture with no end time must not report complete")
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("an in-progress annotation is still VALID, just not complete: %v", err)
	}
}

/* ------------------------------------------------------------------ */
/*  Round trip                                                         */
/* ------------------------------------------------------------------ */

func TestRoundTripThroughTestSet(t *testing.T) {
	ts := &TestSet{}
	want := validAnnotation()

	if err := SetUIJoinAnnotation(ts, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := GetUIJoinAnnotation(ts)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.CaptureID != want.CaptureID ||
		got.SessionNonce != want.SessionNonce ||
		got.T0WallMs != want.T0WallMs ||
		got.T1WallMs != want.T1WallMs ||
		got.CanonicalKeySpec != want.CanonicalKeySpec {
		t.Fatalf("round trip lost a scalar: got %+v want %+v", got, want)
	}
	if len(got.IngressPorts) != 2 || got.IngressPorts[0] != 8080 || got.IngressPorts[1] != 8443 {
		t.Fatalf("ingress ports did not survive: %v", got.IngressPorts)
	}
	if len(got.AppOrigins) != 1 || got.AppOrigins[0] != "http://localhost:3000" {
		t.Fatalf("app origins did not survive: %v", got.AppOrigins)
	}
}

func TestRoundTripSurvivesYAML(t *testing.T) {
	// The annotation is persisted to keploy/<id>/config.yaml, so a YAML
	// round trip is the real storage path, not a hypothetical one.
	//
	// This covers what yaml.v3 ACTUALLY produces, which is
	// map[string]interface{}. The interface-keyed shape this file used to
	// claim it exercised here is covered by
	// TestReadAcceptsEveryDocumentShapeACodecProduces, built directly —
	// deleting the map[interface{}]interface{} branch used to leave this
	// suite entirely green.
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}

	encoded, err := yaml.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded TestSet
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got, err := GetUIJoinAnnotation(&decoded)
	if err != nil {
		t.Fatalf("get after yaml: %v\n---\n%s", err, encoded)
	}
	if got.T0WallMs != 1789000000000 {
		t.Fatalf("millisecond timestamp did not survive YAML: %d", got.T0WallMs)
	}
	if len(got.IngressPorts) != 2 {
		t.Fatalf("ports did not survive YAML: %v", got.IngressPorts)
	}
}

func TestRoundTripSurvivesJSON(t *testing.T) {
	// The upload path is JSON, where every number arrives as float64.
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}

	encoded, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded TestSet
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got, err := GetUIJoinAnnotation(&decoded)
	if err != nil {
		t.Fatalf("get after json: %v", err)
	}
	if got.T0WallMs != 1789000000000 {
		t.Fatalf("millisecond timestamp did not survive JSON: %d", got.T0WallMs)
	}
	if got.SpecVersion != UIJoinSpecVersion {
		t.Fatalf("spec version did not survive JSON: %d", got.SpecVersion)
	}
}

/* ------------------------------------------------------------------ */
/*  Absence and refusal                                                */
/* ------------------------------------------------------------------ */

func TestAbsentIsNotAnError(t *testing.T) {
	// Every test-set recorded without a browser has no annotation. That is
	// the ordinary case, and it must be distinguishable from a corrupt one
	// so a caller does not log it as a fault.
	for name, ts := range map[string]*TestSet{
		"nil test-set":  nil,
		"nil metadata":  {},
		"othermetadata": {Metadata: map[string]interface{}{"unrelated": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := GetUIJoinAnnotation(ts)
			if !errors.Is(err, ErrUIJoinAbsent) {
				t.Fatalf("expected ErrUIJoinAbsent, got %v", err)
			}
		})
	}
}

func TestRefusesToPersistAnUnusableAnnotation(t *testing.T) {
	// Writing a partial annotation would create a test-set advertising a
	// join it cannot support.
	ts := &TestSet{}
	bad := validAnnotation()
	bad.SessionNonce = ""

	if err := SetUIJoinAnnotation(ts, bad); err == nil {
		t.Fatal("expected an incomplete annotation to be refused")
	}
	if _, ok := ts.Metadata[UIJoinMetadataKey]; ok {
		t.Fatal("a refused annotation must not be written")
	}
}

func TestCorruptMetadataIsReportedNotGuessed(t *testing.T) {
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: "not a map"}}
	_, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinNotAnnotation) {
		t.Fatalf("expected ErrUIJoinNotAnnotation, got %v", err)
	}
}

func TestAnnotationStoredIncompleteIsRejectedOnRead(t *testing.T) {
	// Hand-edited or partially-written metadata must not read back as a
	// usable annotation.
	ts := &TestSet{Metadata: map[string]interface{}{
		UIJoinMetadataKey: map[string]interface{}{
			"specVersion": UIJoinSpecVersion,
			"captureId":   "cap_1",
			// sessionNonce deliberately missing
			"t0WallMs":         int64(1789000000000),
			"ingressPorts":     []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}}
	// COMPLETE EXCEPT for sessionNonce. It used to omit appOrigins too, so
	// putting sessionNonce back left the test green — the missing origin
	// silently took over and the test no longer tested what it named.
	err := mustFailIncomplete(t, ts)
	if !strings.Contains(err.Error(), "sessionNonce") {
		t.Fatalf("error must name the deliberately-missing field, got %v", err)
	}
}

// mustFailIncomplete asserts a stored annotation is rejected as
// incomplete and returns the error, so a caller can check WHICH field was
// named rather than trusting that the intended one was the cause.
func mustFailIncomplete(t *testing.T, ts *TestSet) error {
	t.Helper()
	_, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("expected ErrUIJoinIncomplete, got %v", err)
	}
	return err
}

func TestSetPreservesUnrelatedMetadata(t *testing.T) {
	ts := &TestSet{Metadata: map[string]interface{}{"team": "payments"}}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}
	if ts.Metadata["team"] != "payments" {
		t.Fatal("the annotation must not disturb a user's own metadata")
	}
}

/* ------------------------------------------------------------------ */
/*  The header rule                                                    */
/* ------------------------------------------------------------------ */

func TestJoinCarriesNoHeaderTheMatcherForgives(t *testing.T) {
	// The design rule, pinned as a test: this annotation must never come
	// to depend on a header that FlakyHeaders auto-noises, because the
	// matcher is engineered to forgive exactly those and the browser mints
	// a fresh value at replay.
	if len(FlakyHeaders) < 20 {
		t.Fatalf("FlakyHeaders has %d entries; this test is only meaningful against the real list", len(FlakyHeaders))
	}

	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}

	for _, ref := range flakyHeaderRefs(ts.Metadata[UIJoinMetadataKey]) {
		t.Errorf("the stored annotation %s", ref)
	}
	for _, ref := range flakyHeaderRefsInType(reflect.TypeOf(UIJoinAnnotation{})) {
		t.Errorf("UIJoinAnnotation %s", ref)
	}
}

/*
TestTheAnnotationFieldSetIsFixed is the rule's real enforcement.

Matching Go field names against a list of WIRE HEADER NAMES cannot be both
complete and quiet, and trying made it neither. A suffix rule loose enough
to see `SpanID` in `x-b3-spanid` also saw `UserAgent` in
`x-amz-user-agent` (plain `user-agent` is NOT on the list, so that field
would be perfectly legal), `Headers` in `x-amz-signedheaders`, and
`Context` in `x-cloud-trace-context` — and it still missed `SpanID`,
because six characters fell under the threshold that kept `Timestamp`
from firing. Non-monotonic, unpinnable, and on its way to being deleted
the first time it blocked a legitimate field.

So the field set is pinned EXACTLY instead. Every addition fails here and
a person checks it against FlakyHeaders once, by hand, which is the only
place that judgement can actually be made. flakyHeaderRefs still runs
alongside for values and map keys, where exact matching IS decidable.
*/
func TestTheAnnotationFieldSetIsFixed(t *testing.T) {
	want := []string{
		"AppOrigins",
		"CanonicalKeySpec",
		"CaptureID",
		"IngressPorts",
		"SessionNonce",
		"SpecVersion",
		"T0WallMs",
		"T1WallMs",
	}
	got := structFieldPaths(reflect.TypeOf(UIJoinAnnotation{}), "")
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf(`UIJoinAnnotation's field set changed.
  want: %v
   got: %v

Before updating this list, check the new field against models.FlakyHeaders
BY HAND. A join that depends on a header the matcher auto-noises cannot
work: the matcher is built to forgive exactly those, and the browser mints
a fresh value at replay, so a mock keyed on one could never match. If the
new field is fine, add it here.`, want, got)
	}
}

func TestStructFieldPathsSeesThroughNesting(t *testing.T) {
	// The positive control for the walker TestTheAnnotationFieldSetIsFixed
	// depends on. Without it, a walker that stopped at the top level would
	// still pin UIJoinAnnotation correctly — it is flat — and the pin
	// would quietly stop covering the one case it exists for: a
	// dependency added one level down.
	type inner struct {
		Traceparent string
	}
	type outer struct {
		CaptureID string
		inner
		Named   inner
		Pointer *inner
		Many    []inner
		// The both-miss: changing an EXISTING field from []string to a
		// map of structs left the path set identical, because the field
		// name does not change — so a header dependency added by a type
		// change slipped past this pin AND the type walker.
		Keyed map[string]inner
	}
	got := structFieldPaths(reflect.TypeOf(outer{}), "")
	sort.Strings(got)
	want := []string{
		"CaptureID",
		"Keyed.Traceparent",
		"Many.Traceparent",
		"Named.Traceparent",
		"Pointer.Traceparent",
		"inner.Traceparent",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("walker missed a nested field\n want: %v\n  got: %v", want, got)
	}
}

/*
TestTheWireKeysAreTheStructTags closes the gap the field-set pin does not.

The pin compares field NAMES, so retagging CaptureID as
`json:"traceId" yaml:"traceId" bson:"trace_id"` changed nothing it could
see — a genuine trace dependency, in the most idiomatic spelling,
invisible to it. And the on-disk contract is not the struct at all: it is
the map SetUIJoinAnnotation writes, whose keys were guarded by exact
matching alone and by no pin whatsoever, so adding "traceId" or
"clientRequestId" to it was silent.

Pinning the map keys to the struct's json tags makes the two describe
each other: a tag rename shows up as a key-set difference, and a key
added to the map without a field shows up as an extra.
*/
func TestTheWireKeysAreTheStructTags(t *testing.T) {
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}
	stored, why := uiJoinStringMap(ts.Metadata[UIJoinMetadataKey])
	if stored == nil {
		t.Fatalf("stored value is not a document (%T): %s",
			ts.Metadata[UIJoinMetadataKey], why)
	}

	written := make([]string, 0, len(stored))
	for k := range stored {
		written = append(written, k)
	}
	sort.Strings(written)

	// EVERY codec tag, not just json. The pin compared map keys to the
	// json tags alone, so retagging a field's yaml or bson name — and
	// yaml is what names the key in config.yaml, the file this thing
	// actually lives in — changed nothing it could see.
	typ := reflect.TypeOf(UIJoinAnnotation{})
	for _, codec := range []string{"json", "yaml", "bson"} {
		tagged := make([]string, 0, typ.NumField())
		for i := range typ.NumField() {
			tag := strings.Split(typ.Field(i).Tag.Get(codec), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			tagged = append(tagged, tag)
		}
		sort.Strings(tagged)
		if !reflect.DeepEqual(written, tagged) {
			t.Errorf(`the stored keys and the %s tags disagree.
  stored: %v
  %6s: %v

These are the same contract seen twice. A key written without a field, or
a tag renamed without changing the map, means the two halves of the join
no longer agree about what the document is called.`, codec, written, codec, tagged)
		}
	}

	// And the wire keys are subject to the same header rule as the field
	// names, since the keys are what a reader actually sees.
	for _, key := range written {
		if h, ok := matchesFlakyHeader(key); ok {
			t.Errorf("wire key %q references %q, which the matcher auto-noises", key, h)
		}
	}
}

func TestTheFlakyHeaderRuleIsActuallyEnforced(t *testing.T) {
	// The POSITIVE CONTROL the rule needs to mean anything: without it, a
	// detector that always returned "clean" left the rule green and the
	// constraint silently unenforced.
	for name, doc := range map[string]map[string]interface{}{
		"a header as a field name":     {"traceparent": "00-aaaa-bbbb-01"},
		"an idiomatic Go field name":   {"requestId": "abc"},
		"another idiomatic field name": {"correlationId": "abc"},
		"a header named in a value":    {"joinKey": "X-B3-TraceId"},
		"a header in a piped value":    {"spec": "method|path|traceparent"},
		"a header in a dotted value":   {"spec": "headers.x-request-id.value"},
		"nested under another field":   {"join": map[string]interface{}{"baggage": "x"}},
		// appOrigins and ingressPorts are the only list fields this
		// annotation has, so the list branch existed for data no test
		// supplied — deleting it left the suite green.
		"inside a plain list":      {"origins": []interface{}{"https://traceparent.example.com"}},
		"inside a bson.A list":     {"origins": bson.A{"https://tracestate.example.com"}},
		"a serialized header line": {"note": "X-B3-TraceId: abc123"},
	} {
		t.Run(name, func(t *testing.T) {
			if refs := flakyHeaderRefs(doc); len(refs) == 0 {
				t.Fatalf("detector missed a planted dependency in %v", doc)
			}
		})
	}

	t.Run("the TYPE walker finds a planted field", func(t *testing.T) {
		type poisoned struct {
			CaptureID   string `json:"captureId"`
			Traceparent string `json:"traceparent"`
			RequestID   string `json:"requestId"`
		}
		if refs := flakyHeaderRefsInType(reflect.TypeOf(poisoned{})); len(refs) != 2 {
			t.Fatalf("expected both planted fields, got %v", refs)
		}
	})

	t.Run("the TYPE walker reads codec tags, not just field names", func(t *testing.T) {
		// Each case carries the header in exactly ONE place, so the arm
		// under test is the only thing that can catch it. Every planted
		// field used to carry a matching json tag ALONGSIDE its name, so
		// the tag arm always covered for the name arm and deleting the
		// name arm changed nothing.
		for name, typ := range map[string]reflect.Type{
			"the field name alone, untagged": reflect.TypeOf(struct {
				Traceparent string
			}{}),
			"a json tag only": reflect.TypeOf(struct {
				Innocuous string `json:"x-correlation-id"`
			}{}),
			// yaml names the key in config.yaml, the file this annotation
			// actually lives in.
			"a yaml tag only": reflect.TypeOf(struct {
				Innocuous string `yaml:"traceparent"`
			}{}),
			// bson tags on this struct are snake_case, which is why
			// normalizeHeaderish strips "_" as well as "-".
			"a snake_case bson tag only": reflect.TypeOf(struct {
				Innocuous string `bson:"trace_parent"`
			}{}),
			// `json:"traceparent,omitempty"` is the realistic spelling.
			"a tag with options": reflect.TypeOf(struct {
				Innocuous string `json:"traceparent,omitempty"`
			}{}),
			"inside a map value": reflect.TypeOf(struct {
				Origins map[string]struct {
					Traceparent string
				}
			}{}),
		} {
			if refs := flakyHeaderRefsInType(typ); len(refs) == 0 {
				t.Errorf("%s: walker found nothing", name)
			}
		}
	})

	t.Run("a self-referential type terminates", func(t *testing.T) {
		// Both walkers used to recurse until the package-wide 10-minute
		// panic timeout: 600 seconds of CI and no diagnostic, for a
		// routine struct change.
		type recursive struct {
			CaptureID string
			Parent    *recursive
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = structFieldPaths(reflect.TypeOf(recursive{}), "")
			_ = flakyHeaderRefsInType(reflect.TypeOf(recursive{}))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the walkers did not terminate on a self-referential type")
		}
	})

	t.Run("the TYPE walker sees through embedding and nesting", func(t *testing.T) {
		// Embedding a shared struct is the NEXT way a developer adds a
		// dependency, and a one-level walker cannot see it.
		type traceBits struct {
			Traceparent string `json:"traceparent"`
		}
		type outerEmbedded struct {
			CaptureID string
			traceBits
		}
		type outerNamed struct {
			CaptureID string
			Bits      traceBits
		}
		type outerPointer struct {
			Bits *traceBits
		}
		type outerSlice struct {
			Bits []traceBits
		}
		for name, typ := range map[string]reflect.Type{
			"embedded": reflect.TypeOf(outerEmbedded{}),
			"named":    reflect.TypeOf(outerNamed{}),
			"pointer":  reflect.TypeOf(outerPointer{}),
			"slice":    reflect.TypeOf(outerSlice{}),
		} {
			if refs := flakyHeaderRefsInType(typ); len(refs) == 0 {
				t.Errorf("%s: walker missed a nested dependency", name)
			}
		}
	})
}

/*
TestTheHeaderMatcherIsCalibrated pins the FP/FN boundary.

Without it every knob could be set to anything and the suite stayed
green, so "no false positive on realistic data" rested on two hand-picked
strings. These two corpora are what make exact matching a DECISION rather
than a default.
*/
func TestTheHeaderMatcherIsCalibrated(t *testing.T) {
	guilty := []string{
		"traceparent", "Traceparent", "TRACESTATE", "baggage", "b3",
		"x-request-id", "RequestID", "requestId", "request-id",
		"CorrelationID", "x-b3-traceid", "X-B3-SpanId", "authorization",
	}
	for _, name := range guilty {
		t.Run("guilty/"+name, func(t *testing.T) {
			if h, ok := matchesFlakyHeader(name); !ok {
				t.Errorf("%q should be recognised as a flaky header", name)
			} else if h == "" {
				t.Errorf("%q matched but named no entry", name)
			}
		})
	}

	// RECORDED LIMITS. These are genuine dependencies the exact matcher
	// does NOT see, because a Go field name and a wire header name are
	// different namespaces and no mechanical rule spans both without
	// firing on ordinary words. They are covered by
	// TestTheAnnotationFieldSetIsFixed instead, which forces a human to
	// check any new field by hand. Listed so the limit is a recorded
	// decision rather than a curated pass list — the previous corpus
	// contained only spellings that happened to match.
	for _, undetectable := range []string{
		"TraceID", "SpanID", "ParentSpanID", "ClientRequestID",
		"ContentSha256", "SecurityToken",
	} {
		t.Run("undetectable/"+undetectable, func(t *testing.T) {
			if _, ok := matchesFlakyHeader(undetectable); ok {
				t.Errorf(
					"%q is now detected — good, but this list is the "+
						"documented limit; move it to the guilty corpus",
					undetectable,
				)
			}
		})
	}

	// Every one of these fired under the suffix rule that replaced exact
	// matching. `UserAgent` is the one that matters most: recording the
	// browser UA is the obvious next field on this struct, and plain
	// `user-agent` is NOT in FlakyHeaders — only `x-amz-user-agent` is.
	clean := []string{
		"UserAgent", "Timestamp", "Context", "Request", "Headers",
		"Expires", "Priority", "Credential", "Signature", "Sampled",
		"lastUpdated", "captureId", "sessionNonce", "canonicalKeySpec",
		"cap_01HZB3XQ7MDATE9", "app.update.example.com",
		"method, path, headers, body", "method|path|headers",
		"http://localhost:3000", "canonical-key.v1",
	}
	for _, name := range clean {
		t.Run("clean/"+name, func(t *testing.T) {
			for _, token := range headerishTokens(name) {
				if h, ok := matchesFlakyHeader(token); ok {
					t.Errorf("%q (token %q) must not be flagged; matched %q", name, token, h)
				}
			}
		})
	}
}

func TestHeaderishTokensSplitsOnEverySeparatorItClaims(t *testing.T) {
	// Only "|" and "." were pinned; deleting " ", ":", ",", "/" or the
	// whole remaining block each left the suite green. The doc cites
	// "X-B3-TraceId: abc" as the motivating case and no test supplied a
	// serialized header line.
	for _, sep := range []string{
		" ", "\t", "\n", ":", ",", ";", "=", `"`, "'", "/", "?", "&",
		"(", ")", "[", "]", "{", "}", "<", ">", "+", "|", ".",
	} {
		value := "prefix" + sep + "traceparent" + sep + "suffix"
		if _, ok := matchesFlakyHeaderInValue("prefix" + sep + "traceparent"); ok {
			t.Fatalf("precondition: the joined form must not match on its own")
		}
		found := false
		for _, token := range headerishTokens(value) {
			if _, ok := matchesFlakyHeaderInValue(token); ok {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not treated as a separator, so a header beside it is invisible", sep)
		}
	}

	// "-" is NOT a separator: it is part of a header name.
	tokens := headerishTokens("x-request-id")
	if len(tokens) != 1 || tokens[0] != "x-request-id" {
		t.Fatalf(`"-" must not split a header name, got %v`, tokens)
	}
}

func TestTheValueRuleIsDrivenThroughTheWalkers(t *testing.T) {
	// THE WIRING, not just the rule. Both walkers call
	// matchesFlakyHeader; swapping either to the value-scoped variant
	// left the suite green, silently undoing the guarantee that field
	// names and codec tags match EVERY entry exactly.
	t.Run("a field named after a short generic entry is still flagged", func(t *testing.T) {
		for _, name := range []string{"date", "b3", "baggage"} {
			doc := map[string]interface{}{name: "x"}
			if refs := flakyHeaderRefs(doc); len(refs) == 0 {
				t.Errorf("a map key %q must be flagged", name)
			}
		}
		type poisoned struct {
			Date string
			B3   string
		}
		if refs := flakyHeaderRefsInType(reflect.TypeOf(poisoned{})); len(refs) != 2 {
			t.Errorf("struct fields named after short entries must be flagged, got %v", refs)
		}
	})

	t.Run("a VALUE containing one is not", func(t *testing.T) {
		for _, origin := range []string{
			"https://date.example.com", "https://b3.example.com",
			"https://baggage.example.com", "https://baggage.airline.example.com",
		} {
			doc := map[string]interface{}{"appOrigins": []interface{}{origin}}
			if refs := flakyHeaderRefs(doc); len(refs) != 0 {
				t.Errorf("%q is an ordinary origin, flagged: %v", origin, refs)
			}
		}
	})
}

func TestTheValueRuleIgnoresShortGenericEntries(t *testing.T) {
	// `b3` (2 characters) and `date` (4) are legitimate DNS labels. A
	// field NAMED one of them is worth a human look and the field-set pin
	// forces one; a host called date.example.com is not.
	// Single-word and ambiguous: each is a legitimate DNS label.
	for _, generic := range []string{"b3", "date", "baggage"} {
		if _, ok := matchesFlakyHeaderInValue(generic); ok {
			t.Errorf("%q must not be matched inside a value", generic)
		}
		if _, ok := matchesFlakyHeader(generic); !ok {
			t.Errorf("%q must still be matched as a field NAME", generic)
		}
	}
	// And the rule must not have swallowed the entries that matter. Every
	// hyphenated entry is safe inside a value by construction; the
	// single-word ones are admitted by name, and these are the names.
	for _, real := range []string{
		"traceparent", "tracestate", "authorization",
		"x-request-id", "x-b3-traceid", "x-amz-security-token", "x-amz-date",
	} {
		if _, ok := matchesFlakyHeaderInValue(real); !ok {
			t.Errorf("%q must still be matched inside a value", real)
		}
	}
}

/*
flakyHeaderRefs reports every way a decoded annotation references a header
FlakyHeaders auto-noises.

MATCHING IS EXACT, on normalized tokens: both sides are lowercased and
stripped of "-" and "_", so the Go spelling `requestId` is recognised as
the entry `request-id`. Values are split into tokens first, because a
header name can sit inside a larger string ("X-B3-TraceId: abc").

NO SUBSTRING OR SUFFIX MATCHING. Containment flagged every ULID capture id
containing B3 against the two-character `b3` entry, and an origin of
app.update.example.com against `date`. Suffix matching replaced those with
worse ones: `Headers` against `x-amz-signedheaders`, and a canonicalKeySpec
of "method, path, headers, body" — an entirely ordinary value for the
field whose job is to name what the normalizer keys on. Struct growth is
covered by TestTheAnnotationFieldSetIsFixed instead, which needs no
heuristic at all.
*/
func flakyHeaderRefs(v interface{}) []string {
	var found []string
	var walk func(interface{})
	walk = func(node interface{}) {
		if m, _ := uiJoinStringMap(node); m != nil {
			for k, val := range m {
				if h, ok := matchesFlakyHeader(k); ok {
					found = append(found, fmt.Sprintf("has a field %q referencing %q", k, h))
				}
				walk(val)
			}
			return
		}
		if str, ok := node.(string); ok {
			for _, token := range headerishTokens(str) {
				if h, ok := matchesFlakyHeaderInValue(token); ok {
					found = append(found, fmt.Sprintf("carries a value %q referencing %q", str, h))
				}
			}
			return
		}
		// Reuses the production normalizers rather than listing
		// []interface{} alone, so every codec shape the reader accepts is
		// walked here too.
		if items, ok := uiJoinSlice(node); ok {
			for _, item := range items {
				walk(item)
			}
		}
	}
	walk(v)
	return found
}

// flakyHeaderRefsInType inspects the Go declaration rather than an encoded
// instance: field names and every codec tag on them, recursively through
// embedded, nested, pointer and slice fields.
func flakyHeaderRefsInType(t reflect.Type) []string {
	return flakyHeaderRefsInTypeSeen(t, map[reflect.Type]bool{})
}

func flakyHeaderRefsInTypeSeen(t reflect.Type, seen map[reflect.Type]bool) []string {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return nil
	}
	seen[t] = true

	var found []string
	for i := range t.NumField() {
		f := t.Field(i)
		candidates := []string{f.Name}
		for _, tag := range []string{"json", "yaml", "bson"} {
			if v, ok := f.Tag.Lookup(tag); ok {
				candidates = append(candidates, strings.Split(v, ",")[0])
			}
		}
		for _, c := range candidates {
			if h, ok := matchesFlakyHeader(c); ok {
				found = append(found, fmt.Sprintf("declares %s (%q) referencing %q", f.Name, c, h))
				break
			}
		}
		found = append(found, flakyHeaderRefsInTypeSeen(f.Type, seen)...)
	}
	return found
}

// structFieldPaths lists every field reachable from a struct type,
// including through embedding, nesting, pointers and slices, so a
// dependency added one level down cannot slip past the pinned field set.
func structFieldPaths(t reflect.Type, prefix string) []string {
	return structFieldPathsSeen(t, prefix, map[reflect.Type]bool{})
}

func structFieldPathsSeen(t reflect.Type, prefix string, seen map[reflect.Type]bool) []string {
	// UNWRAP Map too. Without it, changing an existing field from
	// []string to map[string]someStruct produced an IDENTICAL path set —
	// the field name does not change — so a header dependency added by a
	// type change slipped past both this pin and the type walker.
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice ||
		t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	// CYCLE GUARD, SCOPED TO THE CURRENT PATH. A self-referential field
	// (`Parent *Thing`) recursed until the 10-minute panic timeout killed
	// the whole package — no diagnostic, 600 seconds of CI, for a routine
	// struct change. That inverts the pin's purpose: it exists to say
	// "check this field by hand", not to hang.
	//
	// Scoped rather than global because two SIBLING fields of the same
	// type are not a cycle, and a global set silently dropped the second
	// one's paths.
	if seen[t] {
		return []string{prefix + "<cycle>"}
	}
	seen[t] = true
	defer delete(seen, t)

	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		path := prefix + f.Name
		nested := structFieldPathsSeen(f.Type, path+".", seen)
		if len(nested) > 0 {
			out = append(out, nested...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// headerishTokens splits a value on the separators that surround a header
// name in prose, in a serialized header line, or in a joined key spec.
// "-" is NOT a separator: it is part of the name.
func headerishTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ':', ',', ';', '=', '"', '\'', '/', '?', '&',
			'(', ')', '[', ']', '{', '}', '<', '>', '+', '|', '.':
			return true
		}
		return false
	})
}

func normalizeHeaderish(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r == '-' || r == '_' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// matchesFlakyHeader reports the entry a token names, EXACTLY.
//
// Each entry contributes two candidates: itself, and its "x-" stripped
// form. The strip is what recognises the Go spelling `CorrelationID` as
// the entry `x-correlation-id`. (`RequestID` would match anyway — the
// list carries a bare `request-id` entry alongside `x-request-id` — so
// that is not the case the strip is for.)
//
// Nothing else. See flakyHeaderRefs for why every looser rule was worse.
func matchesFlakyHeader(token string) (string, bool) {
	return matchFlaky(token, false)
}

/*
matchesFlakyHeaderInValue is the same rule, restricted to the entries that
are unambiguous inside free text.

A LENGTH THRESHOLD CANNOT DO THIS. Seven characters excluded `date` (4)
and `b3` (2) — both legitimate DNS labels, both of which flagged ordinary
appOrigins — and still admitted `baggage`, which is a real W3C header AND
an ordinary English word and a plausible service name for any airline or
logistics customer. Raising the number to eight would drop `baggage` as a
dependency while keeping it as a false positive risk elsewhere; the
quantity was never the property that mattered.

The property that matters is whether the name is header-SHAPED. A
hyphenated entry (`x-request-id`, `x-amz-date`) is not something anyone
names a host or a key-spec token, so containment is safe. A single-word
entry is ambiguous by default and is admitted only by being named here,
once, deliberately.

Field names and codec tags are NOT restricted this way: a field literally
named `date` or `b3` on this struct is worth a human look, and the
field-set pin forces one.
*/
var valueSafeSingleWords = map[string]bool{
	// Coined for tracing; not an English word, not a plausible host label.
	"traceparent": true,
	"tracestate":  true,
	// Distinctive enough that a host or key-spec token would not collide.
	"authorization": true,
}

func matchesFlakyHeaderInValue(token string) (string, bool) {
	return matchFlaky(token, true)
}

// safeInValue reports whether an entry can be matched inside free text
// without flagging ordinary data.
func safeInValue(entry string) bool {
	return strings.Contains(entry, "-") || valueSafeSingleWords[entry]
}

func matchFlaky(token string, inValue bool) (string, bool) {
	t := normalizeHeaderish(token)
	for _, h := range FlakyHeaders {
		// The "x-" stripped form too, so the Go spelling `CorrelationID`
		// is recognised as the entry `x-correlation-id`.
		if inValue && !safeInValue(h) {
			continue
		}
		for _, cand := range []string{
			normalizeHeaderish(h),
			normalizeHeaderish(strings.TrimPrefix(h, "x-")),
		} {
			if t == cand {
				return h, true
			}
		}
	}
	return "", false
}

/* ------------------------------------------------------------------ */
/*  Every codec on the path                                            */
/* ------------------------------------------------------------------ */

func TestRoundTripSurvivesBSON(t *testing.T) {
	// The mongo driver decodes a nested document into an ORDERED bson.D,
	// never a map, and its lists into bson.A rather than []interface{}.
	// A reader handling only the map shapes returned ErrUIJoinNotAnnotation
	// for every single Mongo read while the comment above it said BSON
	// round-tripped.
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}

	raw, err := bson.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded TestSet
	if err := bson.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, isD := decoded.Metadata[UIJoinMetadataKey].(bson.D); !isD {
		t.Fatalf("precondition changed: driver no longer yields bson.D but %T — "+
			"this test no longer covers what it claims",
			decoded.Metadata[UIJoinMetadataKey])
	}

	got, err := GetUIJoinAnnotation(&decoded)
	if err != nil {
		t.Fatalf("get after bson: %v", err)
	}
	assertSurvived(t, got)
}

func TestRoundTripSurvivesJSONNumber(t *testing.T) {
	// A decoder with UseNumber hands every number back as json.Number.
	// Reading one as zero is the worst shape this bug takes: ports came
	// back [0 0] from a source of 8080/8443, Validate passed, and every
	// exchange then classified NO_INGRESS_OBSERVED with nothing to explain
	// why.
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, validAnnotation()); err != nil {
		t.Fatalf("set: %v", err)
	}
	encoded, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var decoded TestSet
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got, err := GetUIJoinAnnotation(&decoded)
	if err != nil {
		t.Fatalf("get after json.Number: %v", err)
	}
	assertSurvived(t, got)
}

func TestReadAcceptsEveryDocumentShapeACodecProduces(t *testing.T) {
	// Built directly rather than round-tripped, so each shape is covered
	// whether or not some decoder on the path happens to produce it today.
	shapes := map[string]interface{}{
		"map[string]interface{} (encoding/json)": map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"map[interface{}]interface{} (yaml.v2)": map[interface{}]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"bson.D (mongo nested document)": bson.D{
			{Key: "specVersion", Value: int32(UIJoinSpecVersion)},
			{Key: "captureId", Value: "cap_1"},
			{Key: "sessionNonce", Value: "n_1"},
			{Key: "t0WallMs", Value: int64(1789000000000)},
			{Key: "t1WallMs", Value: int64(1789000060000)},
			{Key: "ingressPorts", Value: bson.A{int32(8080)}},
			{Key: "appOrigins", Value: bson.A{"http://localhost:3000"}},
			{Key: "canonicalKeySpec", Value: "canonical-key.v1"},
		},
		"bson.M": bson.M{
			"specVersion": int32(UIJoinSpecVersion), "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": bson.A{int32(8080)},
			"appOrigins":       bson.A{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"float64 numbers (json without UseNumber)": map[string]interface{}{
			"specVersion": float64(UIJoinSpecVersion), "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": float64(1789000000000),
			"t1WallMs": float64(1789000060000), "ingressPorts": []interface{}{float64(8080)},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"json.Number": map[string]interface{}{
			"specVersion": json.Number("1"), "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": json.Number("1789000000000"),
			"t1WallMs":         json.Number("1789000060000"),
			"ingressPorts":     []interface{}{json.Number("8080")},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"a Go array, not a slice": map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": [1]int{8080},
			"appOrigins":       [1]string{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
		"typed slices (struct-built metadata)": map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []int{8080},
			"appOrigins":       []string{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}

	for name, doc := range shapes {
		t.Run(name, func(t *testing.T) {
			ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}}
			got, err := GetUIJoinAnnotation(ts)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.T0WallMs != 1789000000000 {
				t.Errorf("t0WallMs read as %d", got.T0WallMs)
			}
			if len(got.IngressPorts) != 1 || got.IngressPorts[0] != 8080 {
				t.Errorf("ingressPorts read as %v, not [8080]", got.IngressPorts)
			}
			if len(got.AppOrigins) != 1 || got.AppOrigins[0] != "http://localhost:3000" {
				t.Errorf("appOrigins read as %v", got.AppOrigins)
			}
		})
	}
}

/* ------------------------------------------------------------------ */
/*  Ports are TCP ports                                                */
/* ------------------------------------------------------------------ */

func TestValidateRejectsPortsOutsideTheTCPRange(t *testing.T) {
	// Port 0 means "the kernel picks one" and can never be a port
	// something was observed on, so it is as wrong as 70000. [0 0] is
	// also precisely what a mis-decoded port list looks like, and
	// accepting it made every exchange classify NO_INGRESS_OBSERVED.
	for _, ports := range [][]int{{0}, {-1}, {70000}, {2147483647}, {8080, 0}, {8080, 65536}} {
		a := validAnnotation()
		a.IngressPorts = ports
		if err := a.Validate(); err == nil {
			t.Errorf("ports %v validated", ports)
		}
	}
	for _, ports := range [][]int{{1}, {65535}, {8080, 8443}} {
		a := validAnnotation()
		a.IngressPorts = ports
		if err := a.Validate(); err != nil {
			t.Errorf("ports %v rejected: %v", ports, err)
		}
	}
}

func TestReadRejectsPortsOutsideTheTCPRange(t *testing.T) {
	// Each case asserts WHICH gate rejected it, not merely that something
	// did. That distinction is the whole test on a 64-bit host: with an
	// unchecked int64->int cast, 1<<32 + 8080 stays 4294975376 and
	// Validate rejects it, so "an error came back" passes for the wrong
	// reason and the narrowing is never exercised. Requiring the READER
	// to have caught it (ErrUIJoinNotAnnotation, from asUint16) kills
	// that mutant on every architecture.
	for name, tc := range map[string]struct {
		ports interface{}
		want  error
	}{
		// In range for uint16, so only Validate can reject these.
		"zeroes from a mis-decoded list": {[]interface{}{0, 0}, ErrUIJoinIncomplete},
		// Outside uint16, so the READER must reject them before narrowing.
		"above the TCP range":             {[]interface{}{70000}, ErrUIJoinNotAnnotation},
		"negative":                        {[]interface{}{-1}, ErrUIJoinNotAnnotation},
		"wraps to a valid port on 32-bit": {[]interface{}{int64(1)<<32 + 8080}, ErrUIJoinNotAnnotation},
		"a binary blob, not a port list":  {[]byte{80, 112}, ErrUIJoinNotAnnotation},
		// The SIGNED byte twin. Testing only reflect.Uint8 left it open,
		// and it is the same hazard wearing a different tag.
		"a signed byte sequence": {[]int8{80, 112}, ErrUIJoinNotAnnotation},
		// A DEFINED type over []byte. A type switch listing `[]byte`
		// matches neither this nor json.RawMessage, which is why the
		// refusal tests the element KIND instead.
		"a named type over []byte": {uiJoinTestBlob{80, 112}, ErrUIJoinNotAnnotation},
		"json.RawMessage":          {json.RawMessage{80, 112}, ErrUIJoinNotAnnotation},
	} {
		t.Run(name, func(t *testing.T) {
			ts := &TestSet{Metadata: map[string]interface{}{
				UIJoinMetadataKey: map[string]interface{}{
					"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
					"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
					"t1WallMs": int64(1789000060000), "ingressPorts": tc.ports,
					"appOrigins":       []interface{}{"http://localhost:3000"},
					"canonicalKeySpec": "canonical-key.v1",
				},
			}}
			got, err := GetUIJoinAnnotation(ts)
			if err == nil {
				t.Fatalf("%v read back as a usable annotation with ports %v", tc.ports, got.IngressPorts)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v to be caught by %v, got %v", tc.ports, tc.want, err)
			}
		})
	}
}

/* ------------------------------------------------------------------ */
/*  Unreadable is not the same as absent                               */
/* ------------------------------------------------------------------ */

func TestPresentButUnreadableIsCorruptionNotIncompleteness(t *testing.T) {
	// Reading a wrong-typed field as its zero value turns corruption into
	// a plausible-looking annotation. These must be distinguishable: a key
	// that is ABSENT is a half-written annotation (ErrUIJoinIncomplete,
	// naming the field); a key that is PRESENT but unreadable is corrupt
	// metadata (ErrUIJoinNotAnnotation).
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		}
	}

	// NOTE the "non-numeric" qualifier: a NUMERIC string is deliberately
	// accepted, because asInt64 parses one and several codecs on this path
	// quote numbers. TestReadAcceptsANumericString pins that, so the
	// leniency is a decision rather than an accident.
	corrupt := map[string]interface{}{
		"captureId is a number":            42,
		"t0WallMs is a non-numeric string": "not a number",
		"t0WallMs is NaN":                  math.NaN(),
		"t0WallMs overflows int64":         1e300,
		"ingressPorts is not a list":       "8080",
		"ingressPorts holds a map":         []interface{}{map[string]interface{}{}},
		"appOrigins holds a number":        []interface{}{8080},
	}
	for name, bad := range corrupt {
		t.Run(name, func(t *testing.T) {
			doc := base()
			doc[strings.SplitN(name, " ", 2)[0]] = bad
			ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}}
			_, err := GetUIJoinAnnotation(ts)
			if err == nil {
				t.Fatal("corrupt metadata read back as a usable annotation")
			}
			if errors.Is(err, ErrUIJoinIncomplete) {
				t.Fatalf("corruption reported as mere incompleteness: %v", err)
			}
		})
	}
}

func TestReadRejectsASpecVersionThatCannotNarrow(t *testing.T) {
	// int is 32 bits on a 32-bit build, so int(1<<32 + 1) is 1 there: a
	// nonsense version would read back as the supported one and the
	// annotation would be interpreted under rules it was not written by.
	// Nothing in the suite fed an out-of-int32 version until this.
	ts := &TestSet{Metadata: map[string]interface{}{
		UIJoinMetadataKey: map[string]interface{}{
			"specVersion": int64(1)<<32 + 1, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}}
	// ErrUIJoinNotAnnotation, not ErrUIJoinUnsupported: the READER could
	// not read the value. Demanding the reader's own sentinel is what
	// makes this test fail when the guard is removed — on a 64-bit host
	// Validate would otherwise reject 4294967297 with the very same
	// ErrUIJoinUnsupported, and the test would pass for the wrong reason.
	if _, err := GetUIJoinAnnotation(ts); !errors.Is(err, ErrUIJoinNotAnnotation) {
		t.Fatalf("expected ErrUIJoinNotAnnotation, got %v", err)
	}
}

func TestAbsentSpecVersionIsIncompleteNotAWrongVersion(t *testing.T) {
	// A truncated write used to report "unsupported spec version: found 0,
	// expected 1" — an error that misdirects, because the version is not
	// wrong, it is not there.
	ts := &TestSet{Metadata: map[string]interface{}{
		UIJoinMetadataKey: map[string]interface{}{
			"captureId": "cap_1", "sessionNonce": "n_1",
			"t0WallMs": int64(1789000000000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}}
	_, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("expected ErrUIJoinIncomplete, got %v", err)
	}
	if !strings.Contains(err.Error(), "specVersion") {
		t.Fatalf("error must name the missing field, got %v", err)
	}
}

func TestReadAcceptsANumericString(t *testing.T) {
	// Pins the leniency asInt64 provides, so it is a decision rather than
	// an accident: several codecs on this path quote numbers.
	ts := &TestSet{Metadata: map[string]interface{}{
		UIJoinMetadataKey: map[string]interface{}{
			"specVersion": "1", "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": "1789000000000",
			"t1WallMs": "1789000060000", "ingressPorts": []interface{}{"8080"},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}}
	got, err := GetUIJoinAnnotation(ts)
	if err != nil {
		t.Fatalf("quoted numbers must read: %v", err)
	}
	if got.T0WallMs != 1789000000000 || len(got.IngressPorts) != 1 || got.IngressPorts[0] != 8080 {
		t.Fatalf("quoted numbers read wrong: %+v", got)
	}
}

func TestANilValuedKeyIsAbsentNotCorrupt(t *testing.T) {
	// `sessionNonce:` with nothing after it in a hand-edited config.yaml
	// decodes to a nil value, which is the most likely real-world shape of
	// a half-written annotation.
	ts := &TestSet{Metadata: map[string]interface{}{
		UIJoinMetadataKey: map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": nil, "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		},
	}}
	_, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("expected ErrUIJoinIncomplete for a nil value, got %v", err)
	}
	if !strings.Contains(err.Error(), "sessionNonce") {
		t.Fatalf("error must name the field, got %v", err)
	}
}

func TestCompleteRejectsANegativeEndTime(t *testing.T) {
	// Complete() is exported and callable on a struct that never went
	// through Validate, so `> 0` rather than `!= 0` is load-bearing there.
	a := validAnnotation()
	a.T1WallMs = -1
	if a.Complete() {
		t.Fatal("a negative end time must not report complete")
	}
}

func TestAbsentFieldIsIncompleteNotCorrupt(t *testing.T) {
	doc := map[string]interface{}{
		"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
		// sessionNonce absent; everything ELSE present, so the error can
		// only be about sessionNonce.
		"t0WallMs": int64(1789000000000), "ingressPorts": []interface{}{8080},
		"appOrigins":       []interface{}{"http://localhost:3000"},
		"canonicalKeySpec": "canonical-key.v1",
	}
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}}
	_, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("expected ErrUIJoinIncomplete, got %v", err)
	}
	if !strings.Contains(err.Error(), "sessionNonce") {
		t.Fatalf("error must name the missing field, got %v", err)
	}
}

func assertSurvived(t *testing.T, got *UIJoinAnnotation) {
	t.Helper()
	want := validAnnotation()
	if got.SpecVersion != want.SpecVersion || got.CaptureID != want.CaptureID ||
		got.SessionNonce != want.SessionNonce || got.T0WallMs != want.T0WallMs ||
		got.T1WallMs != want.T1WallMs || got.CanonicalKeySpec != want.CanonicalKeySpec {
		t.Fatalf("scalar lost: got %+v want %+v", got, want)
	}
	if len(got.IngressPorts) != 2 || got.IngressPorts[0] != 8080 || got.IngressPorts[1] != 8443 {
		t.Fatalf("ingressPorts: got %v want [8080 8443]", got.IngressPorts)
	}
	if len(got.AppOrigins) != 1 || got.AppOrigins[0] != "http://localhost:3000" {
		t.Fatalf("appOrigins: got %v", got.AppOrigins)
	}
}

// uiJoinTestBlob is a DEFINED type over []byte, the shape a type switch
// listing `[]byte` cannot match.
type uiJoinTestBlob []byte

/* ------------------------------------------------------------------ */
/*  Non-integral numbers                                               */
/* ------------------------------------------------------------------ */
/*
 * asInt64/asInt32/asUint16 route through floatToInt64, which truncates
 * toward zero for any in-range float — semantics chosen for a pgtype cell
 * fixture, where a lossy read is fine. Reusing them here read
 * `specVersion: 1.9` as 1 and VALIDATED it as the supported version, and
 * `t0WallMs: 1789000000000.7` as a timestamp 700µs off, both with no
 * error. The package header promises "nothing here reads a value it does
 * not understand"; it did, and landed on a plausible number, which is
 * worse than landing on zero because a zero is visible.
 */

func TestReadRejectsNonIntegralNumbers(t *testing.T) {
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"specVersion":      UIJoinSpecVersion,
			"captureId":        "cap_1",
			"sessionNonce":     "nonce_1",
			"t0WallMs":         int64(1789000000000),
			"t1WallMs":         int64(1789000001000),
			"ingressPorts":     []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		}
	}

	for _, tc := range []struct {
		name  string
		key   string
		value interface{}
	}{
		{"a fractional spec version", "specVersion", 1.9},
		{"a fractional t0", "t0WallMs", 1789000000000.7},
		{"a fractional t1", "t1WallMs", 1789000001000.5},

		{"a fractional spec version as json.Number", "specVersion", json.Number("1.9")},
		{"a fractional t0 as json.Number", "t0WallMs", json.Number("1789000000000.7")},
		{"a quoted fractional t0", "t0WallMs", "1789000000000.7"},
		{"a quoted exponent-notation t0", "t0WallMs", "1.7e12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			m[tc.key] = tc.value
			ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: m}}
			_, err := GetUIJoinAnnotation(ts)
			if err == nil {
				t.Fatalf("%s = %v was accepted; a truncated read is a wrong join, "+
					"not a missing one", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), "whole number") {
				t.Fatalf("expected a whole-number complaint, got %v", err)
			}
		})
	}
}

func TestReadRejectsAFractionalPort(t *testing.T) {
	m := map[string]interface{}{
		"specVersion":      UIJoinSpecVersion,
		"captureId":        "cap_1",
		"sessionNonce":     "nonce_1",
		"t0WallMs":         int64(1789000000000),
		"t1WallMs":         int64(0),
		"ingressPorts":     []interface{}{8080.9},
		"appOrigins":       []interface{}{"http://localhost:3000"},
		"canonicalKeySpec": "canonical-key.v1",
	}
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: m}}
	if _, err := GetUIJoinAnnotation(ts); err == nil {
		t.Fatal("ingressPorts [8080.9] was read as [8080]; a port is not rounded")
	}
}

func TestTheTwoJSONDecodersAgree(t *testing.T) {
	/*
	 * THE SAME BYTES, TWO ANSWERS. json.Unmarshal yields float64 and
	 * truncated; a UseNumber decoder yields json.Number and failed
	 * ParseInt. One decoder accepted the document with a mutated
	 * timestamp, the other called it corruption — and both paths are
	 * documented as supported. A browser producer sending
	 * performance.timeOrigin (a sub-millisecond DOMHighResTimeStamp)
	 * emits exactly this.
	 */
	raw := []byte(`{
		"specVersion": 1,
		"captureId": "cap_1",
		"sessionNonce": "nonce_1",
		"t0WallMs": 1789000000000.7,
		"t1WallMs": 0,
		"ingressPorts": [8080],
		"appOrigins": ["http://localhost:3000"],
		"canonicalKeySpec": "canonical-key.v1"
	}`)

	var plain map[string]interface{}
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatalf("plain unmarshal: %v", err)
	}
	_, plainErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: plain},
	})

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var numbered map[string]interface{}
	if err := dec.Decode(&numbered); err != nil {
		t.Fatalf("UseNumber decode: %v", err)
	}
	_, numberErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: numbered},
	})

	if (plainErr == nil) != (numberErr == nil) {
		t.Fatalf("the two documented JSON paths disagree about the same bytes:\n"+
			"  json.Unmarshal -> %v\n  UseNumber      -> %v", plainErr, numberErr)
	}
	if plainErr == nil {
		t.Fatal("a fractional t0WallMs was accepted by both decoders; it is corruption, not a timestamp")
	}
}

func TestWholeNumbersStillReadOverEveryCodec(t *testing.T) {
	// The positive control: the check above must not refuse the values
	// every real producer sends.
	for name, value := range map[string]interface{}{
		"an int":                int(1789000000000),
		"an int64":              int64(1789000000000),
		"a whole float64":       float64(1789000000000),
		"a whole json.Number":   json.Number("1789000000000"),
		"a quoted whole number": "1789000000000",
	} {
		t.Run(name, func(t *testing.T) {
			m := map[string]interface{}{
				"specVersion":      UIJoinSpecVersion,
				"captureId":        "cap_1",
				"sessionNonce":     "nonce_1",
				"t0WallMs":         value,
				"t1WallMs":         int64(0),
				"ingressPorts":     []interface{}{8080},
				"appOrigins":       []interface{}{"http://localhost:3000"},
				"canonicalKeySpec": "canonical-key.v1",
			}
			got, err := GetUIJoinAnnotation(&TestSet{
				Metadata: map[string]interface{}{UIJoinMetadataKey: m},
			})
			if err != nil {
				t.Fatalf("%v was refused: %v", value, err)
			}
			if got.T0WallMs != 1789000000000 {
				t.Fatalf("t0WallMs = %d, want 1789000000000", got.T0WallMs)
			}
		})
	}
}

/* ------------------------------------------------------------------ */
/*  Validate and Set must answer the same question                     */
/* ------------------------------------------------------------------ */

func TestValidateAgreesWithSet(t *testing.T) {
	/*
	 * Set normalized then validated; Validate did not normalize. So a
	 * producer doing the obvious `if err := a.Validate(); err != nil {
	 * bail }` before Set rejected annotations the storage layer would
	 * have accepted and repaired — and the split was baked into this
	 * package's own tests as two contradictory expectations.
	 *
	 * Validate additionally answers "can this carry a join?", so it and
	 * Set genuinely differ now. The invariant is therefore stated
	 * against the question Set actually asks — Storable — rather than
	 * against Validate with an exemption bolted on.
	 *
	 * That distinction is not cosmetic. Keying the exemption on
	 * errors.Is(..., ErrUIJoinNotJoinable) made ANY future joinability
	 * rule automatically storable, and this test would then have
	 * REQUIRED Set to persist it: adding a "more than one origin is not
	 * joinable" rule silently widened what Set writes, whole suite
	 * green. Keyed on Storable, the two move together by construction.
	 */
	for _, tc := range []struct {
		name   string
		mutate func(*UIJoinAnnotation)
	}{
		{"a trailing slash on an origin", func(a *UIJoinAnnotation) {
			a.AppOrigins = []string{"http://localhost:3000/"}
		}},
		{"a padded capture id", func(a *UIJoinAnnotation) {
			a.CaptureID = "  cap_pad  "
		}},
		{"a padded nonce", func(a *UIJoinAnnotation) {
			a.SessionNonce = "  nonce  "
		}},
		{"an upper-case host", func(a *UIJoinAnnotation) {
			a.AppOrigins = []string{"http://LOCALHOST:3000"}
		}},
		{"a padded canonical key spec", func(a *UIJoinAnnotation) {
			// A KNOWN spec with padding. The old value trimmed to a spec
			// UIJoinKnownKeySpec does not know, so both sides refused it
			// and the case passed on the rejection path rather than on
			// the padding it is named for.
			a.CanonicalKeySpec = "  canonical-key.v1  "
		}},
		{"an origin carrying a path", func(a *UIJoinAnnotation) {
			a.AppOrigins = []string{"http://localhost:3000/app"}
		}},
		{"an empty origin list", func(a *UIJoinAnnotation) {
			a.AppOrigins = nil
		}},
		{"a port out of range", func(a *UIJoinAnnotation) {
			a.IngressPorts = []int{70000}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := validAnnotation()
			tc.mutate(a)
			validateErr := a.Validate()

			b := validAnnotation()
			tc.mutate(b)
			setErr := SetUIJoinAnnotation(&TestSet{}, b)

			c := validAnnotation()
			tc.mutate(c)
			storableErr := c.Storable()

			// Set persists exactly what Storable says it will.
			if (storableErr == nil) != (setErr == nil) {
				t.Fatalf("Storable and Set disagree:\n  Storable -> %v\n  Set      -> %v",
					storableErr, setErr)
			}
			// And Validate refuses everything Storable does, plus the
			// un-joinable ones — never fewer.
			if storableErr != nil && validateErr == nil {
				t.Fatalf("Validate accepted what Storable refused:\n  Storable -> %v",
					storableErr)
			}
		})
	}
}

func TestNormalizedGivesTheProducerTheStoredValue(t *testing.T) {
	// Set does not mutate its argument, so without this the producer's
	// in-memory join key and the key on disk are different strings.
	a := validAnnotation()
	a.CaptureID = "  cap_pad  "
	a.AppOrigins = []string{"http://LOCALHOST:3000/"}

	canonical := a.Normalized()

	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("set: %v", err)
	}
	stored, err := GetUIJoinAnnotation(ts)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(canonical, stored) {
		t.Fatalf("Normalized() is not what Set stores:\n  Normalized -> %+v\n  stored     -> %+v",
			canonical, stored)
	}
	// And the original is untouched, so Normalized() is the way to get it.
	if a.CaptureID != "  cap_pad  " {
		t.Fatal("Set mutated its argument; callers rely on it not doing that")
	}
}

func TestReadRefusesADuplicateKey(t *testing.T) {
	// bson.D is an ordered list that permits repeated keys; collapsing it
	// into a map let the second `captureId` win silently, on the field
	// that IS the join key.
	doc := bson.D{
		{Key: "specVersion", Value: UIJoinSpecVersion},
		{Key: "captureId", Value: "real"},
		{Key: "sessionNonce", Value: "n_1"},
		{Key: "t0WallMs", Value: int64(1789000000000)},
		{Key: "t1WallMs", Value: int64(0)},
		{Key: "ingressPorts", Value: bson.A{8080}},
		{Key: "appOrigins", Value: bson.A{"http://localhost:3000"}},
		{Key: "canonicalKeySpec", Value: "canonical-key.v1"},
		{Key: "captureId", Value: "shadow"},
	}
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}}
	got, err := GetUIJoinAnnotation(ts)
	if err == nil {
		t.Fatalf("a duplicate captureId was accepted and resolved to %q", got.CaptureID)
	}
}

func TestReadAcceptsAnOrderedDocumentWithoutDuplicates(t *testing.T) {
	// The positive control: bson.D is still the normal driver shape.
	doc := bson.D{
		{Key: "specVersion", Value: UIJoinSpecVersion},
		{Key: "captureId", Value: "real"},
		{Key: "sessionNonce", Value: "n_1"},
		{Key: "t0WallMs", Value: int64(1789000000000)},
		{Key: "t1WallMs", Value: int64(0)},
		{Key: "ingressPorts", Value: bson.A{8080}},
		{Key: "appOrigins", Value: bson.A{"http://localhost:3000"}},
		{Key: "canonicalKeySpec", Value: "canonical-key.v1"},
	}
	ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}}
	got, err := GetUIJoinAnnotation(ts)
	if err != nil {
		t.Fatalf("an ordinary bson.D was refused: %v", err)
	}
	if got.CaptureID != "real" {
		t.Fatalf("captureId = %q, want \"real\"", got.CaptureID)
	}
}

func TestOriginsThatCanNeverMatchABrowserAreNormalized(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A fully-qualified trailing dot: legal, and location.origin
		// never produces it.
		{"http://a.example.com.", "http://a.example.com"},
		{"http://a.example.com.:8080", "http://a.example.com:8080"},
		// An IPv4-mapped IPv6 literal is the same address the browser
		// reports in dotted form.
		{"http://[::ffff:127.0.0.1]", "http://127.0.0.1"},
		{"http://[::FFFF:127.0.0.1]:8080", "http://127.0.0.1:8080"},
		// Already canonical: unchanged.
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"https://app.example.com", "https://app.example.com"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeOrigin(tc.in); got != tc.want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateRefusesAnUnknownCanonicalKeySpec(t *testing.T) {
	a := validAnnotation()
	a.CanonicalKeySpec = "$$$garbage$$$"
	if err := a.Validate(); err == nil {
		t.Fatal("an unknown key spec validated; it is the only place a join " +
			"computed under two different normalizers can be detected")
	}
}

func TestValidateRefusesAnImplausibleT1(t *testing.T) {
	// t1 gets the same window as t0. Without it, a stop timestamp could
	// be any int64 at all as long as it was not below t0 — so a
	// seconds-vs-millis mix-up at STOP was accepted while the identical
	// mistake at start was refused.
	/*
	 * EACH CASE NAMES THE RULE, for the reason Finish's table spells out
	 * and this test did not apply: the FLOOR half can never change the
	 * verdict here either. t0 is already guaranteed >= floor three lines
	 * above, so any t1 below the floor also precedes t0 and the NEXT
	 * check refuses it — same sentinel, different message. Deleting the
	 * floor half left the suite green, and the production read path then
	 * told someone whose hand-written config.yaml had t1WallMs in
	 * seconds that it "precedes t0WallMs", which is true of nothing they
	 * did wrong.
	 */
	for name, tc := range map[string]struct {
		t1   int64
		want string
	}{
		"seconds where millis belong": {1789000060, "plausible epoch-ms"},
		"the far future":              {4102444800001, "plausible epoch-ms"},
		"the maximum int64":           {9223372036854775807, "plausible epoch-ms"},
		// The ceiling exactly, so >= cannot be weakened to >.
		"the epoch ceiling itself": {4102444800000, "plausible epoch-ms"},
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.T1WallMs = tc.t1
			err := a.Validate()
			if err == nil {
				t.Fatalf("t1WallMs = %d validated", tc.t1)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused by the wrong rule: wanted %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateStillAcceptsAnUnfinishedRecording(t *testing.T) {
	// t1 == 0 means "still recording" and must stay valid.
	a := validAnnotation()
	a.T1WallMs = 0
	if err := a.Validate(); err != nil {
		t.Fatalf("t1WallMs = 0 means still recording and must validate: %v", err)
	}
}

func TestValidateRefusesAnImplausibleTimestamp(t *testing.T) {
	for name, t0 := range map[string]int64{
		"one millisecond after the epoch": 1,
		"seconds where millis belong":     1789000000,
		"the far future":                  4102444800001,
		"the maximum int64":               9223372036854775807,
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.T0WallMs = t0
			a.T1WallMs = 0
			if err := a.Validate(); err == nil {
				t.Fatalf("t0WallMs = %d validated; that is what a seconds/millis "+
					"mix-up or a mis-decoded read looks like", t0)
			}
		})
	}
}

func TestAnIntegralExponentReadsTheSameThroughEveryDecoder(t *testing.T) {
	/*
	 * THE INPUT THE FIRST FIX MISSED. `1.789e12` is a whole number
	 * written in exponent form. json.Unmarshal yields float64 and the
	 * Trunc check passed it; a UseNumber decoder yields json.Number and
	 * ParseInt refused it outright. The same bytes, two answers — which
	 * is the exact defect integral() was written to close, and the test
	 * for it only tried a FRACTIONAL value, where both paths happen to
	 * agree.
	 */
	raw := []byte(`{
		"specVersion": 1,
		"captureId": "cap_1",
		"sessionNonce": "nonce_1",
		"t0WallMs": 1.789e12,
		"t1WallMs": 0,
		"ingressPorts": [8080],
		"appOrigins": ["http://localhost:3000"],
		"canonicalKeySpec": "canonical-key.v1"
	}`)

	var plain map[string]interface{}
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatalf("plain unmarshal: %v", err)
	}
	plainGot, plainErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: plain},
	})

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var numbered map[string]interface{}
	if err := dec.Decode(&numbered); err != nil {
		t.Fatalf("UseNumber decode: %v", err)
	}
	numberGot, numberErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: numbered},
	})

	if (plainErr == nil) != (numberErr == nil) {
		t.Fatalf("the two documented JSON paths disagree about 1.789e12:\n"+
			"  json.Unmarshal -> %v\n  UseNumber      -> %v", plainErr, numberErr)
	}
	if plainErr != nil {
		t.Fatalf("1.789e12 is a whole number and must be readable: %v", plainErr)
	}
	if plainGot.T0WallMs != 1789000000000 || numberGot.T0WallMs != 1789000000000 {
		t.Fatalf("t0WallMs read back as %d / %d, want 1789000000000",
			plainGot.T0WallMs, numberGot.T0WallMs)
	}
}

func TestAFractionalExponentIsStillRefusedByBothDecoders(t *testing.T) {
	// The other half: accepting the integral exponent form must not have
	// opened the fractional one.
	raw := []byte(`{"specVersion":1,"captureId":"c","sessionNonce":"n",
		"t0WallMs":1.7890000000007e12,"t1WallMs":0,"ingressPorts":[8080],
		"appOrigins":["http://localhost:3000"],"canonicalKeySpec":"canonical-key.v1"}`)
	for _, useNumber := range []bool{false, true} {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if useNumber {
			dec.UseNumber()
		}
		var m map[string]interface{}
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, err := GetUIJoinAnnotation(&TestSet{
			Metadata: map[string]interface{}{UIJoinMetadataKey: m},
		}); err == nil {
			t.Fatalf("a fractional exponent was accepted (useNumber=%v)", useNumber)
		}
	}
}

func TestAFloat32IsRefusedRatherThanRounded(t *testing.T) {
	/*
	 * float32 has 24 bits of mantissa, so every large magnitude is
	 * INTEGRAL and wrong: float32(1.789e12) passes a Trunc check and
	 * converts to 1789000024064. 24 seconds of skew, inside the epoch
	 * window — and a 60-second capture whose t0 and t1 both round to the
	 * same float32 reads as zero duration without tripping T1 < T0.
	 * "A plausible number instead of zero", inside the guard written to
	 * stop exactly that.
	 */
	m := map[string]interface{}{
		"specVersion":      UIJoinSpecVersion,
		"captureId":        "cap_1",
		"sessionNonce":     "nonce_1",
		"t0WallMs":         float32(1.789e12),
		"t1WallMs":         float32(1.78900006e12),
		"ingressPorts":     []interface{}{8080},
		"appOrigins":       []interface{}{"http://localhost:3000"},
		"canonicalKeySpec": "canonical-key.v1",
	}
	got, err := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: m},
	})
	if err == nil {
		t.Fatalf("a float32 timestamp was accepted and read back as t0=%d t1=%d",
			got.T0WallMs, got.T1WallMs)
	}
	if !strings.Contains(err.Error(), "float32") {
		t.Fatalf("the error should name the type that cannot represent it, got %v", err)
	}
}

func TestOriginsAreCanonicalisedToWhatABrowserReports(t *testing.T) {
	/*
	 * A browser's location.origin is the only thing the stored value will
	 * ever be compared against, so any spelling it cannot produce is
	 * unjoinable. url.Parse hands back an internationalised host
	 * percent-encoded; a browser reports the A-label form.
	 */
	for _, tc := range []struct{ in, want string }{
		{"http://ПРИМЕР.РФ", "http://xn--e1afmkfd.xn--p1ai"},
		{"http://例え.テスト:8080", "http://xn--r8jz45g.xn--zckzah:8080"},
		// Already an A-label: unchanged, so the conversion is idempotent.
		{"http://xn--e1afmkfd.xn--p1ai", "http://xn--e1afmkfd.xn--p1ai"},
		// Plain ASCII takes the fast path and is untouched.
		{"https://app.example.com", "https://app.example.com"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := normalizeOrigin(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := normalizeOrigin(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestValidateRefusesAnIPv6ZoneIndex(t *testing.T) {
	// A zone index names an interface on one machine. No location.origin
	// carries one, so it can never match — and unlike the trailing dot
	// and the IPv4-mapped literal, there is no browser-equivalent form to
	// canonicalise it to.
	for _, origin := range []string{
		"http://[fe80::1%25eth0]",
		"http://[fe80::1%25Wi-Fi]:8080",
	} {
		t.Run(origin, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			if err := a.Validate(); err == nil {
				t.Fatal("an IPv6 zone index validated as an app origin")
			}
		})
	}
}

func TestARefusalSaysWhichProblemItIs(t *testing.T) {
	/*
	 * A duplicate key and a value that is genuinely not a document both
	 * came back as "UI join metadata is not an annotation: stored as
	 * bson.D" — so a producer emitting a repeated captureId was told its
	 * metadata was the wrong SHAPE, and sent to look in the wrong place.
	 */
	base := bson.D{
		{Key: "specVersion", Value: UIJoinSpecVersion},
		{Key: "captureId", Value: "real"},
		{Key: "sessionNonce", Value: "n_1"},
		{Key: "t0WallMs", Value: int64(1789000000000)},
		{Key: "t1WallMs", Value: int64(0)},
		{Key: "ingressPorts", Value: bson.A{8080}},
		{Key: "appOrigins", Value: bson.A{"http://localhost:3000"}},
		{Key: "canonicalKeySpec", Value: "canonical-key.v1"},
	}

	dup := append(append(bson.D{}, base...), bson.E{Key: "captureId", Value: "shadow"})
	_, dupErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: dup},
	})
	if dupErr == nil {
		t.Fatal("a duplicate key was accepted")
	}
	if !strings.Contains(dupErr.Error(), "more than once") ||
		!strings.Contains(dupErr.Error(), "captureId") {
		t.Fatalf("the duplicate-key refusal must name the field and the problem, got %v", dupErr)
	}

	_, shapeErr := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: "not a document at all"},
	})
	if shapeErr == nil {
		t.Fatal("a string was accepted as an annotation")
	}
	if !strings.Contains(shapeErr.Error(), "not a document") {
		t.Fatalf("the wrong-shape refusal must say so, got %v", shapeErr)
	}

	// AND THE TWO MUST NOT READ ALIKE. That is the whole finding.
	if dupErr.Error() == shapeErr.Error() {
		t.Fatal("a duplicate key and a wrong shape produce the same message")
	}
}

func TestANonStringKeyIsReportedAsSuch(t *testing.T) {
	// The YAML shape: map[interface{}]interface{} with a non-string key.
	m := map[interface{}]interface{}{42: "nope"}
	_, err := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: m},
	})
	if err == nil {
		t.Fatal("a non-string field name was accepted")
	}
	if !strings.Contains(err.Error(), "key") {
		t.Fatalf("the refusal must name the problem, got %v", err)
	}
}

func TestReadRefusesANumberOutsideInt64(t *testing.T) {
	/*
	 * `int64(f)` on an out-of-range float is implementation-defined and
	 * on amd64 lands on math.MinInt64 — so `1e19`, `1e308` and `Inf` all
	 * became -9223372036854775808 and the epoch check then reported that
	 * number as an implausible timestamp. The producer never wrote it;
	 * the guard invented it. Trunc(Inf) == Inf, which is why the
	 * whole-number check waved Inf through.
	 */
	for _, literal := range []string{"1e19", "1e308", "9.3e18", "-1e19"} {
		t.Run(literal, func(t *testing.T) {
			m := map[string]interface{}{
				"specVersion":      UIJoinSpecVersion,
				"captureId":        "cap_1",
				"sessionNonce":     "nonce_1",
				"t0WallMs":         json.Number(literal),
				"t1WallMs":         int64(0),
				"ingressPorts":     []interface{}{8080},
				"appOrigins":       []interface{}{"http://localhost:3000"},
				"canonicalKeySpec": "canonical-key.v1",
			}
			_, err := GetUIJoinAnnotation(&TestSet{
				Metadata: map[string]interface{}{UIJoinMetadataKey: m},
			})
			if err == nil {
				t.Fatalf("%s was accepted", literal)
			}
			// AND THE MESSAGE MUST NOT QUOTE A FABRICATED VALUE.
			if strings.Contains(err.Error(), "-9223372036854775808") {
				t.Fatalf("the error reports a number the producer never wrote: %v", err)
			}
		})
	}
}

func TestAnErrorNamesItsFieldExactlyOnce(t *testing.T) {
	// integral() prefixed the key into its own message while
	// uiJoinRead.fail prefixes it too, so every failure read
	// "…: t0WallMs: t0WallMs: 1.5 is not a whole number".
	m := map[string]interface{}{
		"specVersion":      UIJoinSpecVersion,
		"captureId":        "cap_1",
		"sessionNonce":     "nonce_1",
		"t0WallMs":         1789000000000.5,
		"t1WallMs":         int64(0),
		"ingressPorts":     []interface{}{8080},
		"appOrigins":       []interface{}{"http://localhost:3000"},
		"canonicalKeySpec": "canonical-key.v1",
	}
	_, err := GetUIJoinAnnotation(&TestSet{
		Metadata: map[string]interface{}{UIJoinMetadataKey: m},
	})
	if err == nil {
		t.Fatal("a fractional t0 was accepted")
	}
	if n := strings.Count(err.Error(), "t0WallMs"); n != 1 {
		t.Fatalf("the field is named %d times in %q; it should be named once", n, err)
	}
}

func TestANilDocumentIsNotAnUnexplainedRefusal(t *testing.T) {
	// A typed-nil map is a legitimate empty document AND satisfies
	// `m == nil`, so branching on the map produced
	// "UI join metadata is not an annotation: " — a dangling colon with
	// no reason at all.
	for name, raw := range map[string]interface{}{
		"a nil string map": map[string]interface{}(nil),
		"a nil bson.M":     bson.M(nil),
		"a nil bson.D":     bson.D(nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := GetUIJoinAnnotation(&TestSet{
				Metadata: map[string]interface{}{UIJoinMetadataKey: raw},
			})
			if err == nil {
				t.Fatal("an empty document was accepted as a complete annotation")
			}
			if strings.HasSuffix(err.Error(), ": ") {
				t.Fatalf("the refusal has no reason: %q", err)
			}
			// An empty document is INCOMPLETE, not the wrong shape.
			if !errors.Is(err, ErrUIJoinIncomplete) {
				t.Fatalf("an empty document should report a missing field, got %v", err)
			}
		})
	}
}

func TestBrowserResolvableIDNHostsAreAccepted(t *testing.T) {
	/*
	 * THE BROWSER'S RULES, NOT THE STRICTEST AVAILABLE.
	 *
	 * This used to use idna.Lookup, which enforces STD3 and CheckHyphens;
	 * the WHATWG URL Standard — which is what computes location.origin —
	 * turns both off. So these were refused outright even though a
	 * browser resolves them and reports the A-label form, and a test
	 * asserted that refusal was correct.
	 *
	 * `ab--cd` is the clearest tell: all-ASCII it takes the fast path and
	 * is accepted, so the SAME label was accepted or refused depending on
	 * whether some other label in the host happened to be non-ASCII.
	 */
	for _, tc := range []struct{ in, want string }{
		{"http://пример_x.рф", "http://xn--_x-mlcluqhd.xn--p1ai"},
		{"http://-пример.рф", "http://xn----jtbiqngd.xn--p1ai"},
		{"http://пример-.рф", "http://xn----itbiqngd.xn--p1ai"},
		{"http://ab--cd.рф", "http://ab--cd.xn--p1ai"},
		{"http://münchen.de", "http://xn--mnchen-3ya.de"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeOrigin(tc.in); got != tc.want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
			a := validAnnotation()
			a.AppOrigins = []string{tc.in}
			if err := a.Validate(); err != nil {
				t.Fatalf("a host a browser resolves was refused: %v", err)
			}
		})
	}
}

func TestAStoredOriginIsAlwaysASCII(t *testing.T) {
	/*
	 * THE PROPERTY, not a list of inputs.
	 *
	 * The stored value is only ever compared against a browser's
	 * location.origin, which is always ASCII once resolved. So the
	 * invariant is: whatever normalizeOrigin produces is ASCII, or the
	 * annotation is refused. Either outcome is safe; a non-ASCII value
	 * that validates is not, because url.String() percent-encodes it into
	 * a spelling no browser emits and every exchange then reads
	 * FOREIGN_ORIGIN with nothing to explain why.
	 *
	 * Written as a property because an earlier version asserted a
	 * hand-picked list of "unconvertible" hosts — and under the browser's
	 * own IDNA profile every one of them converts, so the test was
	 * asserting a refusal that should not happen.
	 */
	refused := 0
	for _, origin := range []string{
		// Convert cleanly and must be accepted.
		"http://\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444",
		"http://\u043f\u0440\u0438\u043c\u0435\u0440_x.\u0440\u0444",
		"http://-\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444",
		"http://ab--cd.\u0440\u0444",
		"http://\u65e5\u672c.example.com",
		"http://m\u00fcnchen.de",
		"http://\u0627\u0644\u0639\u0631\u0628\u064a\u0629.\u0645\u0635\u0631",
		"https://app.example.com",
		// MUST BE REFUSED. Without at least one of these the property
		// below is VACUOUS: every input converts, `err == nil` always
		// holds, and the consequent is satisfied by toASCIIHost rather
		// than by the guard — which is exactly how an earlier version of
		// this test passed while the guard it exists for was deleted.
		//
		// U+FFFD is literally what a mis-decoded read produces.
		"http://\ufffdabc.com",
		// A leading combining mark.
		"http://\u0301abc.com",
		// An empty DNS label: converts under the relaxed profile, cannot
		// be resolved.
		"http://\u043f\u0440\u0438\u043c\u0435\u0440..\u0440\u0444",
		// A label over 63 octets once converted.
		"http://" + strings.Repeat("\u043f\u0440\u0438\u043c\u0435\u0440", 20) + ".\u0440\u0444",
	} {
		t.Run(origin, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Validate()
			stored := a.Normalized().AppOrigins[0]
			if err != nil {
				refused++
				return
			}
			if !isASCII(stored) {
				t.Fatalf("validated but stored a non-ASCII origin %q; a browser "+
					"reports the A-label form and this could never match", stored)
			}
		})
	}
	// THE SET MUST EXERCISE THE REFUSAL. Otherwise the property holds
	// trivially and the guard is unprotected.
	if refused == 0 {
		t.Fatal("no input in this set was refused, so the property is vacuous " +
			"and the non-ASCII guard is not being exercised")
	}
}

func TestNormalizeOriginIsIdempotentOverIDNs(t *testing.T) {
	for _, origin := range []string{
		"http://\u043f\u0440\u0438\u043c\u0435\u0440.\u0440\u0444",
		"http://m\u00fcnchen.de:8080",
		"http://ab--cd.\u0440\u0444",
		"http://xn--e1afmkfd.xn--p1ai",
	} {
		t.Run(origin, func(t *testing.T) {
			once := normalizeOrigin(origin)
			if twice := normalizeOrigin(once); twice != once {
				t.Fatalf("not idempotent: %q -> %q -> %q", origin, once, twice)
			}
		})
	}
}

func TestAHostThatCannotBeResolvedIsRefused(t *testing.T) {
	/*
	 * The relaxed IDNA profile turns off VerifyDNSLength along with
	 * STD3, so it happily produces an empty label or a 136-byte A-label
	 * from a 120-character one. Neither can be resolved, so neither can
	 * ever be a browser's location.origin — and the earlier property
	 * test counted refusals without saying WHICH, so removing the check
	 * that produces these two left it green.
	 */
	for name, origin := range map[string]string{
		"an empty DNS label": "http://пример..рф",
		"a label over 63 octets once converted": "http://" +
			strings.Repeat("пример", 20) + ".рф",
		"an all-ASCII label over 63 octets": "http://" +
			strings.Repeat("a", 64) + ".example.com",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			if err := a.Validate(); err == nil {
				t.Fatalf("a host DNS cannot resolve was accepted, stored as %q",
					a.Normalized().AppOrigins[0])
			}
		})
	}
}

func TestAnEmptyPortIsNamedAsSuchEvenOnAnIDNHost(t *testing.T) {
	/*
	 * An empty port is the truncated-write shape, and the rule that names
	 * it has to be the most specific one. When Host ends in a colon,
	 * normalizeOrigin ASSIGNS neither canonicalHost branch — the port arm
	 * needs a non-empty port, the no-port arm is guarded against exactly
	 * this — so u.Host keeps the raw spelling and arrives non-ASCII.
	 * Ordered after the non-ASCII case, the port rule could never win and
	 * the producer was told their host was unresolvable.
	 */
	for name, origin := range map[string]string{
		"an ASCII host": "http://localhost:",
		"an IDN host":   "http://пример.рф:",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Validate()
			if err == nil {
				t.Fatal("an empty port was accepted")
			}
			if !strings.Contains(err.Error(), "port is empty") {
				t.Fatalf("the empty port must be named; got %v", err)
			}
		})
	}
}

func TestAHostIDNACannotConvertIsRefused(t *testing.T) {
	/*
	 * These pass the DNS shape rules — short labels, short name — so only
	 * the non-ASCII guard refuses them. The property test counted
	 * refusals without naming which, so with the DNS rule in place its
	 * count stayed above zero and removing this guard went unnoticed.
	 *
	 * U+FFFD is literally what a mis-decoded read produces.
	 */
	for name, origin := range map[string]string{
		"a replacement character":  "http://�abc.com",
		"a leading combining mark": "http://́abc.com",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Validate()
			if err == nil {
				t.Fatalf("a host no IDNA profile can convert was accepted, "+
					"stored as %q", a.Normalized().AppOrigins[0])
			}
			if !strings.Contains(err.Error(), "ASCII") {
				t.Fatalf("the refusal should say the host could not be "+
					"converted; got %v", err)
			}
		})
	}
}

func TestAHostOverTheDNSNameLimitIsRefused(t *testing.T) {
	// Every label is legal on its own; the NAME is too long. Only the
	// 253-octet cap catches this, and nothing exercised it.
	label := strings.Repeat("a", 60)
	host := strings.TrimSuffix(strings.Repeat(label+".", 5), ".") // 5 * 61 - 1 = 304
	a := validAnnotation()
	a.AppOrigins = []string{"http://" + host}
	if err := a.Validate(); err == nil {
		t.Fatalf("a %d-octet host name was accepted", len(host))
	}
}

func TestIPv6OriginsAreStoredAsABrowserReportsThem(t *testing.T) {
	/*
	 * unmapIPv4 parsed the address, used the parse for the v4 case and
	 * threw it away for v6 — so an IPv6 origin written with leading zeros
	 * or an uncompressed run of zeros was stored verbatim, while
	 * location.origin always reports the RFC 5952 form. Every exchange
	 * then classified FOREIGN_ORIGIN with nothing to explain why, which
	 * is the failure this function exists to prevent, for the one address
	 * family it did not cover.
	 */
	for _, tc := range []struct{ in, want string }{
		{
			"http://[2001:0db8:85a3:0000:0000:8a2e:0370:7334]",
			"http://[2001:db8:85a3::8a2e:370:7334]",
		},
		{"http://[0:0:0:0:0:0:0:1]", "http://[::1]"},
		{"http://[fe80:0000:0000:0000:0000:0000:0000:0001]", "http://[fe80::1]"},
		{"http://[2001:0DB8::1]:8080", "http://[2001:db8::1]:8080"},
		// Already canonical: unchanged, so the conversion is idempotent.
		{"http://[::1]:8080", "http://[::1]:8080"},
		{"http://[2001:db8:85a3::8a2e:370:7334]", "http://[2001:db8:85a3::8a2e:370:7334]"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := normalizeOrigin(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := normalizeOrigin(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
			a := validAnnotation()
			a.AppOrigins = []string{tc.in}
			if err := a.Validate(); err != nil {
				t.Fatalf("a legitimate IPv6 origin was refused: %v", err)
			}
		})
	}
}

/* ------------------------------------------------------------------ */
/*  "Cannot be joined" is not "corrupt"                                */
/* ------------------------------------------------------------------ */

// originLess is what the browser half mints for a page with an opaque
// origin — a sandboxed iframe, a data: or file: URL. Everything that
// identifies the run is present; only the origin list is empty. See
// CaptureIdentityBuilder.build in @keploy/capture-sdk, which calls that
// "a real answer".
func originLess() *UIJoinAnnotation {
	a := validAnnotation()
	a.AppOrigins = nil
	return a
}

func TestAnUnjoinableCaptureIsRecordedNotDiscarded(t *testing.T) {
	/*
	 * THE DATA-LOSS CASE. Set refused to persist this, so the captureId,
	 * the nonce, the timestamps and the ports — every one present and
	 * correct — went on the floor, and the test-set read back
	 * ErrUIJoinAbsent, which is the ordinary never-attempted case. The
	 * nonce is what stops two runs of one scenario merging, so the run
	 * least able to defend itself was the one left undefended.
	 */
	ts := &TestSet{}
	want := originLess()
	if err := SetUIJoinAnnotation(ts, want); err != nil {
		t.Fatalf("an intact capture with no app origin must be stored, got %v", err)
	}
	if ts.Metadata[UIJoinMetadataKey] == nil {
		t.Fatal("nothing was written to the test-set")
	}

	got, err := GetUIJoinAnnotation(ts)
	// BOTH, deliberately: the refusal keeps a caller that checks only
	// err failing closed, the value keeps the identity reachable.
	if !errors.Is(err, ErrUIJoinNotJoinable) {
		t.Fatalf("expected ErrUIJoinNotJoinable on read, got %v", err)
	}
	if got == nil {
		t.Fatal("the annotation was not returned alongside the refusal, " +
			"so the capture's identity is unreachable — the bug this fixes")
	}
	if got.CaptureID != want.CaptureID || got.SessionNonce != want.SessionNonce {
		t.Errorf("identity did not survive: captureId %q nonce %q",
			got.CaptureID, got.SessionNonce)
	}
	if got.T0WallMs != want.T0WallMs || got.T1WallMs != want.T1WallMs {
		t.Errorf("timestamps did not survive: t0 %d t1 %d", got.T0WallMs, got.T1WallMs)
	}
	if len(got.IngressPorts) != len(want.IngressPorts) {
		t.Errorf("ingress ports did not survive: %v", got.IngressPorts)
	}
	if got.Joinable() {
		t.Error("an annotation with no app origin reports Joinable() true")
	}
	// Still complete: recording finished. Complete and Joinable are
	// different questions and a capture can be one without the other.
	if !got.Complete() {
		t.Error("a finished recording reports Complete() false")
	}
}

func TestNotJoinableIsDistinguishableFromIncomplete(t *testing.T) {
	/*
	 * The whole point of the second sentinel. A monitor asking "did a
	 * producer write a broken record?" must not fire on a sandboxed
	 * iframe, and an operator told "missing a required field" must not
	 * go looking for a truncated write that never happened.
	 */
	notJoinable := originLess().Validate()
	if !errors.Is(notJoinable, ErrUIJoinNotJoinable) {
		t.Fatalf("an empty origin list must be ErrUIJoinNotJoinable, got %v", notJoinable)
	}
	if errors.Is(notJoinable, ErrUIJoinIncomplete) {
		t.Error("an empty origin list also matches ErrUIJoinIncomplete, " +
			"so the two cannot be told apart — check the sentinel does not wrap it")
	}

	// The converse, so this does not pass by making everything
	// not-joinable: a malformed origin stays structural.
	broken := validAnnotation()
	broken.AppOrigins = []string{"http://a/path"}
	structural := broken.Validate()
	if !errors.Is(structural, ErrUIJoinIncomplete) {
		t.Fatalf("a malformed origin must stay ErrUIJoinIncomplete, got %v", structural)
	}
	if errors.Is(structural, ErrUIJoinNotJoinable) {
		t.Error("a malformed origin reports ErrUIJoinNotJoinable, " +
			"which tells an operator their corrupt record is fine")
	}
}

func TestStructureIsReportedAheadOfJoinability(t *testing.T) {
	// Both wrong at once: no origins AND an unreadable spec version. The
	// structural half is the one somebody has to investigate, so it wins.
	a := originLess()
	a.SpecVersion = UIJoinSpecVersion + 1
	err := a.Validate()
	if !errors.Is(err, ErrUIJoinUnsupported) {
		t.Fatalf("expected the structural failure to be reported, got %v", err)
	}
}

func TestSetStillRefusesAStructurallyBrokenAnnotation(t *testing.T) {
	/*
	 * Set's gate moved from validateNormalized to validateStructure.
	 * Without this, the obvious way to make the case above pass — drop
	 * the gate — turns Set into a pass-through that persists anything,
	 * and every "refuse to persist an unusable annotation" guarantee in
	 * this file becomes false at once.
	 */
	for name, mutate := range map[string]func(*UIJoinAnnotation){
		"no captureId":        func(a *UIJoinAnnotation) { a.CaptureID = "" },
		"no sessionNonce":     func(a *UIJoinAnnotation) { a.SessionNonce = "" },
		"unknown key spec":    func(a *UIJoinAnnotation) { a.CanonicalKeySpec = "$$$garbage$$$" },
		"bad spec version":    func(a *UIJoinAnnotation) { a.SpecVersion = UIJoinSpecVersion + 1 },
		"no ingress ports":    func(a *UIJoinAnnotation) { a.IngressPorts = nil },
		"port out of range":   func(a *UIJoinAnnotation) { a.IngressPorts = []int{70000} },
		"implausible t0":      func(a *UIJoinAnnotation) { a.T0WallMs = 1 },
		"malformed origin":    func(a *UIJoinAnnotation) { a.AppOrigins = []string{"http://a/path"} },
		"origin with no host": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"http://"} },
		// The structural shape wearing the un-joinable one's clothes:
		// a list that is non-empty but holds only whitespace.
		"whitespace origin": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"   "} },
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			mutate(a)
			ts := &TestSet{}
			if err := SetUIJoinAnnotation(ts, a); err == nil {
				t.Fatalf("%s was persisted", name)
			}
			if ts.Metadata[UIJoinMetadataKey] != nil {
				t.Fatalf("%s left a value on the test-set despite the refusal", name)
			}
		})
	}
}

func TestJoinableAnswersTheSameQuestionAsValidate(t *testing.T) {
	var nilAnnotation *UIJoinAnnotation
	if nilAnnotation.Joinable() {
		t.Error("a nil annotation reports Joinable() true")
	}

	broken := validAnnotation()
	broken.CaptureID = ""
	if broken.Joinable() {
		t.Error("a structurally broken annotation reports Joinable() true")
	}
	if originLess().Joinable() {
		t.Error("an annotation with no app origin reports Joinable() true")
	}
	if !validAnnotation().Joinable() {
		t.Error("a complete annotation reports Joinable() false")
	}

	// No `Joinable() == (Validate() == nil)` loop here: Joinable is
	// defined as exactly that, so the assertion reduced to X != X and
	// could not fail. The four explicit cases above are what pin it.
}

/* ------------------------------------------------------------------ */
/*  The producer's pre-flight, and what Set actually asks              */
/* ------------------------------------------------------------------ */

func TestAProducerPreflightDoesNotDiscardTheRescuedRecord(t *testing.T) {
	/*
	 * THE REGRESSION THAT THE FIX ITSELF INTRODUCED. Validate refuses an
	 * intact-but-un-joinable capture, so a producer writing the obvious
	 * `if err := a.Validate(); err != nil { bail }` before Set never
	 * stored it — and the test-set read back ErrUIJoinAbsent, which is
	 * the original bug restored by its own fix. Storable is the question
	 * Set asks, exported so it can be asked.
	 */
	a := originLess()
	if err := a.Storable(); err != nil {
		t.Fatalf("an intact capture with no attributable origin must be storable, got %v", err)
	}
	ts := &TestSet{}
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("Storable said yes and Set said %v", err)
	}
	if ts.Metadata[UIJoinMetadataKey] == nil {
		t.Fatal("the producer's pre-flight passed but nothing was written")
	}

	// Storable normalizes, exactly as Set does — otherwise the pre-flight
	// answers for a different value than the one that gets written.
	padded := validAnnotation()
	padded.CaptureID = "  cap_pad  "
	padded.AppOrigins = []string{"http://LOCALHOST:3000/"}
	if err := padded.Storable(); err != nil {
		t.Errorf("Storable rejected what Set repairs and stores: %v", err)
	}
	var nilAnnotation *UIJoinAnnotation
	if err := nilAnnotation.Storable(); err == nil {
		t.Error("a nil annotation reported itself storable")
	}
}

/* ------------------------------------------------------------------ */
/*  Absent is corruption; explicitly empty is a real answer            */
/* ------------------------------------------------------------------ */

func TestAMissingAppOriginsKeyIsCorruptionNotABenignEmptyList(t *testing.T) {
	/*
	 * Set ALWAYS writes the key — `appOrigins: []` for the sandboxed
	 * iframe — so a document missing it is a truncated write or a
	 * hand-edit. Read as a nil slice it reported ErrUIJoinNotJoinable,
	 * whose message ends "the capture itself is intact": a corrupt record
	 * described as fine, in the class this reader is most careful about.
	 */
	base := map[string]interface{}{
		"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
		"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
		"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
		"canonicalKeySpec": "canonical-key.v1",
	}

	absent := map[string]interface{}{}
	for k, v := range base {
		absent[k] = v
	}
	_, err := GetUIJoinAnnotation(&TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: absent}})
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("a MISSING appOrigins key must be ErrUIJoinIncomplete, got %v", err)
	}
	if errors.Is(err, ErrUIJoinNotJoinable) {
		t.Error("a truncated write reports as not-joinable, which tells an " +
			"operator their corrupt record is intact")
	}

	// The other half: explicitly empty stays the benign, storable answer.
	empty := map[string]interface{}{"appOrigins": []interface{}{}}
	for k, v := range base {
		empty[k] = v
	}
	got, err := GetUIJoinAnnotation(&TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: empty}})
	if !errors.Is(err, ErrUIJoinNotJoinable) {
		t.Fatalf("an explicitly empty appOrigins must be ErrUIJoinNotJoinable, got %v", err)
	}
	if got == nil {
		t.Error("the identity was discarded for an explicitly empty origin list")
	}
}

func TestGetReportsStructureBeforeJoinability(t *testing.T) {
	/*
	 * THE MUTATION THE ORDERING TEST MISSED. Swapping the two checks in
	 * GetUIJoinAnnotation left the whole suite green, because the
	 * existing ordering test pins Validate — which cannot return a value.
	 * On the read path the ordering decides whether a corrupt document
	 * comes back as a NON-NIL annotation paired with "the capture itself
	 * is intact", which is the worst output this dual return can produce.
	 */
	doc := map[string]interface{}{
		"specVersion": UIJoinSpecVersion, "captureId": "", // structurally broken
		"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
		"t1WallMs": int64(1789000060000), "ingressPorts": []interface{}{8080},
		// Origin-less AND structurally broken (the empty captureId above,
		// and a key spec this build does not know) — the reader accepts
		// both as READABLE, so the ordering inside validate decides.
		"appOrigins": []interface{}{}, "canonicalKeySpec": "$$$garbage$$$",
	}
	got, err := GetUIJoinAnnotation(&TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: doc}})
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("a doc that is BOTH corrupt and origin-less must report the "+
			"structural failure, got %v", err)
	}
	if got != nil {
		t.Fatal("a structurally broken document came back as a usable annotation")
	}
}

/* ------------------------------------------------------------------ */
/*  Any scheme a real app is served from is stored AND joinable        */
/* ------------------------------------------------------------------ */

func TestAnOriginFromAnyRealAppIsStoredAndJoinable(t *testing.T) {
	/*
	 * currentOrigin() in the SDK filters exactly "" and "null"; every
	 * other location.origin is sent. These are real apps people record,
	 * and refusing them as malformed discarded the captureId and nonce.
	 *
	 * NOT AN ENUMERATION. An earlier version listed four schemes and
	 * asserted them, which meant the rule under test was "these four are
	 * allowed" — reintroducing a structural scheme allowlist containing
	 * exactly those four left the whole suite green, so the next webview
	 * scheme would have gone straight back to being destroyed. The rule
	 * is that the SHAPE is what matters, so the cases include a scheme
	 * nothing has ever heard of.
	 */
	for _, origin := range []string{
		"capacitor://localhost", "ionic://localhost",
		"chrome-extension://abcdefghijklmnop", "app://bundle",
		"moz-extension://11111111-2222-3333-4444-555555555555",
		"tauri://localhost", "ms-appx-web://app",
		// The point of the test: a scheme invented here and known to
		// nothing, which must behave exactly like the four above.
		"zzunheardof://host.example",
	} {
		t.Run(origin, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}

			if err := a.Storable(); err != nil {
				t.Fatalf("%q is a real origin from a correct producer, refused: %v", origin, err)
			}
			ts := &TestSet{}
			if err := SetUIJoinAnnotation(ts, a); err != nil {
				t.Fatalf("%q was not stored: %v", origin, err)
			}
			got, err := GetUIJoinAnnotation(ts)
			if err != nil {
				t.Fatalf("%q must read back cleanly, got %v", origin, err)
			}
			if got == nil || got.CaptureID != a.CaptureID {
				t.Fatalf("%q lost the capture identity", origin)
			}
			/*
			 * JOINABLE. This package does not get to decide otherwise:
			 * nothing reads appOrigins — @keploy/join decides
			 * FOREIGN_ORIGIN from a per-exchange sameOrigin boolean — and
			 * a Capacitor app's backend calls carry a literal
			 * `Origin: capacitor://localhost` header, so the scheme rule
			 * that used to live here was very likely backwards.
			 *
			 * Nothing persists this verdict — it is recomputed from
			 * appOrigins on every read — so the rule is a two-way door.
			 * The one-way door was the OLD placement inside
			 * validateStructure, where Set refused to write and the
			 * captureId and nonce were destroyed.
			 */
			if !got.Joinable() {
				t.Errorf("%q reported un-joinable on a rule no consumer implements", origin)
			}
		})
	}

	// Genuinely malformed origins stay structural — this must not have
	// become "anything with a scheme is fine".
	for _, bad := range []string{
		"not a url at all", "http://a/path", "http://", "*",
		"capacitor://localhost/path", "capacitor://user:pw@host",
		"zzunheardof://host?q=1", "capacitor://host:70000",
	} {
		a := validAnnotation()
		a.AppOrigins = []string{bad}
		if err := a.Storable(); !errors.Is(err, ErrUIJoinIncomplete) {
			t.Errorf("%q must stay ErrUIJoinIncomplete, got %v", bad, err)
		}
	}

	// And the one un-joinable shape is still un-joinable.
	if originLess().Joinable() {
		t.Error("an empty origin list reported joinable")
	}
}

/* ------------------------------------------------------------------ */
/*  Closing out a recording                                            */
/* ------------------------------------------------------------------ */

func TestFinishStampsTheStopTimeOnAnUnjoinableCapture(t *testing.T) {
	/*
	 * The stop-time update Set's own doc used to prescribe —
	 * Get → set T1 → Set — aborts on ErrUIJoinNotJoinable, so the one
	 * capture this package rescues never got a stop time and stayed
	 * Complete() false forever. An unfinished annotation is one the
	 * joiner declines, so it was rescued into permanent uselessness.
	 */
	ts := &TestSet{}
	a := originLess()
	a.T1WallMs = 0 // still recording
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// The hand-rolled read-modify-write, exactly as a producer would
	// write it — this is what must no longer be the prescribed path.
	if _, err := GetUIJoinAnnotation(ts); err == nil {
		t.Fatal("setup assumption broken: Get should refuse an un-joinable capture")
	}

	const stop = int64(1789000060000)
	if err := FinishUIJoinAnnotation(ts, stop); err != nil {
		t.Fatalf("Finish refused an un-joinable capture: %v", err)
	}
	got, err := GetUIJoinAnnotation(ts)
	if !errors.Is(err, ErrUIJoinNotJoinable) || got == nil {
		t.Fatalf("after Finish: got %v, err %v", got, err)
	}
	if got.T1WallMs != stop {
		t.Errorf("stop time not recorded: T1WallMs = %d", got.T1WallMs)
	}
	if !got.Complete() {
		t.Error("the capture is still incomplete after being finished")
	}
}

func TestFinishPreservesKeysItDoesNotKnow(t *testing.T) {
	// Set REPLACES the whole map, so a key written by a newer producer
	// under the same specVersion is dropped by a Set-based stop-time
	// update. Finish edits in place.
	ts := &TestSet{}
	// STILL RECORDING. validAnnotation() is already finished, so building
	// on it made this test a double-finish that asserted the overwrite
	// succeeded — it pinned the very behaviour the monotonicity guard
	// now refuses, and was the only happy-path test Finish had.
	unfinished := validAnnotation()
	unfinished.T1WallMs = 0
	if err := SetUIJoinAnnotation(ts, unfinished); err != nil {
		t.Fatalf("setup: %v", err)
	}
	stored := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})
	stored["somethingNewerWrote"] = "keep me"

	if err := FinishUIJoinAnnotation(ts, 1789000090000); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	after := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})
	if after["somethingNewerWrote"] != "keep me" {
		t.Error("Finish dropped a key it did not know about")
	}
	if after["t1WallMs"] != int64(1789000090000) {
		t.Errorf("t1WallMs = %v", after["t1WallMs"])
	}
}

func TestFinishRefusesAnImplausibleOrBackwardsStopTime(t *testing.T) {
	// A stop time is written once and a wrong one is not repairable from
	// the outside, so it gets the same rigour t0 gets.
	/*
	 * UNFINISHED, and this is the whole test.
	 *
	 * It used to build from validAnnotation(), which already carries
	 * T1WallMs. Every case below differs from that stored value, so the
	 * write-once guard — added in the same change as this comment —
	 * refused all four before the epoch or t0 rules were ever consulted.
	 * Since the test only asserted err != nil, it could not tell which
	 * rule fired: DELETING BOTH PRODUCTION CHECKS left the suite green.
	 *
	 * A guard that makes two other guards untestable is a coverage
	 * regression, and it was caused by the fix that added it. Starting
	 * from an unfinished annotation puts the argument checks back in
	 * front, which is also the order the code runs them in: validate what
	 * you were handed, then the state you are changing.
	 */
	/*
	 * EACH CASE NAMES THE RULE IT EXPECTS, because "err != nil" cannot
	 * tell them apart and two of these were decoration without it.
	 *
	 * Finish's epoch FLOOR can never change the verdict: validateStructure
	 * guarantees t0 >= floor, and Finish only runs after a successful
	 * read, so t1 < floor implies t1 < t0 and the next check would catch
	 * it anyway. The floor earns its place solely by naming the
	 * seconds-vs-millis mix-up instead of reporting "precedes t0WallMs",
	 * so the MESSAGE is the thing under test — without asserting it,
	 * deleting the floor half left the suite green.
	 */
	for name, tc := range map[string]struct {
		t1   int64
		want string
	}{
		"before t0":      {1789000000000 - 1, "precedes t0WallMs"},
		"zero":           {0, "plausible epoch-ms"},
		"seconds not ms": {1789000060, "plausible epoch-ms"},
		"far future":     {1 << 62, "plausible epoch-ms"},
		// The CEILING, exactly. `>=` vs `>` is a one-value difference
		// and nothing else in the table is near it.
		"the epoch ceiling itself": {4102444800000, "plausible epoch-ms"},
	} {
		t1 := tc.t1
		t.Run(name, func(t *testing.T) {
			ts := &TestSet{}
			unfinished := validAnnotation()
			unfinished.T1WallMs = 0
			if err := SetUIJoinAnnotation(ts, unfinished); err != nil {
				t.Fatalf("setup: %v", err)
			}
			before := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})["t1WallMs"]
			err := FinishUIJoinAnnotation(ts, t1)
			if err == nil {
				t.Fatalf("Finish accepted a %s stop time", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s was refused by the wrong rule: wanted %q, got %v",
					name, tc.want, err)
			}
			// Never the already-finished sentinel: these are bad
			// ARGUMENTS to an unfinished annotation, not a lost race.
			if errors.Is(err, ErrUIJoinAlreadyFinished) {
				t.Errorf("%s reported as already finished", name)
			}
			after := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})["t1WallMs"]
			if before != after {
				t.Errorf("a refused stop time was written anyway: %v -> %v", before, after)
			}
		})
	}

	if err := FinishUIJoinAnnotation(&TestSet{}, 1789000060000); !errors.Is(err, ErrUIJoinAbsent) {
		t.Errorf("finishing a test-set with no annotation: got %v", err)
	}
	if err := FinishUIJoinAnnotation(nil, 1789000060000); err == nil {
		t.Error("finishing a nil test-set was accepted")
	}
}

func TestFinishWritesTheStopTimeOnlyOnce(t *testing.T) {
	/*
	 * Finish's doc says "a stop time is written once, and a wrong one is
	 * not repairable from the outside" — offered as the reason for the
	 * epoch and t1>=t0 checks — and then nothing enforced it. A second
	 * call silently moved the stop time, INCLUDING BACKWARDS, and the
	 * only happy-path test was itself a double-finish asserting that the
	 * overwrite worked.
	 */
	fresh := func() *TestSet {
		ts := &TestSet{}
		a := validAnnotation()
		a.T1WallMs = 0
		if err := SetUIJoinAnnotation(ts, a); err != nil {
			t.Fatalf("setup: %v", err)
		}
		return ts
	}
	const first = int64(1789000060000)

	ts := fresh()
	if err := FinishUIJoinAnnotation(ts, first); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	// Idempotent: a retried stop with the SAME answer is not an error.
	if err := FinishUIJoinAnnotation(ts, first); err != nil {
		t.Errorf("re-stamping the same stop time was refused: %v", err)
	}
	for name, t1 := range map[string]int64{
		"forwards":  first + 1000,
		"backwards": first - 1000,
	} {
		if err := FinishUIJoinAnnotation(ts, t1); err == nil {
			t.Errorf("moving the stop time %s was accepted", name)
		}
		got, err := GetUIJoinAnnotation(ts)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.T1WallMs != first {
			t.Errorf("%s: stop time moved to %d anyway", name, got.T1WallMs)
		}
	}
}

func TestFinishHandlesEveryShapeTheReaderAccepts(t *testing.T) {
	/*
	 * Get reads four document shapes — there is a whole
	 * TestReadAcceptsEveryDocumentShapeACodecProduces for it — and Finish
	 * refused three of them, on a comment claiming an in-place update
	 * would "re-encode a document this build may not fully describe".
	 * Nothing re-encodes anything: bson.M IS map[string]interface{}, and
	 * bson.D is a slice whose matching element can be replaced by index.
	 *
	 * The consequence of refusing was exactly what Finish exists to
	 * prevent: Complete() false forever, so the joiner declines.
	 */
	const stop = int64(1789000060000)
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"specVersion": UIJoinSpecVersion, "captureId": "cap_1",
			"sessionNonce": "n_1", "t0WallMs": int64(1789000000000),
			"t1WallMs": int64(0), "ingressPorts": []interface{}{8080},
			"appOrigins":       []interface{}{"http://localhost:3000"},
			"canonicalKeySpec": "canonical-key.v1",
		}
	}
	shapes := map[string]func() interface{}{
		"map[string]interface{}": func() interface{} { return base() },
		"bson.M": func() interface{} {
			m := bson.M{}
			for k, v := range base() {
				m[k] = v
			}
			return m
		},
		"map[interface{}]interface{}": func() interface{} {
			m := map[interface{}]interface{}{}
			for k, v := range base() {
				m[k] = v
			}
			return m
		},
		"bson.D": func() interface{} {
			d := bson.D{}
			for k, v := range base() {
				d = append(d, bson.E{Key: k, Value: v})
			}
			return d
		},
		// The append path: t1WallMs absent entirely from an ordered doc.
		"bson.D without t1WallMs": func() interface{} {
			d := bson.D{}
			for k, v := range base() {
				if k == "t1WallMs" {
					continue
				}
				d = append(d, bson.E{Key: k, Value: v})
			}
			return d
		},
	}

	for name, build := range shapes {
		t.Run(name, func(t *testing.T) {
			ts := &TestSet{Metadata: map[string]interface{}{UIJoinMetadataKey: build()}}
			if _, err := GetUIJoinAnnotation(ts); err != nil {
				t.Fatalf("the reader rejected this shape, so the premise is wrong: %v", err)
			}
			if err := FinishUIJoinAnnotation(ts, stop); err != nil {
				t.Fatalf("Finish refused a shape the reader accepts: %v", err)
			}
			got, err := GetUIJoinAnnotation(ts)
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got.T1WallMs != stop {
				t.Errorf("stop time not recorded: %d", got.T1WallMs)
			}
			if !got.Complete() {
				t.Error("still incomplete after being finished")
			}
		})
	}
}

func TestFinishNamesTheStoredStopTimeWhenItRefusesToMoveIt(t *testing.T) {
	// Keeps the write-once rule distinguishable from the epoch and t0
	// rules, which the shared fixture above deliberately no longer
	// exercises. Without this, making the guard's message identical to
	// the others would go unnoticed.
	ts := &TestSet{}
	a := validAnnotation()
	a.T1WallMs = 0
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("setup: %v", err)
	}
	const first = int64(1789000060000)
	if err := FinishUIJoinAnnotation(ts, first); err != nil {
		t.Fatalf("first finish: %v", err)
	}
	err := FinishUIJoinAnnotation(ts, first+1000)
	if err == nil {
		t.Fatal("moving the stop time was accepted")
	}
	if !strings.Contains(err.Error(), "already finished") {
		t.Errorf("the refusal does not say the capture was already finished: %v", err)
	}
	if !strings.Contains(err.Error(), "1789000060000") {
		t.Errorf("the refusal does not name the stop time already stored: %v", err)
	}
}

func TestTheEpochCeilingIsExclusiveOnBothTimestamps(t *testing.T) {
	// `>=` vs `>` is a one-value difference and every other timestamp
	// case sits far from it, so both boundaries survived every mutation.
	// Finish's table gained an exact-ceiling case; validateStructure's
	// two identical boundaries were left untested.
	/*
	 * ASSERTS THE MESSAGE, not just the sentinel.
	 *
	 * The first version of this test set t0 to the ceiling and checked
	 * for ErrUIJoinIncomplete. Weakening `>=` to `>` let t0 through the
	 * ceiling check — and the annotation was then refused by "t1WallMs
	 * precedes t0WallMs", the same sentinel, so the test passed while
	 * the boundary it names was broken. Setting t1 out of the way and
	 * naming the rule is what makes it pin the boundary.
	 */
	for name, mutate := range map[string]func(*UIJoinAnnotation){
		"t0 at the ceiling": func(a *UIJoinAnnotation) {
			a.T0WallMs = 4102444800000
			a.T1WallMs = 0 // still recording, so the ordering rule cannot fire
		},
		"t1 at the ceiling": func(a *UIJoinAnnotation) { a.T1WallMs = 4102444800000 },
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			mutate(a)
			err := a.Storable()
			if !errors.Is(err, ErrUIJoinIncomplete) {
				t.Fatalf("the ceiling is exclusive, so %s must be refused; got %v", name, err)
			}
			if !strings.Contains(err.Error(), "plausible epoch-ms") {
				t.Errorf("%s was refused by the wrong rule: %v", name, err)
			}
		})
	}
	// And the value one below it is accepted, so this cannot pass by
	// refusing everything near the boundary.
	a := validAnnotation()
	a.T0WallMs = 4102444799999
	a.T1WallMs = 4102444799999
	if err := a.Storable(); err != nil {
		t.Errorf("the last millisecond inside the range was refused: %v", err)
	}
}

func TestStorableReportsAbsenceForANilAnnotation(t *testing.T) {
	// Storable's godoc spends a paragraph on this exact distinction —
	// ErrUIJoinAbsent here, "nil annotation" from Set, because Set is
	// being handed a nil argument rather than an empty test-set — and
	// nothing asserted the sentinel, so any error would have passed.
	var nilAnnotation *UIJoinAnnotation
	if err := nilAnnotation.Storable(); !errors.Is(err, ErrUIJoinAbsent) {
		t.Errorf("expected ErrUIJoinAbsent, got %v", err)
	}
	if err := SetUIJoinAnnotation(&TestSet{}, nil); errors.Is(err, ErrUIJoinAbsent) {
		t.Error("Set reported a nil ARGUMENT as a test-set that carries nothing")
	}
}

func TestTheAlreadyFinishedRefusalHasItsOwnSentinel(t *testing.T) {
	/*
	 * The write-once refusal used ErrUIJoinIncomplete — "missing a
	 * required field" — for an annotation that is missing nothing and is
	 * already complete. A retrying producer could not tell "you lost a
	 * race" from "your record is corrupt" with errors.Is, and the only
	 * test asserted on a substring because there was no sentinel to
	 * match.
	 */
	ts := &TestSet{}
	a := validAnnotation()
	a.T1WallMs = 0
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("setup: %v", err)
	}
	const first = int64(1789000060000)
	if err := FinishUIJoinAnnotation(ts, first); err != nil {
		t.Fatalf("first finish: %v", err)
	}

	err := FinishUIJoinAnnotation(ts, first+1000)
	if !errors.Is(err, ErrUIJoinAlreadyFinished) {
		t.Fatalf("expected ErrUIJoinAlreadyFinished, got %v", err)
	}
	if errors.Is(err, ErrUIJoinIncomplete) {
		t.Error("a complete annotation was reported as missing a required field")
	}
	// The retry a producer actually writes must still be a no-op, not an
	// error, when it carries the same answer.
	if err := FinishUIJoinAnnotation(ts, first); err != nil {
		t.Errorf("re-stamping the same stop time was refused: %v", err)
	}
}

func TestTheExportedSurfaceIsNilSafe(t *testing.T) {
	/*
	 * NIL-SAFETY AS A CONTRACT, pinned by ONE guard.
	 *
	 * An earlier version of this test claimed to pin four nil guards —
	 * in Validate, Storable, Joinable and validateStructure — and pinned
	 * none of them, because they MUTUALLY MASKED: normalized() on a nil
	 * receiver returns nil, so Validate and Storable reached
	 * validateStructure's guard anyway, and Joinable delegates to
	 * Validate. Any one of the four, or any three, could be deleted with
	 * the whole suite green. A test whose preamble asserts a property it
	 * does not deliver is worse than no test, and this file has now been
	 * caught at that twice.
	 *
	 * The three outer guards are gone. What remains is one backstop in
	 * validateStructure, which a single mutation kills — so these
	 * assertions now mean what they say.
	 */
	var a *UIJoinAnnotation
	if err := a.Validate(); !errors.Is(err, ErrUIJoinAbsent) {
		t.Errorf("Validate on a nil annotation: %v", err)
	}
	if err := a.Storable(); !errors.Is(err, ErrUIJoinAbsent) {
		t.Errorf("Storable on a nil annotation: %v", err)
	}
	if a.Joinable() {
		t.Error("Joinable on a nil annotation returned true")
	}
	if a.Complete() {
		t.Error("Complete on a nil annotation returned true")
	}
	if got := a.Normalized(); got != nil {
		t.Errorf("Normalized on a nil annotation returned %v", got)
	}
	// The package-level functions take a TEST-SET, and a nil one is the
	// caller's bug rather than a test-set that carries no annotation —
	// different mistakes, so deliberately not ErrUIJoinAbsent.
	if err := FinishUIJoinAnnotation(nil, 1789000060000); err == nil {
		t.Error("Finish on a nil test-set was accepted")
	} else if errors.Is(err, ErrUIJoinAbsent) {
		t.Error("Finish reported a nil TEST-SET as a test-set carrying no annotation")
	}
	if err := SetUIJoinAnnotation(nil, validAnnotation()); err == nil {
		t.Error("Set on a nil test-set was accepted")
	}
}

func TestABadStopTimeIsReportedBeforeAlreadyFinished(t *testing.T) {
	/*
	 * ORDERING, which was a surviving mutant: moving the write-once guard
	 * above the epoch or t0 check left the suite green.
	 *
	 * It is load-bearing because Finish's godoc tells a retrying producer
	 * to treat ErrUIJoinAlreadyFinished as success. Under the flipped
	 * order, a producer whose clock is broken hands Finish a nonsense t1,
	 * gets "already finished", swallows it as success, and never learns —
	 * the argument it passed is never validated at all.
	 */
	ts := &TestSet{}
	a := validAnnotation()
	a.T1WallMs = 0
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := FinishUIJoinAnnotation(ts, 1789000060000); err != nil {
		t.Fatalf("first finish: %v", err)
	}

	for name, t1 := range map[string]int64{
		"seconds not ms":   1789000060,
		"zero":             0,
		"before t0":        1789000000000 - 1,
		"past the ceiling": 4102444800000,
	} {
		t.Run(name, func(t *testing.T) {
			err := FinishUIJoinAnnotation(ts, t1)
			if err == nil {
				t.Fatalf("a %s stop time was accepted", name)
			}
			if errors.Is(err, ErrUIJoinAlreadyFinished) {
				t.Errorf("a bad ARGUMENT was reported as a lost race, which a "+
					"retrying producer is told to swallow: %v", err)
			}
			if !errors.Is(err, ErrUIJoinIncomplete) {
				t.Errorf("expected the argument to be refused: %v", err)
			}
		})
	}
}

func TestAWildcardIsRefusedBecauseNoBrowserReportsOne(t *testing.T) {
	/*
	 * `*` is a non-empty label under 63 octets, so the DNS-shape rule
	 * waved these through and they were STORED. A joiner then compares
	 * every exchange origin against a string no browser ever produces,
	 * so everything classifies FOREIGN_ORIGIN with nothing to explain
	 * why — the silent empty join this whole function exists to prevent.
	 *
	 * The route in is the one the tests call THE PATH THAT MATTERS: a
	 * hand-written config.yaml, filled in by a human reading it off the
	 * server's CORS configuration, which is exactly where wildcards live.
	 */
	for _, origin := range []string{
		"https://*.example.com", "http://*", "https://*.*",
		"https://app.*.example.com", "capacitor://*",
	} {
		t.Run(origin, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if !errors.Is(err, ErrUIJoinIncomplete) {
				t.Fatalf("%q was accepted: %v", origin, err)
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Errorf("%q refused by the wrong rule: %v", origin, err)
			}
			ts := &TestSet{}
			if err := SetUIJoinAnnotation(ts, a); err == nil {
				t.Errorf("%q was stored", origin)
			}
		})
	}

	// A literal asterisk cannot appear in a real host, but the rule must
	// not have become "refuse anything with a star anywhere" — the path
	// and query already carry their own refusals.
	ok := validAnnotation()
	ok.AppOrigins = []string{"https://star.example.com", "http://localhost:3000"}
	if err := ok.Storable(); err != nil {
		t.Errorf("an ordinary origin was caught by the wildcard rule: %v", err)
	}
}

/* ------------------------------------------------------------------ */
/*  What is ON DISK, not what the reader hands back                    */
/* ------------------------------------------------------------------ */

// storedOrigins reads the raw map SetUIJoinAnnotation wrote, without
// going through GetUIJoinAnnotation.
func storedOrigins(t *testing.T, ts *TestSet) []string {
	t.Helper()
	m, ok := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})
	if !ok {
		t.Fatalf("stored value is %T, not a map", ts.Metadata[UIJoinMetadataKey])
	}
	raw, ok := m["appOrigins"].([]interface{})
	if !ok {
		t.Fatalf("appOrigins is %T", m["appOrigins"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("an origin is %T", v)
		}
		out = append(out, s)
	}
	return out
}

func TestTheBYTESONDISKAreTheCanonicalForm(t *testing.T) {
	/*
	 * THE BLIND SPOT THAT HID A REAL BUG FOR SEVEN ROUNDS.
	 *
	 * Every test named for Set's normalization asserted on
	 * GetUIJoinAnnotation's return — and Get runs `a = a.normalized()`
	 * before returning, so the READER launders whatever the writer put
	 * down. Replacing Set's `a = normalized` with `_ = normalized`, so it
	 * stores the raw annotation, left the entire suite green.
	 *
	 * And the reader is not the documented consumer. This package's whole
	 * premise is an OFFLINE joiner — in TypeScript — reading
	 * keploy/<id>/config.yaml, which sees the raw map and nothing else.
	 * Set's own comment argues at length that storing
	 * " http://localhost:3000" verbatim makes the join silently never
	 * match; that regression was invisible here.
	 *
	 * So this asserts the bytes, and it is how the trailing-dot bug
	 * surfaced: `http://a.example.com....` was accepted and written as
	 * `http://a.example.com..` — an empty DNS label, on disk.
	 */
	ts := &TestSet{}
	a := validAnnotation()
	a.CaptureID = "  cap_pad  "
	a.AppOrigins = []string{" http://App.Example.COM:3000/ ", "http://b.example.com."}
	if err := SetUIJoinAnnotation(ts, a); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m := ts.Metadata[UIJoinMetadataKey].(map[string]interface{})
	if got := m["captureId"]; got != "cap_pad" {
		t.Errorf("captureId written as %q, padding and all", got)
	}
	got := storedOrigins(t, ts)
	want := []string{"http://app.example.com:3000", "http://b.example.com"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("stored origins %v, want %v", got, want)
		}
	}
}

func TestWhatIsStoredIsAFixedPoint(t *testing.T) {
	/*
	 * THE PROPERTY THAT MAKES THE CLASS VISIBLE, rather than one more
	 * example of it.
	 *
	 * canonicalHost's docstring called itself idempotent and was not —
	 * trimTrailingDot is a single TrimSuffix, and canonicalHost used to
	 * be invoked a varying number of times per branch — so an origin
	 * could be "normalized" into something that still needed
	 * normalizing. Anything that survives Set must
	 * therefore be a FIXED POINT: normalizing the stored bytes again
	 * changes nothing, and re-validating them passes.
	 *
	 * Stated as a property because the bug was found at three dots and
	 * the same shape exists at four, five, and in any future rule that
	 * rewrites a host a varying number of times.
	 */
	inputs := []string{
		"http://localhost:3000", "https://app.example.com",
		"http://A.EXAMPLE.COM", "http://a.example.com.",
		// MORE THAN ONE DOT, because one is where the original test
		// stopped and the bug lived at three. canonicalHost trims a
		// single suffix per call — it is called once per origin now, but
		// each extra dot still probes a different depth of the
		// empty-label rule.
		"http://a.example.com..", "http://a.example.com...",
		"http://a.example.com....", "http://a.example.com.....",
		"http://a.example.com..:8080", "http://a.example.com...:8080",
		"https://app.example.com:443",
		"http://app.example.com:80", "http://[2001:0db8::0001]",
		"http://127.0.0.1:5173", "https://пример.рф",
		"capacitor://localhost", "http://localhost:3000/",
	}
	stored := 0
	for _, in := range inputs {
		a := validAnnotation()
		a.AppOrigins = []string{in}
		ts := &TestSet{}
		if err := SetUIJoinAnnotation(ts, a); err != nil {
			continue // refused is a fine answer; this is about what survives
		}
		stored++
		onDisk := storedOrigins(t, ts)[0]

		// (1) Normalizing again must change nothing.
		again := normalizeOrigin(onDisk)
		if again != onDisk {
			t.Errorf("%q was stored as %q, which normalizes further to %q — "+
				"the stored value is not canonical", in, onDisk, again)
		}
		// (2) And the stored bytes must still validate, on their own.
		round := validAnnotation()
		round.AppOrigins = []string{onDisk}
		if err := round.Storable(); err != nil {
			t.Errorf("%q was stored as %q, which does not validate: %v", in, onDisk, err)
		}
		// (3) No empty DNS label may ever reach disk, in any position.
		if u, err := url.Parse(onDisk); err == nil {
			for _, label := range strings.Split(u.Hostname(), ".") {
				if label == "" {
					t.Errorf("%q was stored as %q, whose host carries an empty label",
						in, onDisk)
				}
			}
		}
	}
	if stored == 0 {
		t.Fatal("every input was refused, so this asserted nothing")
	}
}

func TestKnownKeySpecToleratesPaddingForExternalCallers(t *testing.T) {
	// The exported entry point is reached directly by a producer asking
	// about a value read from a config file or an env var — the caller
	// who has whitespace. No in-package path arrives untrimmed, which is
	// why this went unpinned and a comment claimed pinning was impossible.
	if !UIJoinKnownKeySpec("  canonical-key.v1  ") {
		t.Error("a known spec with padding was reported unknown")
	}
	if UIJoinKnownKeySpec("  $$$garbage$$$  ") {
		t.Error("padding made an unknown spec look known")
	}
}

func TestNormalizedCopiesEveryListSoAProducerCannotMutateIt(t *testing.T) {
	// AppOrigins was copied and IngressPorts was not, and the difference
	// is externally observable: a producer that keeps its annotation and
	// mutates a port after calling Normalized() would change the value it
	// had just been handed.
	a := validAnnotation()
	got := a.Normalized()
	a.IngressPorts[0] = 9999
	a.AppOrigins[0] = "http://mutated.example.com"
	if got.IngressPorts[0] == 9999 {
		t.Error("IngressPorts is aliased to the caller's slice")
	}
	if got.AppOrigins[0] == "http://mutated.example.com" {
		t.Error("AppOrigins is aliased to the caller's slice")
	}
}

func TestTheVERDICTDoesNotDependOnThePort(t *testing.T) {
	/*
	 * THE DIMENSION TestWhatIsStoredIsAFixedPoint CANNOT SEE.
	 *
	 * That test asserts things about the BYTES of whatever survives Set,
	 * and skips every refusal — `if err != nil { continue }`. So it is
	 * silent about WHICH inputs reach disk, and the defect lived exactly
	 * there: canonicalHost was applied once or twice depending on which
	 * branches normalizeOrigin took, so
	 *
	 *     http://a.example.com..        was stored
	 *     http://a.example.com..:8080   was refused
	 *
	 * — the same host and the same empty trailing label, decided by the
	 * port. Both spellings sat four lines apart in that test's input list
	 * and it reported nothing, because a refusal is not a byte.
	 *
	 * A Set refusal is total: captureId, sessionNonce, timestamps and
	 * ports all go on the floor and the test-set reads back
	 * ErrUIJoinAbsent. So an asymmetry here is not cosmetic — it decides
	 * whether a capture survives.
	 */
	hosts := []string{
		"a.example.com", "a.example.com.", "a.example.com..",
		"a.example.com...", "A.Example.COM", "localhost",
		"127.0.0.1", "[::1]", "[::ffff:127.0.0.1]",
		"пример.рф", "xn--e1afmkfd.xn--p1ai", "a..example.com",
	}
	// http/https each with: no port, the scheme default, a non-default.
	type variant struct{ scheme, port string }
	variants := []variant{
		{"http", ""}, {"http", ":80"}, {"http", ":8080"},
		{"https", ""}, {"https", ":443"}, {"https", ":8443"},
	}

	for _, h := range hosts {
		t.Run(h, func(t *testing.T) {
			var first *bool
			var firstSpelling string
			for _, v := range variants {
				origin := v.scheme + "://" + h + v.port
				a := validAnnotation()
				a.AppOrigins = []string{origin}
				accepted := SetUIJoinAnnotation(&TestSet{}, a) == nil
				if first == nil {
					first = &accepted
					firstSpelling = origin
					continue
				}
				if accepted != *first {
					t.Errorf("the verdict depends on the port: %q -> %v but %q -> %v",
						firstSpelling, *first, origin, accepted)
				}
			}
		})
	}
}

func TestNoRefUSALArmEverEchoesAPassword(t *testing.T) {
	/*
	 * EVERY ARM, not the one that refuses credentials.
	 *
	 * The first fix redacted at `case u.User != nil:` — the LAST arm of
	 * the switch — so it covered the one shape that never reaches the
	 * others. Every arm above it, and the parse-error path, formatted the
	 * raw value: thirteen message sites, one fixed. And the likeliest
	 * real shape, APP_ORIGINS="http://user:pass@api.internal/v1", takes
	 * the PATH arm, not the credentials arm.
	 */
	/*
	 * EACH CASE NAMES WHAT MUST NOT APPEAR, because "hunter2" alone is
	 * not enough. url.Parse splits a password on the first `/`, so
	 * `admin:hun/ter2@host` leaks `hun` and nothing else — a fragment
	 * the whole-password assertion cannot see. A test that only ever
	 * looks for the complete secret scores a partial disclosure green.
	 */
	type leak struct {
		origin string
		forbid []string
	}
	for name, tc := range map[string]leak{
		"credentials arm": {origin: "http://admin:hunter2@a.example.com"},
		/*
		 * THE SECRET GOES WHERE THE ARM IS, not always in userinfo.
		 *
		 * These three rows used to carry `hunter2` in the password and
		 * a fixed `/foo`, `?x=1`, `#f` — so they were three more tests
		 * of the userinfo replacement under names promising path, query
		 * and fragment coverage. Making uiJoinDisplay append the path
		 * or query reddened none of them. The secret now sits in the
		 * component each row is named for.
		 */
		"path arm":     {origin: "http://a.example.com/hunter2"},
		"query arm":    {origin: "http://a.example.com?t=hunter2"},
		"fragment arm": {origin: "http://a.example.com#hunter2"},
		// ...and the userinfo versions of the same three, kept because
		// they exercise a different arm reaching the same message.
		"path arm with userinfo":     {origin: "http://admin:hunter2@a.example.com/foo"},
		"query arm with userinfo":    {origin: "http://admin:hunter2@a.example.com?x=1"},
		"fragment arm with userinfo": {origin: "http://admin:hunter2@a.example.com#f"},
		"wildcard arm":               {origin: "http://admin:hunter2@*.example.com"},
		"DNS-shape arm":              {origin: "http://admin:hunter2@a..example.com"},
		"empty-port arm":             {origin: "http://admin:hunter2@a.example.com:"},
		"parse-error path":           {origin: "http://admin:hunter2@a.example.com:abc"},
		"port-range arm":             {origin: "http://admin:hunter2@a.example.com:70000"},
		// THE ARM THE FIRST NINE OMITTED. Only `err == nil && u.User ==
		// nil` reaches "no scheme and host", and that was the one state
		// with no redaction — one slash short of the example this whole
		// fix was written for.
		"no-scheme-and-host arm": {origin: "http:/admin:hunter2@api.internal/v1"},
		"opaque url":             {origin: "http:admin:hunter2@api.internal/v1"},
		"bare userinfo":          {origin: "admin:hunter2@api.internal"},
		"percent-encoded":        {origin: "http://admin%3Ahunter2%40a.example.com/"},
		/*
		 * THE ARMS THAT REACH url.Error's OWN REASON TEXT, which the
		 * thirteen above could not.
		 *
		 * Every case above puts the password in userinfo behind a
		 * literal `@` with no `/` inside it, so url.Parse either
		 * succeeds — and the whole-userinfo replacement handles it;
		 * url.URL.Redacted() is explicitly NOT used, because it masks
		 * only a password — or fails with a
		 * reason that quotes something OTHER than the secret. The only
		 * parse-failure case was ":abc", whose inner error quotes
		 * `abc`. So the test named for covering the parse path passed
		 * for an unrelated reason, and could not see a leak through
		 * ue.Err at all.
		 *
		 * A `/` in the password is what makes url.Parse read the
		 * password as a PORT: `invalid port ":hunter2" after host`.
		 * A base64 password contains a slash, so this is the ordinary
		 * shape rather than an exotic one.
		 */
		"inner-error port echo":    {origin: "http://admin:hunter2/@api.internal"},
		"inner-error encoded echo": {origin: "http://admin:hunter2%40api.internal/v1"},
		// The slash splits the password: url.Parse reports `invalid port
		// ":hun" after host`, so `hun` is the whole of the disclosure and
		// "hunter2" never appears. Named explicitly for that reason.
		"inner-error partial echo": {origin: "http://admin:hun/ter2@api.internal", forbid: []string{"hun"}},
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{tc.origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", tc.origin)
			}
			forbid := tc.forbid
			if len(forbid) == 0 {
				forbid = []string{"hunter2"}
			}
			for _, secret := range forbid {
				// CASE-FOLDED. normalizeOrigin lowercases the host before
				// the validator runs, so an exact-case check cannot see a
				// leak through the host position AT ALL — an independent
				// review measured 11 of 55 messages carrying its probe
				// token in lowercase while the exact assertion scored zero.
				if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(secret)) {
					t.Errorf("the password fragment %q reached the log: %v", secret, err)
				}
			}
			/*
			 * STILL ACTIONABLE — by INDEX, always, and by host only
			 * when there provably is one.
			 *
			 * This used to require the host in every message. That is
			 * a promise the display cannot keep: locating a host in a
			 * string url.Parse rejected means guessing where the
			 * authority ends, and every attempt at that guess leaked
			 * something — the password, then the whole userinfo tail,
			 * then the query. The index is derived from the list
			 * rather than the entry, so it is always safe and always
			 * enough to find the line in APP_ORIGINS.
			 */
			if !strings.Contains(err.Error(), "appOrigins[0]") {
				t.Errorf("the refusal does not name which entry failed: %v", err)
			}
			// And where the authority DID parse, it is named — so the
			// index is not a licence to say nothing. Keyed on the
			// display's own prefix, not a bare "(": several messages
			// carry parenthetical prose of their own, and matching that
			// made this assertion fire on the arms it does not cover.
			if strings.Contains(err.Error(), "appOrigins[0] (") &&
				!strings.Contains(err.Error(), "example.com") &&
				!strings.Contains(err.Error(), "internal") {
				t.Errorf("an authority was shown but names no host: %v", err)
			}
		})
	}
}

func TestACredentialedOriginIsRefusedWithoutEchoingThePassword(t *testing.T) {
	// The refusal goes into the recorder's log, and the documented way
	// origins arrive — strings.Split(os.Getenv("APP_ORIGINS"), ",") — is
	// exactly where a credentialed URL comes from. Echoing %q verbatim
	// logged the password inside an error about there being a password.
	a := validAnnotation()
	a.AppOrigins = []string{"http://admin:hunter2@a.example.com"}
	err := a.Storable()
	if err == nil {
		t.Fatal("a credentialed origin was accepted")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("refused by the wrong rule: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the refusal echoes the password: %v", err)
	}
	/*
	 * THE USERNAME IS MASKED TOO, and this assertion used to require
	 * the opposite.
	 *
	 * url.URL.Redacted() rewrites userinfo only when a password is
	 * present, so `https://<token>@host` — how git, npm and curl
	 * credential helpers all spell a token — printed the token in full
	 * under a message about credentials. A username IS a secret in that
	 * shape, and there is no way to tell the two apart from here.
	 */
	if strings.Contains(err.Error(), "admin") {
		t.Errorf("the refusal echoes the username, which is a token in the "+
			"`https://<token>@host` shape: %v", err)
	}
	// The host still identifies the entry.
	if !strings.Contains(err.Error(), "a.example.com") {
		t.Errorf("the refusal does not name the host: %v", err)
	}
}

/*
A USERNAME-ONLY CREDENTIAL is the canonical token-in-URL shape, and
every redaction this file has had masked only the PASSWORD.

url.URL.Redacted() is explicit about it: it rewrites u.User only when
`u.User.Password()` reports ok. So `http://<token>@host` walked through
four rounds of credential redaction untouched, inside the one message
whose subject is that the origin carries a credential.
*/
func TestAUsernameOnlyCredentialIsNotEchoed(t *testing.T) {
	const token = "ghp0123456789abcdefSECRET"
	a := validAnnotation()
	a.AppOrigins = []string{"http://" + token + "@a.example.com"}
	err := a.Storable()
	if err == nil {
		t.Fatal("a credentialed origin was accepted")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("a username-only credential was echoed in full: %v", err)
	}
	if !strings.Contains(err.Error(), "a.example.com") {
		t.Errorf("the refusal does not name the host: %v", err)
	}
}

func TestTheDNSNameLengthBoundaryIsExact(t *testing.T) {
	// 253 is the limit; the existing test builds a 304-octet host, so it
	// cannot tell 253 from 254 and `> 253` -> `> 254` survived. One
	// octet either side is the only thing that pins the constant.
	host := func(n int) string {
		// Labels of 63 plus separators, trimmed to exactly n octets.
		s := ""
		for len(s) < n {
			if len(s) > 0 {
				s += "."
			}
			s += strings.Repeat("a", 63)
		}
		return s[:n]
	}
	for name, tc := range map[string]struct {
		n    int
		want bool
	}{
		"exactly 253": {253, true},
		"254":         {254, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := host(tc.n)
			if strings.HasSuffix(h, ".") {
				t.Skipf("generated host ends in a separator at %d", tc.n)
			}
			a := validAnnotation()
			a.AppOrigins = []string{"http://" + h}
			got := a.Storable() == nil
			if got != tc.want {
				t.Errorf("a %d-octet host: accepted=%v, want %v", tc.n, got, tc.want)
			}
		})
	}
}

func TestAMultiDotHostIsREFUSEDNotRepaired(t *testing.T) {
	/*
	 * THE ASSERTION BOTH EXISTING TESTS ARE BLIND TO BY CONSTRUCTION.
	 *
	 * TestWhatIsStoredIsAFixedPoint skips refusals outright
	 * (`if err != nil { continue }`), and TestTheVERDICTDoesNotDependOnThePort
	 * asserts only that the port variants AGREE. So changing
	 * trimTrailingDot's TrimSuffix to TrimRight — which silently repairs
	 * `a.example.com..` into `a.example.com` instead of refusing it —
	 * left the whole suite green while flipping the verdict: uniformly
	 * accepted, still a fixed point, still no empty label.
	 *
	 * The file argues this at length ("Making the trim a loop would paper
	 * over it; one call plus refusing the input is the honest answer"),
	 * and the guarantee was prose only. A browser never produces these
	 * spellings, so repairing them invents an origin the producer did not
	 * write — which is the whole failure class this validator exists for.
	 */
	for _, origin := range []string{
		"http://a.example.com..",
		"http://a.example.com...",
		"http://a.example.com....",
		"https://a.example.com..:8443",
		"http://пример.рф..",
	} {
		t.Run(origin, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			if err := a.Storable(); err == nil {
				t.Errorf("%q was repaired and accepted; a multi-dot host must "+
					"be refused, not silently turned into a different origin", origin)
			}
		})
	}

	// The single fully-qualified dot IS canonically equivalent and is
	// still repaired — this must not have become "refuse every dot".
	ok := validAnnotation()
	ok.AppOrigins = []string{"http://a.example.com."}
	if err := ok.Storable(); err != nil {
		t.Errorf("a single trailing dot is the same host and must still normalize: %v", err)
	}
}

func TestTheEpochFloorIsExact(t *testing.T) {
	// The ceiling was pinned and the floor was not: `< floor` -> `<= floor`
	// survived, and the constant 1577836800000 appeared nowhere in this
	// file. Same class as the DNS-length boundary, one constant over.
	for name, tc := range map[string]struct {
		t0   int64
		want bool
	}{
		"the floor itself": {1577836800000, true},
		"one below":        {1577836800000 - 1, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.T0WallMs = tc.t0
			a.T1WallMs = 0
			if got := a.Storable() == nil; got != tc.want {
				t.Errorf("t0 = %d: accepted=%v, want %v", tc.t0, got, tc.want)
			}
		})
	}
}

func TestASchemeRelativeOriginIsRefused(t *testing.T) {
	// `//example.com` has a Host and no Scheme. The `u.Scheme == ""` half
	// of that check is load-bearing — dropping it accepts and STORES this
	// verbatim — while the comment on the line describes the other half,
	// which really is wording-only. No scheme-relative origin appeared
	// anywhere in this file.
	for _, origin := range []string{"//example.com", "//example.com:8080"} {
		a := validAnnotation()
		a.AppOrigins = []string{origin}
		if err := a.Storable(); err == nil {
			t.Errorf("%q was accepted; a scheme-relative reference is not an origin", origin)
		}
	}
}

/*
THE AUTHORITY IS THE WHOLE OF WHAT A REFUSAL PRINTS.

This used to be a direct test of redactUserinfo, a textual scanner that
masked the userinfo span of a string url.Parse could not handle. That
function is gone: it was the last piece of "print everything except what
I found to be secret", and two separate leaks came out of it — it echoed
the entire tail after the last at-sign (so `?token=...` survived), and it
returned the raw string untouched whenever the at-sign sat at or before
the scheme prefix.

What replaced it prints only an authority it can PROVE, so there is no
scanner left to test directly. What remains worth pinning is the
property the old direct cases were really about: a credential-free
origin must not come back mangled, with its port rendered as a password.
That lives in the caller, and this is it.
*/
func TestACredentialFreeOriginIsNotMangledInItsOwnRefusal(t *testing.T) {
	/*
	 * NO AT-SIGN ANYWHERE, so the authority is provably a host and is
	 * shown — spelled correctly, with the port still a port.
	 *
	 * This is the anti-MANGLING property: a textual scanner once turned
	 * `http://a.example.com:8080/x` into "http://a.example.com:xxxxx@x",
	 * reporting an operator's port as a password.
	 */
	for _, tc := range []struct {
		origin    string
		authority string
		inPath    string
	}{
		// The path segment must be a string that CANNOT appear in the
		// message's own prose. A first version used "path", which the
		// refusal says twice ("followed by a path; an origin carries no
		// path"), so the assertion fired on the wording rather than on
		// an echo.
		{"http://a.example.com/zqxpathmarker", "http://a.example.com", "zqxpathmarker"},
		{"http://a.example.com:8080/zqxpathmarker", "http://a.example.com:8080", "zqxpathmarker"},
		{"http://[::1]:8080/zqxpathmarker", "http://[::1]:8080", "zqxpathmarker"},
		{"http://[2001:db8::1]/x?zqxquerymarker=1", "http://[2001:db8::1]", "zqxquerymarker"},
	} {
		a := validAnnotation()
		a.AppOrigins = []string{tc.origin}
		err := a.Storable()
		if err == nil {
			t.Errorf("%q carries a path and should be refused", tc.origin)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.authority) {
			t.Errorf("the refusal does not name the authority %q: %v", tc.authority, err)
		}
		if strings.Contains(msg, "xxxxx") {
			t.Errorf("a credential-free origin was redacted anyway: %v", err)
		}
		// The path is where a token lives; it is never echoed.
		if strings.Contains(strings.ToLower(msg), strings.ToLower(tc.inPath)) {
			t.Errorf("the refusal echoed %q, which is where a token lives: %v",
				tc.inPath, err)
		}
	}
}

/*
AN AT-SIGN PAST THE AUTHORITY WITHHOLDS THE AUTHORITY.

net/url ends the authority at the first '/', '?' or '#', BEFORE it looks
for '@'. So a credential carrying any of those three is classified as
the HOST with a nil User, and "print the authority" printed it whole:

	https://ghp_ABCDEF/@api.internal  ->  Host="ghp_ABCDEF", User=nil

That is indistinguishable, syntactically, from `http://a.example.com/pa@th`
— identical nil User, identical host-shaped Host, identical at-sign past
the authority. Since no rule can tell them apart, both withhold: the
entry is named by index alone.

The benign half costs an operator the host in one message; the other half
is a live token in the recorder's log.
*/
func TestAnAtSignPastTheAuthorityWithholdsIt(t *testing.T) {
	const secret = "ghpZQXSECRET42"
	for name, origin := range map[string]string{
		"slash in the token":    "https://" + secret + "/@api.internal",
		"query in the token":    "https://" + secret + "?@api.internal",
		"fragment in the token": "https://" + secret + "#@api.internal",
		/*
		 * A DIGITS-ONLY PASSWORD, and the digits are load-bearing.
		 *
		 * `admin:<letters>` is not a valid host:port, so url.Parse
		 * REFUSES it and the row never reaches the authority builder —
		 * a first version used the letter-bearing secret above and
		 * passed with the guard deleted, pinning nothing. Only a
		 * numeric password parses as a port, which is what makes
		 * net/url call the whole credential a host.
		 */
		"user and numeric password": "http://admin:8675309/@api.internal",
		// NAMED FOR WHAT IT ACTUALLY DOES. u.Path arrives DECODED, so
		// this reaches the guard as "/@api.internal" and is caught by
		// the plain at-sign test — deleting the encoded-form handling
		// left this row green. It is kept as a path-decoding case, not
		// as encoded-at-sign coverage; that lives in
		// TestTheEncodedAtSignClauseIsReachable.
		"an encoded at-sign decodes into the path": "https://" + secret + "/%40api.internal",
		/*
		 * USERINFO PRESENT **AND** A SECOND AT-SIGN PAST THE AUTHORITY.
		 *
		 * The guard was briefly gated on `u.User == nil`, which made it
		 * weaker the more credential markers the input carried: the
		 * rows above withheld, while these — one character longer and
		 * more obviously credentials — set u.User, skipped the guard
		 * entirely, and printed the token-shaped host. Nothing covered
		 * the gate in either direction.
		 */
		"userinfo and a slashed token":  "https://x@" + secret + "/@api.internal",
		"userinfo and a queried token":  "https://u:p@" + secret + "?@api.internal",
		"userinfo and a fragment token": "https://u:p@" + secret + "#@api.internal",
		// The benign twin: same shape, no secret. Withheld too, because
		// nothing can tell it from the rows above.
		"an at-sign in a real path": "http://a.example.com/pa@th",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) {
				t.Fatalf("the token was echoed as a host:\n%s", msg)
			}
			// The numeric row carries its own secret, which the shared
			// one cannot express.
			if strings.Contains(msg, "8675309") {
				t.Fatalf("a numeric password was echoed as a port:\n%s", msg)
			}
			// No authority at all — the display is index-only.
			if strings.Contains(msg, "appOrigins[0] (") {
				t.Fatalf("an unprovable authority was shown anyway:\n%s", msg)
			}
			if !strings.Contains(msg, "appOrigins[0]") {
				t.Fatalf("the refusal does not say which entry failed:\n%s", msg)
			}
		})
	}
}

/*
A parse failure is the one message that quotes url.Parse's own text, and
url.Error renders that text with %q. The redaction used to go looking for
the raw origin INSIDE that quoted message — so the moment %q escaped
anything, the search found nothing, replaced nothing, and returned the
password.

A control character is the sharp case because it is one of the very few
things url.Parse actually refuses, so the failing input and the escaping
input are the same input. 0x7f is `\x7f` in the message and one byte in
the origin; they are not the same string and never were.
*/
func TestAParseFailureCannotLeakAPasswordThroughGoQuoting(t *testing.T) {
	/*
	 * THE INPUT MUST PRODUCE A QUOTED SPAN, and the first version of
	 * this test did not.
	 *
	 * It used a trailing 0x7f, whose inner error is `net/url: invalid
	 * control character in URL` — no quoted span anywhere, so the scrub
	 * has nothing to do and deleting it left this test GREEN. It pinned
	 * the '@' fallback arm while its name promised Go-quoting.
	 *
	 * url.Parse embeds the offending fragment with %q, and the fragment
	 * is the SECRET whenever a slash inside the password makes the
	 * stdlib read the password as a port. That is the shape this test
	 * needs.
	 *
	 * TWO OF THESE FIVE ROWS PIN THE SCRUB — "a slash in the password"
	 * and "an encoded at-sign". Both produce `invalid port ":hunter2"
	 * after host`, whose two quotes are BALANCED, so the unbalanced
	 * check never fires and the scrub is the only thing standing
	 * between the secret and the log. Delete the scrub and exactly
	 * those two red. The other three carry no quoted span holding the
	 * secret and pin the ALLOWLIST instead.
	 *
	 * THIS PARAGRAPH HAS BEEN WRONG IN BOTH DIRECTIONS, which is worth
	 * recording because of HOW. It first said two; a review measured
	 * zero and it was rewritten to say so, asserting "Measured:" — but
	 * the measurement had been taken when the balance check was
	 * strip-based and therefore DEPENDED on the scrub having run.
	 * Changing that check to count quotes on the original made the
	 * scrub load-bearing again and invalidated the measurement, and the
	 * conclusion was left standing over the code that had falsified it.
	 * Re-measured directly: two rows, as above.
	 */
	for name, origin := range map[string]string{
		"a slash in the password":   "http://admin:hunter2/@api.internal",
		"an encoded at-sign":        "http://admin:hunter2%40api.internal/v1",
		"a control character":       "http://admin:hunter2@api.internal/\x7f",
		"a bad percent escape":      "http://admin:hunter2@api.internal/%zz",
		"a token in a query string": "http://api.internal/cb?apikey=hunter2\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := uiJoinOriginIsWellFormed(0, origin)
			if err == nil {
				t.Fatalf("%q must be refused; got nil", origin)
			}
			msg := err.Error()
			if strings.Contains(msg, "hunter2") {
				t.Fatalf("the secret reached the message:\n%s", msg)
			}
			if strings.Contains(msg, "admin") {
				t.Fatalf("the username reached the message:\n%s", msg)
			}
			// WHICH ENTRY, always. An unparseable origin has no
			// authority this package can locate, so the index is the
			// whole of the identification — and it has to be there.
			if !strings.Contains(msg, "appOrigins[0]") {
				t.Fatalf("the refusal does not say which entry failed:\n%s", msg)
			}
			// The DIAGNOSIS must survive the scrub. A redaction that
			// throws url.Parse's own words away is not a fix, it is a
			// different bug: the unquoted words are what tell an
			// operator what kind of thing is wrong.
			if !strings.Contains(msg, "is not a URL") {
				t.Fatalf("the refusal does not say it failed to parse:\n%s", msg)
			}
		})
	}
}

/*
The scrub removes QUOTED spans and leaves the words around them, because
the words are the diagnosis and the quoted fragment is an echo of input
this package has already decided it must not print.
*/
func TestTheParseReasonKeepsItsWordsAndLosesItsQuotes(t *testing.T) {
	// No quoted span: nothing to scrub, everything survives.
	err := uiJoinOriginIsWellFormed(0, "http://api.internal/\x7f")
	if err == nil {
		t.Fatal("a control character must be refused")
	}
	if !strings.Contains(err.Error(), "invalid control character") {
		t.Errorf("an unquoted reason must survive intact: %v", err)
	}
	// A quoted span: the words survive, the fragment does not.
	err = uiJoinOriginIsWellFormed(0, "http://api.internal:notaport")
	if err == nil {
		t.Fatal("a non-numeric port must be refused")
	}
	if !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("the words of the reason must survive: %v", err)
	}
	if strings.Contains(err.Error(), "notaport") {
		t.Errorf("the quoted fragment must not survive: %v", err)
	}
	/*
	 * A QUOTE INSIDE THE SECRET, which is the only input that separates
	 * uiJoinQuotedSpan's escape-aware pattern from a naive `"[^"]*"`.
	 *
	 * net/url escapes an embedded quote as \" inside the span it quotes.
	 * A naive pattern stops at that backslash-quote and leaves the rest
	 * of the value in the message: for this origin the raw reason is
	 *
	 *   invalid port ":pa\"ss" after host
	 *
	 * which the strict pattern scrubs to `invalid port "..." after host`
	 * and the naive one to `invalid port "..."ss" after host` — the
	 * password's tail, printed. Measured both ways.
	 *
	 * Every other property of uiJoinParseReason had a test; the one that
	 * actually prevents a leak had none, so swapping the pattern for
	 * `"[^"]*"` passed the whole package. A quote in a password is no
	 * more exotic than the slash this function's docstring already
	 * builds its argument around.
	 */
	err = uiJoinOriginIsWellFormed(0, "http://admin:pa\"ss/@api.internal")
	if err == nil {
		t.Fatal("a quote in the userinfo must be refused")
	}
	for _, leak := range []string{"ss\"", "pa", "admin"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("a quoted credential fragment %q survived the "+
				"scrub: %v", leak, err)
		}
	}
}

/*
AN OPAQUE URL HAS A SCHEME AND NO HOST, and only that input pins the
second half of the guard that refuses it.

`if u.Scheme == "" || u.Host == ""` covers three states, and every test
that reached it reached it with BOTH empty — so deleting `|| u.Host ==
""` left the whole package green. The arm's message was reworded in the
round before this one precisely because it was false for this input (it
said the entry "carries no scheme and host" about a URL whose scheme is
"zqxtok"), and that rewording shipped with no test at all: `grep "needs
both a scheme" uijoin_test.go` returned nothing.

net/url reads everything after the first colon of "ZQXTOK:pw@evil://..."
as Opaque, so Scheme is non-empty and Host is "". With the Host half
deleted the entry falls through to the DNS arm instead and is told its
"host is not a resolvable DNS name" — true of the empty string, and
useless to anyone holding a URL that has no host field at all.

It fails shut either way, so this is a diagnosis test, not a leak test.
The leak half is covered separately: the secret must not appear, and it
does not, because uiJoinDisplay returns at its empty-Host guard.
*/
func TestAnOpaqueURLIsToldWhatAnOriginNeeds(t *testing.T) {
	for _, tc := range []struct{ origin, secret string }{
		{"ZQXTOK:pw@evil://api.internal/x", "zqxtok"},
		{"mailto:admin:hunter2@api.internal", "hunter2"},
		{"urn:ZQXTOK:pw", "zqxtok"},
	} {
		a := validAnnotation()
		a.AppOrigins = []string{tc.origin}
		err := a.Storable()
		if err == nil {
			t.Errorf("%q is not an origin and should be refused", tc.origin)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "an origin needs both a scheme and a host") {
			t.Errorf("%q reached the wrong arm: %v", tc.origin, msg)
		}
		// The DNS arm is where it lands when the Host half of the guard
		// is dropped, and its wording is actively misleading here.
		if strings.Contains(msg, "resolvable DNS name") {
			t.Errorf("%q was diagnosed as a bad DNS name, but it has no "+
				"host field at all: %v", tc.origin, msg)
		}
		if strings.Contains(strings.ToLower(msg), strings.ToLower(tc.secret)) {
			t.Errorf("the refusal for %q leaks %q: %v", tc.origin, tc.secret, msg)
		}
	}
}

/*
THREE GUARDS THAT WERE CORRECT AND UNOBSERVED.

Each could be deleted with the whole package green, which is this file's
own definition of a branch that should not be here — but all three are
right, so the answer is to observe them rather than remove them. All are
unexported and this test file is in the same package, so there was never
a reason a test could not reach them; the same correction was applied to
UIJoinKnownKeySpec and to uiJoinParseReason's two arms in earlier rounds.

 1. uiJoinDisplay's `if u.Scheme != ""`. Reachable, not dead: a
    scheme-relative origin has a Host and no Scheme, so without the
    guard the authority renders as "://example.com".
    TestASchemeRelativeOriginIsRefused asserts only that the entry is
    refused, never what the message says, so nothing saw it.

 2. uiJoinIsHex's uppercase clause. The suite had ZERO uppercase-hex
    coverage of the hand-rolled percent decoder. Not exploitable — every
    layer of an encoded at-sign chain is digit-only ("%40", "%2540",
    "%252540"), so uppercase hex cannot hide an at-sign — but it is an
    unpinned clause in the redaction path, and "not exploitable today"
    is the argument that let two other clauses rot.

 3. normalizeOrigin's `|| u.Host == ""` bail. With no host there is
    nothing to canonicalise, and falling through would rebuild the URL:
    "http:/" comes back as "http:". Every such value is refused
    downstream either way, so the refusal path cannot see the
    difference — a direct call can.
*/
func TestGuardsThatNothingWasWatching(t *testing.T) {
	// (1) A scheme-relative origin renders its authority without an
	// empty scheme prefix.
	u, err := url.Parse("//example.com/x")
	if err != nil {
		t.Fatalf("scheme-relative parse: %v", err)
	}
	if got := uiJoinDisplay(u); got != "example.com" {
		t.Errorf("a scheme-relative origin renders %q, want %q", got, "example.com")
	}

	// (2) Uppercase hex decodes. %4A is 'J'; a decoder that accepts only
	// lowercase copies the escape through untouched.
	for _, tc := range []struct{ in, want string }{
		{"a%4A", "aJ"},
		{"a%4a", "aJ"},
		{"%2F%2f", "//"},
	} {
		if got := uiJoinUnescapeOnce(tc.in); got != tc.want {
			t.Errorf("uiJoinUnescapeOnce(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// (3) An origin with no host is returned unchanged rather than
	// rebuilt. Not a security property — a diagnosis one: the value the
	// message names should be the value the operator wrote.
	for _, tc := range []struct{ in, want string }{
		{"http:/", "http:/"},
		{"mailto:someone", "mailto:someone"},
		{"  http:/  ", "http:/"},
	} {
		if got := normalizeOrigin(tc.in); got != tc.want {
			t.Errorf("normalizeOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

/*
THE ESCAPE SCANNER'S BOUND, at the boundary.

uiJoinUnescapeOnce reads s[i+1] and s[i+2] behind `i+2 < len(s)`. That
comparison is the only thing standing between a truncated percent escape
and an index panic, and nothing in the suite fed it one: every origin
tested elsewhere either has no '%' or has a complete escape after it.

Measured: changing `<` to `<=` — a one-character edit in the redaction
path — passes the ENTIRE package, and then panics with "index out of
range [3] with length 3" on "a%4". That is not a hypothetical input.
APP_ORIGINS arrives as strings.Split(os.Getenv(...), ","), so a truncated
env var (`https://h/?x=%4`) crashes the recorder outright.

DRIVEN DIRECTLY, because the panic is in an unexported helper and this
test file is in the same package. Routing it through Storable() would
work too, but a direct call names the function whose bound is at issue.

Each row also asserts the RESULT, not merely the absence of a panic: a
bound that is too tight ("i+2 < len(s)-1") stops decoding a trailing
complete escape, which is silent and would let "%40" at end-of-string
through the at-sign scan.
*/
func TestATruncatedEscapeIsCopiedRatherThanRead(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a%4", "a%4"},
		{"%4", "%4"},
		{"a%", "a%"},
		{"%", "%"},
		{"%%", "%%"},
		{"a%zz", "a%zz"},
		// ...and a COMPLETE escape at the very end still decodes, which
		// is the half a too-tight bound would break silently.
		{"a%40", "a@"},
		{"%40", "@"},
		{"%2540", "%40"},
	} {
		if got := uiJoinUnescapeOnce(tc.in); got != tc.want {
			t.Errorf("uiJoinUnescapeOnce(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// And the property that matters: a trailing complete escape is still
	// caught by the scan that consumes this function.
	if !uiJoinCarriesAtSign("x%40") {
		t.Error("a trailing encoded at-sign must still be caught")
	}
	if uiJoinCarriesAtSign("x%4") {
		t.Error("a truncated escape is not an at-sign")
	}
}

/*
uiJoinParseReason's TWO DEFENSIVE ARMS, pinned rather than argued.

Both were deletable with the package green, and the file's own standard
calls that a branch that should not be there. They ARE reachable and
they ARE worth keeping — the point is that nothing proved it. This
function is unexported and this test file is in the same package, so
there was never a reason they could not be driven directly. The same
correction was applied to UIJoinKnownKeySpec two rounds ago, whose
comment had claimed a test could not reach it; two functions later the
identical mistake was still standing.
*/
func TestTheParseReasonHandlesErrorsItCannotRedact(t *testing.T) {
	// A *url.Error carrying no inner error. Deleting this arm makes the
	// next line nil-dereference rather than returning a phrase.
	if got := uiJoinParseReason(&url.Error{Err: nil}); got != "unspecified parse failure" {
		t.Errorf("a url.Error with no cause must be named, not dereferenced: %q", got)
	}
	// Anything that is not a *url.Error has no structure to scrub, so
	// its text cannot be shown to be free of input. Replacing this arm
	// with err.Error() prints the whole thing.
	got := uiJoinParseReason(errors.New("ZQXSECRET was rejected"))
	if strings.Contains(got, "ZQXSECRET") {
		t.Errorf("an unredactable error must not be echoed: %q", got)
	}
	if !strings.Contains(got, "cannot be redacted") {
		t.Errorf("an unredactable error must say so: %q", got)
	}
}

/*
url.URL.Port() is digits-only by construction and url.Parse refuses a
non-numeric port outright, so the only way strconv.Atoi fails on it is
RANGE. Telling an operator that a string of twenty digits "is not a
number" sends them looking for a typo that is not there; the value is a
number, and it is out of range.
*/
func TestAnOverflowingPortIsCalledOutOfRangeNotNotANumber(t *testing.T) {
	// Twenty digits: valid per url.Parse's validOptionalPort (all
	// digits), and beyond int64 for strconv.Atoi.
	err := uiJoinOriginIsWellFormed(0, "http://a.example.com:99999999999999999999")
	if err == nil {
		t.Fatalf("a port beyond int range must be refused; got nil")
	}
	if strings.Contains(err.Error(), "not a number") {
		t.Fatalf("a twenty-digit port was reported as not a number:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Fatalf("want an out-of-range message, got:\n%s", err.Error())
	}
	if !errors.Is(err, ErrUIJoinIncomplete) {
		t.Fatalf("want ErrUIJoinIncomplete, got %v", err)
	}
}

/*
This validator parses the string it was GIVEN. It used to parse a trimmed
copy, which was dead — normalized() runs before every call site and
normalizeOrigin trims — but it meant the value that was parsed, the value
the messages quoted with %q, and the value the redaction was handed could
all be three different strings.

The public path is unaffected and TestTheBYTESONDISKAreTheCanonicalForm
pins that: `" http://App.Example.COM:3000/ "` still round-trips, because
normalizeOrigin trims it before this function ever sees it.
*/
func TestAnOriginIsValidatedExactlyAsGiven(t *testing.T) {
	if err := uiJoinOriginIsWellFormed(0, " http://a.example.com"); err == nil {
		t.Fatalf("a leading space makes it not an origin; want refusal, got nil")
	}
	if err := uiJoinOriginIsWellFormed(0, "http://a.example.com "); err == nil {
		t.Fatalf("a trailing space makes it not an origin; want refusal, got nil")
	}
	// The trimmed spelling is the one that is accepted, and it is what
	// every in-package caller supplies.
	if err := uiJoinOriginIsWellFormed(0, "http://a.example.com"); err != nil {
		t.Fatalf("the trimmed origin must still be accepted, got %v", err)
	}
	// And the whole-annotation path still tolerates padding, because
	// normalized() trims before validateStructure runs.
	a := &UIJoinAnnotation{
		CaptureID:        "cap-0000000000000001",
		SessionNonce:     "nonce-0000000000000001",
		CanonicalKeySpec: "canonical-key.v1",
		SpecVersion:      1,
		T0WallMs:         1700000000000,
		T1WallMs:         1700000001000,
		IngressPorts:     []int{8080},
		AppOrigins:       []string{"  http://a.example.com  "},
	}
	if err := a.normalized().validateStructure(); err != nil {
		t.Fatalf("padding must survive the public path, got %v", err)
	}
}

/*
canonicalHost's bracket handling had no test. It also had TWO
implementations: a `strings.Trim(hostname, "[]")` in canonicalHost
itself, and unmapIPv4's own Trim one call down. The first was dead —
removing it left the entire package suite green, INCLUDING the first
version of this test, which was written to pin it and did not. It has
been removed; unmapIPv4 provides the behaviour.

So this pins the OBSERVABLE contract rather than a line: whatever
spelling of a host goes in, exactly one pair of brackets comes out for
an IPv6 literal and none for a name. That is the claim the docstring
makes, and it survives wherever the implementation moves.
*/
func TestCanonicalHostDoesNotDoubleBrackets(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The shape the Trim exists for: already bracketed in, exactly
		// one pair out.
		{"[::1]", "[::1]"},
		{"[2001:db8::1]", "[2001:db8::1]"},
		// The shape production actually supplies — Hostname() strips the
		// brackets, canonicalHost puts them back.
		{"::1", "[::1]"},
		{"2001:db8::1", "[2001:db8::1]"},
		// And a name, which must not acquire brackets at all.
		{"a.example.com", "a.example.com"},
		{"a.example.com.", "a.example.com"},
	} {
		if got := canonicalHost(tc.in); got != tc.want {
			t.Errorf("canonicalHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

/*
THE INPUTS AN INDEPENDENT REVIEW CONFIRMED LEAKING, kept as a battery.

Each of these printed a secret through a DIFFERENT hole, and each hole
was opened by the same habit: deciding what to hide and printing the
rest. In order of discovery —

 1. masking only the userinfo PASSWORD left a username-only credential
    whole, which is how every git/npm/curl helper spells a token;
 2. masking the whole userinfo span left everything after the last '@',
    so a query string went out verbatim;
 3. showing the parsed authority closed that — but only on the arm
    where the parse SUCCEEDED. An entry that merely failed to parse
    fell through the switch to the raw string, and one trailing newline
    out of a .env file is enough to make url.Parse fail.

The fix is an allowlist: print an authority only when the stdlib proved
there is one, and otherwise print the entry's INDEX and nothing else.
There is no fall-through because the raw string is never a candidate.
*/
func TestTheConfirmedLeakInputsStayShut(t *testing.T) {
	const secret = "SECRETTOKEN"
	for name, origin := range map[string]string{
		/*
		 * A TRAILING NEWLINE DOES NOT REACH url.Parse, and this row used
		 * to claim it did.
		 *
		 * normalized() runs strings.TrimSpace before the validator, so
		 * the newline is gone and the origin PARSES. The row therefore
		 * takes the path arm, not the fall-through — which makes it
		 * vacuous for the allowlist (it passes with uiJoinDisplay
		 * returning the raw string) and is exactly what an independent
		 * review caught.
		 *
		 * Kept, renamed to what it actually pins: that a query string
		 * is withheld when the authority IS printable. The
		 * fall-through is pinned by the control-character and
		 * percent-escape rows below, which TrimSpace does not touch.
		 */
		"a query behind a trimmed newline": "http://api.internal/cb?apikey=" + secret + "\n",
		"control character":                "http://api.internal/v1?token=" + secret + "\x7f",
		"bad percent escape":               "http://api.internal/%zz?token=" + secret,
		"unclosed ipv6":                    "http://[::1?token=" + secret,
		"space in the scheme":              "ht tp://api.internal?token=" + secret,
		"password as port":                 "http://api.internal:pw" + secret,
		"null byte":                        "http://api.internal\x00/?t=" + secret,
		"userinfo then query":              "http:/admin:pw@api.internal/v1?token=" + secret,
		"username-only token":              "http://" + secret + "@api.internal",
		"token in a live path":             "http://api.internal/v1?token=" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			// CASE-FOLDED — see TestNoRefUSALArmEverEchoesAPassword.
			if strings.Contains(strings.ToLower(err.Error()), strings.ToLower(secret)) {
				t.Fatalf("the secret reached the log:\n%s", err.Error())
			}
			// Whatever else is withheld, the entry is still named.
			if !strings.Contains(err.Error(), "appOrigins[0]") {
				t.Fatalf("the refusal does not say which entry failed:\n%s", err.Error())
			}
		})
	}
}

/*
THE SCRUB IS FAIL-SHUT ON UNBALANCED QUOTING.

uiJoinQuotedSpan matches a COMPLETE quoted span, so an opening quote
with no close passes through untouched — `invalid port "hunter2 after
host` would have been printed whole. No stdlib error produces that
shape today, which is exactly the sort of assumption this file has been
caught by twice; the %T branch beside it already withholds a reason it
cannot take apart, and this makes the two agree.

Tested directly because no reachable input produces it: a test that can
only be written against the function is still worth more than a comment
asserting the property.
*/
func TestAnUnbalancedReasonIsWithheldEntirely(t *testing.T) {
	for name, tc := range map[string]struct {
		reason string
		shown  bool
	}{
		"balanced":            {`invalid port ":hunter2" after host`, false},
		"two balanced spans":  {`bad "a" and "hunter2"`, false},
		"escaped inner quote": {`invalid port ":hun\"ter2" after host`, false},
		"no quotes at all":    {`net/url: invalid control character in URL`, true},
		// The shapes the pattern cannot account for.
		"unterminated":        {`invalid port "hunter2 after host`, false},
		"trailing open quote": {`a "b" c "hunter2`, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := uiJoinParseReason(&url.Error{Op: "parse", URL: "x", Err: errors.New(tc.reason)})
			if strings.Contains(got, "hunter2") {
				t.Fatalf("the secret survived the scrub: %q -> %q", tc.reason, got)
			}
			// A reason with nothing to hide must still be readable.
			if tc.shown && !strings.Contains(got, "control character") {
				t.Errorf("an unquoted reason must pass through intact: %q", got)
			}
			if !tc.shown && strings.Contains(got, `"hunter2`) {
				t.Errorf("an unaccountable reason must be withheld: %q", got)
			}
		})
	}
}

/*
THE %40 CLAUSE, pinned at last — and pinned through RawQuery, because
that is the only field that reaches it.

u.Path is DECODED, so `https://TOKEN/%40api.internal` arrives as
Path="/@api.internal" and is caught by the plain at-sign check. The row
named "encoded at-sign" therefore pinned nothing: delete
`strings.Contains(strings.ToLower(part), "%40")` and the whole package
stayed green. u.RawQuery is never decoded, so `?%40b` is the shape that
needs the clause.

And u.Host is in the scanned set for the same reason it was the eighth
leak: net/url rejects a bare %40 in an authority but passes %25 through,
so `https://TOKEN%2540api.internal` — one ordinary layer of URL encoding
over `TOKEN%40api.internal` — parses to a host carrying %40 with every
other field empty.
*/
func TestTheEncodedAtSignClauseIsReachable(t *testing.T) {
	const secret = "ghpZQXENCODED42"
	for name, origin := range map[string]string{
		// Reaches the clause through RawQuery, which is never decoded.
		"encoded at-sign in a query": "http://a.example.com?%40" + secret,
		// Reaches it through the HOST — the field the guard did not scan.
		"double-encoded host credential": "https://" + secret + "%2540api.internal",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) {
				t.Fatalf("the token was echoed:\n%s", msg)
			}
			if strings.Contains(msg, "appOrigins[0] (") {
				t.Fatalf("an unprovable authority was shown anyway:\n%s", msg)
			}
		})
	}
}

/*
AT ANY ENCODING DEPTH, not just one.

net/url unescapes a host exactly once, so a check for the literal "%40"
is exactly one decode deep. `%2540` arrives as `%40` and is caught;
`%252540` arrives as `%2540`, in which the four characters "%40" do not
occur at all — the guard cannot fire and the token reaches the log. That
was leak #9, one layer above leak #8, inside the clause written for #8.

Two layers is ordinary: the defence of the old check named "a CI
variable, a compose interpolation, a proxy config" as the source of the
first, and any two of those give the second.
*/
func TestAnEncodedAtSignIsCaughtAtEveryDepth(t *testing.T) {
	const secret = "ghpZQXDEEP42"
	for name, origin := range map[string]string{
		// Depth 2 is caught by the literal test at level 0, so it does
		// NOT discriminate the fixpoint — only depths 3+ do. Kept as
		// the boundary between the two mechanisms, labelled rather
		// than counted: an earlier docstring presented all four as
		// pinning leak #9.
		/*
		 * THE QUERY POSITION, because the HOST position no longer
		 * reaches the scan at all.
		 *
		 * uiJoinDisplay returns early for any host containing '%', so
		 * every host-position row here is decided by that rule and
		 * exercises none of the fixpoint. Measured by a review:
		 * swapping the whole fixpoint for the pre-leak-#9 literal check
		 * left every public-path test green.
		 *
		 * A query carries the same at-sign signal with a '%'-free host,
		 * and u.RawQuery is never decoded by net/url — so these are the
		 * inputs that actually drive the multi-round decode through
		 * Storable().
		 */
		"host, depth 2 (the '%' rule, not the fixpoint)": "https://" + secret + "%2540api.internal",
		"host, depth 3":  "https://" + secret + "%252540api.internal",
		"query, depth 1": "https://a.example.com?%40" + secret,
		"query, depth 2": "https://a.example.com?%2540" + secret,
		"query, depth 3": "https://a.example.com?%252540" + secret,
		"query, depth 4": "https://a.example.com?%25252540" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) {
				t.Fatalf("the token was echoed at %s:\n%s", name, msg)
			}
			if strings.Contains(msg, "appOrigins[0] (") {
				t.Fatalf("an unprovable authority was shown at %s:\n%s", name, msg)
			}
		})
	}
}

/*
AN ORDINARY IPv6 ZONE ID MUST STILL DISPLAY.

The fixpoint scan stops when PathUnescape ERRORS, because a '%' that is
not an escape is not an encoding layer — `[fe80::1%eth0]` is a zone
identifier, not a hidden credential. Without that distinction the scan
would withhold the authority on every zoned IPv6 origin, which is a
diagnosability cost paid for nothing.
*/
func TestAZoneIdentifierIsNotAnEncodingLayer(t *testing.T) {
	if uiJoinCarriesAtSign("[fe80::1%eth0]") {
		t.Error("a zone identifier was read as an encoded at-sign")
	}
	if uiJoinCarriesAtSign("a.example.com") {
		t.Error("an ordinary host was read as carrying an at-sign")
	}
	// And the shapes that DO carry one, at each depth.
	for _, s := range []string{"a@b", "a%40b", "a%2540b", "a%252540b"} {
		if !uiJoinCarriesAtSign(s) {
			t.Errorf("%q carries an at-sign and was not detected", s)
		}
	}
}

/*
NO AUTHORITY WITHOUT A HOST — the other half of uiJoinDisplay's pair,
which the call-site paragraph claims is pinned by tests and was not.
Deleting `|| u.Host == ""` left the whole package green.
*/
func TestAnOriginWithNoHostShowsNoAuthority(t *testing.T) {
	const secret = "8675309"
	a := validAnnotation()
	// Parses, has userinfo, and has an EMPTY Host.
	a.AppOrigins = []string{"http://user:" + secret + "@/path"}
	err := a.Storable()
	if err == nil {
		t.Fatal("an origin with no host was accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "appOrigins[0] (") {
		t.Fatalf("an authority was shown for a hostless origin:\n%s", msg)
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("the password reached the message:\n%s", msg)
	}
}

/*
LEAK #10: url.PathUnescape is ALL-OR-NOTHING.

One malformed '%' anywhere aborts the decode of the whole string,
including well-formed escapes elsewhere in it — and the first fixpoint
read that abort as "no at-sign". So a credentialed host with one stray
'%' after it leaked, and ANY host carrying an IPv6 zone identifier had
the at-sign check disabled outright, because a zone's '%' is not an
escape.

That made the fix for leak #9 WEAKER than the literal check it replaced
on exactly these inputs. The per-escape decoder removes the coupling.
*/
func TestAStrayPercentDoesNotDisableTheAtSignCheck(t *testing.T) {
	const secret = "ghpZQXSTRAY42"
	for name, origin := range map[string]string{
		/*
		 * THE STRAY '%' MUST BE OUTSIDE THE HOST to reach the decoder.
		 *
		 * The host rows put it inside, where the '%' rule returns
		 * before the scan runs — so they pin that rule, not the
		 * all-or-nothing decode this test is named for. Measured:
		 * disabling uiJoinCarriesAtSign entirely fails many subtests and
		 * none of them were these.
		 */
		"host, trailing stray percent": "https://" + secret + "%2540api.internal%25",
		"host, interior stray percent": "https://" + secret + "%2540api%25internal",
		"host, zone id as a prefix":    "https://[fe80::1%25" + secret + "%2540evil]",
		// A '%'-free host with a stray '%' in the QUERY: PathUnescape
		// would abort the whole string and report "no at-sign".
		"query, stray percent":          "https://a.example.com?%2540" + secret + "%25",
		"query, stray percent interior": "https://a.example.com?%2540" + secret + "%25x",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) {
				t.Fatalf("the token was echoed:\n%s", msg)
			}
			if strings.Contains(msg, "appOrigins[0] (") {
				t.Fatalf("an unprovable authority was shown:\n%s", msg)
			}
		})
	}
}

/*
THE NEW CHECK MUST STRICTLY DOMINATE THE ONE IT REPLACED.

Leak #10 existed because a replacement was assumed to subsume its
predecessor and did not — it was stronger on five inputs and WEAKER on
three. This asserts the containment directly rather than trusting it:
anything the level-0 literal test catches, the fixpoint must also catch.
*/
func TestTheFixpointDominatesTheLiteralCheck(t *testing.T) {
	literal := func(s string) bool {
		return strings.Contains(s, "@") ||
			strings.Contains(strings.ToLower(s), "%40")
	}
	for _, s := range []string{
		"a@b", "a%40b", "a%40b%", "a%40b%zz", "A%2540B",
		"[fe80::1%tok%40evil]", "%40", "%2540", "%252540",
		"a%40b@c", "tok%40host%", "%40%",
	} {
		if literal(s) && !uiJoinCarriesAtSign(s) {
			t.Errorf("the literal check catches %q and the fixpoint does not — "+
				"the replacement is weaker than what it replaced", s)
		}
	}
}

/*
THE EXHAUSTION VERDICT AND THE BOUND, both previously unpinned.

Reaching exhaustion needs GENUINELY NESTED escaping — repeated
url.QueryEscape, not a flat run of "%25"s, which PathUnescape collapses
in a single pass. Note url.PathEscape is the wrong tool: it leaves '@'
alone, because '@' is legal in a path segment.
*/
func TestTheFixpointBoundAndItsExhaustionVerdict(t *testing.T) {
	// Ten nested layers: more rounds than the bound allows.
	deep := "@"
	for i := 0; i < 10; i++ {
		deep = url.QueryEscape(deep)
	}
	if !uiJoinCarriesAtSign(deep) {
		t.Fatalf("a value needing more rounds than the bound must fail SHUT; "+
			"got false for %q", deep)
	}
	// And it reaches that verdict by exhaustion, not by finding an
	// at-sign early: at the bound minus one it is still undecided.
	if strings.Contains(deep, "@") {
		t.Fatal("the fixture is not actually nested")
	}
	// The bound itself: a value needing exactly one round is caught by
	// the loop, and a value needing none is caught at level 0.
	if !uiJoinCarriesAtSign("a%2540b") {
		t.Error("one round is within the bound")
	}
	if !uiJoinCarriesAtSign("a@b") {
		t.Error("zero rounds is the level-0 literal test")
	}
	// A value that needs NO decoding and has no at-sign must not be
	// withheld, or the bound would be withholding everything.
	if uiJoinCarriesAtSign("a.example.com") {
		t.Error("an ordinary host must not be withheld")
	}
}

/*
THE BOUND MUST NOT WITHHOLD AN HONEST DEEPLY-ENCODED HOST.

Reducing uiJoinUnescapeRounds is invisible to a leak test: exhaustion
fails SHUT, so a smaller bound only ever withholds MORE. What it breaks
is diagnosability — a host with several harmless encoding layers stops
showing its authority for no reason.

THE INPUT THAT DISCRIMINATES: nested encoding that resolves to something
with NO at-sign. `%252561` -> `%2561` -> `%61` -> `a`. With the real
bound that is three quiet rounds ending in "no"; with a bound of 1 it is
exhaustion, and the operator loses the authority.
*/
func TestTheBoundDoesNotWithholdAnHonestEncodedHost(t *testing.T) {
	for _, host := range []string{
		"a%2561.example.com",       // one layer, decodes to a-a.example.com
		"a%252561.example.com",     // two
		"a%25252561.example.com",   // three
		"a%2525252561.example.com", // four
	} {
		if uiJoinCarriesAtSign(host) {
			t.Errorf("%q carries no at-sign at any depth and must not be "+
				"withheld; the bound is withholding honest hosts", host)
		}
	}
}

/*
THE RULE THAT ENDS THE ENCODING CLASS, rather than patching its next
spelling.

Leaks #8, #9 and #10 were three encodings of ONE shape: a credential
that net/url classified as the host, smuggled past an at-sign check by
`%2540`, then `%252540`, then a stray `%` that aborted the decode
entirely. Each fix chased a spelling; the next spelling arrived.

validateStructure already refuses EVERY host containing '%'. So the
display has nothing to gain by rendering one — the entry is refused
either way and the index names it — and everything to lose, since '%'
is the carrier every one of those leaks needed.

This asserts the rule directly, so a future refactor that reintroduces
host rendering has to argue with a test rather than with a comment.
*/
func TestAPercentBearingHostIsNeverRendered(t *testing.T) {
	const secret = "ghpZQXPCT42"
	for name, origin := range map[string]string{
		"single-encoded":       "https://" + secret + "%2540api.internal",
		"double-encoded":       "https://" + secret + "%25252540api.internal",
		"stray percent":        "https://" + secret + "%2540api.internal%25",
		"zone id":              "https://[fe80::1%25" + secret + "]",
		"zone id with at-sign": "https://[fe80::1%25" + secret + "%2540evil]",
		"plain escape":         "https://a%25" + secret + ".example.com",
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.AppOrigins = []string{origin}
			err := a.Storable()
			if err == nil {
				t.Fatalf("%q was accepted", origin)
			}
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) {
				t.Fatalf("the token was echoed:\n%s", msg)
			}
			if strings.Contains(msg, "appOrigins[0] (") {
				t.Fatalf("a percent-bearing host was rendered:\n%s", msg)
			}
		})
	}
}

/*
AND ORDINARY HOSTS STILL RENDER — the negative control for the rule
above. A guard that withheld everything would satisfy that test and
destroy every refusal message in the file.
*/
/*
THE CREDENTIAL MARKER, ASSERTED PRESENT.

uiJoinDisplay writes a literal "xxxxx@" when the parsed origin carries
userinfo, under a comment arguing why the WHOLE userinfo is replaced
rather than url.URL.Redacted() being called. Nothing asserted the marker
was there: deleting the clause outright left the entire package green,
which is this file's own definition of a branch that should not be here.

What the deletion produces is worse than a missing marker. The refusal
becomes

	appOrigins[0] (https://api.internal); an origin carries no credentials

— a message stating that the entry carries credentials while rendering
an authority in which none are visible, so an operator reading it sees a
contradiction and no way to tell which of their origins is at fault.

THE USERNAME-ONLY ROW IS THE DISCRIMINATING ONE. Redacted() rewrites
userinfo only when a password is present (`ru.User.Password()` must
report ok), so an implementation that called it would still satisfy the
user:pass row and fail this one. That is precisely the substitution the
comment on the clause warns against, and it is the only input that can
tell the two apart.

The absence half is covered separately, by the credential-free case that
asserts no marker is invented where there is no userinfo.
*/
func TestTheCredentialMarkerIsPresentWhenThereIsUserinfo(t *testing.T) {
	for _, tc := range []struct{ origin, secret, want string }{
		{"https://admin:hunter2@api.internal", "hunter2", "https://xxxxx@api.internal"},
		{"https://admin@api.internal", "admin", "https://xxxxx@api.internal"},
		{"https://admin:hunter2@api.internal/v1", "hunter2", "https://xxxxx@api.internal"},
		{"http://svcacct:tok@10.0.0.5:8080", "tok", "http://xxxxx@10.0.0.5:8080"},
	} {
		a := validAnnotation()
		a.AppOrigins = []string{tc.origin}
		err := a.Storable()
		if err == nil {
			t.Errorf("%q carries credentials and should be refused", tc.origin)
			continue
		}
		msg := err.Error()
		/*
		 * THE WHOLE AUTHORITY, NOT `Contains(msg, "xxxxx@")`.
		 *
		 * The first version of this test asserted only that the marker
		 * appeared somewhere. A review moved `b.WriteString(u.Host)`
		 * ABOVE the marker clause and the entire package stayed green:
		 * the refusal then read
		 *
		 *   appOrigins[0] (https://api.internalxxxxx@)
		 *
		 * which satisfies a substring check and is exactly the display
		 * the paragraph above forbids — a marker welded to the end of a
		 * hostname tells an operator nothing about where the credential
		 * was. Round 22's finding, one layer down, inside the test
		 * written to close it.
		 *
		 * The parentheses are part of `want` on purpose: uiJoinNamed
		 * renders the authority as "appOrigins[N] (AUTHORITY)", so
		 * matching them pins the rendering end to end and no substring
		 * of a longer authority can satisfy it.
		 */
		if !strings.Contains(msg, "("+tc.want+")") {
			t.Errorf("the refusal for %q does not render %q: %v",
				tc.origin, tc.want, msg)
		}
		// Case-folded: normalizeOrigin lowercases the host, so a secret
		// with capitals would re-emerge in a form a literal Contains
		// would miss.
		if strings.Contains(strings.ToLower(msg), strings.ToLower(tc.secret)) {
			t.Errorf("the refusal for %q leaks %q: %v",
				tc.origin, tc.secret, msg)
		}
	}
}

func TestOrdinaryHostsStillRenderTheirAuthority(t *testing.T) {
	for _, tc := range []struct{ origin, want string }{
		{"http://localhost:3000/x", "http://localhost:3000"},
		{"http://127.0.0.1:8080/x", "http://127.0.0.1:8080"},
		{"http://[::1]:3000/x", "http://[::1]:3000"},
		{"https://app.example.com/x", "https://app.example.com"},
		{"capacitor://localhost/x", "capacitor://localhost"},
	} {
		a := validAnnotation()
		a.AppOrigins = []string{tc.origin}
		err := a.Storable()
		if err == nil {
			t.Errorf("%q carries a path and should be refused", tc.origin)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("the refusal for %q does not name %q: %v",
				tc.origin, tc.want, err)
		}
	}
}

/*
THE SCHEME ECHO, asserted rather than argued.

uiJoinDisplay prints the scheme, and the paragraph defending the host
echo does not cover it — so the defence rests on net/url restricting a
scheme to alphanumerics plus `+-.`, which means it cannot carry
userinfo, an at-sign or a percent. That is a property of the stdlib, not
of this file, so it is worth a test: if it ever stops holding, the
scheme becomes a credential channel and nothing else here would notice.
*/
/*
WHAT THE FIRST HALF OF THIS TEST PINS IS net/url, NOT THIS FILE.

uiJoinDisplay writes u.Scheme into the message unredacted, and the
paragraph defending that rests entirely on a premise about the standard
library: getScheme requires a leading alpha followed by alphanum, '+',
'-' or '.', so no '@', '%' or userinfo can reach u.Scheme. If that ever
stopped holding, the scheme echo would become a leak and nothing else in
this file would notice.

So the three end-to-end rows below are a CHARACTERIZATION TEST of
net/url. No mutation of uijoin.go kills them, and an earlier version of
this paragraph gave the wrong reason for that: it said "all three fail
to parse". Two do. The third, "ZQXTOK:pw@evil://api.internal/x", PARSES
— net/url reads "zqxtok" as the scheme and puts the rest in Opaque —
so it reaches uiJoinDisplay with a NON-EMPTY Scheme and is stopped by
the empty-Host guard instead. The conclusion survives; the mechanism
named for it did not. Written in the round whose whole purpose was
correcting measured claims in this file, which is how easy this is. They were previously
presented as evidence about this file's redaction. They are evidence
about the premise that redaction rests on, which is worth pinning
precisely because it is someone else's code and can change under us.

The premise is therefore also asserted DIRECTLY, against the parse
result rather than against the message: an input that began parsing
successfully would leave the end-to-end rows green while breaking the
argument, because they only check that a token is absent from a refusal
that is issued for an unrelated reason. That loop `continue`s on a parse
error, so of its five strings only the ones net/url ACCEPTS reach the
assertion — which is the point (a refused input cannot violate the
premise), but it does mean the loop is not a second check on the three
rows below it.

The negative control at the end is the part that does exercise this
file — it fails when the scheme write is deleted from uiJoinDisplay.
*/
func TestASchemeCannotCarryACredentialDelimiter(t *testing.T) {
	for _, o := range []string{
		"ZQXTOK@evil://api.internal/x",
		"ZQXTOK%40evil://api.internal/x",
		"ZQXTOK:pw@evil://api.internal/x",
		"ghp_TOKEN://api.internal",
		"user:pass@http://api.internal",
	} {
		u, err := url.Parse(o)
		if err != nil {
			// Refused outright, so the premise cannot be violated.
			continue
		}
		if strings.ContainsAny(u.Scheme, "@%") {
			t.Errorf("net/url put a credential delimiter in the scheme of "+
				"%q: %q. uiJoinDisplay prints u.Scheme unredacted on the "+
				"strength of this not happening, so the echo is now a leak",
				o, u.Scheme)
		}
	}

	for _, o := range []string{
		"ZQXTOK@evil://api.internal/x",
		"ZQXTOK%40evil://api.internal/x",
		"ZQXTOK:pw@evil://api.internal/x",
	} {
		a := validAnnotation()
		a.AppOrigins = []string{o}
		err := a.Storable()
		if err == nil {
			t.Errorf("%q was accepted", o)
			continue
		}
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "zqxtok@") || strings.Contains(msg, "zqxtok%40") {
			t.Errorf("a credential delimiter survived into the scheme: %v", err)
		}
	}
	// AND THE NEGATIVE CONTROL: a legitimate non-http scheme is still
	// named. capacitor://, ionic://, chrome-extension:// and app:// are
	// real origins from real producers — the scheme allowlist was
	// removed deliberately — so withholding them would make every
	// message about them unactionable.
	a := validAnnotation()
	a.AppOrigins = []string{"capacitor://localhost/x"}
	err := a.Storable()
	if err == nil {
		t.Fatal("an origin carrying a path should be refused")
	}
	if !strings.Contains(err.Error(), "capacitor://localhost") {
		t.Errorf("a legitimate scheme must still be named: %v", err)
	}
}
