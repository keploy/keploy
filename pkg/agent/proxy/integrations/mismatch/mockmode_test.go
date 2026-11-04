package mismatch

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Every report shape whose hint tells the user to record again.
func rerecordReports() map[string]*models.MockMismatchReport {
	return map[string]*models.MockMismatchReport{
		"no mocks": NewReport(ProtocolGeneric, "opaque exchange").WithPhase(models.MatchPhaseNoMocks, 0).Build(),
		"body values drifted": NewReport(ProtocolHTTP, "POST /reserve").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "body.qty", Kind: models.DiffKindValueChanged, Expected: "2", Actual: "1"}}).Build(),
		// No keploy.yml key changes how an HTTP call's path or query is
		// matched (url noise is never handed to the matcher), so their drift
		// is offered only recording again.
		"other values drifted": NewReport(ProtocolHTTP, "POST /v2/reserve").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/reserve", Actual: "/v2/reserve"}}).Build(),
		"query values drifted": NewReport(ProtocolHTTP, "GET /orders?page=2").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "query.page", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"}}).Build(),
		// A browser's Referer drifting on a call to the dependency: the miss a
		// keploy.yml header noise line is the remedy for, in either mode.
		"header values drifted": NewReport(ProtocolHTTP, "GET /api/method/ping").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "header.referer", Kind: models.DiffKindValueChanged, Expected: "http://localhost:8000/app", Actual: "http://localhost:8000/desk"}}).Build(),
		// The same Referer recorded and not sent at replay (a page whose
		// Referrer-Policy sends none): without the proven switch the HTTP
		// matcher requires a recorded header unless keploy.yml declares it
		// noise. HeaderKeyDiffs names it as recorded.
		"recorded header missing": NewReport(ProtocolHTTP, "GET /api/method/ping").WithPhase(models.MatchPhaseSchema, 3).
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "header.Referer", Kind: models.DiffKindMissingInLive, Expected: "http://localhost:8000/app"}}).Build(),
		"structure changed": NewReport(ProtocolHTTP, "GET /a").WithPhase(models.MatchPhaseSchema, 5).
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"}}).Build(),
	}
}

// A `keploy mock` run is recorded again with `keploy mock record`; pointing it
// at `keploy record` sends the user to a command that records something else.
func TestHintsNameKeployMockRecordInMockMode(t *testing.T) {
	SetMockMode(false)
	outside := rerecordReports()
	SetMockMode(true)
	t.Cleanup(func() { SetMockMode(false) })
	for name, r := range rerecordReports() {
		if !strings.Contains(r.NextSteps, "'keploy mock record'") {
			t.Errorf("%s: a mock run's hint should name 'keploy mock record', got %q", name, r.NextSteps)
		}
		if strings.Contains(r.NextSteps, "'keploy record'") {
			t.Errorf("%s: a mock run's hint must not name 'keploy record', got %q", name, r.NextSteps)
		}
		if strings.Contains(r.NextSteps, "--update-test-mapping") {
			t.Errorf("%s: --update-test-mapping is a keploy test flag, not a keploy mock one: %q", name, r.NextSteps)
		}
		// keploy mock replay hands the agent keploy.yml's test.globalNoise as
		// keploy test does, so a mock run's hint offers the noise exactly
		// where keploy test's does.
		if want := strings.Contains(outside[name].NextSteps, "test.globalNoise"); strings.Contains(r.NextSteps, "test.globalNoise") != want {
			t.Errorf("%s: a mock run's hint should offer test.globalNoise as keploy test's does (%v): %q", name, want, r.NextSteps)
		}
	}
}

