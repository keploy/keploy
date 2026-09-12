package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
		// Without an origin the joiner cannot classify FOREIGN_ORIGIN at
		// all, and every foreign exchange reads as simply missing.
		"no app origins":    func(a *UIJoinAnnotation) { a.AppOrigins = nil },
		"empty app origins": func(a *UIJoinAnnotation) { a.AppOrigins = []string{} },
		"blank app origin":  func(a *UIJoinAnnotation) { a.AppOrigins = []string{"  "} },
		// A joiner comparing an exchange origin against any of these
		// classifies EVERY exchange FOREIGN_ORIGIN, with nothing to say
		// why — the same silent-wrong-answer the port range check exists
		// to prevent.
		"origin is not a URL":   func(a *UIJoinAnnotation) { a.AppOrigins = []string{"not a url at all"} },
		"origin is a wildcard":  func(a *UIJoinAnnotation) { a.AppOrigins = []string{"*"} },
		"origin has no scheme":  func(a *UIJoinAnnotation) { a.AppOrigins = []string{"localhost:3000"} },
		"origin is javascript:": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"javascript:alert(1)"} },
		// Isolates the SCHEME check: a real host, a scheme a browser
		// never serves an app origin over.
		"origin is ftp": func(a *UIJoinAnnotation) { a.AppOrigins = []string{"ftp://example.com"} },
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
	// GetUIJoinAnnotation returns a nil annotation on every error path,
	// and a caller that checks a.Complete() before err would panic.
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
			a.CanonicalKeySpec = "  spec/canonical-key.v1.json  "
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

			if (validateErr == nil) != (setErr == nil) {
				t.Fatalf("Validate and Set disagree:\n  Validate -> %v\n  Set      -> %v",
					validateErr, setErr)
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
	for name, t1 := range map[string]int64{
		"seconds where millis belong": 1789000060,
		"the far future":              4102444800001,
		"the maximum int64":           9223372036854775807,
	} {
		t.Run(name, func(t *testing.T) {
			a := validAnnotation()
			a.T1WallMs = t1
			if err := a.Validate(); err == nil {
				t.Fatalf("t1WallMs = %d validated", t1)
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
	 * normalizeOrigin skips both canonicalHost branches — one needs a
	 * non-empty port, the other is guarded against exactly this — so the
	 * host never reaches IDNA and arrives non-ASCII. Ordered after the
	 * non-ASCII case, the port rule could never win and the producer was
	 * told their host was unresolvable.
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
