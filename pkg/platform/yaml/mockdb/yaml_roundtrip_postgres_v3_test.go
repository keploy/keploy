// YAML round-trip tests for the single PostgresV3 Kind with its five
// discriminated sub-types. The gob tests already exist in
// gob_roundtrip_test.go and catch encode/decode regressions in the
// binary path, but v3 is loaded from mocks.yaml in the default
// configuration. Any drift between the EncodeMock yaml envelope and
// the DecodeMocks reader would silently drop fields (or outright fail
// to parse on NUL bytes / missing yaml tags), and only an actual yaml
// marshal → unmarshal cycle exercises that path.
//
// The NULL-cell case is called out as its own test: prior revisions
// used string sentinels (\x00NULL\x00, then ~~KEPLOY_PG_NULL~~) that
// repeatedly caused yaml.v3 edge-case failures. The current
// representation uses PostgresV3Cell.IsNull + native YAML null, which
// gives a round-trip distinction between SQL NULL and empty string
// without escape hacks. That test is the regression canary if the
// struct-level encoding ever slips back to a string-sentinel scheme.
package mockdb

import (
	"errors"
	"reflect"
	"strings"
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
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config", "connID": "0"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeSession,
				Session: &models.PostgresV3SessionSpec{
					ProtocolVersion:  "3.0",
					SSLResponse:      "N",
					ServerVersion:    "15.17 (Debian 15.17-1.pgdg13+1)",
					ParameterStatus:  map[string]string{"DateStyle": "ISO, MDY", "client_encoding": "UTF8"},
					BackendProcessID: 573,
					BackendSecretKey: -271483429,
					ObservedAuthMode: "scram",
				},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Session", in)
	if got.Spec.PostgresV3 == nil {
		t.Fatal("got.Spec.PostgresV3 is nil after round-trip")
	}
	if got.Spec.PostgresV3.Type != models.PostgresV3TypeSession {
		t.Errorf("Type mismatch: got %q, want %q", got.Spec.PostgresV3.Type, models.PostgresV3TypeSession)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3.Session, in.Spec.PostgresV3.Session) {
		t.Fatalf("session mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Session, got.Spec.PostgresV3.Session)
	}
	// Only the Type-matching sub-pointer must be populated; the rest
	// must be nil after round-trip.
	if got.Spec.PostgresV3.Catalog != nil || got.Spec.PostgresV3.Data != nil ||
		got.Spec.PostgresV3.Query != nil || got.Spec.PostgresV3.Generator != nil {
		t.Errorf("non-Session sub-pointers unexpectedly populated: %#v", got.Spec.PostgresV3)
	}
	if !reflect.DeepEqual(got.Spec.Metadata, in.Spec.Metadata) {
		t.Fatalf("metadata mismatch: got %v, want %v", got.Spec.Metadata, in.Spec.Metadata)
	}
}

func TestYAMLRoundTrip_PostgresV3Catalog(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeCatalog,
				Catalog: &models.PostgresV3CatalogSpec{
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
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Catalog", in)
	if got.Spec.PostgresV3 == nil || got.Spec.PostgresV3.Type != models.PostgresV3TypeCatalog {
		t.Fatalf("Type/spec mismatch: got %#v", got.Spec.PostgresV3)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3.Catalog, in.Spec.PostgresV3.Catalog) {
		t.Fatalf("catalog mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Catalog, got.Spec.PostgresV3.Catalog)
	}
}

func TestYAMLRoundTrip_PostgresV3Data(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeData,
				Data: &models.PostgresV3DataSpec{
					Schema:     "public",
					Table:      "customer_tag",
					PrimaryKey: []string{"id"},
					Columns:    []string{"id", "tag", "created_at"},
					Rows: []models.PostgresV3Cells{
						{models.NewValueCell("1"), models.NewValueCell("vip"), models.NewValueCell("2026-04-22")},
						{models.NewValueCell("2"), models.NewValueCell("churn-risk"), models.NewValueCell("2026-04-22")},
					},
					Truncated: false,
				},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Data", in)
	if got.Spec.PostgresV3 == nil || got.Spec.PostgresV3.Type != models.PostgresV3TypeData {
		t.Fatalf("Type/spec mismatch: got %#v", got.Spec.PostgresV3)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3.Data, in.Spec.PostgresV3.Data) {
		t.Fatalf("data mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Data, got.Spec.PostgresV3.Data)
	}
}

func TestYAMLRoundTrip_PostgresV3Query(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks", "class": "APP"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					Class:         "APP",
					Lifetime:      "perTest",
					SQLAstHash:    "sha256:abcd",
					SQLNormalized: "select id from customer_tag where id=$1",
					ParamOIDs:     []uint32{20},
					InvocationID:  "0:0",
					// Logical int64 bind for the bigint $1 — BindFormats
					// records that the wire format was binary; the cell
					// itself stays in logical form so the replayer can
					// re-encode it to match whatever format the live
					// client selects at Bind time.
					BindValues:    models.PostgresV3Cells{models.NewValueCell(int64(1))},
					BindFormats:   []int{1},
					ResultFormats: []int{1}, // binary int4 — the lib/pq RETURNING id shape; lost format codes broke round 4 listmonk validation
					Response: &models.PostgresV3Response{
						RowDescription: []models.PostgresV3ColumnDescriptor{
							{Name: "id", TypeOID: 20, TypeSize: 8, TypeMod: -1},
						},
						Rows:            []models.PostgresV3Cells{{models.NewValueCell("1")}},
						CommandComplete: "SELECT 1",
					},
					SideEffects: &models.PostgresV3SideEffects{TxTransition: ""},
				},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Query", in)
	if got.Spec.PostgresV3 == nil || got.Spec.PostgresV3.Type != models.PostgresV3TypeQuery {
		t.Fatalf("Type/spec mismatch: got %#v", got.Spec.PostgresV3)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3.Query, in.Spec.PostgresV3.Query) {
		t.Fatalf("query mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Query, got.Spec.PostgresV3.Query)
	}
}

// TestYAMLRoundTrip_PostgresV3_WireShape pins the on-disk yaml shape
// the wave 3 refactor produces. If someone re-nests the envelope,
// this test fails with a visible diff — regression canary for
// analytics / search consumers that read mocks.yaml directly.
func TestYAMLRoundTrip_PostgresV3_WireShape(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					SQLAstHash:    "sha256:shape",
					SQLNormalized: "select 1",
					InvocationID:  "0:0",
				},
			},
		},
	}
	doc, err := EncodeMock(in, zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	buf, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	body := string(buf)
	// Kind line must be the single top-level form.
	if !strings.Contains(body, "kind: PostgresV3\n") {
		t.Fatalf("expected top-level `kind: PostgresV3`, body:\n%s", body)
	}
	// Spec must carry `postgresV3:` + `type: query`.
	if !strings.Contains(body, "postgresV3:") {
		t.Fatalf("expected `postgresV3:` block under spec, body:\n%s", body)
	}
	if !strings.Contains(body, "type: query") {
		t.Fatalf("expected `type: query` discriminator, body:\n%s", body)
	}
	// Legacy per-sub-type top-level-in-spec keys MUST be gone.
	for _, legacy := range []string{
		"postgresV3Session:", "postgresV3Catalog:", "postgresV3Data:",
		"postgresV3Query:", "postgresV3Generator:",
	} {
		if strings.Contains(body, legacy) {
			t.Errorf("legacy yaml key %q leaked into post-wave-3 output, body:\n%s", legacy, body)
		}
	}
}

// TestYAMLRoundTrip_PostgresV3Query_NullCell_IsNullMarker — the reason
// this sub-type gets a dedicated yaml test. The current encoding marks
// SQL NULL via PostgresV3Cell.IsNull and emits a native YAML null on
// disk (no string sentinel). Earlier revisions used NUL-byte and then
// printable string sentinels; both were retired. A future regression
// that re-introduces a string- or control-byte-based sentinel must
// fail here first rather than silently at record time when mocks.yaml
// becomes unwritable.
func TestYAMLRoundTrip_PostgresV3Query_NullCell_IsNullMarker(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					Class:         "APP",
					Lifetime:      "perTest",
					SQLAstHash:    "sha256:null",
					SQLNormalized: "select comment from customer_note where id=$1",
					ParamOIDs:     []uint32{20},
					InvocationID:  "0:0",
					BindValues:    models.PostgresV3Cells{models.NewValueCell(int64(1))},
					BindFormats:   []int{1},
					Response: &models.PostgresV3Response{
						RowDescription: []models.PostgresV3ColumnDescriptor{
							{Name: "comment", TypeOID: 25, TypeSize: -1, TypeMod: -1},
						},
						// Row 0 is SQL NULL (Cell.IsNull=true), row 1 is the
						// text "hello". The Postgres-semantic distinction
						// between NULL and '' is the whole reason the Cell
						// type exists.
						Rows: []models.PostgresV3Cells{
							{models.NullCell()},
							{models.NewValueCell("hello")},
						},
						CommandComplete: "SELECT 2",
					},
					SideEffects: &models.PostgresV3SideEffects{},
				},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Query-NullCell", in)
	// Peel the nested structure one layer at a time so a future
	// regression that drops (say) Response would fail with a clear
	// "Response is nil" message instead of panicking on a nil-ptr
	// dereference inside the Rows indexing. t.Fatal stops the test
	// before any dependent assertions run.
	if got.Spec.PostgresV3 == nil {
		t.Fatal("expected non-nil PostgresV3 spec after round-trip")
	}
	q := got.Spec.PostgresV3.Query
	if q == nil {
		t.Fatal("expected non-nil Query spec after round-trip")
	}
	if q.Response == nil {
		t.Fatal("expected non-nil Query.Response after round-trip (a dropped Response would otherwise NPE below)")
	}
	if len(q.Response.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(q.Response.Rows))
	}
	if len(q.Response.Rows[0]) == 0 {
		t.Fatal("Rows[0] is empty; expected the NULL cell")
	}
	if !q.Response.Rows[0][0].IsNull() {
		t.Fatalf("NULL cell lost in yaml round-trip: got %+v, want IsNull()=true", q.Response.Rows[0][0])
	}
	if q.Response.Rows[1][0].IsNull() {
		t.Fatalf("Rows[1][0]: want text cell, got NULL")
	}
	if s, ok := q.Response.Rows[1][0].Value.(string); !ok || s != "hello" {
		t.Fatalf("Rows[1][0]: want text \"hello\", got %+v", q.Response.Rows[1][0])
	}
	if !reflect.DeepEqual(q, in.Spec.PostgresV3.Query) {
		t.Fatalf("query mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Query, q)
	}
}

