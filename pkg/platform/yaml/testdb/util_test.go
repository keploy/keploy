package testdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func httpTestCase() models.TestCase {
	return models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    "test-1",
		HTTPReq: models.HTTPReq{Method: models.Method("GET"), URL: "http://example.com/users"},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Body:       `{"ok":true}`,
		},
		Noise: map[string][]string{},
	}
}

func grpcTestCase() models.TestCase {
	return models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.GRPC_EXPORT,
		Name:    "test-1",
		Noise:   map[string][]string{},
	}
}

// TestSpecMetadataStaysAbsentWhenUnset pins byte-compatibility with existing
// files: a testcase with no description must encode a nil Metadata map,
// exactly as before the metadata map existed.
func TestSpecMetadataStaysAbsentWhenUnset(t *testing.T) {
	if md := specMetadata(""); md != nil {
		t.Fatalf("expected nil metadata for unset fields, got %v", md)
	}
	if schema := buildHTTPSchema(httpTestCase(), zap.NewNop()); schema.Metadata != nil {
		t.Fatalf("unmarked http testcase must not grow a metadata map, got %v", schema.Metadata)
	}
	if spec := buildGrpcSpec(grpcTestCase()); spec.Metadata != nil {
		t.Fatalf("unmarked grpc testcase must not grow a metadata map, got %v", spec.Metadata)
	}
}

// TestDecodeIgnoresUnknownMetadataKeys pins the forward-compat contract: a
// reader at this version decodes files whose metadata carries keys it does not
// know without error, and an absent metadata map yields empty fields.
func TestDecodeIgnoresUnknownMetadataKeys(t *testing.T) {
	logger := zap.NewNop()

	doc, err := EncodeTestcase(httpTestCase(), logger)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Splice a metadata map with only unknown keys into the spec node, the way
	// a newer writer would.
	var raw map[string]interface{}
	if err := doc.Spec.Decode(&raw); err != nil {
		t.Fatalf("spec to map: %v", err)
	}
	raw["metadata"] = map[string]string{"some_future_key": "x"}
	var node yamlLib.Node
	if err := node.Encode(raw); err != nil {
		t.Fatalf("map to node: %v", err)
	}
	doc.Spec = node

	got, err := Decode(doc, logger)
	if err != nil {
		t.Fatalf("decode with unknown metadata keys must not fail: %v", err)
	}
	if got.Description != "" {
		t.Fatalf("unknown keys must not bleed into fields: %+v", got)
	}
}

