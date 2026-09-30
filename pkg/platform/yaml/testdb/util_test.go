package testdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
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
