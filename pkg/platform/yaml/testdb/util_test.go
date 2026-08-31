package testdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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

// TestDuplicateOfRoundTripYAML pins the cross-pod duplicate mark's persistence
// through the YAML encode/decode pair for both testcase kinds. The mark rides
// the spec Metadata map, so old readers (which only look up "description")
// ignore it and old files (no metadata) decode to an empty mark.
func TestDuplicateOfRoundTripYAML(t *testing.T) {
	logger := zap.NewNop()

	t.Run("http", func(t *testing.T) {
		tc := httpTestCase()
		tc.Description = "a described case"
		tc.DuplicateOf = "test-set-0/test-3@pod-a"

		doc, err := EncodeTestcase(tc, logger)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := Decode(doc, logger)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DuplicateOf != tc.DuplicateOf {
			t.Fatalf("DuplicateOf lost in yaml round-trip: got %q want %q", got.DuplicateOf, tc.DuplicateOf)
		}
		if got.Description != tc.Description {
			t.Fatalf("Description must survive alongside the mark: got %q want %q", got.Description, tc.Description)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		tc := grpcTestCase()
		tc.DuplicateOf = "test-set-0/test-7@pod-b"

		doc, err := EncodeTestcase(tc, logger)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := Decode(doc, logger)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DuplicateOf != tc.DuplicateOf {
			t.Fatalf("DuplicateOf lost in grpc yaml round-trip: got %q want %q", got.DuplicateOf, tc.DuplicateOf)
		}
	})
}

// TestDuplicateOfRoundTripJSON covers the JSON-native encoder/decoder pair
// (the storage fast path) for both kinds.
func TestDuplicateOfRoundTripJSON(t *testing.T) {
	logger := zap.NewNop()

	t.Run("http", func(t *testing.T) {
		tc := httpTestCase()
		tc.DuplicateOf = "test-set-0/test-3@pod-a"

		doc, err := EncodeTestcaseJSON(tc, logger)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := DecodeJSON(doc, logger)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DuplicateOf != tc.DuplicateOf {
			t.Fatalf("DuplicateOf lost in json round-trip: got %q want %q", got.DuplicateOf, tc.DuplicateOf)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		tc := grpcTestCase()
		tc.DuplicateOf = "test-set-0/test-7@pod-b"

		doc, err := EncodeTestcaseJSON(tc, logger)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		got, err := DecodeJSON(doc, logger)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.DuplicateOf != tc.DuplicateOf {
			t.Fatalf("DuplicateOf lost in grpc json round-trip: got %q want %q", got.DuplicateOf, tc.DuplicateOf)
		}
	})
}

// TestSpecMetadataStaysAbsentWhenUnset pins byte-compatibility with existing
// files: a testcase with neither description nor duplicate mark must encode a
// nil Metadata map, exactly as before the mark existed.
func TestSpecMetadataStaysAbsentWhenUnset(t *testing.T) {
	if md := specMetadata("", ""); md != nil {
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
	if got.DuplicateOf != "" || got.Description != "" {
		t.Fatalf("unknown keys must not bleed into fields: %+v", got)
	}
}
