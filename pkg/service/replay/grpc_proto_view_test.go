package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/protoschema"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const ordersProto = `syntax = "proto3";
package demo;
service Orders { rpc Read(ReadReq) returns (Order); }
message ReadReq { string id = 1; }
message Ts { int64 seconds = 1; int32 nanos = 2; }
message Order {
  string id = 1;
  string status = 3;
  Ts created_at = 8;
}
`

func protoReplayer(t *testing.T) *Replayer {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orders.proto"), []byte(ordersProto), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Test.ProtoDir = dir
	return &Replayer{config: cfg, logger: zap.NewNop()}
}

func ordersCase(body string, noise map[string][]string) *models.TestCase {
	return &models.TestCase{
		Name: "orders-read",
		Kind: models.GRPC_EXPORT,
		GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{
			PseudoHeaders: map[string]string{":path": "/demo.Orders/Read"},
		}},
		GrpcResp: models.GrpcResp{
			Headers: models.GrpcHeaders{
				PseudoHeaders:   map[string]string{":status": "200"},
				OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
			},
			Body:     models.GrpcLengthPrefixedMessage{MessageLength: uint32(len(body)), DecodedData: body},
			Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
		},
		Noise: noise,
	}
}

func ordersResp(body string) *models.GrpcResp {
	tc := ordersCase(body, nil)
	return &tc.GrpcResp
}

// compareWithProtos runs the comparison both replay sites run.
func compareWithProtos(r *Replayer, tc *models.TestCase, actual *models.GrpcResp) (bool, *models.Result) {
	return r.compareGRPCWithSchemas(tc, actual, "set-1", false, false)
}

func TestFieldNumberPathTranslatesNames(t *testing.T) {
	r := protoReplayer(t)
	view := r.grpcProtoViewFor(ordersCase("", nil), "set-1")
	if view == nil || view.noiseMessage() == nil {
		t.Fatal("the test proto did not resolve /demo.Orders/Read")
	}
	md := view.noiseMessage()
	for key, want := range map[string]string{
		"created_at.seconds":      "8.1",
		"createdAt.seconds":       "8.1",
		"body.status":             "3",
		"body.created_at":         "8",
		"decoded_data.created_at": "8",
	} {
		if got, ok := fieldNumberPath(md, key); !ok || got != want {
			t.Errorf("%q -> %q (%v), want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"missing", "8.1", "created_at.unknown"} {
		if got, ok := fieldNumberPath(md, key); ok {
			t.Errorf("%q translated to %q, want no translation", key, got)
		}
	}
}

// A change in a field the proto does not define used to pass with protos
// given: protojson dropped it from both sides and the length difference was
// forgiven. Without protos it failed. Protos must never weaken the verdict.
func TestProtosNeverHideAChangeInAFieldTheyDoNotDefine(t *testing.T) {
	r := protoReplayer(t)
	recorded := "1: {\"o-1\"}\n3: {\"Pending\"}\n9: {\"tenant-a\"}"
	actual := "1: {\"o-1\"}\n3: {\"Pending\"}\n9: {\"tenant-b-longer\"}"
	pass, _ := compareWithProtos(r, ordersCase(recorded, nil), ordersResp(actual))
	if pass {
		t.Fatal("field 9 is not in the proto and changed; the test passed")
	}
}

// Noise written by field name keeps working: the protos translate it into
// the field-number path the comparison applies.
func TestNoiseByFieldNameStillAppliesWithProtos(t *testing.T) {
	recorded := "1: {\"o-1\"}\n3: {\"Pending\"}\n8: {\n  1: 1795582799\n  2: 5\n}"
	actual := "1: {\"o-1\"}\n3: {\"Pending\"}\n8: {\n  1: 1795590000\n  2: 5\n}"

	t.Run("test-case noise", func(t *testing.T) {
		r := protoReplayer(t)
		pass, _ := compareWithProtos(r, ordersCase(recorded, map[string][]string{"body": {"created_at.seconds"}}), ordersResp(actual))
		if !pass {
			t.Fatal("created_at.seconds is noise; only it differs; the test failed")
		}
	})
	t.Run("global noise", func(t *testing.T) {
		r := protoReplayer(t)
		r.config.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"createdAt.seconds": {}}}
		pass, _ := compareWithProtos(r, ordersCase(recorded, nil), ordersResp(actual))
		if !pass {
			t.Fatal("createdAt.seconds is global noise; only it differs; the test failed")
		}
	})
	t.Run("an unlisted field is still compared", func(t *testing.T) {
		r := protoReplayer(t)
		changed := strings.Replace(actual, "Pending", "Shipped", 1)
		pass, _ := compareWithProtos(r, ordersCase(recorded, map[string][]string{"body": {"created_at.seconds"}}), ordersResp(changed))
		if pass {
			t.Fatal("status changed and is not noise; the test passed")
		}
	})
}

// A failed test shows both bodies with field names; the verdict is the
// field-number comparison's.
func TestAFailedGrpcTestShowsBodiesWithFieldNames(t *testing.T) {
	r := protoReplayer(t)
	pass, res := compareWithProtos(r, ordersCase("1: {\"o-1\"}\n3: {\"Pending\"}", nil), ordersResp("1: {\"o-1\"}\n3: {\"Shipped\"}"))
	if pass {
		t.Fatal("status changed; the test passed")
	}
	d := res.GrpcDecode
	if d == nil {
		t.Fatal("no decoded view")
	}
	var exp, act map[string]any
	if json.Unmarshal([]byte(d.ExpectedJSON), &exp) != nil || json.Unmarshal([]byte(d.ActualJSON), &act) != nil ||
		exp["status"] != "Pending" || act["status"] != "Shipped" {
		t.Fatalf("decoded bodies not shown: %+v", d)
	}
	for _, b := range res.BodyResult {
		if b.Type == models.GrpcData && strings.Contains(b.Expected, "status") {
			t.Fatalf("BodyResult must keep the recorded protoscope; got %q", b.Expected)
		}
	}
}

// fakeSchemas is a GrpcSchemaSource (what cloud replay sets).
type fakeSchemas struct {
	recorded      map[string]*protoschema.Schema
	recordedLabel string
	target        *protoschema.Schema
	targetNote    string
	recordedAsked int
}

func (f *fakeSchemas) RecordedSchema(_ context.Context, testSetID string) (*protoschema.Schema, string, string) {
	f.recordedAsked++
	if s := f.recorded[testSetID]; s != nil {
		return s, f.recordedLabel, ""
	}
	return nil, "", "schema not uploaded for release app:v1: run keploy proto upload --release-tag app:v1"
}

func (f *fakeSchemas) TargetSchema(context.Context) (*protoschema.Schema, string, string) {
	if f.target == nil {
		return nil, "", f.targetNote
	}
	return f.target, "release:app:v2", ""
}

func ordersSchema(t *testing.T, proto string) *protoschema.Schema {
	t.Helper()
	c, err := protoschema.Compile(context.Background(), map[string][]byte{"orders.proto": []byte(proto)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := protoschema.Load(c.DescriptorSet)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// With a schema source, each side decodes with its own release's schema:
// field 3 is `status` in the recorded release and `state` in the target.
func TestEachSideDecodesWithItsOwnReleasesSchema(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	v2 := strings.Replace(ordersProto, "string status = 3;", "string state = 3;", 1)
	src := &fakeSchemas{
		recorded:      map[string]*protoschema.Schema{"set-1": ordersSchema(t, ordersProto)},
		recordedLabel: "release:app:v1",
		target:        ordersSchema(t, v2),
	}
	r.SetGrpcSchemaSource(src)
	pass, res := compareWithProtos(r, ordersCase("1: {\"o-1\"}\n3: {\"Pending\"}", nil), ordersResp("1: {\"o-1\"}\n3: {\"Shipped\"}"))
	if pass {
		t.Fatal("field 3 changed; the test passed")
	}
	if res.GrpcDecode == nil || res.GrpcDecode.Expected != "release:app:v1" || res.GrpcDecode.Actual != "release:app:v2" {
		t.Fatalf("labels: %+v", res.GrpcDecode)
	}
	var exp, act map[string]any
	_ = json.Unmarshal([]byte(res.GrpcDecode.ExpectedJSON), &exp)
	_ = json.Unmarshal([]byte(res.GrpcDecode.ActualJSON), &act)
	if exp["status"] != "Pending" || act["state"] != "Shipped" {
		t.Fatalf("each side must use its own schema: %+v", res.GrpcDecode)
	}
}

// A recorded release with no uploaded schema shows both sides as recorded,
// with the reason; it never decodes with another release's schema.
func TestARecordedReleaseWithNoSchemaIsShownAsRecordedAndSaysSo(t *testing.T) {
	r := protoReplayer(t) // local protos exist, but must not stand in for the recorded release
	r.SetGrpcSchemaSource(&fakeSchemas{})
	rec, act := "3: {\"Pending\"}", "3: {\"Shipped\"}"
	_, res := compareWithProtos(r, ordersCase(rec, nil), ordersResp(act))
	if res.GrpcDecode == nil || !strings.Contains(res.GrpcDecode.Note, "not uploaded for release app:v1") || res.GrpcDecode.Expected != "" {
		t.Fatalf("decode info: %+v", res.GrpcDecode)
	}
	if res.GrpcDecode.ExpectedJSON != "" || res.GrpcDecode.ActualJSON != "" {
		t.Fatalf("bodies were decoded without the recorded release's schema: %+v", res.GrpcDecode)
	}
}

// The recorded release has its upload and the release under test has none:
// both sides stay as recorded, and the report says why in the source's words
// (the release to upload, and how), not a generic "upload it" -- the end of
// the run names the same release.
func TestTheReleaseUnderTestWithNoUploadIsNamed(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	const why = "schema not uploaded for release app:v2: run keploy proto upload --release-tag app:v2"
	r.SetGrpcSchemaSource(&fakeSchemas{
		recorded:      map[string]*protoschema.Schema{"set-1": ordersSchema(t, ordersProto)},
		recordedLabel: "release:app:v1",
		targetNote:    why,
	})
	_, res := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	d := res.GrpcDecode
	if d == nil || d.ExpectedJSON != "" || d.ActualJSON != "" {
		t.Fatalf("both sides must stay as recorded: %+v", d)
	}
	if d.Note != "no schema for the new response: "+why {
		t.Fatalf("note = %q", d.Note)
	}
}

// Neither release has an upload: the note gives both reasons, as uploading
// either alone still decodes nothing.
func TestBothReleasesWithNoUploadAreNamed(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	const why = "schema not uploaded for release app:v2: run keploy proto upload --release-tag app:v2"
	r.SetGrpcSchemaSource(&fakeSchemas{targetNote: why})
	_, res := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	want := "schema not uploaded for release app:v1: run keploy proto upload --release-tag app:v1; no schema for the new response: " + why
	if d := res.GrpcDecode; d == nil || d.Note != want {
		t.Fatalf("note = %+v, want %q", d, want)
	}
}

// The release under test is the recorded release, with no upload: its
// reason is given once, not once per side.
func TestTheSameReleaseWithNoUploadIsNamedOnce(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	const why = "schema not uploaded for release app:v1: run keploy proto upload --release-tag app:v1"
	r.SetGrpcSchemaSource(&fakeSchemas{targetNote: why})
	_, res := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	if d := res.GrpcDecode; d == nil || d.Note != why {
		t.Fatalf("note = %+v, want %q", d, why)
	}
}

// The release under test answered with no schema and a reason (nothing was
// uploaded yet): it is asked again for the next test, and its schema used
// once the source has one.
func TestATargetWithNoSchemaIsAskedAgain(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	src := &fakeSchemas{
		recorded:      map[string]*protoschema.Schema{"set-1": ordersSchema(t, ordersProto)},
		recordedLabel: "release:app:v1",
		targetNote:    "schema not uploaded for release app:v2",
	}
	r.SetGrpcSchemaSource(src)
	_, first := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	if first.GrpcDecode == nil || first.GrpcDecode.ActualJSON != "" {
		t.Fatalf("first test: %+v", first.GrpcDecode)
	}
	src.target = ordersSchema(t, ordersProto)
	_, second := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	if d := second.GrpcDecode; d == nil || d.Actual != "release:app:v2" || !strings.Contains(d.ActualJSON, "Shipped") {
		t.Fatalf("second test: %+v", d)
	}
}

// A test set's schema is asked for once per run, not once per test case.
func TestTheRecordedSchemaIsResolvedOncePerTestSet(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	src := &fakeSchemas{recorded: map[string]*protoschema.Schema{"set-1": ordersSchema(t, ordersProto)}, recordedLabel: "release:app:v1"}
	r.SetGrpcSchemaSource(src)
	for i := 0; i < 5; i++ {
		compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	}
	if src.recordedAsked != 1 {
		t.Fatalf("RecordedSchema asked %d times, want 1", src.recordedAsked)
	}
}

// A set whose schema was not given on the first ask (a passing read failure)
// is asked again for its next test, and decoded once the source answers.
func TestARecordedSchemaNotGivenIsAskedAgain(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	src := &flakySchemas{schema: ordersSchema(t, ordersProto)}
	r.SetGrpcSchemaSource(src)
	_, first := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	_, second := compareWithProtos(r, ordersCase("3: {\"Pending\"}", nil), ordersResp("3: {\"Shipped\"}"))
	if first.GrpcDecode == nil || first.GrpcDecode.ExpectedJSON != "" {
		t.Fatalf("first test: %+v", first.GrpcDecode)
	}
	if second.GrpcDecode == nil || second.GrpcDecode.ExpectedJSON == "" || src.recordedAsked != 2 || src.targetAsked != 2 {
		t.Fatalf("second test: %+v (recorded asked %d, target %d)", second.GrpcDecode, src.recordedAsked, src.targetAsked)
	}
}

// flakySchemas answers nothing on the first ask, then its schema.
type flakySchemas struct {
	schema                     *protoschema.Schema
	recordedAsked, targetAsked int
}

func (f *flakySchemas) RecordedSchema(context.Context, string) (*protoschema.Schema, string, string) {
	f.recordedAsked++
	if f.recordedAsked == 1 {
		return nil, "", "could not be read (timeout)"
	}
	return f.schema, "release:app:v1", ""
}

func (f *flakySchemas) TargetSchema(context.Context) (*protoschema.Schema, string, string) {
	f.targetAsked++
	if f.targetAsked == 1 {
		return nil, "", ""
	}
	return f.schema, "release:app:v2", ""
}

// A stream shows the message that differs, not the first one.
func TestAStreamShowsTheMessageThatDiffers(t *testing.T) {
	r := protoReplayer(t)
	rec := ordersCase("1: {\"same\"}", nil)
	rec.GrpcResp.AdditionalMessages = []models.GrpcLengthPrefixedMessage{{DecodedData: "1: {\"rec\"}"}}
	act := ordersResp("1: {\"same\"}")
	act.AdditionalMessages = []models.GrpcLengthPrefixedMessage{{DecodedData: "1: {\"live\"}"}}
	pass, res := compareWithProtos(r, rec, act)
	if pass {
		t.Fatal("message 1 differs; the test passed")
	}
	d := res.GrpcDecode
	if d == nil || d.MessageIndex != 1 || !strings.Contains(d.ExpectedJSON, "rec") || !strings.Contains(d.ActualJSON, "live") || d.Note == encodingOnlyNote {
		t.Fatalf("decoded view: %+v", d)
	}
}

// Test-case noise wins over global noise naming the same field, every run.
func TestTestCaseNoiseWinsOverGlobalForTheSameField(t *testing.T) {
	for i := 0; i < 20; i++ {
		r := protoReplayer(t)
		r.config.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"createdAt.seconds": {"^9"}}}
		tc := ordersCase("8: {\n  1: 1795582799\n}", map[string][]string{"body.created_at.seconds": {"^1"}})
		view := r.grpcProtoViewFor(tc, "set-1")
		got := r.withNameNoiseAsFieldNumbers(tc, view, "set-1").Noise["body.8.1"]
		if len(got) != 1 || got[0] != "^1" {
			t.Fatalf("run %d: body.8.1 = %v, want the test case's [^1]", i, got)
		}
	}
}

// A pattern written by name for an enum or message field is not translated:
// the recorded text is not the value the pattern was written against.
func TestPatternedNoiseOnANonScalarIsNotTranslated(t *testing.T) {
	r := protoReplayer(t)
	tc := ordersCase("", map[string][]string{"body.created_at": {"^2026-"}})
	view := r.grpcProtoViewFor(tc, "set-1")
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, view, "set-1").Noise["body.8"]; ok {
		t.Fatal("a patterned key on message field created_at was translated")
	}
	tc = ordersCase("", map[string][]string{"body.created_at": nil})
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, view, "set-1").Noise["body.8"]; !ok {
		t.Fatal("an unpatterned key on a message field must still translate")
	}
}

// The map fields a schema names are found at any depth.
func TestMapFieldPathsFromTheSchema(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
service Orders { rpc Read(R) returns (O); }
message R {}
message Inner { map<string,int32> tags = 2; }
message O { map<string,string> labels = 5; Inner inner = 6; repeated Inner many = 7; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	if got := strings.Join(mapFieldPaths(resp), ","); got != "5,6.2,7.2" {
		t.Fatalf("map paths = %s", got)
	}
}

// A map inside a map's value is found too.
func TestMapFieldPathsInsideMapValues(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
service Orders { rpc Read(R) returns (O); }
message R {}
message Inner { map<string,int32> tags = 1; }
message O { map<string,Inner> by = 5; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	if got := strings.Join(mapFieldPaths(resp), ","); got != "5,5.2.1" {
		t.Fatalf("map paths = %s", got)
	}
}

// Noise is the run's own configuration, written against the run's own
// protos. With an upload source but no upload for the recorded release, those
// protos still translate it, as before uploads existed: a run that never
// uploads keeps its verdicts. The bodies are still shown as recorded
// (TestARecordedReleaseWithNoSchemaIsShownAsRecordedAndSaysSo).
func TestNameNoiseFallsBackToTheRunsProtosWithoutAnUpload(t *testing.T) {
	recorded := "1: {\"o-1\"}\n3: {\"Pending\"}\n8: {\n  1: 1795582799\n  2: 5\n}"
	actual := strings.Replace(recorded, "1795582799", "1795590000", 1)
	r := protoReplayer(t)
	r.SetGrpcSchemaSource(&fakeSchemas{})
	pass, _ := compareWithProtos(r, ordersCase(recorded, map[string][]string{"body": {"created_at.seconds"}}), ordersResp(actual))
	if !pass {
		t.Fatal("created_at.seconds is noise in the run's protos and only it differs; the test failed")
	}
	changed := strings.Replace(actual, "Pending", "Shipped", 1)
	pass, res := compareWithProtos(r, ordersCase(recorded, map[string][]string{"body": {"created_at.seconds"}}), ordersResp(changed))
	if pass {
		t.Fatal("status changed and is not noise; the test passed")
	}
	if res.GrpcDecode == nil || res.GrpcDecode.ExpectedJSON != "" || !strings.Contains(res.GrpcDecode.Note, "not uploaded") {
		t.Fatalf("the run's protos must not stand in for the recorded release in the display: %+v", res.GrpcDecode)
	}
}

// Without the run's protos or an upload for the recorded release, a name is
// never translated: the release under test is another release, and its schema
// never reads the recorded side.
func TestNameNoiseIsNeverReadWithTheTargetRelease(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	r.SetGrpcSchemaSource(&fakeSchemas{target: ordersSchema(t, ordersProto)})
	tc := ordersCase("", map[string][]string{"body": {"status"}})
	view := r.grpcProtoViewFor(tc, "set-1")
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, view, "set-1").Noise["body.3"]; ok {
		t.Fatal("status was translated with the target release's schema")
	}
}

// Grading reads both sides with the recorded schema: a rename plus a value
// change grades as the value change it is.
func TestGradingUsesOneSchema(t *testing.T) {
	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	v2 := strings.Replace(ordersProto, "string status = 3;", "string state = 3;", 1)
	r.SetGrpcSchemaSource(&fakeSchemas{
		recorded:      map[string]*protoschema.Schema{"set-1": ordersSchema(t, ordersProto)},
		recordedLabel: "release:app:v1",
		target:        ordersSchema(t, v2),
	})
	_, res := compareWithProtos(r, ordersCase("1: {\"o-1\"}\n3: {\"Pending\"}", nil), ordersResp("1: {\"o-2\"}\n3: {\"Pending\"}"))
	if res.FailureInfo.Risk != models.Medium {
		t.Fatalf("a value change under a renamed field graded %v (%+v)", res.FailureInfo.Risk, res.FailureInfo.Assessment)
	}
}

// Patterns are translated only for strings and plain integers.
func TestPatternReadableKinds(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
service Orders { rpc Read(R) returns (O); }
message R {}
message O { string s = 1; int64 i = 2; bool b = 3; sint32 z = 4; double d = 5; fixed32 f = 6; uint64 u = 7; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	want := map[string]bool{"s": true, "i": true, "u": true, "b": false, "z": false, "d": false, "f": false}
	for name, ok := range want {
		if got := patternReadable(resp.Fields().ByName(protoreflect.Name(name))); got != ok {
			t.Errorf("%s: readable = %v, want %v", name, got, ok)
		}
	}
}

// A recursive message's maps a few levels down are listed (a tree's attrs),
// and several recursive fields cannot make the list explode.
func TestMapFieldPathsFollowRecursiveTypes(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
service Orders { rpc Read(R) returns (Node); }
message R {}
message Node { map<string,string> attrs = 1; repeated Node children = 2; repeated Node more = 3; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	paths := mapFieldPaths(resp)
	got := strings.Join(paths, ",")
	if !strings.Contains(","+got+",", ",1,") || !strings.Contains(","+got+",", ",2.1,") || !strings.Contains(","+got+",", ",2.2.1,") {
		t.Fatalf("a child's attrs are not listed: %v", paths[:min(len(paths), 10)])
	}
}

// A schema whose recursive types reach many fields at every level is not
// walked exhaustively: the walk stops after maxWalkFields fields and says it
// is incomplete.
func TestTheSchemaWalkIsBounded(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
service Orders { rpc Read(R) returns (Expr); }
message R {}
message Expr { Expr a = 1; Expr b = 2; Expr c = 3; Expr d = 4; Expr e = 5; Expr f = 6; Expr g = 7; Expr h = 8; Expr i = 9; Expr j = 10; Expr k = 11; string name = 12; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	start := time.Now()
	w := walkSchema(resp)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the walk took %v", took)
	}
	if w.complete || w.visited != maxWalkFields {
		t.Fatalf("the budget must cut the walk: complete %v, %d fields visited", w.complete, w.visited)
	}
	flat := ordersSchema(t, ordersProto)
	_, flatResp, _ := flat.MessageTypes("/demo.Orders/Read")
	if !walkSchema(flatResp).complete {
		t.Fatal("a small schema's walk says it is incomplete")
	}
}

// Each replay says an invalid noise pattern once, not once per process.
func TestAnInvalidNoisePatternIsSaidOncePerRun(t *testing.T) {
	for run := 0; run < 2; run++ {
		core, logs := observer.New(zap.WarnLevel)
		r := &Replayer{config: &config.Config{}, logger: zap.New(core)}
		r.config.Test.GlobalNoise.Global = map[string]map[string][]string{"body": {"body.1": {"[unclosed"}}}
		for i := 0; i < 3; i++ {
			r.compareGRPCWithSchemas(ordersCase("1: {\"a\"}", nil), ordersResp("1: {\"b\"}"), "set-1", false, false)
		}
		if n := logs.FilterMessageSnippet("does not compile").Len(); n != 1 {
			t.Fatalf("run %d said it %d times, want once", run, n)
		}
	}
}

// A noise learner reading the decoded diff may name these fields whole but
// never reach into them: the replay translates the field and refuses a step
// past it (a map's key, a Struct's key, an Any's packed field). A failed
// test's GrpcDecode carries them.
func TestOpaqueFieldsAreWhereNoiseTranslationStops(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
import "google/protobuf/struct.proto";
import "google/protobuf/any.proto";
import "google/protobuf/timestamp.proto";
service Orders { rpc Read(R) returns (O); }
message R {}
message Inner { map<string,int32> tags = 2; google.protobuf.Any extra = 3; string name = 4; }
message O { map<string,Inner> labels = 5; Inner inner = 6; repeated Inner many = 7; google.protobuf.Struct meta = 8; google.protobuf.Timestamp at = 9; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	got := opaqueFieldNames(resp)
	if strings.Join(got, ",") != "inner.extra,inner.tags,labels,many.extra,many.tags,meta" {
		t.Fatalf("opaque fields = %v", got)
	}
	for _, p := range got {
		if _, ok := fieldNumberPath(resp, p); !ok {
			t.Errorf("%s must translate whole", p)
		}
		if path, ok := fieldNumberPath(resp, p+".k"); ok {
			t.Errorf("%s.k translated to %s; the learner was told it does not", p, path)
		}
	}
	// A step past one names an entry or a key, even when it is spelled like
	// one of the type's own fields or a list index: never widened to the whole
	// field.
	for _, key := range []string{"meta.fields", "inner.extra.value", "inner.extra.type_url", "labels.0", "labels.k.name", "meta.123"} {
		if path, ok := fieldNumberPath(resp, key); ok {
			t.Errorf("%s translated to %s", key, path)
		}
	}
	if path, ok := fieldNumberPath(resp, "many.0.name"); !ok || path != "7.4" {
		t.Errorf("a list index still reads every element: many.0.name = %q %v", path, ok)
	}

	r := &Replayer{config: &config.Config{}, logger: zap.NewNop()}
	r.SetGrpcSchemaSource(&fakeSchemas{recorded: map[string]*protoschema.Schema{"set-1": s}, recordedLabel: "release:app:v1", target: s})
	_, res := compareWithProtos(r, ordersCase("6: {\n  4: {\"a\"}\n}", nil), ordersResp("6: {\n  4: {\"b\"}\n}"))
	if res.GrpcDecode == nil || strings.Join(res.GrpcDecode.OpaqueFields, ",") != strings.Join(got, ",") || !res.GrpcDecode.OpaqueFieldsComplete {
		t.Fatalf("a failed test's decode must name its opaque fields: %+v", res.GrpcDecode)
	}
}

// Struct, Value and ListValue fields are not walked (a Struct recurses through
// Value without end): many of them spend no budget, a map declared after them
// is still listed, and the walk is complete.
func TestWellKnownTypesDoNotSpendTheWalk(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
import "google/protobuf/struct.proto";
service Orders { rpc Read(R) returns (Page); }
message R {}
message Item { google.protobuf.Struct a = 1; google.protobuf.Struct b = 2; google.protobuf.Value v = 3; google.protobuf.ListValue l = 4; }
message Page { Item items = 1; Item featured = 2; Item prev = 3; Item next = 4; Item related = 5; Item more = 6; map<string,string> tags = 9; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	w := walkSchema(resp)
	if !w.complete || w.visited >= maxWalkFields {
		t.Fatalf("complete %v after %d fields", w.complete, w.visited)
	}
	got := "," + strings.Join(w.maps, ",") + ","
	// A Struct's entries, a nested object in an entry, an object in a list in
	// an entry, a Value's object and its list's objects, a ListValue's.
	for _, want := range []string{",9,", ",1.1.1,", ",1.1.1.2.5.1,", ",1.1.1.2.6.1.5.1,", ",1.3.5.1,", ",1.3.6.1.5.1,", ",1.4.1.5.1,"} {
		if !strings.Contains(got, want) {
			t.Fatalf("maps %v lack %s", w.maps, strings.Trim(want, ","))
		}
	}
}

// The well-known types' generated map paths count against the walk budget: a
// recursive type full of Struct fields cannot list millions of them.
func TestWellKnownPathsSpendTheBudget(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
import "google/protobuf/struct.proto";
service Orders { rpc Read(R) returns (Expr); }
message R {}
message Expr { Expr a = 1; Expr b = 2; Expr c = 3; Expr d = 4; Expr e = 5;
  google.protobuf.Struct s1 = 11; google.protobuf.Struct s2 = 12; google.protobuf.Struct s3 = 13; google.protobuf.Struct s4 = 14; google.protobuf.Struct s5 = 15; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	w := walkSchema(resp)
	if len(w.maps) > maxWalkFields || w.visited > maxWalkFields {
		t.Fatalf("%d map paths after %d counted, budget %d", len(w.maps), w.visited, maxWalkFields)
	}
	if w.complete {
		t.Fatal("a walk the budget cut says it is complete")
	}
	if len(w.mapSet) != len(w.maps) {
		t.Fatalf("the set holds %d of %d paths", len(w.mapSet), len(w.maps))
	}
}

// The messages inside map values are walked after every field a name path can
// reach: a recursive map value spending the whole budget hides no later map or
// opaque field, and leaves the walk complete.
func TestMapValuesAreWalkedLast(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
import "google/protobuf/struct.proto";
service Orders { rpc Read(R) returns (O); }
message R {}
message Expr { Expr a = 1; Expr b = 2; Expr c = 3; Expr d = 4; Expr e = 5; Expr f = 6; Expr g = 7; Expr h = 8; Expr i = 9; Expr j = 10; Expr k = 11; }
message O { map<string,Expr> exprs = 1; map<string,string> tags = 2; google.protobuf.Struct meta = 3; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	w := walkSchema(resp)
	if !w.complete || w.visited != maxWalkFields {
		t.Fatalf("complete %v after %d fields: a cut inside a map value leaves every name-reachable field walked", w.complete, w.visited)
	}
	if !slices.Contains(w.maps, "2") || !slices.Contains(w.opaque, "tags") || !slices.Contains(w.opaque, "meta") {
		t.Fatalf("a field after the map was not walked: maps %v, opaque %v", w.maps, w.opaque)
	}
}

// A number names a list element only right after a list: anywhere else it is
// refused, never skipped onto another field or widened to the whole one.
func TestANumberInANamePathIsAListIndexOnly(t *testing.T) {
	s := ordersSchema(t, `syntax = "proto3"; package demo;
