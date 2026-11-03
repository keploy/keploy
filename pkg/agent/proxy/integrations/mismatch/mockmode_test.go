package mismatch

import (
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
		"other values drifted": NewReport(ProtocolHTTP, "POST /v2/reserve").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/reserve", Actual: "/v2/reserve"}}).Build(),
		"structure changed": NewReport(ProtocolHTTP, "GET /a").WithPhase(models.MatchPhaseSchema, 5).
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"}}).Build(),
	}
}

// A `keploy mock` run is recorded again with `keploy mock record`; pointing it
// at `keploy record` sends the user to a command that records something else.
func TestHintsNameKeployMockRecordInMockMode(t *testing.T) {
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
		// No hint names a noise bucket at a path keploy.yml does not read.
		if strings.Contains(r.NextSteps, "test.globalNoise.requestbody") {
			t.Errorf("%s: keploy.yml reads no noise at test.globalNoise.requestbody: %q", name, r.NextSteps)
		}
	}
}

// The exact mock-mode text, so a change to it is a deliberate one.
func TestHintTextInMockMode(t *testing.T) {
	SetMockMode(true)
	t.Cleanup(func() { SetMockMode(false) })
	want := map[string]string{
		"no mocks":             "No recorded mocks were available to match against for this protocol in the selected mock set. Re-record the mock set with 'keploy mock record'.",
		"body values drifted":  "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {qty: []}); otherwise re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"other values drifted": "Only values drifted (path). If these are dynamic (timestamps, ids, tokens), add a regex of the part of path that changes under test.globalNoise.global.url in keploy.yml (e.g. url: {\"<regex>\": []}); otherwise re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.",
		"structure changed":    "Request structure changed since recording. Re-record the mock set with 'keploy mock record'.",
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
	other := NewReport(ProtocolGeneric, "opaque exchange").
		WithClosest("mock-1", []models.MockFieldDiff{{Path: "payload", Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}}).Build()
	if want := "Only values drifted (payload). If the change is expected, re-record the mock set with 'keploy mock record'."; other.NextSteps != want {
		t.Errorf("non-HTTP drift hint in mock mode:\n got  %q\n want %q", other.NextSteps, want)
	}
}

// A run that is not a mock run gets keploy test's hints (keploy record, the
// test set, --update-test-mapping), also after mock mode was on, so a flag
// left over from one setup cannot leak into a later one. It covers the HTTP
// reports of rerecordReports; another protocol's value-drift hint is
// TestValueDriftHintNamesOnlyANoiseBucketThatCoversIt's.
func TestHintsOutsideMockModeAreKeployTests(t *testing.T) {
	SetMockMode(true)
	SetMockMode(false)
	if got := RecordCommand(); got != "keploy record" || MockMode() {
		t.Errorf("RecordCommand outside mock mode = %q, MockMode = %v", got, MockMode())
	}
	want := map[string]string{
		"no mocks":             "No recorded mocks were available to match against for this protocol in the selected test set. Re-record the test set with 'keploy record'.",
		"body values drifted":  "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {qty: []}); otherwise re-record with 'keploy record'.",
		"other values drifted": "Only values drifted (path). If these are dynamic (timestamps, ids, tokens), add a regex of the part of path that changes under test.globalNoise.global.url in keploy.yml (e.g. url: {\"<regex>\": []}); otherwise re-record with 'keploy record'.",
		"structure changed":    "Request structure changed since recording. Re-record the test set with 'keploy record', or, if you edited its mocks, remap it with --disable-mapping --update-test-mapping.",
	}
	for name, r := range rerecordReports() {
		if r.NextSteps != want[name] {
			t.Errorf("%s: hint changed outside mock mode:\n got  %q\n want %q", name, r.NextSteps, want[name])
		}
	}
}

// A value drift's hint names keploy.yml noise only in a bucket keploy hands
// the matcher that reads that field (replay.PrepareMockNoiseConfig: header,
// body, requestbody, url), at the path keploy.yml reads it from. An HTTP
// drift no bucket covers (the method, the destination, a body that is no
// JSON) is told that no noise covers it. Of another protocol, a body field
// of MongoDB, MySQL or HBase names requestbody, which their matchers read;
// any other drift names re-recording alone and claims nothing about noise,
// in keploy test and in a mock run.
func TestValueDriftHintNamesOnlyANoiseBucketThatCoversIt(t *testing.T) {
	SetMockMode(false)
	t.Cleanup(func() { SetMockMode(false) })
	drift := func(paths ...string) []models.MockFieldDiff {
		var d []models.MockFieldDiff
		for _, p := range paths {
			d = append(d, models.MockFieldDiff{Path: p, Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"})
		}
		return d
	}
	const rerecord = "re-record with 'keploy record'."
	for _, c := range []struct {
		name     string
		protocol string
		paths    []string
		want     string
	}{
		{"a body field", ProtocolHTTP, []string{"body.qty"},
			"Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {qty: []}); otherwise " + rerecord},
		{"the path and a query value", ProtocolHTTP, []string{"path", "query.page"},
			"Only values drifted (path, query.page). If these are dynamic (timestamps, ids, tokens), add a regex of the part of path, query.page that changes under test.globalNoise.global.url in keploy.yml (e.g. url: {\"<regex>\": []}); otherwise " + rerecord},
		{"a header's value and a part of one", ProtocolHTTP, []string{"header.x-request-id", "header.authorization.signed_headers", "header.authorization.region"},
			"Only values drifted (header.x-request-id, header.authorization.signed_headers, header.authorization.region). If these are dynamic (timestamps, ids, tokens), add the headers under test.globalNoise.global.header in keploy.yml (e.g. header: {x-request-id: [], authorization: []}); otherwise " + rerecord},
		{"a body field, the path and a header", ProtocolHTTP, []string{"body.id", "path", "header.x-trace"},
			"Only values drifted (body.id, path, header.x-trace). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {id: []}), and a regex of the part of path that changes under test.globalNoise.global.url in keploy.yml (e.g. url: {\"<regex>\": []}), and the headers under test.globalNoise.global.header in keploy.yml (e.g. header: {x-trace: []}); otherwise " + rerecord},
		{"the method", ProtocolHTTP, []string{"method"},
			"Only values drifted (method). No noise keploy.yml can declare covers method: if the change is expected, " + rerecord},
		{"the destination", ProtocolHTTP, []string{"host"},
			"Only values drifted (host). No noise keploy.yml can declare covers host: if the change is expected, " + rerecord},
		{"a body that is no JSON", ProtocolHTTP, []string{"body"},
			"Only values drifted (body). No noise keploy.yml can declare covers body: if the change is expected, " + rerecord},
		{"a body field beside the method", ProtocolHTTP, []string{"body.qty", "method"},
			"Only values drifted (body.qty, method). No noise keploy.yml can declare covers method: if the change is expected, " + rerecord},
		{"a MongoDB body field", ProtocolMongo, []string{"body.cmd.filter.tag"},
			"Only values drifted (body.cmd.filter.tag). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {cmd.filter.tag: []}); otherwise " + rerecord},
		{"two MySQL body fields", ProtocolMySQL, []string{"body.parameters.0.value", "body.parameters.1.value"},
			"Only values drifted (body.parameters.0.value, body.parameters.1.value). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {parameters.0.value: [], parameters.1.value: []}); otherwise " + rerecord},
		{"an HBase body field", "HBase", []string{"body.get.row"},
			"Only values drifted (body.get.row). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {get.row: []}); otherwise " + rerecord},
		{"a MongoDB field outside the body", ProtocolMongo, []string{"header.opcode"},
			"Only values drifted (header.opcode). If the change is expected, " + rerecord},
		{"a MongoDB body field beside one outside it", ProtocolMongo, []string{"body.cmd.filter.tag", "header.opcode"},
			"Only values drifted (body.cmd.filter.tag, header.opcode). If the change is expected, " + rerecord},
		{"a body field of a protocol that reads no requestbody", ProtocolGeneric, []string{"body.qty"},
			"Only values drifted (body.qty). If the change is expected, " + rerecord},
		{"a Postgres body field", ProtocolPostgres, []string{"body.query"},
			"Only values drifted (body.query). If the change is expected, " + rerecord},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := NewReport(c.protocol, "call").WithClosest("mock-1", drift(c.paths...)).Build()
			if r.NextSteps != c.want {
				t.Errorf("hint:\n got  %q\n want %q", r.NextSteps, c.want)
			}
			if c.protocol != ProtocolHTTP && strings.Contains(r.NextSteps, "No noise") {
				t.Errorf("a %s drift's hint claims no noise covers it; this package cannot know that: %q", c.protocol, r.NextSteps)
			}
		})
	}
	// A mock run gets the same remedy, with the mock run's re-record.
	SetMockMode(true)
	for _, c := range []struct{ protocol, path, want string }{
		{ProtocolMongo, "body.cmd.filter.tag", "Only values drifted (body.cmd.filter.tag). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.global.requestbody in keploy.yml with root-relative keys (e.g. requestbody: {cmd.filter.tag: []}); otherwise re-record the mock set with 'keploy mock record'."},
		{ProtocolGeneric, "body.qty", "Only values drifted (body.qty). If the change is expected, re-record the mock set with 'keploy mock record'."},
	} {
		r := NewReport(c.protocol, "call").WithClosest("mock-1", drift(c.path)).Build()
		if r.NextSteps != c.want {
			t.Errorf("%s drift in mock mode:\n got  %q\n want %q", c.protocol, r.NextSteps, c.want)
		}
	}
}

// A body field keploy.yml declares with patterns (WithBodyNoise) is named
// with its patterns, and the hint says to widen them or record again, in
// keploy test and keploy mock alike; never to declare it. A diff that is
// no body field keeps its own hint though a body entry's key names it, and
// so does every diff of a miss whose closest mock is on another upstream.
func TestHintNamesTheDeclaredPatternsAValueIsOutside(t *testing.T) {
	t.Cleanup(func() { SetMockMode(false) })
	noise := map[string][]string{"id": {"^o-", "^p-"}}
	report := func(path string, kind models.MockFieldDiffKind) *Builder {
		return NewReport(ProtocolHTTP, "POST /find").WithDestination("api.test:443").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: path, Kind: kind, Expected: "r-1", Actual: "x-9"}}).WithBodyNoise(noise)
	}
	for _, c := range []struct {
		name  string
		mock  bool
		b     *Builder
		want  string
		never []string
	}{
		{"keploy test", false, report("body.id", models.DiffKindValueChanged),
			"body.id is noise in keploy.yml only for a value matching ^o- or ^p-, and the live value is outside it. If the change is expected, widen the pattern under test.globalNoise.global.requestbody in keploy.yml, or re-record with 'keploy record'.", nil},
		{"keploy mock", true, report("body.id", models.DiffKindMissingInLive),
			"body.id is noise in keploy.yml only for a value matching ^o- or ^p-, and the recorded value is outside it. If the change is expected, widen the pattern under test.globalNoise.global.requestbody in keploy.yml, or re-record the mock set with 'keploy mock record', or, with the dependency reachable, capture the new calls with 'keploy mock replay --on-miss record'.", nil},
		{"a query value a body entry's key names", false, report("query.id", models.DiffKindValueChanged),
			"Only values drifted (query.id).", []string{"is noise in keploy.yml only"}},
		{"a closest mock on another upstream", false, report("body.id", models.DiffKindValueChanged).WithComparedDestinations([]string{"other.test:443"}),
			"No recorded HTTP mock in the compared set targets api.test:443.", []string{"is noise in keploy.yml only"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			SetMockMode(c.mock)
			got := c.b.Build().NextSteps
			if !strings.Contains(got, c.want) {
				t.Errorf("the hint\n%s\ndoes not say\n%s", got, c.want)
			}
			for _, n := range append(c.never, "{id:") {
				if strings.Contains(got, n) {
					t.Errorf("the hint says %q:\n%s", n, got)
				}
			}
		})
	}
}
