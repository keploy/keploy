package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	recordedAsked int
}

func (f *fakeSchemas) RecordedSchema(_ context.Context, testSetID string) (*protoschema.Schema, string, string) {
	f.recordedAsked++
	if s := f.recorded[testSetID]; s != nil {
		return s, f.recordedLabel, ""
	}
	return nil, "", "schema not uploaded for release app:v1: run keploy proto upload --release-tag app:v1"
}

func (f *fakeSchemas) TargetSchema(context.Context) (*protoschema.Schema, string) {
	if f.target == nil {
		return nil, ""
	}
	return f.target, "release:app:v2"
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

// With a schema source and no schema uploaded for the recorded release, a
// noise key written by name is not translated with another release's schema.
func TestNameNoiseIsNotTranslatedWithoutTheRecordedSchema(t *testing.T) {
	r := protoReplayer(t) // local protos would translate it
	r.SetGrpcSchemaSource(&fakeSchemas{})
	tc := ordersCase("", map[string][]string{"body": {"status"}})
	view := r.grpcProtoViewFor(tc, "set-1")
	if _, ok := r.withNameNoiseAsFieldNumbers(tc, view, "set-1").Noise["body.3"]; ok {
		t.Fatal("status was translated without the recorded release's schema")
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
	if len(paths) > maxMapFieldPaths {
		t.Fatalf("%d paths, above the cap %d", len(paths), maxMapFieldPaths)
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