// The exact mock-mode text, so a change to it is a deliberate one.
func TestHintTextInMockMode(t *testing.T) {
	SetMockMode(true)
	t.Cleanup(func() { SetMockMode(false) })
	want := map[string]string{
		"no mocks":                "No recorded mocks were available to match against for this protocol in the selected mock set. Re-record the mock set with 'keploy mock record'.",
		"body values drifted":     "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {qty: []}); otherwise re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"other values drifted":    "Only values drifted (path). If the change is expected, re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"query values drifted":    "Only values drifted (query.page). If the change is expected, re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"header values drifted":   "Only values drifted (header.referer). If these are dynamic (timestamps, ids, tokens), add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"recorded header missing": "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"structure changed":       "Request structure changed since recording. Re-record the mock set with 'keploy mock record'.",
	}
	for name, r := range rerecordReports() {
		if r.NextSteps != want[name] {
			t.Errorf("%s: mock-mode hint:\n got  %q\n want %q", name, r.NextSteps, want[name])
		}
	}
	if got := RecordCommand(); got != "keploy mock record" || !MockMode() {
		t.Errorf("RecordCommand in mock mode = %q, MockMode = %v", got, MockMode())
	}
	// --on-miss is acted on by the HTTP parser alone; other protocols are told only to re-record.
	for _, protocol := range []string{ProtocolGeneric, ProtocolGRPC, ProtocolMySQL, ProtocolPostgres, ProtocolMongo} {
		other := NewReport(protocol, "opaque exchange").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "payload", Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}}).Build()
		if want := "Only values drifted (payload). If these are dynamic (timestamps, ids, tokens), add them to the matching noise (test.globalNoise); otherwise re-record the mock set with 'keploy mock record'."; other.NextSteps != want {
			t.Errorf("%s drift hint in mock mode:\n got  %q\n want %q", protocol, other.NextSteps, want)
		}
	}
}

// The exact keploy test text, which a mock run's differs from only in how it
// is recorded again; it is given after mock mode was on too, so a flag left
// over from one setup cannot leak into a later one.
func TestHintTextOutsideMockMode(t *testing.T) {
	SetMockMode(true)
	SetMockMode(false)
	if got := RecordCommand(); got != "keploy record" || MockMode() {
		t.Errorf("RecordCommand outside mock mode = %q, MockMode = %v", got, MockMode())
	}
	want := map[string]string{
		"no mocks":                "No recorded mocks were available to match against for this protocol in the selected test set. Re-record the test set with 'keploy record'.",
		"body values drifted":     "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {qty: []}); otherwise re-record with 'keploy record'.",
		"other values drifted":    "Only values drifted (path). If the change is expected, re-record with 'keploy record'.",
		"query values drifted":    "Only values drifted (query.page). If the change is expected, re-record with 'keploy record'.",
		"header values drifted":   "Only values drifted (header.referer). If these are dynamic (timestamps, ids, tokens), add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise re-record with 'keploy record'.",
		"recorded header missing": "Recorded headers are missing from this call (header.Referer). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: []}); otherwise re-record with 'keploy record'.",
		"structure changed":       "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping.",
	}
	for name, r := range rerecordReports() {
		if r.NextSteps != want[name] {
			t.Errorf("%s: hint outside mock mode:\n got  %q\n want %q", name, r.NextSteps, want[name])
		}
	}
}

