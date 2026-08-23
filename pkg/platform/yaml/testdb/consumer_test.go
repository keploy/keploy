package testdb

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// consumerFixture is design §2's on-disk example, as a TestCase: a Kafka
// consumer unit whose worker produced one message, with per-test noise on a
// payload path.
//
// Nothing in it is interpreted by this package. `coords` in particular is an
// opaque map: partition/offset/timestamp are copied through and never read.
func consumerFixture() models.TestCase {
	req := time.Date(2026, 8, 22, 10, 4, 12, 401000000, time.UTC)
	res := time.Date(2026, 8, 22, 10, 4, 12, 456000000, time.UTC)
	return models.TestCase{
		Version:     models.V1Beta1,
		Kind:        models.CONSUMER,
		Name:        "test-7",
		Description: "order confirmation worker",
		Created:     1755861852,
		Noise:       map[string][]string{"effects.0.body.processedAt": {}},
		Assertions:  map[models.AssertionType]interface{}{},
		AppPort:     0,
		ConsumerSpec: &models.ConsumerSpec{
			Protocol: "kafka",
			Trigger: models.EffectView{
				Protocol: "kafka",
				Op:       "fetch",
				Target:   "orders",
				Key:      "o-4c1",
				Headers:  map[string]string{"traceparent": "00-4bf9a1d2e3f4-01"},
				Body:     `{"orderId":"o-4c1","items":[{"sku":"SKU-9","qty":3}]}`,
				BodyType: models.JSON,
				Decoded:  models.DecodedConfident,
				Coords:   map[string]string{"partition": "0", "offset": "1840", "timestamp": "1755861852401"},
				Records:  1,
			},
			Effects: []models.EffectView{{
				Protocol: "kafka",
				Op:       "produce",
				Target:   "order-events",
				Key:      "o-4c1",
				Body:     `{"orderId":"o-4c1","status":"CONFIRMED","total":41.97}`,
				BodyType: models.JSON,
				Decoded:  models.DecodedConfident,
				Coords:   map[string]string{"partition": "0"},
				Assert:   models.AssertFull,
				Records:  1,
			}},
			// The lane discriminator a Kafka recorder stamps. OSS never
			// interprets the value; it is here so the round trip proves the
			// field persists — without it the judge silently takes the
			// STRICTER one-lane-per-(protocol, target) reading and two
			// goroutines producing to different partitions are reported as a
			// routing regression.
			OrderBy: "partition",
			// The consume-and-write marker. Without it on disk a
			// consume-to-a-database test is indistinguishable from a unit in
			// which nothing happened, and the judge refuses it as vacuous.
			SideEffects: 1,
			Completion: models.ConsumerCompletion{
				ExpectEffects: 1, GraceMs: 250, TimeoutMs: 5000,
			},
			Created:          1755861852,
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
	}
}

// assertConsumerTestCasesEqual compares the fields the storage layer is
// responsible for round-tripping. It deliberately does NOT reflect.DeepEqual
// the whole TestCase: Mocks, Curl and the transient routing fields are not
// part of the on-disk spec.
func assertConsumerTestCasesEqual(t *testing.T, want, got *models.TestCase) {
	t.Helper()
	if got.Kind != want.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	}
	if got.Name != want.Name {
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	}
	if got.Version != want.Version {
		t.Errorf("Version = %q, want %q", got.Version, want.Version)
	}
	if got.Description != want.Description {
		t.Errorf("Description = %q, want %q", got.Description, want.Description)
	}
	if got.Created != want.Created {
		t.Errorf("Created = %d, want %d", got.Created, want.Created)
	}
	if got.AppPort != want.AppPort {
		t.Errorf("AppPort = %d, want %d", got.AppPort, want.AppPort)
	}
	if !reflect.DeepEqual(got.Noise, want.Noise) {
		t.Errorf("Noise = %#v, want %#v", got.Noise, want.Noise)
	}
	if got.ConsumerSpec == nil {
		t.Fatal("ConsumerSpec is nil after decode — the whole test case is gone")
	}
	// Timestamps must survive as instants, not as formatted strings.
	if !got.ConsumerSpec.ReqTimestampMock.Equal(want.ConsumerSpec.ReqTimestampMock) {
		t.Errorf("ReqTimestampMock = %v, want %v", got.ConsumerSpec.ReqTimestampMock, want.ConsumerSpec.ReqTimestampMock)
	}
	if !got.ConsumerSpec.ResTimestampMock.Equal(want.ConsumerSpec.ResTimestampMock) {
		t.Errorf("ResTimestampMock = %v, want %v", got.ConsumerSpec.ResTimestampMock, want.ConsumerSpec.ResTimestampMock)
	}
	gotSpec, wantSpec := *got.ConsumerSpec, *want.ConsumerSpec
	gotSpec.ReqTimestampMock, wantSpec.ReqTimestampMock = time.Time{}, time.Time{}
	gotSpec.ResTimestampMock, wantSpec.ResTimestampMock = time.Time{}, time.Time{}
	// Assertions and Metadata are lifted onto the TestCase by Decode and
	// rebuilt by Encode, so the decoded spec carries neither.
	wantSpec.Assertions, wantSpec.Metadata = nil, nil
	if !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Errorf("ConsumerSpec mismatch\n got: %#v\nwant: %#v", gotSpec, wantSpec)
	}
}

