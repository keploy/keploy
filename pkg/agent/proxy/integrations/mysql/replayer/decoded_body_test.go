package replayer

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestARecordingsBodyIsMadeDecodedAsItsBytesDecode checks the body the strict
// gate compares a recording on (mysqlRequestJSONBody) against the bytes it was
// compared on before (mysqlRequestBody): the same bytes, whether it has a body
// at all, and, for a body made decoded, the value json.Unmarshal makes of
// those bytes, which the engine then neither validates nor decodes. A value
// whose encoding is not plain is encoded and decoded as before.
func TestARecordingsBodyIsMadeDecodedAsItsBytesDecode(t *testing.T) {
	type param = mysql.Parameter
	longData := func(data []byte) mysql.PacketBundle {
		return mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{}, Type: "COM_STMT_SEND_LONG_DATA"},
			Message: &mysql.StmtSendLongDataPacket{Status: 0x18, StatementID: 1, ParameterID: 0, Data: data},
		}
	}
	// A query that reads an attribute it names keeps the ones it reads, by
	// name; one that reads it by a variable keeps every parameter, by
	// position, with the name it was sent with.
	attrQuery := func(sql string, params ...param) mysql.PacketBundle {
		b := comQueryReq(sql).PacketBundle
		b.Message.(*mysql.QueryPacket).Parameters = params
		return b
	}
	named := "SELECT mysql_query_attribute_string('tenant') AS t"
	byVariable := "SELECT mysql_query_attribute_string(@name) AS t"
	for _, tc := range []struct {
		name   string
		bundle mysql.PacketBundle
		plain  bool
	}{
		{"integers", executeBundle(1, param{Type: 1, Value: int8(-5)}, param{Type: 2, Value: int16(-300)}, param{Type: 3, Value: int32(70000)},
			param{Type: 8, Value: int64(1<<53 + 1)}, param{Type: 8, Value: int64(math.MaxInt64)}, param{Type: 8, Value: int64(math.MinInt64)},
			param{Type: 1, Unsigned: true, Value: uint8(200)}, param{Type: 2, Unsigned: true, Value: uint16(65535)}, param{Type: 3, Unsigned: true, Value: uint32(4000000000)},
			param{Type: 8, Unsigned: true, Value: uint64(math.MaxUint64)}, param{Type: 8, Value: 7}, param{Type: 8, Unsigned: true, Value: uint(9)}), true},
		{"doubles", executeBundle(1, param{Type: 5, Value: 0.1}, param{Type: 5, Value: -1e300}, param{Type: 5, Value: math.Copysign(0, -1)}, param{Type: 5, Value: 5e-324}), true},
		{"strings, booleans and nulls", executeBundle(1, param{Type: 253, Value: "héllo <&>  "}, param{Type: 253, Value: ""}, param{Type: 6, Value: nil},
			param{Type: 1, Value: true}, param{Type: 253, Name: "tenant", Value: "acme"}), true},
		{"a value that is an object", executeBundle(1, param{Type: 245, Value: map[string]any{"a": 1, "b": map[string]any{"c": "d", "e": nil}}}), true},
		{"a typeless execute", executeBundle(0, param{Type: 254, Value: "a"}), true},
		{"a query's literals and comments", comQueryReq("SELECT /* tag */ id FROM t WHERE a = 'x' AND b = 5").PacketBundle, true},
		{"a query that does not parse", comQueryReq("%%% not sql %%%").PacketBundle, true},
		{"a query reading an attribute", attrQuery(named, param{Type: 253, Name: "Tenant", Value: "acme"}, param{Type: 253, Name: "traceparent", Value: "00-ab"}), true},
		{"a query reading an attribute by a variable", attrQuery(byVariable, param{Type: 253, Name: "Tenant", Value: "acme"}, param{Type: 253, Value: "b"}), true},
		{"a prepare", stmtPrepareReq("SELECT id FROM t WHERE a = ?").PacketBundle, true},
		{"binary long data", longData([]byte{0xff, 0x00, 0x01}), true},
		{"a float", executeBundle(1, param{Type: 4, Value: float32(0.1)}), false},
		{"bytes", executeBundle(1, param{Type: 252, Value: []byte{1, 2}}), false},
		{"a time", executeBundle(1, param{Type: 12, Value: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}), false},
		{"a NaN", executeBundle(1, param{Type: 5, Value: math.NaN()}), false},
		{"an infinity", executeBundle(1, param{Type: 5, Value: math.Inf(-1)}), false},
		{"a value that is not UTF-8", executeBundle(1, param{Type: 253, Value: "a\xffb"}), false},
		{"a name that is not UTF-8", attrQuery(byVariable, param{Type: 253, Name: "n\xff", Value: "a"}), false},
		{"a key that is not UTF-8", executeBundle(1, param{Type: 245, Value: map[string]any{"k\xff": "v"}}), false},
		{"a literal that is not UTF-8", comQueryReq("SELECT id FROM t WHERE a = 'x\xffy'").PacketBundle, false},
		{"a number of another type", executeBundle(1, param{Type: 8, Value: json.Number("5")}), false},
		{"a list", executeBundle(1, param{Type: 245, Value: []any{1, "a"}}), false},
		{"JSON long data", longData([]byte(`{"a":1,"b":[true,null]}`)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.bundle
			raw, rawOK := mysqlRequestBody(&b, -1, "")
			body, ok := mysqlRequestJSONBody(&b, -1, "")
			doc, _ := mysqlRequestDoc(&b, -1, "")
			value, plain := decodedRequestValue(doc)
			if plain != tc.plain {
				t.Fatalf("made decoded = %v, want %v (doc %#v)", plain, tc.plain, doc)
			}
			if ok != rawOK {
				t.Fatalf("has a body = %v, while its bytes say %v", ok, rawOK)
			}
			if !ok {
				return
			}
			if plain {
				var want any
				if err := json.Unmarshal(raw, &want); err != nil {
					t.Fatalf("the bytes %s do not decode: %v", raw, err)
				}
				if !reflect.DeepEqual(value, want) {
					t.Fatalf("made decoded:\n%#v\nits bytes decode to:\n%#v", value, want)
				}
			}
			drift, comparable := mocknoise.DetectJSONBodyDrift(body, mocknoise.NewJSONBody(raw), nil, nil, false, nil)
			if !comparable || len(drift) != 0 {
				t.Fatalf("compared with its own bytes: drift %v, comparable %v", drift, comparable)
			}
			// Made decoded, the engine neither validates nor decodes it.
			if reads, want := body.Reads(), map[bool]int{true: 0, false: 2}[plain]; reads != want {
				t.Fatalf("the body was read %d times by the comparison, want %d", reads, want)
			}
			if got := body.Raw(); string(got) != string(raw) {
				t.Fatalf("its bytes are %s, want %s", got, raw)
			}
		})
	}
}