// The noise a hint names is the keploy.yml bucket the matcher reads for each
// field, in both modes, and only when the noise reaches every field: a
// request-body field's drift under requestbody by its root-relative key, an
// HTTP header that drifted or that the call does not carry under header by
// its lowercased name (once), outside HTTP any other drift as
// test.globalNoise. An HTTP call with a field no bucket reaches (its path,
// query, a part of a SigV4 Authorization) is offered only recording again.
// Only the way to record again differs between modes.
func TestHintNamesTheNoiseBucket(t *testing.T) {
	drift := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}
	}
	missing := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindMissingInLive, Expected: "a"}
	}
	added := func(path string) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Kind: models.DiffKindMissingInMock, Actual: "b"}
	}
	const (
		bodyQty  = "the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {qty: []})"
		referer  = "the headers under test.globalNoise.global.header (e.g. header: {referer: []})"
		dynamic  = "If these are dynamic (timestamps, ids, tokens), add "
		expected = "If the change is expected, "
	)
	// want is the hint with %[1]s for the way to record again; structural
	// cases want the structure-change hint.
	cases := []struct {
		name       string
		protocol   string
		diffs      []models.MockFieldDiff
		want       string
		structural bool
	}{
		{name: "a header and a body field", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.referer"), drift("body.qty")},
			want: "Only values drifted (header.referer, body.qty). " + dynamic + bodyQty + " and " + referer + "; otherwise %[1]s."},
		{name: "a header in two spellings, named once", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.X-Request-Id"), drift("header.x-request-id")},
			want: "Only values drifted (header.X-Request-Id, header.x-request-id). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {x-request-id: []}); otherwise %[1]s."},
		{name: "two headers", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization"), drift("header.referer")},
			want: "Only values drifted (header.authorization, header.referer). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {authorization: [], referer: []}); otherwise %[1]s."},
		{name: "a dotted header name stays whole", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.x.trace.id")},
			want: "Only values drifted (header.x.trace.id). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {x.trace.id: []}); otherwise %[1]s."},
		// The parts the SigV4 differ compares, so that only the signature
		// may differ: declaring authorization would stop that comparison.
		{name: "a SigV4 part", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization.region")},
			want: "Only values drifted (header.authorization.region). " + expected + "%[1]s."},
		{name: "a SigV4 part beside a header", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization.signed_headers"), drift("header.referer")},
			want: "Only values drifted (header.authorization.signed_headers, header.referer). " + expected + "%[1]s."},
		{name: "a header and the path", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.referer"), drift("path")},
			want: "Only values drifted (header.referer, path). " + expected + "%[1]s."},
		{name: "a body field and a query parameter", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty"), drift("query.page")},
			want: "Only values drifted (body.qty, query.page). " + expected + "%[1]s."},
		{name: "the method", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("method")},
			want: "Only values drifted (method). " + expected + "%[1]s."},
		{name: "a whole body", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body")},
			want: "Only values drifted (body). " + expected + "%[1]s."},
		{name: "a header path outside HTTP", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{drift("header.referer")},
			want: "Only values drifted (header.referer). " + dynamic + "them to the matching noise (test.globalNoise); otherwise %[1]s."},
		{name: "a body field and another outside HTTP", protocol: ProtocolGRPC, diffs: []models.MockFieldDiff{drift("body.qty"), drift("payload")},
			want: "Only values drifted (body.qty, payload). " + dynamic + bodyQty + "; otherwise %[1]s."},
		// A recorded header the call does not carry: the HTTP matcher
		// requires it unless keploy.yml declares it noise.
		{name: "a recorded header missing", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer")},
			want: "Recorded headers are missing from this call (header.Referer). If they are optional, add " + referer + "; otherwise %[1]s."},
		{name: "a missing header beside drifted values", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), drift("body.qty"), drift("header.x-request-id")},
			want: "Recorded headers are missing from this call (header.Referer) and values drifted (body.qty, header.x-request-id). If these are optional or dynamic (timestamps, ids, tokens), add " + bodyQty + " and the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-request-id: []}); otherwise %[1]s."},
		{name: "a missing header beside a path drift", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), drift("path")}, structural: true},
		{name: "a header only the call carries", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{added("header.X-New")}, structural: true},
		{name: "a missing body field", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("body.qty")}, structural: true},
		{name: "a missing header outside HTTP", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{missing("header.Referer")}, structural: true},
		// A miss that names no field has nothing for noise to reach.
		{name: "no field named", protocol: ProtocolHTTP, structural: true},
		{name: "no field named outside HTTP", protocol: ProtocolGeneric, structural: true},
	}
	rerecord := func(mock bool, protocol string) string {
		switch {
		case !mock:
			return "re-record with 'keploy record'"
		case protocol == ProtocolHTTP:
			return "re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'"
		}
		return "re-record the mock set with 'keploy mock record'"
	}
	structure := map[bool]string{
		false: "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping.",
		true:  "Request structure changed since recording. Re-record the mock set with 'keploy mock record'.",
	}
	for _, mock := range []bool{false, true} {
		SetMockMode(mock)
		t.Cleanup(func() { SetMockMode(false) })
		for _, c := range cases {
			r := NewReport(c.protocol, "GET /a").WithClosest("mock-1", c.diffs).Build()
			want := fmt.Sprintf(c.want, rerecord(mock, c.protocol))
			if c.structural {
				want = structure[mock]
			}
			if r.NextSteps != want {
				t.Errorf("%s (mock mode %v):\n got  %q\n want %q", c.name, mock, r.NextSteps, want)
			}
		}
	}
}