import "google/protobuf/struct.proto";
service Orders { rpc Read(R) returns (O); }
message R {}
message Inner { string name = 4; }
message O { Inner inner = 6; repeated Inner many = 7; string name = 4; repeated google.protobuf.Struct docs = 8; }`)
	_, resp, _ := s.MessageTypes("/demo.Orders/Read")
	for key, want := range map[string]string{"many.0.name": "7.4", "many.name": "7.4", "docs.0": "8", "docs": "8"} {
		if got, ok := fieldNumberPath(resp, key); !ok || got != want {
			t.Errorf("%s = %q %v, want %s", key, got, ok, want)
		}
	}
	for _, key := range []string{"inner.3", "6.name", "inner.0.name", "many.0.0", "docs.0.k"} {
		if got, ok := fieldNumberPath(resp, key); ok {
			t.Errorf("%s translated to %s", key, got)
		}
	}
}

// Noise reads the recorded release's schema first: where it and the run's own
// protos name field 3 differently, the recorded name translates.
func TestNameNoisePrefersTheRecordedSchema(t *testing.T) {
	r := protoReplayer(t) // local: field 3 is `status`
	recorded := ordersSchema(t, strings.Replace(ordersProto, "string status = 3;", "string state = 3;", 1))
	r.SetGrpcSchemaSource(&fakeSchemas{recorded: map[string]*protoschema.Schema{"set-1": recorded}, recordedLabel: "release:app:v1"})
	tc := ordersCase("", map[string][]string{"body": {"state", "status"}})
	got := r.withNameNoiseAsFieldNumbers(tc, r.grpcProtoViewFor(tc, "set-1"), "set-1").Noise
	if _, ok := got["body.3"]; !ok {
		t.Fatalf("state is field 3 in the recorded release; not translated: %v", got)
	}
	tc = ordersCase("", map[string][]string{"body": {"status"}})
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, r.grpcProtoViewFor(tc, "set-1"), "set-1").Noise["body.3"]; ok {
		t.Fatal("status, the run's own name, was translated though the recorded release names field 3 state")
	}
}

// An upload that does not define the method (another service's protos) leaves
// the method's noise to the run's own protos.
func TestNameNoiseFallsBackPerMethod(t *testing.T) {
	r := protoReplayer(t)
	other := ordersSchema(t, `syntax = "proto3"; package other; service Billing { rpc Pay(P) returns (P); } message P { string id = 1; }`)
	r.SetGrpcSchemaSource(&fakeSchemas{recorded: map[string]*protoschema.Schema{"set-1": other}, recordedLabel: "release:app:v1"})
	tc := ordersCase("", map[string][]string{"body": {"status"}})
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, r.grpcProtoViewFor(tc, "set-1"), "set-1").Noise["body.3"]; !ok {
		t.Fatal("the recorded upload does not define /demo.Orders/Read; the run's protos must translate its noise")
	}
}