// A consumer test case must survive encode -> bytes -> decode -> encode with
// the second encoding byte-identical to the first. Anything less means an
// artifact that mutates every time keploy touches it (which is what
// --update-test-mapping and `keploy normalize` do).
func TestConsumerTestCaseRoundTripsYAML(t *testing.T) {
	log := goldenLogger(t)
	fixture := consumerFixture()

	doc, err := EncodeTestcase(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	if doc.Kind != models.CONSUMER {
		t.Fatalf("doc.Kind = %q, want %q", doc.Kind, models.CONSUMER)
	}
	if doc.Curl != "" {
		t.Errorf("doc.Curl = %q — a consumer test case has no HTTP request", doc.Curl)
	}
	first, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	// The document must actually carry the model, not an empty spec.
	for _, want := range []string{"kind: Consumer", "protocol: kafka", "order-events", "expectEffects: 1", "effects.0.body.processedAt"} {
		if !strings.Contains(string(first), want) {
			t.Errorf("encoded document is missing %q:\n%s", want, first)
		}
	}

	back, err := yaml.UnmarshalDoc(yaml.FormatYAML, first)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err := Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	assertConsumerTestCasesEqual(t, &fixture, decoded)

	again, err := EncodeTestcase(*decoded, log)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	second, err := yaml.MarshalDoc(yaml.FormatYAML, again)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("consumer test case is not stable under re-encode.\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestConsumerTestCaseRoundTripsJSON(t *testing.T) {
	log := goldenLogger(t)
	fixture := consumerFixture()

	doc, err := EncodeTestcaseJSON(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcaseJSON: %v", err)
	}
	if doc.Curl != "" {
		t.Errorf("doc.Curl = %q — a consumer test case has no HTTP request", doc.Curl)
	}
	first, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back yaml.NetworkTrafficDocJSON
	if err := json.Unmarshal(first, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	decoded, err := DecodeJSON(&back, log)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	assertConsumerTestCasesEqual(t, &fixture, decoded)

	again, err := EncodeTestcaseJSON(*decoded, log)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	second, err := json.MarshalIndent(again, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(second) != string(first) {
		t.Fatalf("consumer test case is not stable under re-encode (json).\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// The two storage formats must describe the same test case. A field that
// round-trips through YAML but is dropped by JSON (or vice versa) would make a
// suite's meaning depend on config.StorageFormat.
func TestConsumerYAMLAndJSONDecodeToTheSameTestCase(t *testing.T) {
	log := goldenLogger(t)
	fixture := consumerFixture()

	ydoc, err := EncodeTestcase(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	ybytes, err := yaml.MarshalDoc(yaml.FormatYAML, ydoc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	yback, err := yaml.UnmarshalDoc(yaml.FormatYAML, ybytes)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	fromYAML, err := Decode(yback, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	jdoc, err := EncodeTestcaseJSON(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcaseJSON: %v", err)
	}
	jbytes, err := json.Marshal(jdoc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var jback yaml.NetworkTrafficDocJSON
	if err := json.Unmarshal(jbytes, &jback); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fromJSON, err := DecodeJSON(&jback, log)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}

	if !reflect.DeepEqual(fromYAML.ConsumerSpec, fromJSON.ConsumerSpec) {
		t.Errorf("the two formats disagree.\nyaml: %#v\njson: %#v", fromYAML.ConsumerSpec, fromJSON.ConsumerSpec)
	}
	if !reflect.DeepEqual(fromYAML.Noise, fromJSON.Noise) {
		t.Errorf("noise differs.\nyaml: %#v\njson: %#v", fromYAML.Noise, fromJSON.Noise)
	}
}

// Every switch the consumer Kind has to pass through, driven end to end.
// Before this slice each of these fell to a `default:` that either rejected
// the test case outright or produced a garbage value; the test exists so a
// forgotten arm is a failure here rather than a silently broken artifact.
func TestConsumerKindReachesNoUnguardedDefault(t *testing.T) {
	log := goldenLogger(t)
	fixture := consumerFixture()

	t.Run("EncodeTestcase", func(t *testing.T) {
		if _, err := EncodeTestcase(fixture, log); err != nil {
			t.Fatalf("hit the default reject: %v", err)
		}
	})
	t.Run("EncodeTestcaseJSON", func(t *testing.T) {
		if _, err := EncodeTestcaseJSON(fixture, log); err != nil {
			t.Fatalf("hit the default reject: %v", err)
		}
	})
	t.Run("Decode", func(t *testing.T) {
		doc, err := EncodeTestcase(fixture, log)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := Decode(doc, log); err != nil {
			t.Fatalf("hit the default reject: %v", err)
		}
	})
	t.Run("DecodeJSON", func(t *testing.T) {
		doc, err := EncodeTestcaseJSON(fixture, log)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := DecodeJSON(doc, log); err != nil {
			t.Fatalf("hit the default reject: %v", err)
		}
	})
	t.Run("BuildTestCaseSlug", func(t *testing.T) {
		got := BuildTestCaseSlug(&fixture)
		if got != "kafka-orders" {
			t.Errorf("BuildTestCaseSlug = %q, want %q", got, "kafka-orders")
		}
		// The default branch's HTTP-first fallback would have produced
		// this instead, which is what the arm exists to prevent.
		if got == fallbackTC+"-consumer" {
			t.Error("fell through to the kind-tagged fallback")
		}
	})
	t.Run("BuildTestCaseSlug/degenerate", func(t *testing.T) {
		bare := models.TestCase{Kind: models.CONSUMER}
		if got := BuildTestCaseSlug(&bare); got != "consumer" {
			t.Errorf("a consumer test case with no spec must still get a stable slug, got %q", got)
		}
		onlyProtocol := models.TestCase{Kind: models.CONSUMER, ConsumerSpec: &models.ConsumerSpec{Protocol: "pulsar"}}
		if got := BuildTestCaseSlug(&onlyProtocol); got != "pulsar" {
			t.Errorf("slug with no target = %q, want %q", got, "pulsar")
		}
	})
}

// InsertTestCase stamps pkg.MakeCurlCommand(tc.HTTPReq) unconditionally.
// A consumer test case has an entirely empty HTTPReq, so without the guard
// every consumer artifact would ship the literal string
// `curl --request  --url `.
func TestInsertTestCaseDoesNotStampACurlOnAConsumerTest(t *testing.T) {
	dir := t.TempDir()
	db := New(goldenLogger(t), dir)

	tc := consumerFixture()
	if err := db.InsertTestCase(context.Background(), &tc, "test-set-0", false); err != nil {
		t.Fatalf("InsertTestCase: %v", err)
	}
	if tc.Curl != "" {
		t.Errorf("Curl = %q, want empty", tc.Curl)
	}

	path := filepath.Join(dir, "test-set-0", "tests", "test-7.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.Contains(string(raw), "curl") {
		t.Errorf("the persisted artifact carries a curl command:\n%s", raw)
	}

	// And the HTTP path must be untouched.
	httpTC := httpFixture()
	httpTC.Name = "test-1"
	httpTC.Curl = ""
	if err := db.InsertTestCase(context.Background(), &httpTC, "test-set-0", false); err != nil {
		t.Fatalf("InsertTestCase(http): %v", err)
	}
	if !strings.HasPrefix(httpTC.Curl, "curl") {
		t.Errorf("HTTP test cases must still be stamped with a curl, got %q", httpTC.Curl)
	}
}

// Reading a recorded set back must order consumer tests by their recorded
// window. Without the sort arm every consumer test compares equal on both
// timestamps and the set falls through to extractTestNumber(name), which only
// understands "test-N" — so a descriptively named set would replay in
// directory order.
func TestGetTestCasesOrdersConsumerTestsByTheirRecordedWindow(t *testing.T) {
	dir := t.TempDir()
	db := New(goldenLogger(t), dir)
	base := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

	// Inserted deliberately out of order, with names that carry no number
	// the legacy tie-break could use.
	for _, spec := range []struct {
		name   string
		offset time.Duration
	}{
		{"kafka-orders-c", 3 * time.Second},
		{"kafka-orders-a", 1 * time.Second},
		{"kafka-orders-b", 2 * time.Second},
	} {
		tc := consumerFixture()
		tc.Name = spec.name
		tc.ConsumerSpec.ReqTimestampMock = base.Add(spec.offset)
		tc.ConsumerSpec.ResTimestampMock = base.Add(spec.offset + 50*time.Millisecond)
		if err := db.InsertTestCase(context.Background(), &tc, "test-set-0", false); err != nil {
			t.Fatalf("InsertTestCase(%s): %v", spec.name, err)
		}
	}

	got, err := db.GetTestCases(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("GetTestCases: %v", err)
	}
	var names []string
	for _, tc := range got {
		names = append(names, tc.Name)
	}
	want := []string{"kafka-orders-a", "kafka-orders-b", "kafka-orders-c"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
}

// Encoding must FAIL LOUD rather than write a test case that can only ever
// pass. A Kind: Consumer document with no spec has no trigger, no effects and
// no completion rule; one with no protocol cannot be routed to a deliverer, a
// projector or a comparison group.
func TestEncodingAnUnusableConsumerTestCaseIsRefused(t *testing.T) {
	log := goldenLogger(t)
	cases := []struct {
		name string
		tc   models.TestCase
	}{
		{
			name: "no spec at all",
			tc:   models.TestCase{Version: models.V1Beta1, Kind: models.CONSUMER, Name: "test-1"},
		},
		{
			name: "spec with no protocol on either the spec or the trigger",
			tc: models.TestCase{Version: models.V1Beta1, Kind: models.CONSUMER, Name: "test-1",
				ConsumerSpec: &models.ConsumerSpec{
					Trigger:    models.EffectView{Op: "fetch", Target: "orders"},
					Completion: models.ConsumerCompletion{ExpectEffects: 1},
				}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := EncodeTestcase(c.tc, log); err == nil {
				t.Error("EncodeTestcase accepted it")
			}
			if _, err := EncodeTestcaseJSON(c.tc, log); err == nil {
				t.Error("EncodeTestcaseJSON accepted it")
			}
		})
	}
}

// A spec that omits the denormalised Protocol but carries it on the trigger is
// REPAIRED, not refused — the two are the same fact and a producer that fills
// only one should not lose its recording.
func TestConsumerProtocolIsRepairedFromTheTrigger(t *testing.T) {
	log := goldenLogger(t)
	tc := consumerFixture()
	tc.ConsumerSpec.Protocol = ""

	doc, err := EncodeTestcase(tc, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	raw, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	back, err := yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err := Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.ConsumerSpec.Protocol != "kafka" {
		t.Errorf("protocol = %q, want %q", decoded.ConsumerSpec.Protocol, "kafka")
	}
}

// TestCase.Created and TestCase.AppPort are the source of truth, as they are
// for every other kind. A spec carrying a second, contradicting copy must not
// win: two fields for one fact is how a recording ends up disagreeing with
// itself about when it happened.
func TestConsumerCreatedAndAppPortComeFromTheTestCase(t *testing.T) {
	log := goldenLogger(t)
	tc := consumerFixture()
	tc.Created = 1755861999
	tc.AppPort = 9092
	tc.ConsumerSpec.Created = 1 // stale copy
	tc.ConsumerSpec.AppPort = 1

	doc, err := EncodeTestcase(tc, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	raw, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	back, err := yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err := Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Created != 1755861999 {
		t.Errorf("Created = %d, want %d", decoded.Created, 1755861999)
	}
	if decoded.AppPort != 9092 {
		t.Errorf("AppPort = %d, want %d", decoded.AppPort, 9092)
	}
	if decoded.ConsumerSpec.Created != 1755861999 {
		t.Errorf("spec.Created = %d, want it to mirror the test case", decoded.ConsumerSpec.Created)
	}
}

// Noise is authored by the user and is the one thing on a consumer test that
// silences an assertion, so it must survive a round trip exactly — not be
// re-derived, and not accumulate.
func TestConsumerNoiseIsCarriedNotDerived(t *testing.T) {
	log := goldenLogger(t)
	fixture := consumerFixture()
	fixture.Noise = map[string][]string{
		"effects.0.body.processedAt": {},
		"effects.0.body.traceId":     {"^[0-9a-f]{32}$"},
	}

	doc, err := EncodeTestcase(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	raw, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	back, err := yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err := Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(decoded.Noise, fixture.Noise) {
		t.Fatalf("noise = %#v, want %#v", decoded.Noise, fixture.Noise)
	}

	// A payload full of names the mock matcher filters by bare name
	// (timestamp, host, sequence, epoch) must NOT be auto-noised on the way
	// to disk. The HTTP encoder runs FindNoisyFields over the response;
	// running anything like it over a consumer payload would silence the
	// exact fields the judge exists to compare.
	fixture.Noise = map[string][]string{}
	fixture.ConsumerSpec.Effects[0].Body = `{"timestamp":"2026-08-22T10:00:00Z","host":"worker-1","sequence":7,"epoch":3,"status":"CONFIRMED"}`
	doc, err = EncodeTestcase(fixture, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	raw, err = yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	back, err = yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err = Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded.Noise) != 0 {
		t.Errorf("the encoder auto-noised a consumer payload: %#v", decoded.Noise)
	}
}

// The opaque-coords promise: OSS copies protocol coordinates through without
// interpreting them, so a protocol nobody has written a parser for yet still
// round-trips.
func TestOpaqueCoordsSurviveAnUnknownProtocol(t *testing.T) {
	log := goldenLogger(t)
	tc := consumerFixture()
	tc.ConsumerSpec.Protocol = "some-future-broker"
	tc.ConsumerSpec.Trigger.Protocol = "some-future-broker"
	tc.ConsumerSpec.Trigger.Coords = map[string]string{
		"shard": "7", "cursor": "eyJhIjoxfQ==", "lease": "abc/def",
	}
	tc.ConsumerSpec.Effects[0].Protocol = "some-future-broker"
	tc.ConsumerSpec.Effects[0].Coords = map[string]string{"stream": "x"}

	doc, err := EncodeTestcase(tc, log)
	if err != nil {
		t.Fatalf("EncodeTestcase: %v", err)
	}
	raw, err := yaml.MarshalDoc(yaml.FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	back, err := yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	decoded, err := Decode(back, log)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(decoded.ConsumerSpec.Trigger.Coords, tc.ConsumerSpec.Trigger.Coords) {
		t.Errorf("trigger coords = %#v, want %#v", decoded.ConsumerSpec.Trigger.Coords, tc.ConsumerSpec.Trigger.Coords)
	}
	if !reflect.DeepEqual(decoded.ConsumerSpec.Effects[0].Coords, tc.ConsumerSpec.Effects[0].Coords) {
		t.Errorf("effect coords = %#v, want %#v", decoded.ConsumerSpec.Effects[0].Coords, tc.ConsumerSpec.Effects[0].Coords)
	}
	if decoded.ConsumerSpec.Protocol != "some-future-broker" {
		t.Errorf("protocol = %q", decoded.ConsumerSpec.Protocol)
	}
}

const (
	goldenConsumerYAML = "testdata/consumer-testcase.yaml"
	goldenConsumerJSON = "testdata/consumer-testcase.json"
)

// The consumer test-case schema is a published, hand-editable on-disk format:
// a user pastes a reported diff path into spec.assertions.noise, and an
// enterprise parser in another repository writes the rest. Checking in the
// exact bytes makes any change to it show up as a diff in review rather than
// as a silently unreadable older recording.
//
// Regenerate deliberately with:
//
//	go test ./pkg/platform/yaml/testdb/... -run TestConsumerTestCaseSchema -update-golden
func TestConsumerTestCaseSchemaIsPinned(t *testing.T) {
	yamlBytes, jsonBytes := encodeBoth(t, consumerFixture())
	compareGolden(t, goldenConsumerYAML, yamlBytes)
	compareGolden(t, goldenConsumerJSON, jsonBytes)
}
