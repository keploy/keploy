package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// genericNoiseMock builds a non-HTTP (Generic-kind) mock whose request lives in
// the wire-encoded GenericRequests payload — exactly the shape (Redis/Kafka/
// Pulsar/Generic) that has no per-protocol struct to hang schema-noise on and
// therefore relies on the kind-agnostic MockSpec.ReqBodyNoise field.
func genericNoiseMock(name string) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    models.GENERIC,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"src": "noisetest"},
			GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: "GET key"}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "value"}}}},
		},
	}
}

// TestNonHTTPNoise_YAMLRoundTrip is the core of the persistence blocker: a
// non-HTTP mock's learned MockSpec.ReqBodyNoise must survive the YAML encode →
// disk → decode cycle. The per-kind GenericSchema envelope has no field for it,
// so it rides on the shared NetworkTrafficDoc.ReqBodyNoise and is restored to
// MockSpec on read.
func TestNonHTTPNoise_YAMLRoundTrip(t *testing.T) {
	mock := genericNoiseMock("mock-0")
	mock.Spec.ReqBodyNoise = map[string][]string{"body.eventTs": {}, "body.traceId": {}}

	doc, err := EncodeMock(mock, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	if doc.Noise == nil || len(doc.Noise.Req) != 2 {
		t.Fatalf("envelope must carry noise.req paths, got %#v", doc.Noise)
	}

	decoded, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocks: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("want 1 decoded mock, got %d", len(decoded))
	}
	got := decoded[0].Spec.ReqBodyNoise
	if _, ok := got["body.eventTs"]; !ok {
		t.Fatalf("body.eventTs lost on YAML round-trip: %v", got)
	}
	if _, ok := got["body.traceId"]; !ok {
		t.Fatalf("body.traceId lost on YAML round-trip: %v", got)
	}
}

// TestNonHTTPNoise_JSONRoundTrip is the JSON-format sibling of the above (Generic
// is JSON-native, so it exercises the EncodeMockJSON / DecodeMocksJSON path).
func TestNonHTTPNoise_JSONRoundTrip(t *testing.T) {
	mock := genericNoiseMock("mock-0")
	mock.Spec.ReqBodyNoise = map[string][]string{"body.eventTs": {}}

	doc, ok, err := EncodeMockJSON(mock, zap.NewNop())
	if err != nil || !ok {
		t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
	}
	if doc.Noise == nil || len(doc.Noise.Req) != 1 {
		t.Fatalf("JSON envelope must carry noise.req paths, got %#v", doc.Noise)
	}

	decoded, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocksJSON: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("want 1 decoded mock, got %d", len(decoded))
	}
	if _, ok := decoded[0].Spec.ReqBodyNoise["body.eventTs"]; !ok {
		t.Fatalf("body.eventTs lost on JSON round-trip: %v", decoded[0].Spec.ReqBodyNoise)
	}
}

// TestNonHTTPNoise_HTTPUsesSameEnvelope confirms HTTP now stores schema-noise on
// the SAME kind-agnostic MockSpec.ReqBodyNoise as every other parser, and that it
// rides through the shared top-level envelope on encode and round-trips back on
// decode — i.e. the storage is uniform across protocols.
func TestNonHTTPNoise_HTTPUsesSameEnvelope(t *testing.T) {
	mock := noiseTestMock(`{"a":"b"}`)
	mock.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}

	doc, err := EncodeMock(mock, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	if !reqNoiseHas(doc.Noise, "body.a") {
		t.Fatalf("HTTP noise must ride noise.req, got %#v", doc.Noise)
	}

	decoded, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocks: %v", err)
	}
	if _, ok := decoded[0].Spec.ReqBodyNoise["body.a"]; !ok {
		t.Fatalf("HTTP noise lost on round-trip: %v", decoded[0].Spec.ReqBodyNoise)
	}
}