// A test case whose body a block scalar cannot carry (tab-indented JSON, a
// body that opens with a line break) is encoded, and reads back exactly.
// Node.Encode wrote it as a block and failed to parse that back: the test
// case was not saved.
func TestEncodeTestcaseCarriesABodyABlockScalarCannot(t *testing.T) {
	for _, body := range []string{"\t{\n\t\"a\": 1\n}", "\n\thello", " a\nb"} {
		tc := httpTestCase()
		tc.HTTPReq.Body, tc.HTTPResp.Body = body, body
		doc, err := EncodeTestcase(tc, zap.NewNop())
		if err != nil {
			t.Fatalf("body %q: EncodeTestcase: %v", body, err)
		}
		out, err := yamlLib.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var read yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(out, &read); err != nil {
			t.Fatalf("body %q: the test case does not load: %v\n%s", body, err, out)
		}
		back, err := Decode(&read, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		if back.HTTPReq.Body != body || back.HTTPResp.Body != body {
			t.Fatalf("read back request %q, response %q, want %q", back.HTTPReq.Body, back.HTTPResp.Body, body)
		}
	}
}

// A test case keeps the line lengths of a header that arrived on more than one
// wire line (models.HeaderLineLengths), whether its body is written as yaml.v3
// picks or double-quoted through yaml.EncodeQuoted: replay sends the request
// with those lines again, and a test case that reads back without them is sent
// with the header folded into one line.
func TestEncodeTestcaseKeepsRecordedHeaderLines(t *testing.T) {
	reqLines := models.HeaderLineLengths{"X-Forwarded-For": {8, 8}}
	respLines := models.HeaderLineLengths{"Set-Cookie": {5, 5}}
	for _, body := range []string{`{"id":7}`, "\t{\n\t\"a\": 1\n}"} {
		tc := httpTestCase()
		tc.HTTPReq.Header = map[string]string{"X-Forwarded-For": "10.0.0.1,10.0.0.2"}
		tc.HTTPReq.HeaderLineLengths = reqLines
		tc.HTTPReq.Body = body
		tc.HTTPResp.Header = map[string]string{"Set-Cookie": "a=1;x,b=2;y"}
		tc.HTTPResp.HeaderLineLengths = respLines
		doc, err := EncodeTestcase(tc, zap.NewNop())
		if err != nil {
			t.Fatalf("body %q: EncodeTestcase: %v", body, err)
		}
		out, err := yamlLib.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var read yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(out, &read); err != nil {
			t.Fatalf("body %q: the test case does not load: %v\n%s", body, err, out)
		}
		back, err := Decode(&read, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.HTTPReq.HeaderLineLengths, reqLines) || !reflect.DeepEqual(back.HTTPResp.HeaderLineLengths, respLines) {
			t.Fatalf("body %q: read back request lines %v and response lines %v, want %v and %v:\n%s",
				body, back.HTTPReq.HeaderLineLengths, back.HTTPResp.HeaderLineLengths, reqLines, respLines, out)
		}
		if back.HTTPReq.Body != body {
			t.Fatalf("read back request body %q, want %q", back.HTTPReq.Body, body)
		}
	}
}

// timeShapedTestCase is a recorded GET /projects whose answer carries every
// value shape pkg.IsTime reads as a time: RFC 3339 instants (created_at and
// updated_at, as a database returns a timestamptz), a date alone (since), a
// number IsTime reads as an epoch in nanoseconds within a day of now (seq),
// and the HTTP Date header. Nothing else in it reads as a time.
func timeShapedTestCase(now time.Time) models.TestCase {
	tc := httpTestCase()
	tc.Name = "get-projects-1"
	tc.HTTPReq.URL = "http://localhost:8080/projects"
	tc.HTTPResp.Header = map[string]string{"Content-Type": "application/json", "Date": now.UTC().Format(http.TimeFormat)}
	tc.HTTPResp.Body = projectsBody(now.Add(-350*time.Millisecond), now, now.UnixNano())
	return tc
}

func projectsBody(created, updated time.Time, seq int64) string {
	return fmt.Sprintf(`[{"id":"b3dae852-82a0-49b7-b9db-123c79dcabd6","name":"Alpha Project","status":"completed","created_at":%q,"updated_at":%q,"since":"2024-01-02","seq":%d}]`,
		created.UTC().Format(time.RFC3339Nano), updated.UTC().Format(time.RFC3339Nano), seq)
}

// timeShapedNoise is the noise keploy writes onto timeShapedTestCase with the
// proven switch off: every field whose value reads as a time.
var timeShapedNoise = map[string][]string{
	"body.created_at": {},
	"body.seq":        {},
	"body.since":      {},
	"body.updated_at": {},
	"header.Date":     {},
}

// writeAndReadBack writes tc the way keploy writes a test case file, in the
// given format, and reads the file back the way a replay loads it.
func writeAndReadBack(t *testing.T, tc models.TestCase, format string) *models.TestCase {
	t.Helper()
	logger := zap.NewNop()
	switch format {
	case "yaml":
		doc, err := EncodeTestcase(tc, logger)
		if err != nil {
			t.Fatalf("EncodeTestcase: %v", err)
		}
		out, err := yamlLib.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var read yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal(out, &read); err != nil {
			t.Fatalf("the test case does not load: %v\n%s", err, out)
		}
		back, err := Decode(&read, logger)
		if err != nil {
			t.Fatal(err)
		}
		return back
	case "json":
		doc, err := EncodeTestcaseJSON(tc, logger)
		if err != nil {
			t.Fatalf("EncodeTestcaseJSON: %v", err)
		}
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var read yaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(out, &read); err != nil {
			t.Fatalf("the test case does not load: %v\n%s", err, out)
		}
		back, err := DecodeJSON(&read, logger)
		if err != nil {
			t.Fatal(err)
		}
		return back
	}
	t.Fatalf("no format %q", format)
	return nil
}

// Under the proven switch keploy writes no noise onto a test case for what a
// value looks like. A field is noise there only when it is declared or when
// the learn step of record's full round F saw it drift, so a date-shaped value
// that replays the same (a created_at a mocked database answers) is asserted.
// With the switch off every time-looking field is noise, as before. Both
// encoders are the ones record (InsertTestCase) and the learn step
// (UpdateTestCase) write test case files with.
func TestTimeShapedNoiseIsWrittenOnlyWithTheProvenSwitchOff(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format+", switch on", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			tc := timeShapedTestCase(time.Now())
			back := writeAndReadBack(t, tc, format)
			if len(back.Noise) != 0 {
				t.Fatalf("the test case was written with noise %v; under the switch no field is noise for its look", back.Noise)
			}
			if _, ok := back.Assertions[models.NoiseAssertion]; ok {
				t.Fatalf("the test case was written with a noise assertion: %v", back.Assertions)
			}
			if len(tc.Noise) != 0 {
				t.Fatalf("encoding added %v to the caller's test case", tc.Noise)
			}
		})
		t.Run(format+", switch off", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			back := writeAndReadBack(t, timeShapedTestCase(time.Now()), format)
			if !reflect.DeepEqual(back.Noise, timeShapedNoise) {
				t.Fatalf("with the switch off the test case was written with noise %v, want every time-looking field %v", back.Noise, timeShapedNoise)
			}
		})
	}
}

