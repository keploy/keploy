// YAML round-trip tests for the five PostgresV3 mock kinds. The gob
// tests already exist in gob_roundtrip_test.go and catch encode/decode
// regressions in the binary path, but v3 is loaded from mocks.yaml
// in the default configuration. Any drift between the EncodeMock yaml
// envelope and the DecodeMocks reader would silently drop fields (or
// outright fail to parse on NUL bytes / missing yaml tags), and only
// an actual yaml marshal → unmarshal cycle exercises that path.
//
// The sentinel case (PostgresV3NullCell) is called out as its own test:
// its previous value (\x00NULL\x00) was a control character that
// gopkg.in/yaml.v3 rejects. If someone reverts the sentinel to a
// control sequence, that test is the first to fail.
package mockdb

import (
	"errors"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	pyaml "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// yamlRoundTrip encodes the mock through EncodeMock, marshals to yaml
// bytes, unmarshals back to a NetworkTrafficDoc, decodes through
// DecodeMocks, and compares the resulting *models.Mock to the input.
// This exercises the *on-disk* path the replay-time loader takes.
func yamlRoundTrip(t *testing.T, name string, m *models.Mock) *models.Mock {
	t.Helper()
	logger := zap.NewNop()

	doc, err := EncodeMock(m, logger)
	if err != nil {
		t.Fatalf("%s: EncodeMock: %v", name, err)
	}

	// Serialise + deserialise — the real replay-time path reads
	// fresh yaml bytes, so parsing them here is the authentic test.
	buf, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatalf("%s: yaml.Marshal(doc): %v", name, err)
	}
	var back pyaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(buf, &back); err != nil {
		t.Fatalf("%s: yaml.Unmarshal: %v\nbody:\n%s", name, err, string(buf))
	}

	mocks, err := DecodeMocks([]*pyaml.NetworkTrafficDoc{&back}, logger)
	if err != nil {
		t.Fatalf("%s: DecodeMocks: %v", name, err)
	}
	if len(mocks) != 1 {
		t.Fatalf("%s: want 1 mock, got %d", name, len(mocks))
	}
	return mocks[0]
}

