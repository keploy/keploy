package testdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher"
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

// A field that reads as a time under every id key of a map keyed by ids is
// written once, naming every id (matcher.NoiseIDKey); one that reads as a time
// under some ids only is written for those ids, as before. One entry per id
// made a response keyed by a few dozen option ids carry hundreds of entries.
func TestTimeShapedNoiseUnderAnIDKeyedMap(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	now := time.Now().UTC()
	at := now.Format(time.RFC3339)
	ids := []string{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3f2504e0-4f89-11d3-9a0c-0305e82c3302"}
	tc := httpTestCase()
	tc.Name = "search-1"
	tc.HTTPResp.Header = map[string]string{"Content-Type": "application/json"}
	tc.HTTPResp.Body = fmt.Sprintf(`{"results":{"options":{"%s":{"start":%q,"hold":%q,"price":10},"%s":{"start":%q,"hold":null,"price":11}}}}`,
		ids[0], at, at, ids[1], at)
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			back := writeAndReadBack(t, tc, format)
			want := map[string][]string{
				"body.results.options.{id}.start":          {},
				"body.results.options." + ids[0] + ".hold": {},
			}
			if !reflect.DeepEqual(back.Noise, want) {
				t.Fatalf("noise = %v, want %v", back.Noise, want)
			}
			// Through the matcher a replay judges with: the start under every
			// id and the one hold time noised may move; the price may not.
			later := now.Add(3 * time.Second).Format(time.RFC3339)
			replayed := func(price int) *models.HTTPResp {
				return &models.HTTPResp{StatusCode: tc.HTTPResp.StatusCode, Header: tc.HTTPResp.Header,
					Body: fmt.Sprintf(`{"results":{"options":{"%s":{"start":%q,"hold":%q,"price":%d},"%s":{"start":%q,"hold":null,"price":11}}}}`,
						ids[0], later, later, price, ids[1], later)}
			}
			if pass, _ := httpMatcher.Match(back, replayed(10), nil, false, false, zap.NewNop(), false); !pass {
				t.Fatal("a replay whose noised time fields moved failed")
			}
			if pass, _ := httpMatcher.Match(back, replayed(99), nil, false, false, zap.NewNop(), false); pass {
				t.Fatal("a replay whose price changed passed: a field under an id key is no longer asserted")
			}
		})
	}
}