// TestYAMLRoundTrip_PostgresV3_NilPayloadEncodeRejected pins the
// encode-side nil-payload guard for every PostgresV3 sub-type. Writing
// a mock with Type set but no matching sub-pointer silently corrupts
// downstream replay (the loader dereferences the typed payload
// unconditionally), so the encoder has to fail fast with the typed
// sentinel error.
func TestYAMLRoundTrip_PostgresV3_NilPayloadEncodeRejected(t *testing.T) {
	types := []string{
		models.PostgresV3TypeSession,
		models.PostgresV3TypeCatalog,
		models.PostgresV3TypeData,
		models.PostgresV3TypeQuery,
		models.PostgresV3TypeGenerator,
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			// Type set but all sub-pointers left nil on purpose.
			in := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.PostgresV3,
				Spec: models.MockSpec{
					PostgresV3: &models.PostgresV3Spec{Type: typ},
				},
			}
			_, err := EncodeMock(in, zap.NewNop())
			if err == nil {
				t.Fatalf("%s: EncodeMock unexpectedly succeeded on nil sub-payload", typ)
			}
			if !errors.Is(err, errPostgresV3NilPayload) {
				t.Fatalf("%s: expected errPostgresV3NilPayload, got: %v", typ, err)
			}
		})
	}

	// Also pin the outer-nil case: Spec.PostgresV3 itself absent.
	in := &models.Mock{Version: "api.keploy.io/v1beta1", Kind: models.PostgresV3}
	_, err := EncodeMock(in, zap.NewNop())
	if err == nil {
		t.Fatal("EncodeMock unexpectedly succeeded with nil Spec.PostgresV3")
	}
	if !errors.Is(err, errPostgresV3NilPayload) {
		t.Fatalf("expected errPostgresV3NilPayload for nil Spec.PostgresV3, got: %v", err)
	}
}