func TestYAMLRoundTrip_PostgresV3Session(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Session,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config", "connID": "0"},
			PostgresV3Session: &models.PostgresV3SessionSpec{
				ProtocolVersion:  "3.0",
				SSLResponse:      "N",
				ServerVersion:    "15.17 (Debian 15.17-1.pgdg13+1)",
				ParameterStatus:  map[string]string{"DateStyle": "ISO, MDY", "client_encoding": "UTF8"},
				BackendProcessID: 573,
				BackendSecretKey: -271483429,
				ObservedAuthMode: "scram",
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Session", in)
	if !reflect.DeepEqual(got.Spec.PostgresV3Session, in.Spec.PostgresV3Session) {
		t.Fatalf("session mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Session, got.Spec.PostgresV3Session)
	}
	if !reflect.DeepEqual(got.Spec.Metadata, in.Spec.Metadata) {
		t.Fatalf("metadata mismatch: got %v, want %v", got.Spec.Metadata, in.Spec.Metadata)
	}
}

func TestYAMLRoundTrip_PostgresV3Catalog(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Catalog,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			PostgresV3Catalog: &models.PostgresV3CatalogSpec{
				Schemas: []models.PostgresV3Schema{{
					Name: "public",
					Tables: []models.PostgresV3TableDef{{
						Name: "customer_tag",
						Columns: []models.PostgresV3Column{
							{Name: "id", TypeOID: 20, TypeName: "bigint", NotNull: true, IsPrimary: true, AttNum: 1},
							{Name: "tag", TypeOID: 1043, TypeName: "varchar", NotNull: true, AttNum: 2},
						},
					}},
				}},
				Extensions: []string{"pgcrypto"},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Catalog", in)
	if !reflect.DeepEqual(got.Spec.PostgresV3Catalog, in.Spec.PostgresV3Catalog) {
		t.Fatalf("catalog mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Catalog, got.Spec.PostgresV3Catalog)
	}
}

func TestYAMLRoundTrip_PostgresV3Data(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Data,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			PostgresV3Data: &models.PostgresV3DataSpec{
				Schema:     "public",
				Table:      "customer_tag",
				PrimaryKey: []string{"id"},
				Columns:    []string{"id", "tag", "created_at"},
				Rows: [][]string{
					{"1", "vip", "2026-04-22"},
					{"2", "churn-risk", "2026-04-22"},
				},
				Truncated: false,
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Data", in)
	if !reflect.DeepEqual(got.Spec.PostgresV3Data, in.Spec.PostgresV3Data) {
		t.Fatalf("data mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Data, got.Spec.PostgresV3Data)
	}
}

func TestYAMLRoundTrip_PostgresV3Query(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Query,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks", "class": "APP"},
			PostgresV3Query: &models.PostgresV3QuerySpec{
				Class:         "APP",
				Lifetime:      "perTest",
				Scope:         "session",
				SQLAstHash:    "sha256:abcd",
				SQLNormalized: "select id from customer_tag where id=$1",
				ParamOIDs:     []uint32{20},
				InvocationID:  "sha256:abcd:0",
				BindValues:    []string{"AAAAAQ=="},
				BindFormats:   []int{1},
				Response: &models.PostgresV3Response{
					RowDescription: []models.PostgresV3ColumnDescriptor{
						{Name: "id", TypeOID: 20, TypeSize: 8, TypeMod: -1},
					},
					Rows:            [][]string{{"MQ=="}},
					CommandComplete: "SELECT 1",
				},
				SideEffects: &models.PostgresV3SideEffects{TxTransition: ""},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Query", in)
	if !reflect.DeepEqual(got.Spec.PostgresV3Query, in.Spec.PostgresV3Query) {
		t.Fatalf("query mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Query, got.Spec.PostgresV3Query)
	}
}

// TestYAMLRoundTrip_PostgresV3Query_NullCellSentinel — the reason this
// kind gets a dedicated yaml test. The original sentinel used NUL
// bytes which yaml.v3 rejects outright; a future revert to any
// control-byte-based sentinel must fail here first rather than
// silently at record time when mocks.yaml becomes unwritable.
func TestYAMLRoundTrip_PostgresV3Query_NullCellSentinel(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Query,
		Spec: models.MockSpec{
			PostgresV3Query: &models.PostgresV3QuerySpec{
				Class:         "APP",
				Lifetime:      "perTest",
				Scope:         "session",
				SQLAstHash:    "sha256:null",
				SQLNormalized: "select comment from customer_note where id=$1",
				InvocationID:  "sha256:null:0",
				BindValues:    []string{"AAAAAQ=="},
				BindFormats:   []int{1},
				Response: &models.PostgresV3Response{
					RowDescription: []models.PostgresV3ColumnDescriptor{
						{Name: "comment", TypeOID: 25, TypeSize: -1, TypeMod: -1},
					},
					Rows: [][]string{
						{models.PostgresV3NullCell},
						{"aGVsbG8="},
					},
					CommandComplete: "SELECT 2",
				},
				SideEffects: &models.PostgresV3SideEffects{},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Query-NullCell", in)
	if got.Spec.PostgresV3Query == nil {
		t.Fatal("expected non-nil Query spec after round-trip")
	}
	if len(got.Spec.PostgresV3Query.Response.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got.Spec.PostgresV3Query.Response.Rows))
	}
	if got.Spec.PostgresV3Query.Response.Rows[0][0] != models.PostgresV3NullCell {
		t.Fatalf("NULL sentinel lost in yaml round-trip: got %q, want %q",
			got.Spec.PostgresV3Query.Response.Rows[0][0], models.PostgresV3NullCell)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3Query, in.Spec.PostgresV3Query) {
		t.Fatalf("query mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Query, got.Spec.PostgresV3Query)
	}
}

// TestYAMLRoundTrip_PostgresV3_NilPayloadEncodeRejected pins the
// encode-side nil-payload guard for every PostgresV3 Kind. Writing
// `<field>: null` to disk silently corrupts downstream replay (the
// loader dereferences the typed payload unconditionally), so the
// encoder has to fail fast with the typed sentinel error.
func TestYAMLRoundTrip_PostgresV3_NilPayloadEncodeRejected(t *testing.T) {
	kinds := []models.Kind{
		models.PostgresV3Session,
		models.PostgresV3Catalog,
		models.PostgresV3Data,
		models.PostgresV3Query,
		models.PostgresV3Generator,
	}
	for _, k := range kinds {
		t.Run(string(k), func(t *testing.T) {
			// All payload pointers left nil on purpose.
			in := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: k}
			_, err := EncodeMock(in, zap.NewNop())
			if err == nil {
				t.Fatalf("%s: EncodeMock unexpectedly succeeded on nil payload", k)
			}
			if !errors.Is(err, errPostgresV3NilPayload) {
				t.Fatalf("%s: expected errPostgresV3NilPayload, got: %v", k, err)
			}
		})
	}
}

// TestYAMLRoundTrip_PostgresV3_NilPayloadDecodeRejected pins the
// decode-side guard. A hand-edited mocks.yaml with `session: null`
// (or any other kind's payload field explicitly null) must not load
// silently — downstream engines assume non-nil payloads and would
// NPE at replay time. The decode helper must surface the typed
// sentinel so operators see the actionable next_step in logs.
func TestYAMLRoundTrip_PostgresV3_NilPayloadDecodeRejected(t *testing.T) {
	cases := []struct {
		kind models.Kind
		body string
	}{
		{models.PostgresV3Session, "metadata: {}\nsession: null\n"},
		{models.PostgresV3Catalog, "metadata: {}\ncatalog: null\n"},
		{models.PostgresV3Data, "metadata: {}\ndata: null\n"},
		{models.PostgresV3Query, "metadata: {}\nquery: null\n"},
		{models.PostgresV3Generator, "metadata: {}\ngenerator: null\n"},
	}
	for _, c := range cases {
		t.Run(string(c.kind), func(t *testing.T) {
			var spec yamlLib.Node
			if err := yamlLib.Unmarshal([]byte(c.body), &spec); err != nil {
				t.Fatalf("%s: yaml.Unmarshal fixture: %v", c.kind, err)
			}
			doc := &pyaml.NetworkTrafficDoc{
				Version: "api.keploy.io/v1beta1",
				Kind:    c.kind,
				Spec:    spec,
			}
			_, err := DecodeMocks([]*pyaml.NetworkTrafficDoc{doc}, zap.NewNop())
			if err == nil {
				t.Fatalf("%s: DecodeMocks unexpectedly accepted null payload", c.kind)
			}
			if !errors.Is(err, errPostgresV3NilPayload) {
				t.Fatalf("%s: expected errPostgresV3NilPayload, got: %v", c.kind, err)
			}
		})
	}
}

func TestYAMLRoundTrip_PostgresV3Generator(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3Generator,
		Spec: models.MockSpec{
			PostgresV3Generator: &models.PostgresV3GeneratorSpec{
				Name:           "uuid_generate_v4",
				Type:           "uuid",
				RecordedValues: []string{"6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
				Policy:         "replay",
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Generator", in)
	if !reflect.DeepEqual(got.Spec.PostgresV3Generator, in.Spec.PostgresV3Generator) {
		t.Fatalf("generator mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3Generator, got.Spec.PostgresV3Generator)
	}
}