// The record-time entry for a time field under every id key is written only
// when it silences nothing the per-id entries would not. Each shape holds a
// field that is NOT a time where the one entry would reach it (another id's
// longer field name, an id map nested under another, a sibling id map whose
// name ends the same, an id map at the root, an empty object under another
// id); a replay that changes that field must fail.
func TestTimeShapedIDEntryReachesNoOtherField(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	at := time.Now().UTC().Format(time.RFC3339)
	u0, u1 := "3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3f2504e0-4f89-11d3-9a0c-0305e82c3302"
	p0, o0 := "3f2504e0-4f89-11d3-9a0c-0305e82c3303", "3f2504e0-4f89-11d3-9a0c-0305e82c3304"
	for _, tc := range []struct {
		name, shape string
		rec, rep    string // the non-time value, recorded and replayed
	}{
		{"another id's longer field", `{"options":{"%[1]s":{"hold":%[3]q},"%[2]s":{"holdReason":%[6]s}}}`, `"supplier"`, `"changed"`},
		{"nested id map", `{"options":{"%[1]s":{"ts":%[3]q},"%[2]s":{"ts":%[3]q}},"packages":{"%[4]s":{"options":{"%[5]s":{"ts":%[6]s}}}}}`, `"label-a"`, `"label-b"`},
		{"sibling id map", `{"options":{"%[1]s":{"ts":%[3]q},"%[2]s":{"ts":%[3]q}},"addonOptions":{"%[5]s":{"ts":%[6]s}},"x":"%[4]s"}`, `"label-a"`, `"label-b"`},
		{"root id map", `{"%[1]s":{"ts":%[3]q},"%[2]s":{"ts":%[3]q},"meta":{"%[5]s":{"ts":%[6]s}},"x":"%[4]s"}`, `"label-a"`, `"label-b"`},
		{"empty object under another id", `{"options":{"%[1]s":{"hold":%[3]q},"%[2]s":{"hold":%[6]s}},"x":"%[4]s%[5]s"}`, `{}`, `{"by":"label-b"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := func(v string) string { return fmt.Sprintf(tc.shape, u0, u1, at, p0, o0, v) }
			c := httpTestCase()
			c.HTTPResp.Header = map[string]string{"Content-Type": "application/json"}
			c.HTTPResp.Body = body(tc.rec)
			back := writeAndReadBack(t, c, "yaml")
			if len(back.Noise) == 0 {
				t.Fatalf("no noise written: the time field is not found")
			}
			same := &models.HTTPResp{StatusCode: c.HTTPResp.StatusCode, Header: c.HTTPResp.Header, Body: body(tc.rec)}
			if pass, _ := httpMatcher.Match(back, same, nil, false, false, zap.NewNop(), false); !pass {
				t.Fatalf("the recorded response itself fails with %v", back.Noise)
			}
			changed := &models.HTTPResp{StatusCode: c.HTTPResp.StatusCode, Header: c.HTTPResp.Header, Body: body(tc.rep)}
			if pass, _ := httpMatcher.Match(back, changed, nil, false, false, zap.NewNop(), false); pass {
				t.Fatalf("a replay changing a field that is not a time (%s -> %s) passed with %v", tc.rec, tc.rep, back.Noise)
			}
		})
	}
}

// An entry naming id keys speaks for the ids the recording has: a value that
// moves there, or a field a replay drops there, is excused as the per-id
// entries main writes would excuse it. A key the recording lacks (a new id, a
// field appearing under an id that did not carry it, under another id map, or
// under an id map nested in another) is a change, and the replay fails, as it
// does against the per-id entries. Each row is judged against both and must
// get the same verdict from each.
func TestTimeShapedIDEntryExcusesNoKeyTheRecordingLacks(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ids := strings.NewReplacer(
		"U1", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", "U2", "3f2504e0-4f89-11d3-9a0c-0305e82c3302",
		"U3", "3f2504e0-4f89-11d3-9a0c-0305e82c3303", "A1", "3f2504e0-4f89-11d3-9a0c-0305e82c3304",
		"C1", "3f2504e0-4f89-11d3-9a0c-0305e82c3305", "C2", "3f2504e0-4f89-11d3-9a0c-0305e82c3306")
	recAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	repAt := time.Now().UTC().Format(time.RFC3339)
	fill := func(s, at string) string { return strings.ReplaceAll(ids.Replace(s), "T", at) }
	nested := `{"U1":{"ts":"T","children":{"C1":{"ts":"T","name":"x"}}},"U2":{"ts":"T"}}`
	nestedEntries := "body.{id}.ts body.{id}.children.{id}.ts"
	for _, r := range []struct {
		name, rec, rep string
		entries        string // the noise written, space-separated
		pass           bool
	}{
		{"times move", `{"options":{"U1":{"hold":"T"},"U2":{"hold":"T"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"hold":"T"},"U2":{"hold":"T"},"U3":{"price":5}}}`, "body.options.{id}.hold", true},
		{"a new id under a map of times", `{"lastSeen":{"U1":"T","U2":"T"}}`,
			`{"lastSeen":{"U1":"T","U2":"T","U3":"T"}}`, "body.lastSeen.{id}", false},
		{"the field appears under an id that lacked it", `{"options":{"U1":{"hold":"T"},"U2":{"hold":"T"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"hold":"T"},"U2":{"hold":"T"},"U3":{"price":5,"hold":"T"}}}`, "body.options.{id}.hold", false},
		{"the field appears under another id map", `{"options":{"U1":{"ts":"T"},"U2":{"ts":"T"}},"addonOptions":{"A1":{"label":"x"}}}`,
			`{"options":{"U1":{"ts":"T"},"U2":{"ts":"T"}},"addonOptions":{"A1":{"label":"x","ts":"T"}}}`, "body.options.{id}.ts", false},
		{"root id map: a nested id map gains the field", `{"U1":{"ts":"T","children":{"C1":{"name":"x"}}},"U2":{"ts":"T"}}`,
			`{"U1":{"ts":"T","children":{"C1":{"name":"x","ts":"T"}}},"U2":{"ts":"T"}}`, "body.{id}.ts", false},
		{"an id dropped from a map of times", `{"lastSeen":{"U1":"T","U2":"T"}}`, `{"lastSeen":{"U1":"T"}}`, "body.lastSeen.{id}", true},
		{"the field dropped under an id that had it", `{"options":{"U1":{"hold":"T"},"U2":{"hold":"T"},"U3":{"price":5}}}`,
			`{"options":{"U1":{"hold":"T"},"U2":{},"U3":{"price":5}}}`, "body.options.{id}.hold", true},
		{"nested id maps: times move", nested, nested, nestedEntries, true},
		{"nested id maps: the field dropped under the nested id", nested,
			`{"U1":{"ts":"T","children":{"C1":{"name":"x"}}},"U2":{"ts":"T"}}`, nestedEntries, true},
		{"nested id maps: a new nested id", nested,
			`{"U1":{"ts":"T","children":{"C1":{"ts":"T","name":"x"},"C2":{"ts":"T"}}},"U2":{"ts":"T"}}`, nestedEntries, false},
		{"nested id maps: an outer id gains a nested map", nested,
			`{"U1":{"ts":"T","children":{"C1":{"ts":"T","name":"x"}}},"U2":{"ts":"T","children":{"C2":{"ts":"T"}}}}`, nestedEntries, false},
	} {
		t.Run(r.name, func(t *testing.T) {
			tc := httpTestCase()
			tc.HTTPResp.Header = map[string]string{"Content-Type": "application/json"}
			tc.HTTPResp.Body = fill(r.rec, recAt)
			back := writeAndReadBack(t, tc, "yaml")
			entries := strings.Fields(r.entries)
			for _, e := range entries {
				if _, ok := back.Noise[e]; !ok {
					t.Fatalf("noise written = %v, want only %v", back.Noise, entries)
				}
			}
			if len(back.Noise) != len(entries) {
				t.Fatalf("noise written = %v, want only %v", back.Noise, entries)
			}
			replayed := &models.HTTPResp{StatusCode: tc.HTTPResp.StatusCode, Header: tc.HTTPResp.Header, Body: fill(r.rep, repAt)}
			pass, _ := httpMatcher.Match(back, replayed, nil, false, false, zap.NewNop(), false)
			// The per-id entries main writes: one per recorded field the
			// entry reaches.
			perID := *back
			perID.Noise = map[string][]string{}
			var doc interface{}
			if err := json.Unmarshal([]byte(tc.HTTPResp.Body), &doc); err != nil {
				t.Fatal(err)
			}
			var walk func(p string, v interface{})
			walk = func(p string, v interface{}) {
				for _, e := range entries {
					if p != "" && matcher.NoiseKeyCovers("body."+p, e) {
						perID.Noise["body."+p] = []string{}
					}
				}
				if m, ok := v.(map[string]interface{}); ok {
					for k, c := range m {
						cp := k
						if p != "" {
							cp = p + "." + k
						}
						walk(cp, c)
					}
				}
			}
			walk("", doc)
			var perIDNoise []string
			for k := range perID.Noise {
				perIDNoise = append(perIDNoise, strings.TrimPrefix(k, "body."))
			}
			perIDPass, _ := httpMatcher.Match(&perID, replayed, nil, false, false, zap.NewNop(), false)
			if pass != r.pass || perIDPass != r.pass {
				t.Fatalf("passed=%v with %v and %v with the per-id entries %v, want %v", pass, back.Noise, perIDPass, perIDNoise, r.pass)
			}
		})
	}
}

// A test case can reach the encoder with no noise map at all: a test case
// created from the dashboard (its form sends no noise), or any stored case
// decoded from JSON whose "noise" is absent or null. With the proven switch
// off the encoder adds the response's time-shaped fields (a Date header) as
// noise, and it must add them to a map, not to the nil one: assigning to an
// entry of a nil map panics, and that panic took down the k8s-proxy laying out
// such a case for a replay. The case is written with the same noise as one
// that came with an empty map.
func TestATestCaseWithNoNoiseMapIsWrittenWithItsTimeShapedNoise(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "")
			tc := timeShapedTestCase(time.Now())
			tc.Noise = nil
			var back *models.TestCase
			if p := panicOf(func() { back = writeAndReadBack(t, tc, format) }); p != nil {
				t.Fatalf("writing a test case that has no noise map panicked: %v", p)
			}
			if !reflect.DeepEqual(back.Noise, timeShapedNoise) {
				t.Fatalf("the test case was written with noise %v, want every time-looking field %v, as a case with an empty map gets", back.Noise, timeShapedNoise)
			}
		})
	}
}

// panicOf runs f and returns what it panicked with, or nil.
func panicOf(f func()) (p any) {
	defer func() { p = recover() }()
	f()
	return nil
}