// TestYAMLRoundTrip_PostgresV3_NilPayloadDecodeRejected pins the
// decode-side guard. A hand-edited mocks.yaml with `session: null`
// (or any other sub-type's payload field explicitly null) must not
// load silently — downstream engines assume non-nil payloads and
// would NPE at replay time. The decode helper must surface the typed
// sentinel so operators see the actionable next_step in logs.
func TestYAMLRoundTrip_PostgresV3_NilPayloadDecodeRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"session", "metadata: {}\npostgresV3:\n  type: session\n  session: null\n"},
		{"catalog", "metadata: {}\npostgresV3:\n  type: catalog\n  catalog: null\n"},
		{"data", "metadata: {}\npostgresV3:\n  type: data\n  data: null\n"},
		{"query", "metadata: {}\npostgresV3:\n  type: query\n  query: null\n"},
		{"generator", "metadata: {}\npostgresV3:\n  type: generator\n  generator: null\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var spec yamlLib.Node
			if err := yamlLib.Unmarshal([]byte(c.body), &spec); err != nil {
				t.Fatalf("%s: yaml.Unmarshal fixture: %v", c.name, err)
			}
			doc := &pyaml.NetworkTrafficDoc{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.PostgresV3,
				Spec:    spec,
			}
			_, err := DecodeMocks([]*pyaml.NetworkTrafficDoc{doc}, zap.NewNop())
			if err == nil {
				t.Fatalf("%s: DecodeMocks unexpectedly accepted null payload", c.name)
			}
			if !errors.Is(err, errPostgresV3NilPayload) {
				t.Fatalf("%s: expected errPostgresV3NilPayload, got: %v", c.name, err)
			}
		})
	}
}

