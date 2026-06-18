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
	if len(doc.ReqBodyNoise) != 2 {
		t.Fatalf("envelope must carry req_body_noise, got %v", doc.ReqBodyNoise)
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
	if len(doc.ReqBodyNoise) != 1 {
		t.Fatalf("JSON envelope must carry req_body_noise, got %v", doc.ReqBodyNoise)
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

// TestNonHTTPNoise_HTTPLeavesEnvelopeEmpty confirms HTTP keeps carrying its noise
// inside HTTPReq.ReqBodyNoise and never populates the shared envelope field (so
// the two paths can't double-count).
func TestNonHTTPNoise_HTTPLeavesEnvelopeEmpty(t *testing.T) {
	mock := noiseTestMock(`{"a":"b"}`)
	mock.Spec.HTTPReq.ReqBodyNoise = map[string][]string{"body.a": {}}

	doc, err := EncodeMock(mock, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	if len(doc.ReqBodyNoise) != 0 {
		t.Fatalf("HTTP must not use the kind-agnostic envelope field, got %v", doc.ReqBodyNoise)
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
	if !strings.Contains(content, "req_body_noise") || !strings.Contains(content, "body.eventTs") {
		t.Fatalf("non-HTTP learned noise not persisted; file:\n%s", content)
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