// TestAValueNotPlainInAnyPlaceIsNotMadeDecoded: decodedRequestValue leaves a
// document to be encoded and decoded when any key, name or value in it is
// not plain, wherever it sits, in a shape the request bodies do not make
// today too.
func TestAValueNotPlainInAnyPlaceIsNotMadeDecoded(t *testing.T) {
	for name, v := range map[string]any{
		"a key of a parameter map":   map[string]mysql.Parameter{"k\xff": {Type: 253, Value: "a"}},
		"a key of a literal map":     map[string]string{"k\xff": "a"},
		"a parameter's nested value": map[string]any{"p": map[string]mysql.Parameter{"0": {Value: map[string]any{"a": float32(1)}}}},
		"a nested value":             map[string]any{"a": map[string]any{"b": []byte("c")}},
	} {
		if d, plain := decodedRequestValue(v); plain {
			t.Errorf("%s: made decoded as %#v", name, d)
		}
	}
}

// TestTheStrictGateComparesARecordingOnItsBodyMadeDecoded checks that the
// strict gate's compare takes a recording's body from mysqlRequestJSONBody:
// the package's check of decoded bodies (CheckDecodedBodies, on in TestMain)
// sees each new recording it compares, once.
func TestTheStrictGateComparesARecordingOnItsBodyMadeDecoded(t *testing.T) {
	if decodedBodyCheck == nil {
		t.Fatal("the package's check of decoded bodies is off")
	}
	type param = mysql.Parameter
	live := executeBundle(1, param{Type: 253, Value: "a"}, param{Type: 8, Value: int64(5)})
	for _, tc := range []struct {
		name    string
		rec     mysql.PacketBundle
		allowed bool
	}{
		{"the same call", executeBundle(1, param{Type: 253, Value: "a"}, param{Type: 8, Value: int64(5)}), true},
		{"another value", executeBundle(1, param{Type: 253, Value: "b"}, param{Type: 8, Value: int64(5)}), false},
		{"another number", executeBundle(1, param{Type: 253, Value: "a"}, param{Type: 8, Value: int64(6)}), false},
	} {
		gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, "", nil)
		rec := &models.Mock{Name: "rec", Kind: models.MySQL}
		rec.Spec.MySQLRequests = []mysql.Request{{PacketBundle: tc.rec}}
		before := DecodedBodiesChecked()
		if got := gate.allows(rec); got != tc.allowed {
			t.Errorf("%s: allowed = %v, want %v", tc.name, got, tc.allowed)
		}
		if got := DecodedBodiesChecked() - before; got != 1 {
			t.Errorf("%s: the compare made %d bodies decoded, want 1", tc.name, got)
		}
	}
	// A recording that is not one request has no body to compare on: it is
	// allowed, as before.
	gate := newStrictGate(mocknoise.New(mysqlNoiseAdapter{}, false, true), zap.NewNop(), "COM_STMT_EXECUTE", &live, -1, "", nil)
	for _, reqs := range [][]mysql.Request{nil, {{PacketBundle: live}, {PacketBundle: live}}} {
		rec := &models.Mock{Name: "rec", Kind: models.MySQL}
		rec.Spec.MySQLRequests = reqs
		if !gate.allows(rec) {
			t.Errorf("a recording of %d requests: rejected, want allowed", len(reqs))
		}
	}
}