// The false green the switch closes, end to end through the matcher a replay
// judges with: a test case whose created_at replays the same is kept, and a
// replay whose created_at changed must fail it. With the switch off the same
// change passes, the field noised for its look. The Date header, which the
// server mints on every answer, still passes when it changes: the replayer
// forgives a per-request response header by its name (WithAutoHeaderNoise, on
// unless --disableAutoHeaderNoise), never by its value.
func TestUnderTheProvenSwitchAStableTimeShapedFieldIsAsserted(t *testing.T) {
	now := time.Now()
	replayed := func(created time.Time) *models.HTTPResp {
		return &models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Content-Type": "application/json", "Date": now.Add(62 * time.Second).UTC().Format(http.TimeFormat)},
			Body:       projectsBody(created, now, now.UnixNano()),
		}
	}
	same, changed := replayed(now.Add(-350*time.Millisecond)), replayed(now.Add(-2*time.Hour))
	match := func(t *testing.T, tc *models.TestCase, actual *models.HTTPResp) bool {
		t.Helper()
		pass, _ := httpMatcher.Match(tc, actual, nil, false, false, zap.NewNop(), false, httpMatcher.WithAutoHeaderNoise(true))
		return pass
	}
	for _, format := range []string{"yaml", "json"} {
		t.Run(format+", switch on", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			tc := writeAndReadBack(t, timeShapedTestCase(now), format)
			if !match(t, tc, same) {
				t.Fatal("a replay that answered the recorded created_at (and a new Date) failed")
			}
			if match(t, tc, changed) {
				t.Fatal("a replay whose created_at changed passed: the field is not asserted")
			}
		})
		t.Run(format+", switch off", func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			tc := writeAndReadBack(t, timeShapedTestCase(now), format)
			if !match(t, tc, changed) {
				t.Fatal("with the switch off a changed created_at must pass as it always has (noised for its look)")
			}
		})
	}
}