// TestYAMLRoundTrip_PostgresV3_TabBearingFieldsSurvive reproduces the
// echo-sql pipeline failure on this branch: when a recorded mock
// contains a string field with embedded tabs or a leading newline,
// yaml.v3 v3.0.1 emits a literal block scalar (`|N-`) whose
// indentation indicator races with the surrounding sequence offset
// and the same document fails to re-parse with "found a tab character
// where an indentation space is expected". sanitizeYAMLStringNodes
// rewrites those scalars to DoubleQuotedStyle in EncodeMock; this
// test pins the contract by feeding the suspect shapes through every
// PostgresV3 string field that can carry user-controlled text.
func TestYAMLRoundTrip_PostgresV3_TabBearingFieldsSurvive(t *testing.T) {
	tabSQL := "SELECT\n\tid,\n\tname\nFROM\n\tcustomer_tag\nWHERE\n\tid = $1"
	leadingNlTab := "\n\thello"
	tabbedNotice := "Multi-line notice:\n\tdetail with tab\n\tmore detail"

	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					Class:         "APP",
					Lifetime:      "perTest",
					SQLAstHash:    "sha256:tab",
					SQLNormalized: tabSQL, // multiline SQL with tabs
					ParamOIDs:     []uint32{20},
					InvocationID:  "0:0",
					BindValues:    models.PostgresV3Cells{models.NewValueCell(int64(1))},
					BindFormats:   []int{1},
					Response: &models.PostgresV3Response{
						RowDescription: []models.PostgresV3ColumnDescriptor{
							{Name: "id", TypeOID: 20, TypeSize: 8, TypeMod: -1},
							{Name: "tag", TypeOID: 25, TypeSize: -1, TypeMod: -1},
						},
						// Cell values that a real DB column could legitimately
						// return — markdown / templated text often has these.
						Rows: []models.PostgresV3Cells{
							{models.NewValueCell(int64(1)), models.NewValueCell(leadingNlTab)},
							{models.NewValueCell(int64(2)), models.NewValueCell("plain")},
						},
						CommandComplete: "SELECT 2",
						Notices: []models.PostgresV3Notice{
							{Severity: "NOTICE", Code: "00000", Message: tabbedNotice, Detail: leadingNlTab},
						},
					},
					SideEffects: &models.PostgresV3SideEffects{},
				},
			},
		},
	}

	got := yamlRoundTrip(t, "PostgresV3-TabBearing", in)
	if got.Spec.PostgresV3 == nil {
		t.Fatal("expected non-nil PostgresV3 spec")
	}
	q := got.Spec.PostgresV3.Query
	if q == nil {
		t.Fatal("expected non-nil Query spec")
	}
	if q.SQLNormalized != tabSQL {
		t.Errorf("SQLNormalized lost data:\n got %q\nwant %q", q.SQLNormalized, tabSQL)
	}
	if q.Response == nil {
		t.Fatal("Response went nil after round-trip")
	}
	if len(q.Response.Rows) != 2 {
		t.Fatalf("Rows: want 2, got %d", len(q.Response.Rows))
	}
	if s, _ := q.Response.Rows[0][1].Value.(string); s != leadingNlTab {
		t.Errorf("Rows[0][1] (leading nl+tab cell) = %q, want %q", s, leadingNlTab)
	}
	if len(q.Response.Notices) != 1 {
		t.Fatalf("Notices: want 1, got %d", len(q.Response.Notices))
	}
	if q.Response.Notices[0].Message != tabbedNotice {
		t.Errorf("Notice.Message = %q, want %q", q.Response.Notices[0].Message, tabbedNotice)
	}
	if q.Response.Notices[0].Detail != leadingNlTab {
		t.Errorf("Notice.Detail = %q, want %q", q.Response.Notices[0].Detail, leadingNlTab)
	}
}

