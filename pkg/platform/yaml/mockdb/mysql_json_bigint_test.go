package mockdb

import (
	"encoding/json"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// DecodeMocksJSON lands each MySQL packet body in a map[string]any before
// retypeMySQLResponse re-marshals it into the concrete packet struct.
// encoding/json's reflective default puts every number in that map as a
// float64, so without UseNumber a BIGINT UNSIGNED column is destroyed in
// the intermediate hop: 18446744073709551615 becomes 1.8446744073709552e+19
// and re-marshals as 18446744073709552000, a literal now larger than
// MaxUint64 that nothing downstream can recover.
//
// This is not confined to replay. UpdateMocks is a read-modify-write of
// the user's mock file: it decodes through here, drops unused mocks and
// writes the survivors back. So one `keploy test --remove-unused-mocks`
// run rewrites 18446744073709551615 on disk as 18446744073709552000 —
// permanent corruption of a file people keep in git.
func TestDecodeMocksJSON_MySQLBigintUnsignedSurvivesRetype(t *testing.T) {
	const want = uint64(18446744073709551615)

	resp := mysql.Response{
		PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{
				Header: &mysql.Header{PayloadLength: 12, SequenceID: 1},
				Type:   string(mysql.Binary),
			},
			Message: &mysql.BinaryProtocolResultSet{
				ColumnCount: 1,
				Rows: []*mysql.BinaryRow{{
					Header: mysql.Header{PayloadLength: 12, SequenceID: 1},
					Values: []mysql.ColumnEntry{{
						Type:     mysql.FieldTypeLongLong,
						Name:     "big_u",
						Value:    want,
						Unsigned: true,
					}},
					RowNullBuffer: []byte{0},
				}},
			},
		},
	}

	specJSON, err := json.Marshal(map[string]any{
		"metadata":  map[string]string{},
		"requests":  []mysql.Request{},
		"responses": []mysql.Response{resp},
	})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	doc := &yaml.NetworkTrafficDocJSON{
		Version: models.GetVersion(),
		Kind:    models.MySQL,
		Name:    "mysql-bigint",
		Spec:    json.RawMessage(specJSON),
	}

	mocks, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocksJSON: %v", err)
	}
	if len(mocks) != 1 {
		t.Fatalf("got %d mocks, want 1", len(mocks))
	}

	resps := mocks[0].Spec.MySQLResponses
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	rs, ok := resps[0].Message.(*mysql.BinaryProtocolResultSet)
	if !ok {
		t.Fatalf("response message is %T, want *mysql.BinaryProtocolResultSet — spec was %s",
			resps[0].Message, specJSON)
	}
	if len(rs.Rows) != 1 || len(rs.Rows[0].Values) != 1 {
		t.Fatalf("unexpected row shape: %+v", rs.Rows)
	}

	got := rs.Rows[0].Values[0].Value
	u, ok := got.(uint64)
	if !ok {
		t.Fatalf("value is %v (%T), want uint64 %d — the float64 intermediate ate it", got, got, want)
	}
	if u != want {
		t.Errorf("value = %d, want %d", u, want)
	}
}