// TestPersistMockNoise_NonHTTP is the end-to-end persistence proof: a Generic
// mock that learns noise via the no-prune PersistMockNoise path (detection
// without --remove-unused-mocks) must have it written to disk under
// req_body_noise. Previously this path skipped every non-HTTP mock, silently
// discarding the learned noise at process exit.
func TestPersistMockNoise_NonHTTP(t *testing.T) {
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "")
	ctx := context.Background()
	testSet := "test-set-1"

	if err := ys.InsertMock(ctx, genericNoiseMock("ignored"), testSet); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ys.PersistMockNoise(ctx, testSet, map[string]models.MockState{
		"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.eventTs": {}}},
	}); err != nil {
		t.Fatalf("PersistMockNoise: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, testSet, "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "noise:") || !strings.Contains(content, "req:") || !strings.Contains(content, "body.eventTs") {
		t.Fatalf("non-HTTP learned noise not persisted under noise.req; file:\n%s", content)
	}
	if strings.Contains(content, "req_body_noise") {
		t.Fatalf("legacy req_body_noise key must no longer be written; file:\n%s", content)
	}

	// And it must round-trip back into MockSpec.ReqBodyNoise on read.
	reader, err := yaml.NewMockReaderF(ctx, zap.NewNop(), filepath.Join(dir, testSet), "mocks", yaml.FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var docs []*yaml.NetworkTrafficDoc
	for {
		doc, derr := reader.ReadNextDoc()
		if derr != nil {
			break
		}
		docs = append(docs, doc)
	}
	mocks, err := DecodeMocks(docs, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if len(mocks) != 1 {
		t.Fatalf("want 1 mock back, got %d", len(mocks))
	}
	if _, ok := mocks[0].Spec.ReqBodyNoise["body.eventTs"]; !ok {
		t.Fatalf("persisted non-HTTP noise did not round-trip: %v", mocks[0].Spec.ReqBodyNoise)
	}
}

// reqNoiseHas reports whether the unified noise block lists the given request-body
// field path under noise.req.
func reqNoiseHas(n *yaml.DocNoise, path string) bool {
	if n == nil {
		return false
	}
	for _, p := range n.Req {
		if p == path {
			return true
		}
	}
	return false
}

// TestLegacyNoiseFormats_StillDecode is the backward-compatibility guard: a mock
// written in the OLD on-disk shape — a top-level `noise:` string list (obfuscator
// value-regexes) plus a separate `req_body_noise:` map (with per-path regex values)
// — must still decode. The legacy noise list folds into value-noise (Mock.Noise),
// the req_body_noise keys fold into MockSpec.ReqBodyNoise, and the now-unused regex
// values are dropped.
func TestLegacyNoiseFormats_StillDecode(t *testing.T) {
	raw := []byte(`version: api.keploy.io/v1beta1
kind: Http
name: legacy-1
spec:
  metadata: {}
  req:
    method: POST
    url: http://x/y
    header:
      Content-Type: application/json
    body: '{"a":"b"}'
  resp:
    status_code: 200
    header: {}
    body: '{"ok":true}'
noise:
  - "^tok-.*$"
req_body_noise:
  body.a: ["^x.*$"]
  body.b: []
`)

	doc, err := yaml.UnmarshalDoc(yaml.FormatYAML, raw)
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	// Legacy bare `noise:` list must decode into the unified block's value-noise.
	if got := doc.Noise.ValueNoise(); len(got) != 1 || got[0] != "^tok-.*$" {
		t.Fatalf("legacy noise list must decode into value noise, got %v", got)
	}

	mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocks: %v", err)
	}
	if len(mocks) != 1 {
		t.Fatalf("want 1 decoded mock, got %d", len(mocks))
	}
	m := mocks[0]

	if len(m.Noise) != 1 || m.Noise[0] != "^tok-.*$" {
		t.Fatalf("Mock.Noise must come from the legacy noise list, got %v", m.Noise)
	}
	rb := m.Spec.ReqBodyNoise
	if _, ok := rb["body.a"]; !ok {
		t.Fatalf("legacy req_body_noise key body.a lost on decode: %v", rb)
	}
	if _, ok := rb["body.b"]; !ok {
		t.Fatalf("legacy req_body_noise key body.b lost on decode: %v", rb)
	}
	// The legacy per-path regex value must be dropped (req noise is path-only now).
	if len(rb["body.a"]) != 0 {
		t.Fatalf("legacy regex value on body.a must be dropped, got %v", rb["body.a"])
	}
}
