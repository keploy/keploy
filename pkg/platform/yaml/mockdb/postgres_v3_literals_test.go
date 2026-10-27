package mockdb

// PostgresV3QuerySpec.Literals carries the constants an extended-protocol
// statement's SQL text holds, which SQLNormalized has as $N placeholders.
// These tests pin that it survives every on-disk path (yaml, json, gob) with
// its order, its text values and a NULL constant, and that a mock written
// before the field existed loads unchanged and is written back without it.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	pyaml "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// literalsMock is a recording of an extended-protocol statement whose SQL
// held three constants: a string, a NULL and a number, as their text values.
// The number is a string cell ("11"): the text the SQL spelled, which yaml
// would read back as an integer if it lost the cell's type.
func literalsMock() *models.Mock {
	return &models.Mock{
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
					SQLNormalized: "SELECT id FROM users WHERE name = $1 AND note IS NOT DISTINCT FROM $2 LIMIT $3",
					InvocationID:  "0:7",
					Literals:      models.PostgresV3Cells{models.NewValueCell("alice"), models.NullCell(), models.NewValueCell("11")},
					Response: &models.PostgresV3Response{
						RowDescription:  []models.PostgresV3ColumnDescriptor{{Name: "id", TypeOID: 20, TypeSize: 8, TypeMod: -1}},
						Rows:            []models.PostgresV3Cells{{models.NewValueCell(int64(1))}},
						CommandComplete: "SELECT 1",
					},
				},
			},
		},
	}
}

func TestPostgresV3QueryLiterals_RoundTripOnEveryPath(t *testing.T) {
	want := literalsMock().Spec.PostgresV3.Query.Literals
	check := func(path string, got *models.Mock) {
		t.Helper()
		if got.Spec.PostgresV3 == nil || got.Spec.PostgresV3.Query == nil {
			t.Fatalf("%s: the query spec did not come back", path)
		}
		if lits := got.Spec.PostgresV3.Query.Literals; !reflect.DeepEqual(lits, want) {
			t.Fatalf("%s: literals came back as %#v; want %#v", path, lits, want)
		}
		if !got.Spec.PostgresV3.Query.Literals[1].IsNull() {
			t.Fatalf("%s: the NULL constant came back as %#v, not NULL", path, got.Spec.PostgresV3.Query.Literals[1])
		}
	}

	t.Run("yaml", func(t *testing.T) {
		check("yaml", yamlRoundTrip(t, "PostgresV3QueryLiterals", literalsMock()))
	})

	t.Run("json", func(t *testing.T) {
		doc, ok, err := EncodeMockJSON(literalsMock(), zap.NewNop())
		if err != nil || !ok {
			t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		var back pyaml.NetworkTrafficDocJSON
		if err := json.Unmarshal(body, &back); err != nil {
			t.Fatalf("json.Unmarshal: %v\n%s", err, body)
		}
		got, err := DecodeMocksJSON([]*pyaml.NetworkTrafficDocJSON{&back}, zap.NewNop())
		if err != nil || len(got) != 1 {
			t.Fatalf("DecodeMocksJSON: %d mocks, err %v", len(got), err)
		}
		check("json", got[0])
	})

	t.Run("gob", func(t *testing.T) {
		roundTrip(t, "PostgresV3QueryLiterals", literalsMock())
	})
}

// A mock written before Literals existed, as it is on disk: it loads with no
// literals and every other field as before, and is written back without a
// literals key.
func TestPostgresV3QueryLiterals_AnOlderMockLoadsUnchanged(t *testing.T) {
	const older = `version: api.keploy.io/v1beta1
kind: PostgresV3
name: mock-3
spec:
    metadata:
        class: APP
        type: mocks
    postgresV3:
        type: query
        query:
            class: APP
            lifetime: perTest
            sqlAstHash: sha256:abcd
            sqlNormalized: SELECT id FROM users WHERE name = $1
            paramOids:
                - 25
            invocationId: "0:3"
            bindValues:
                - alice
            bindFormats:
                - 0
            response:
                commandComplete: SELECT 1
    reqTimestampMock: 2026-10-16T23:27:37.97284862Z
    resTimestampMock: 2026-10-16T23:27:37.973176621Z
`
	var doc pyaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal([]byte(older), &doc); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	mocks, err := DecodeMocks([]*pyaml.NetworkTrafficDoc{&doc}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("DecodeMocks: %d mocks, err %v", len(mocks), err)
	}
	q := mocks[0].Spec.PostgresV3.Query
	if q.Literals != nil {
		t.Fatalf("an older mock loaded with literals %#v; want none", q.Literals)
	}
	if q.SQLNormalized != "SELECT id FROM users WHERE name = $1" || q.InvocationID != "0:3" ||
		!reflect.DeepEqual(q.ParamOIDs, []uint32{25}) || len(q.BindValues) != 1 || q.BindValues[0].Value != "alice" ||
		!reflect.DeepEqual(q.BindFormats, []int{0}) || q.Response == nil || q.Response.CommandComplete != "SELECT 1" {
		t.Fatalf("an older mock loaded differently: %#v", q)
	}

	back, err := EncodeMock(mocks[0], zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	body, err := yamlLib.Marshal(back)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(body), "literals") {
		t.Fatalf("an older mock was written back with a literals key:\n%s", body)
	}
	jdoc, ok, err := EncodeMockJSON(mocks[0], zap.NewNop())
	if err != nil || !ok {
		t.Fatalf("EncodeMockJSON: ok=%v err=%v", ok, err)
	}
	jbody, err := json.Marshal(jdoc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(jbody), "literals") {
		t.Fatalf("an older mock was written back to json with a literals key:\n%s", jbody)
	}
}
