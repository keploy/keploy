// Tests for the MockYAMLMapper registry contract: built-in kinds
// cannot be overridden, registered mappers drive both EncodeMock and
// DecodeMocks, and malformed registrations are silently dropped.
package mockdb

import (
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func clearRegistry(t *testing.T) {
	t.Helper()
	mockYAMLMappers.Range(func(k, _ any) bool {
		mockYAMLMappers.Delete(k)
		return true
	})
	t.Cleanup(func() {
		mockYAMLMappers.Range(func(k, _ any) bool {
			mockYAMLMappers.Delete(k)
			return true
		})
	})
}

func TestRegisterMockYAMLMapper_RejectsBuiltins(t *testing.T) {
	clearRegistry(t)
	called := false
	mapper := MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { called = true; return nil },
		Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { called = true; return nil },
	}
	for _, k := range []models.Kind{models.HTTP, models.HTTP2, models.GENERIC, models.MySQL, models.Postgres, models.PostgresV2, models.Mongo, models.GRPC_EXPORT, models.DNS} {
		RegisterMockYAMLMapper(k, mapper)
		if _, ok := mockYAMLMappers.Load(k); ok {
			t.Fatalf("built-in kind %q must not be registrable", k)
		}
	}
	if called {
		t.Fatalf("mapper functions must not be invoked during registration")
	}
}

func TestRegisterMockYAMLMapper_IgnoresMalformed(t *testing.T) {
	clearRegistry(t)
	ok := func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil }
	okDec := func(*yaml.NetworkTrafficDoc, *models.Mock) error { return nil }

	RegisterMockYAMLMapper("", MockYAMLMapper{Encode: ok, Decode: okDec})
	RegisterMockYAMLMapper("custom-a", MockYAMLMapper{Decode: okDec})
	RegisterMockYAMLMapper("custom-b", MockYAMLMapper{Encode: ok})

	for _, k := range []models.Kind{"", "custom-a", "custom-b"} {
		if _, ok := mockYAMLMappers.Load(k); ok {
			t.Fatalf("malformed registration for %q must be a no-op", k)
		}
	}
}

func TestEncodeDecodeMock_UsesRegisteredMapper(t *testing.T) {
	clearRegistry(t)
	kind := models.Kind("custom-parser")

	var encoded, decoded bool
	RegisterMockYAMLMapper(kind, MockYAMLMapper{
		Encode: func(mock *models.Mock, doc *yaml.NetworkTrafficDoc) error {
			encoded = true
			if mock.Kind != kind {
				t.Fatalf("encode: want kind %q, got %q", kind, mock.Kind)
			}
			return doc.Spec.Encode(map[string]string{"hello": "world"})
		},
		Decode: func(doc *yaml.NetworkTrafficDoc, mock *models.Mock) error {
			decoded = true
			var payload map[string]string
			if err := doc.Spec.Decode(&payload); err != nil {
				return err
			}
			mock.Spec = models.MockSpec{Metadata: payload}
			return nil
		},
	})

	logger := zap.NewNop()
	doc, err := EncodeMock(&models.Mock{Version: "v1", Name: "m1", Kind: kind}, logger)
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	if !encoded {
		t.Fatalf("registered encode mapper was not invoked")
	}
	if doc.Kind != kind {
		t.Fatalf("doc.Kind: want %q, got %q", kind, doc.Kind)
	}

	got, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, logger)
	if err != nil {
		t.Fatalf("DecodeMocks: %v", err)
	}
	if !decoded {
		t.Fatalf("registered decode mapper was not invoked")
	}
	if len(got) != 1 || got[0].Kind != kind || got[0].Spec.Metadata["hello"] != "world" {
		t.Fatalf("round-trip payload mismatch: %+v", got)
	}
}

func TestEncodeDecodeMock_WrapsMapperError(t *testing.T) {
	clearRegistry(t)
	kind := models.Kind("custom-fail")
	bad := errors.New("mapper blew up")

	RegisterMockYAMLMapper(kind, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return bad },
		Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { return bad },
	})

	logger := zap.NewNop()
	_, err := EncodeMock(&models.Mock{Kind: kind, Name: "m1"}, logger)
	if err == nil || !errors.Is(err, bad) {
		t.Fatalf("EncodeMock: want wrapped mapper error, got %v", err)
	}
	if !containsAll(err.Error(), string(kind)) {
		t.Fatalf("EncodeMock error should mention kind, got %q", err.Error())
	}

	doc := &yaml.NetworkTrafficDoc{Version: "v1", Kind: kind, Name: "m1"}
	_, err = DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, logger)
	if err == nil || !errors.Is(err, bad) {
		t.Fatalf("DecodeMocks: want wrapped mapper error, got %v", err)
	}
	if !containsAll(err.Error(), string(kind), "m1") {
		t.Fatalf("DecodeMocks error should mention kind and name, got %q", err.Error())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
