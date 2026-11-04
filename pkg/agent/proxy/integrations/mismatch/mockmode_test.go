package mismatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	// --on-miss is acted on by the HTTP parser alone; other protocols are
	// told only to re-record. No matcher outside HTTP reads keploy.yml's
	// noise for a field that is no request-body field or header, so none is
	// offered, and the hint says so.
	for _, protocol := range []string{ProtocolGeneric, ProtocolGRPC, ProtocolMySQL, ProtocolPostgres, ProtocolMongo} {
		other := NewReport(protocol, "opaque exchange").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "payload", Kind: models.DiffKindValueChanged, Expected: "a", Actual: "b"}}).Build()
		if want := "Only values drifted (payload). keploy's " + protocol + " mock matcher reads no keploy.yml noise for payload. If the change is expected, re-record the mock set with 'keploy mock record'."; other.NextSteps != want {
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
// query, an Authorization of any scheme or a part of a SigV4 one) is offered
// only recording again.
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
	// series is n diffs of a kind, their paths prefix and a number from 00.
	series := func(of func(string) models.MockFieldDiff, prefix string, n int) []models.MockFieldDiff {
		out := make([]models.MockFieldDiff, n)
		for i := range out {
			out[i] = of(fmt.Sprintf("%s%02d", prefix, i))
		}
		return out
	}
	// listed is how a hint lists n names, each prefix and a number from 00:
	// the first maxFieldDiffs (as many as a report carries), then how many
	// more.
	listed := func(prefix string, n int) string {
		var names []string
		for i := 0; i < n && i < maxFieldDiffs; i++ {
			names = append(names, fmt.Sprintf("%s%02d", prefix, i))
		}
		if n > maxFieldDiffs {
			return strings.Join(names, ", ") + fmt.Sprintf(", and %d more", n-maxFieldDiffs)
		}
		return strings.Join(names, ", ")
	}
	// example is how a hint shows a keploy.yml noise map of n keys, each
	// prefix and a number from 00, each with an empty list.
	example := func(prefix string, n int) string {
		var entries []string
		for i := 0; i < n; i++ {
			entries = append(entries, fmt.Sprintf("%s%02d: []", prefix, i))
		}
		return "{" + strings.Join(entries, ", ") + "}"
	}
	// lengths is n misses of path under entry, each a live value of
	// another length, from 2 bytes.
	lengths := func(path, entry string, n int) []PatternMiss {
		out := make([]PatternMiss, n)
		for i := range out {
			out[i] = PatternMiss{Path: path, Entry: entry, Patterns: []string{"^1$"}, Live: ShapesOf(fmt.Sprintf("a string of %d bytes", i+2))}
		}
		return out
	}
	const (
		bodyQty  = "the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {qty: []})"
		referer  = "the headers under test.globalNoise.global.header (e.g. header: {referer: []})"
		dynamic  = "If these are dynamic (timestamps, ids, tokens), add "
		expected = "If the change is expected, "
	)
	// want is the hint with %[1]s for the way to record again; structural
	// cases want the structure-change hint. on, when set, is the hint under
	// the proven switch (want and structural are then the switch-off ones).
	// guarded, misses and unreachable are what the parser names
	// WithGuarded, WithPatternMisses and WithoutNoise.
	cases := []struct {
		name                 string
		protocol             string
		diffs                []models.MockFieldDiff
		guarded, unreachable []string
		misses               []PatternMiss
		want, on             string
		structural           bool
		onStructural         bool // under the switch, the structure-change hint
	}{
		{name: "a header and a body field", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.referer"), drift("body.qty")},
			want: "Only values drifted (header.referer, body.qty). " + dynamic + bodyQty + " and " + referer + "; otherwise %[1]s."},
		{name: "a header in two spellings, named once", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.X-Request-Id"), drift("header.x-request-id")},
			want: "Only values drifted (header.X-Request-Id, header.x-request-id). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {x-request-id: []}); otherwise %[1]s."},
		{name: "two headers", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.x-id"), drift("header.referer")},
			want: "Only values drifted (header.x-id, header.referer). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {x-id: [], referer: []}); otherwise %[1]s."},
		// A dotted header name with a part that may be a value is no name to
		// declare: the sanitizer would not shape it in the example.
		{name: "a dotted header name with a part that may be a value", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.x.4815162342")},
			want: "Only values drifted (header.x.4815162342). " + expected + "%[1]s."},
		{name: "a dotted header name stays whole", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.x.trace.id")},
			want: "Only values drifted (header.x.trace.id). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: {x.trace.id: []}); otherwise %[1]s."},
		// The parts the SigV4 differ compares, so that an Authorization may
		// differ only in its access key id, date and signature: declaring
		// authorization would stop that comparison.
		{name: "a SigV4 part", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization.region")},
			want: "Only values drifted (header.authorization.region). " + expected + "%[1]s."},
		{name: "a SigV4 part beside a header", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization.signed_headers"), drift("header.referer")},
			want: "Only values drifted (header.authorization.signed_headers, header.referer). " + expected + "%[1]s."},
		// Nor is an Authorization of any scheme, in any case of its name:
		// keploy.yml's header noise reaches every recording, the signed
		// ones among them, and the parser cannot tell that none is signed.
		{name: "an Authorization", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.Authorization")},
			want: "Only values drifted (header.Authorization). " + expected + "%[1]s."},
		{name: "an Authorization beside a header", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.authorization"), drift("header.referer")},
			want: "Only values drifted (header.authorization, header.referer). " + expected + "%[1]s."},
		{name: "a recorded Authorization missing beside a recorded header missing", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.AUTHORIZATION"), missing("header.Referer")}, structural: true},
		{name: "a header and the path", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.referer"), drift("path")},
			want: "Only values drifted (header.referer, path). " + expected + "%[1]s."},
		{name: "a body field and a query parameter", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty"), drift("query.page")},
			want: "Only values drifted (body.qty, query.page). " + expected + "%[1]s."},
		{name: "the method", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("method")},
			want: "Only values drifted (method). " + expected + "%[1]s."},
		{name: "a whole body", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body")},
			want: "Only values drifted (body). " + expected + "%[1]s."},
		// Outside HTTP no matcher reads keploy.yml's noise for a header but
		// gRPC's and HTTP/2's, and neither reports one: none is offered, and
		// none is said to be unread.
		{name: "a header path outside HTTP", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{drift("header.referer")},
			want: "Only values drifted (header.referer). " + expected + "%[1]s."},
		{name: "a body field and another outside HTTP", protocol: ProtocolGRPC, diffs: []models.MockFieldDiff{drift("body.qty"), drift("payload")},
			want: "Only values drifted (body.qty, payload). keploy's gRPC mock matcher reads no keploy.yml noise for body.qty, payload. " + expected + "%[1]s."},
		// A request-body field is offered under the bucket its protocol's
		// matcher reads, one row per protocol: requestbody for HTTP, MySQL,
		// MongoDB and HBase, body for Pulsar, and none for the rest.
		{name: "a body field of an HTTP call", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s."},
		{name: "a body field of a MySQL call", protocol: ProtocolMySQL, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s."},
		{name: "a body field of a MongoDB call", protocol: ProtocolMongo, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s."},
		{name: "a body field of an HBase call", protocol: ProtocolHBase, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s."},
		{name: "a body field of a Pulsar call", protocol: ProtocolPulsar, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + "the message payload fields under test.globalNoise.global.body with root-relative keys (e.g. body: {qty: []}), the bucket that also stops keploy test asserting those fields in HTTP response bodies; otherwise %[1]s."},
		{name: "a body field of a gRPC call", protocol: ProtocolGRPC, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's gRPC mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of an HTTP/2 call", protocol: ProtocolHTTP2, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's HTTP/2 mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a PostgreSQL call", protocol: ProtocolPostgres, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's PostgreSQL mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a Redis call", protocol: ProtocolRedis, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Redis mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a Generic call", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Generic mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a DNS call", protocol: ProtocolDNS, diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's DNS mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		// Enterprise parsers label their reports with these protocols; their
		// matchers read no keploy.yml noise.
		{name: "a body field of a Kafka call", protocol: "Kafka", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Kafka mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of an Aerospike call", protocol: "Aerospike", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Aerospike mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a Couchbase call", protocol: "Couchbase", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Couchbase mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a Memcached call", protocol: "Memcached", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's Memcached mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of an SQS call", protocol: "SQS", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's SQS mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a body field of a ZooKeeper call", protocol: "ZooKeeper", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). keploy's ZooKeeper mock matcher reads no keploy.yml noise for body.qty. " + expected + "%[1]s."},
		{name: "a whole body of a PostgreSQL call", protocol: ProtocolPostgres, diffs: []models.MockFieldDiff{drift("body")},
			want: "Only values drifted (body). keploy's PostgreSQL mock matcher reads no keploy.yml noise for body. " + expected + "%[1]s."},
		// A whole body is no field a requestbody entry names.
		{name: "a whole body of a MongoDB call", protocol: ProtocolMongo, diffs: []models.MockFieldDiff{drift("body")},
			want: "Only values drifted (body). " + expected + "%[1]s."},
		// A field that is no body field or header: no matcher outside HTTP
		// reads keploy.yml's noise for it.
		{name: "a bind parameter of a PostgreSQL call", protocol: ProtocolPostgres, diffs: []models.MockFieldDiff{drift("bind.$1")},
			want: "Only values drifted (bind.$1). keploy's PostgreSQL mock matcher reads no keploy.yml noise for bind.$1. " + expected + "%[1]s."},
		// No shipped MongoDB parser names a field outside its body (its
		// report's diffs are all "body."-prefixed): the row pins the builder
		// for a parser that would.
		{name: "a body field and another of a MongoDB call", protocol: ProtocolMongo, diffs: []models.MockFieldDiff{drift("body.qty"), drift("collection")},
			want: "Only values drifted (body.qty, collection). keploy's MongoDB mock matcher reads no keploy.yml noise for collection. " + expected + "%[1]s."},
		{name: "a body field and a header of a MongoDB call", protocol: ProtocolMongo, diffs: []models.MockFieldDiff{drift("body.qty"), drift("header.x")},
			want: "Only values drifted (body.qty, header.x). " + expected + "%[1]s."},
		{name: "a body field of a call of no protocol", diffs: []models.MockFieldDiff{drift("body.qty")},
			want: "Only values drifted (body.qty). " + expected + "%[1]s."},
		// A recorded header the call does not carry: the HTTP matcher
		// requires it unless keploy.yml declares it noise.
		{name: "a recorded header missing", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer")},
			want: "Recorded headers are missing from this call (header.Referer). If they are optional, add " + referer + "; otherwise %[1]s."},
		{name: "a missing header beside drifted values", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), drift("body.qty"), drift("header.x-request-id")},
			want: "Recorded headers are missing from this call (header.Referer) and values drifted (body.qty, header.x-request-id). If these are optional or dynamic (timestamps, ids, tokens), add " + bodyQty + " and the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-request-id: []}); otherwise %[1]s."},
		{name: "a missing header beside a path drift", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), drift("path")}, structural: true},
		// Without the switch the HTTP matcher lets a call carry headers its
		// recording does not, so such a header is no cause and no remedy;
		// under the switch every header is compared (except host,
		// content-length and keploy* headers and those keploy's own or
		// keploy.yml's header noise names), and noise reaches it.
		{name: "a header only the call carries", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{added("header.X-New")}, structural: true,
			on: "This call carries headers its recording does not (header.X-New). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {x-new: []}); otherwise %[1]s."},
		{name: "a recorded header missing and one only the call carries", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), added("header.X-New")},
			want: "Recorded headers are missing from this call (header.Referer). If they are optional, add " + referer + "; otherwise %[1]s.",
			on:   "Recorded headers are missing from this call (header.Referer) and it carries headers its recording does not (header.X-New). If they are optional, add the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-new: []}); otherwise %[1]s."},
		{name: "a header only the call carries beside a drifted value", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{added("header.X-New"), drift("body.qty")},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s.",
			on:   "This call carries headers its recording does not (header.X-New) and values drifted (body.qty). If these are optional or dynamic (timestamps, ids, tokens), add " + bodyQty + " and the headers under test.globalNoise.global.header (e.g. header: {x-new: []}); otherwise %[1]s."},
		{name: "headers missing, carried and drifted", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer"), added("header.X-New"), drift("header.x-request-id")},
			want: "Recorded headers are missing from this call (header.Referer) and values drifted (header.x-request-id). If these are optional or dynamic (timestamps, ids, tokens), add the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-request-id: []}); otherwise %[1]s.",
			on:   "Recorded headers are missing from this call (header.Referer), it carries headers its recording does not (header.X-New) and values drifted (header.x-request-id). If these are optional or dynamic (timestamps, ids, tokens), add the headers under test.globalNoise.global.header (e.g. header: {referer: [], x-new: [], x-request-id: []}); otherwise %[1]s."},
		{name: "a body field only the call carries", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{added("body.new"), drift("body.qty")}, structural: true},
		{name: "a header only the call carries outside HTTP", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{added("header.X-New"), drift("payload")}, structural: true},
		// A field the parser names WithoutNoise (an x-amz-content-sha256, a
		// Content-Type the schema phase refuses) is in no bucket's reach, in
		// any case of its name.
		{name: "a missing field offered no noise", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.X-Amz-Content-Sha256")}, unreachable: []string{"header.x-amz-content-sha256"}, structural: true},
		{name: "a drifted field offered no noise", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.X-Amz-Content-Sha256")}, unreachable: []string{"header.x-amz-content-sha256"},
			want: "Only values drifted (header.X-Amz-Content-Sha256). " + expected + "%[1]s."},
		{name: "a field offered no noise beside one in reach", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.Content-Type"), drift("body.qty")}, unreachable: []string{"header.Content-Type"},
			want: "Only values drifted (header.Content-Type, body.qty). " + expected + "%[1]s."},
		// A field keploy.yml already declares with patterns has its patterns
		// widened, never added or emptied: an entry with no patterns would
		// stop the field being compared at all. No pattern matches an
		// absent value, so a guarded field missing on a side is offered no
		// noise.
		{name: "a guarded header missing", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer")}, guarded: []string{"header.referer"}, structural: true},
		{name: "a guarded header missing, its patterns named", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("header.Referer")},
			misses: []PatternMiss{{Path: "header.referer", Patterns: []string{"^https://a/"}, Live: ShapesOf("absent")}}, structural: true},
		{name: "a guarded header only the call carries, beside a drifted value", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{added("header.X-New"), drift("body.qty")},
			misses: []PatternMiss{{Path: "header.x-new", Patterns: []string{"^n$"}, Recorded: ShapesOf("absent")}},
			want:   "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s.", onStructural: true},
		{name: "guarded and new fields together", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.referer"), drift("header.x-request-id"), drift("body.qty"), drift("body.id")}, guarded: []string{"header.Referer", "body.qty"},
			want: "Only values drifted (header.referer, header.x-request-id, body.qty, body.id). " + dynamic + "the request-body fields under test.globalNoise.global.requestbody with root-relative keys (e.g. requestbody: {id: []}) and the headers under test.globalNoise.global.header (e.g. header: {x-request-id: []}) and widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty so they match the recorded and the live value and widen the patterns of keploy.yml's test.globalNoise header entries for referer so they match the recorded and the live value; otherwise %[1]s."},
		// A parser that names the patterns and the side: the hint names
		// them, and that value by its shape, never its text.
		{name: "a body field whose live value is outside its patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{`^\d{3}$`}, Live: ShapesOf("a number of 4 characters")}},
			want:   "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^\\\\d{3}$\") so one matches the live value (a number of 4 characters); otherwise %[1]s."},
		{name: "a header whose recorded value is outside its two patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.X-Request-Id")},
			misses: []PatternMiss{{Path: "header.x-request-id", Patterns: []string{"^req-", "^r-"}, Recorded: ShapesOf("a string of 9 bytes")}},
			want:   "Only values drifted (header.X-Request-Id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise header entries for x-request-id (\"^req-\", \"^r-\") so one matches the recorded value (a string of 9 bytes); otherwise %[1]s."},
		{name: "two guarded body fields beside a new header", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty"), drift("body.id"), drift("header.referer")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.id", Patterns: []string{"^a"}, Recorded: ShapesOf("a string of 3 bytes")}},
			want:   "Only values drifted (body.qty, body.id, header.referer). " + dynamic + referer + " and widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^1$\") so one matches the live value (a number of 2 characters) and for id (\"^a\") so one matches the recorded value (a string of 3 bytes); otherwise %[1]s."},
		// Both sides outside the patterns: widened to match one side's
		// value alone, they would leave the call a miss, so both are named.
		{name: "a body field whose two values are outside its patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters"), Recorded: ShapesOf("a number of 3 characters")}},
			want:   "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^1$\") so they match the live value (a number of 2 characters) and the recorded value (a number of 3 characters); otherwise %[1]s."},
		{name: "a form field with two live values outside its patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.code")},
			misses: []PatternMiss{{Path: "body.code", Patterns: []string{`^c\d$`}, Live: ShapesOf("a string of 3 bytes", "a string of 4 bytes")}},
			want:   "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live values (a string of 3 bytes, a string of 4 bytes); otherwise %[1]s."},
		// The hint names the entry that holds the patterns, which may name
		// the field by its last name alone: the user edits that entry.
		{name: "a nested body field an entry names by its last name", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.order.qty")},
			misses: []PatternMiss{{Path: "body.order.qty", Entry: "qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}},
			want:   "Only values drifted (body.order.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^1$\") so one matches the live value (a number of 2 characters); otherwise %[1]s."},
		// Two fields one entry guards: its patterns are to match the values
		// of each.
		{name: "two body fields one entry guards", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.order.qty"), drift("body.item.qty")},
			misses: []PatternMiss{{Path: "body.order.qty", Entry: "qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.item.qty", Entry: "qty", Patterns: []string{"^1$"}, Recorded: ShapesOf("a number of 3 characters")}},
			want:   "Only values drifted (body.order.qty, body.item.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^1$\") so they match the live value (a number of 2 characters) and the recorded value (a number of 3 characters); otherwise %[1]s."},
		// A field named twice (an array's elements share a path), and named
		// so by the diffs: its patterns are its first naming's, and are to
		// match the values of every naming, each named once.
		{name: "a field named twice", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.ids[]"), drift("body.ids[]")},
			misses: []PatternMiss{{Path: "body.ids[]", Entry: "ids", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.ids[]", Entry: "ids", Patterns: []string{"^2$"}, Live: ShapesOf("a number of 3 characters")}},
			want:   "Only values drifted (body.ids[], body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^1$\") so they match the live values (a number of 2 characters, a number of 3 characters); otherwise %[1]s."},
		{name: "a field named twice, each naming a side", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.ids[]")},
			misses: []PatternMiss{{Path: "body.ids[]", Entry: "ids", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.ids[]", Entry: "ids", Patterns: []string{"^1$"}, Recorded: ShapesOf("a number of 3 characters")}},
			want:   "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^1$\") so they match the live value (a number of 2 characters) and the recorded value (a number of 3 characters); otherwise %[1]s."},
		// However many values miss, the hint names each distinct shape of a
		// side once, the first three, and counts the values past them
		// (TestTheHintStaysSmallHoweverManyValuesMiss).
		{name: "a field named many times, each a value of another shape", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.ids[]")},
			misses: lengths("body.ids[]", "ids", 3000),
			want:   "Only values drifted (body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^1$\") so they match the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 2997 more); otherwise %[1]s."},
		{name: "a field named twice, a value of one shape each", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.ids[]"), drift("body.ids[]")},
			misses: []PatternMiss{{Path: "body.ids[]", Entry: "ids", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.ids[]", Entry: "ids", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}},
			want:   "Only values drifted (body.ids[], body.ids[]). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for ids (\"^1$\") so they match the live values (a number of 2 characters); otherwise %[1]s."},
		{name: "a form field with many values of one shape", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.code")},
			misses: []PatternMiss{{Path: "body.code", Patterns: []string{`^c\d$`}, Live: ShapesOf(slices.Repeat([]string{"a string of 3 bytes"}, 3000)...), Recorded: ShapesOf("a string of 4 bytes")}},
			want:   "Only values drifted (body.code). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for code (\"^c\\\\d$\") so they match the live values (a string of 3 bytes) and the recorded value (a string of 4 bytes); otherwise %[1]s."},
		// Array elements that hold containers share a path: the hint says
		// why it offers no noise for it once.
		{name: "array elements guarded with patterns that hold containers", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.rows[]"), drift("body.rows[]")},
			misses: []PatternMiss{{Path: "body.rows[]", Patterns: []string{"^1$"}, Live: ShapesOf("an array"), Container: true}, {Path: "body.rows[]", Patterns: []string{"^1$"}, Live: ShapesOf("an object"), Container: true}},
			want:   "Only values drifted (body.rows[], body.rows[]). keploy.yml's noise lets body.rows[] vary only within \"^1$\", and a pattern matches a single value, never an array or an object. " + expected + "%[1]s."},
		// Two fields one entry guards, past the shapes a hint names: the
		// second field's values are counted with the first's.
		{name: "two fields one entry guards, past the shapes a hint names", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.order.qty"), drift("body.item.qty")},
			misses: []PatternMiss{
				{Path: "body.order.qty", Entry: "qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters", "a number of 3 characters", "a number of 4 characters", "a number of 5 characters")},
				{Path: "body.item.qty", Entry: "qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 3 characters", "a number of 6 characters", "a number of 6 characters")},
			},
			want: "Only values drifted (body.order.qty, body.item.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty (\"^1$\") so they match the live values (a number of 2 characters, a number of 3 characters, a number of 4 characters and 3 more); otherwise %[1]s."},
		// A pattern miss with no patterns, or no value of either side,
		// names what it can: both values.
		{name: "a pattern miss naming no patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")},
			misses: []PatternMiss{{Path: "body.qty", Live: ShapesOf("a number of 2 characters")}},
			want:   "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty so they match the recorded and the live value; otherwise %[1]s."},
		{name: "a pattern miss naming no value", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{"^1$"}}},
			want:   "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for qty so they match the recorded and the live value; otherwise %[1]s."},
		// A pattern matches a single value: for an array or an object no
		// widening serves, so the hint offers recording again and says why.
		{name: "a guarded array whose length changed", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.items")},
			misses: []PatternMiss{{Path: "body.items", Patterns: []string{`^\d+$`}, Live: ShapesOf("an array"), Recorded: ShapesOf("an array"), Container: true}},
			want:   "Only values drifted (body.items). keploy.yml's noise lets body.items vary only within \"^\\\\d+$\", and a pattern matches a single value, never an array or an object. " + expected + "%[1]s."},
		{name: "a guarded array beside a plain field", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty"), drift("body.items")},
			misses: []PatternMiss{{Path: "body.items", Patterns: []string{"^1$"}, Live: ShapesOf("an array"), Container: true}},
			want:   "Only values drifted (body.qty, body.items). keploy.yml's noise lets body.items vary only within \"^1$\", and a pattern matches a single value, never an array or an object. " + expected + "%[1]s."},
		// A field named twice is a container if either naming says so.
		{name: "a guarded array named twice, the second a container", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.items")},
			misses: []PatternMiss{{Path: "body.items", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}, {Path: "body.items", Patterns: []string{"^1$"}, Live: ShapesOf("an array"), Container: true}},
			want:   "Only values drifted (body.items). keploy.yml's noise lets body.items vary only within \"^1$\", and a pattern matches a single value, never an array or an object. " + expected + "%[1]s."},
		{name: "a guarded array named twice, the first a container", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.items")},
			misses: []PatternMiss{{Path: "body.items", Patterns: []string{"^1$"}, Live: ShapesOf("an array"), Container: true}, {Path: "body.items", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}},
			want:   "Only values drifted (body.items). keploy.yml's noise lets body.items vary only within \"^1$\", and a pattern matches a single value, never an array or an object. " + expected + "%[1]s."},
		// A scalar field outside its patterns, offered no noise for another
		// field's sake, is no container.
		{name: "a guarded scalar beside a field offered no noise", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty"), drift("path")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{"^1$"}, Live: ShapesOf("a number of 2 characters")}},
			want:   "Only values drifted (body.qty, path). " + expected + "%[1]s."},
		// A header is named by its lowercased name, as the HTTP matcher
		// reads it.
		{name: "a guarded header its parser names in another case", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("header.X-Request-Id")},
			misses: []PatternMiss{{Path: "header.X-Request-Id", Patterns: []string{"^r-"}, Live: ShapesOf("a string of 4 bytes")}},
			want:   "Only values drifted (header.X-Request-Id). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise header entries for x-request-id (\"^r-\") so one matches the live value (a string of 4 bytes); otherwise %[1]s."},
		{name: "a container naming no patterns", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.items")},
			misses: []PatternMiss{{Path: "body.items", Live: ShapesOf("an array"), Container: true}},
			want:   "Only values drifted (body.items). " + expected + "%[1]s."},
		// Pulsar's matcher reads its message payload's noise under body.
		{name: "a Pulsar body field whose live value is outside its patterns", protocol: ProtocolPulsar, diffs: []models.MockFieldDiff{drift("body.qty")},
			misses: []PatternMiss{{Path: "body.qty", Patterns: []string{`^\d{3}$`}, Live: ShapesOf("a number of 4 characters")}},
			want:   "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise body entries for qty (\"^\\\\d{3}$\") so one matches the live value (a number of 4 characters); otherwise %[1]s."},
		// MongoDB's matcher reads requestbody's patterns under the switch.
		{name: "a MongoDB body field whose live value is outside its patterns", protocol: ProtocolMongo, diffs: []models.MockFieldDiff{drift("body.filter.status")},
			misses: []PatternMiss{{Path: "body.filter.status", Patterns: []string{"^open$"}, Live: ShapesOf("a string of 6 bytes")}},
			want:   "Only values drifted (body.filter.status). If these are dynamic (timestamps, ids, tokens), widen the patterns of keploy.yml's test.globalNoise requestbody entries for filter.status (\"^open$\") so one matches the live value (a string of 6 bytes); otherwise %[1]s."},
		{name: "a body field's key keeps its case", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{drift("body.qty")}, guarded: []string{"body.QTY"},
			want: "Only values drifted (body.qty). " + dynamic + bodyQty + "; otherwise %[1]s."},
		{name: "a missing body field", protocol: ProtocolHTTP, diffs: []models.MockFieldDiff{missing("body.qty")}, structural: true},
		{name: "a missing header outside HTTP", protocol: ProtocolGeneric, diffs: []models.MockFieldDiff{missing("header.Referer")}, structural: true},
		// The hint decides from every diff, not only the maxFieldDiffs a
		// report carries: a field out of every bucket's reach past them
		// keeps noise out of the hint. It names only the fields the report
		// carries, and counts the rest; so it offers noise only when the
		// report carries every cause, as it could not name the rest of the
		// remedy.
		{name: "a path drift past the diffs a report carries", protocol: ProtocolHTTP, diffs: slices.Concat(series(drift, "header.x-h", maxFieldDiffs), []models.MockFieldDiff{drift("path")}),
			want: "Only values drifted (" + listed("header.x-h", maxFieldDiffs) + ", and 1 more). " + expected + "%[1]s."},
		// A difference past them that is no value drift: not "only values".
		{name: "a recorded header missing past the diffs a report carries", protocol: ProtocolHTTP,
			diffs: slices.Concat(series(drift, "header.x-h", maxFieldDiffs), []models.MockFieldDiff{missing("header.Referer")}), structural: true},
		{name: "a field offered no noise behind headers only the call carries, beside a recorded header missing", protocol: ProtocolHTTP,
			diffs:       slices.Concat([]models.MockFieldDiff{missing("header.Referer")}, series(added, "header.X-H", maxFieldDiffs-1), []models.MockFieldDiff{drift("header.Content-Type")}),
			unreachable: []string{"header.Content-Type"}, structural: true},
		{name: "more body fields in reach than a report carries", protocol: ProtocolHTTP, diffs: series(drift, "body.f", 30),
			want: "Only values drifted (" + listed("body.f", 30) + "). " + expected + "%[1]s."},
		{name: "more headers in reach missing and drifted than a report carries", protocol: ProtocolHTTP,
			diffs: slices.Concat(series(missing, "header.M", 13), series(drift, "header.D", 13)), structural: true},
		{name: "as many drifted headers as a report carries", protocol: ProtocolHTTP, diffs: series(drift, "header.x-h", maxFieldDiffs),
			want: "Only values drifted (" + listed("header.x-h", maxFieldDiffs) + "). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: " + example("x-h", maxFieldDiffs) + "); otherwise %[1]s."},
		// Without the switch a header only the call carries is no cause, and
		// the report carries the causes first: all 25 here.
		{name: "as many drifted headers as a report carries and one only the call carries", protocol: ProtocolHTTP,
			diffs:        slices.Concat([]models.MockFieldDiff{added("header.X-New")}, series(drift, "header.x-h", maxFieldDiffs)),
			want:         "Only values drifted (" + listed("header.x-h", maxFieldDiffs) + "). " + dynamic + "the headers under test.globalNoise.global.header (e.g. header: " + example("x-h", maxFieldDiffs) + "); otherwise %[1]s.",
			onStructural: true},
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
	for _, on := range []bool{false, true} {
		t.Setenv(provenswitch.Env, map[bool]string{false: "", true: "1"}[on])
		for _, mock := range []bool{false, true} {
			SetMockMode(mock)
			t.Cleanup(func() { SetMockMode(false) })
			for _, c := range cases {
				r := NewReport(c.protocol, "GET /a").WithClosest("mock-1", c.diffs).WithGuarded(c.guarded...).WithPatternMisses(c.misses...).WithoutNoise(c.unreachable...).Build()
				want := fmt.Sprintf(c.want, rerecord(mock, c.protocol))
				switch {
				case on && c.onStructural:
					want = structure[mock]
				case on && c.on != "":
					want = fmt.Sprintf(c.on, rerecord(mock, c.protocol))
				case c.structural:
					want = structure[mock]
				}
				if r.NextSteps != want {
					t.Errorf("%s (switch %v, mock mode %v):\n got  %q\n want %q", c.name, on, mock, r.NextSteps, want)
				}
			}
		}
	}
}