func TestYAMLRoundTrip_PostgresV3Generator(t *testing.T) {
	in := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeGenerator,
				Generator: &models.PostgresV3GeneratorSpec{
					Name:           "uuid_generate_v4",
					Type:           "uuid",
					RecordedValues: []string{"6ba7b810-9dad-11d1-80b4-00c04fd430c8"},
					Policy:         "replay",
				},
			},
		},
	}
	got := yamlRoundTrip(t, "PostgresV3Generator", in)
	if got.Spec.PostgresV3 == nil || got.Spec.PostgresV3.Type != models.PostgresV3TypeGenerator {
		t.Fatalf("Type/spec mismatch: got %#v", got.Spec.PostgresV3)
	}
	if !reflect.DeepEqual(got.Spec.PostgresV3.Generator, in.Spec.PostgresV3.Generator) {
		t.Fatalf("generator mismatch:\n in  %#v\n got %#v", in.Spec.PostgresV3.Generator, got.Spec.PostgresV3.Generator)
	}
}

// TestDecodeMocks_PostgresV3_BackwardCompat_ScopeFieldIgnored pins the
// READ-side backward compatibility contract for old recordings that
// still stamp the retired `scope` key inside PostgresV3QuerySpec and
// PostgresV3SessionSpec metadata. The struct no longer carries a Scope
// field (removed after pool routing moved to lifetime-first), but
// mocks.yaml produced by prior builds routinely contain lines like
//
//	spec:
//	  postgresV3:
//	    query:
//	      lifetime: perTest
//	      scope: session    # <- retired key; must load cleanly
//	      sqlAstHash: sha256:...
//
// The reader path (DecodeMocks → yamlLib.Unmarshal) must accept these
// silently — gopkg.in/yaml.v3 is non-strict by default, so unknown
// keys are skipped and the struct fills with only the known fields.
// If a future caller flips on Decoder.KnownFields(true) for any reason
// this test fails LOUD.
//
// The fixture below mirrors a real fkppl customer360 debug-bundle
// mocks.yaml fragment (see
// /tmp/auto-replay-issue/debug-bundle-sap-demo-customer360-...).
func TestDecodeMocks_PostgresV3_BackwardCompat_ScopeFieldIgnored(t *testing.T) {
	const oldYAMLWithScope = `
metadata:
  type: mocks
  class: SELECT
  lifetime: perTest
  scope: session
postgresV3:
  type: query
  query:
    class: SELECT
    lifetime: perTest
    scope: session
    sqlAstHash: sha256:a66bf255653587dd476dfd0524632134bc0623f293ee93d2bf2666447cd2b4d1
    sqlNormalized: "select 1"
    invocationId: sha256:abcd:0
    response:
      commandComplete: "SELECT 1"
    sideEffects: {}
`
	var spec yamlLib.Node
	if err := yamlLib.Unmarshal([]byte(oldYAMLWithScope), &spec); err != nil {
		t.Fatalf("yaml.Unmarshal fixture: %v", err)
	}
	doc := &pyaml.NetworkTrafficDoc{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Name:    "mock-legacy-scope",
		Spec:    spec,
	}
	mocks, err := DecodeMocks([]*pyaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocks returned error on legacy `scope:` recording — "+
			"backward-compat broke. err=%v", err)
	}
	if len(mocks) != 1 {
		t.Fatalf("want 1 mock, got %d", len(mocks))
	}
	q := mocks[0].Spec.PostgresV3
	if q == nil || q.Query == nil {
		t.Fatalf("Spec.PostgresV3.Query unexpectedly nil: %#v", mocks[0].Spec)
	}
	if q.Query.Lifetime != "perTest" {
		t.Fatalf("Query.Lifetime: want %q, got %q", "perTest", q.Query.Lifetime)
	}
	if q.Query.SQLAstHash == "" {
		t.Fatalf("Query.SQLAstHash: want non-empty, got empty — known keys stopped parsing")
	}
	if q.Query.InvocationID != "sha256:abcd:0" {
		t.Fatalf("Query.InvocationID: want %q, got %q", "sha256:abcd:0", q.Query.InvocationID)
	}
}
