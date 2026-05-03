package mysql

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestColumnEntryJSONRoundTrip pins the JSON round-trip for every
// concrete Go type that lands in ColumnEntry.Value during recording.
// Value-type fidelity is load-bearing: the integrations-side codec
// dispatches its wire-format encoder via type-switch on Value, so an
// int64 that drifts to float64 across record→replay produces malformed
// MySQL binary-protocol bytes and the driver disconnects with `invalid
// connection`. That was the production failure mode for the MySQL
// fuzzer and echo-mysql samples on `record_build_replay_build` until
// MarshalJSON / UnmarshalJSON were added.
func TestColumnEntryJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"null", nil},
		{"bool_true", true},
		{"bool_false", false},
		{"int_positive", 42},
		{"int_negative", -12345},
		// MaxInt64 still fits in Go `int` on 64-bit hosts (which is
		// the only platform keploy ships on), so the recovered type
		// is still `int`. The literal exists in this test to pin
		// that we don't lose precision on the boundary value.
		{"int_max", 9223372036854775807},
		{"float", 3.14},
		{"string", "Lizard"},
		{"empty_string", ""},
		{"binary", []byte{0x01, 0x02, 0x03, 0xFF}},
		{"empty_binary", []byte{}},
		{"timestamp", time.Date(2026, 5, 3, 12, 30, 45, 123_456_789, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := ColumnEntry{
				Type:     FieldTypeVarChar,
				Name:     "col1",
				Value:    tc.in,
				Unsigned: false,
			}
			body, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ColumnEntry
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v\n--JSON--\n%s", err, body)
			}
			if got.Type != orig.Type || got.Name != orig.Name || got.Unsigned != orig.Unsigned {
				t.Errorf("non-Value field drift:\n got  %+v\n want %+v", got, orig)
			}
			if !valuesEqual(got.Value, tc.in) {
				t.Errorf("Value drift (Go-type fidelity broken):\n got  %#v (%T)\n want %#v (%T)\n--JSON--\n%s",
					got.Value, got.Value, tc.in, tc.in, body)
			}
		})
	}
}

// TestParameterJSONRoundTrip mirrors the ColumnEntry test for the
// StmtExecutePacket parameter list. Same type-fidelity contract.
func TestParameterJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"null", nil},
		{"bool", true},
		{"int", 42},
		{"float", 1.5},
		{"string", "hello"},
		{"binary", []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		{"timestamp", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := Parameter{
				Type:     254,
				Unsigned: false,
				Name:     "p1",
				Value:    tc.in,
			}
			body, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got Parameter
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v\n--JSON--\n%s", err, body)
			}
			if got.Type != orig.Type || got.Name != orig.Name || got.Unsigned != orig.Unsigned {
				t.Errorf("non-Value field drift:\n got  %+v\n want %+v", got, orig)
			}
			if !valuesEqual(got.Value, tc.in) {
				t.Errorf("Value drift:\n got  %#v (%T)\n want %#v (%T)\n--JSON--\n%s",
					got.Value, got.Value, tc.in, tc.in, body)
			}
		})
	}
}

// TestColumnEntryJSONIntStaysInt is the focused regression test for
// the production bug. encoding/json's default decoder maps every JSON
// number to float64 — for an INT column, the integrations-side
// encoder type-switches on Value via `ce.Value.(int)` (not int64) and
// panics on a float64 input. yaml.v3's reflective default on the
// existing YAML path returns Go `int` for !!int into interface{}, so
// the JSON decoder must match that exact type to keep the wire
// encoder consistent — int64 from json.Number.Int64() also panics
// the same .(int) assertion. This stays even if every other test in
// this file is deleted.
func TestColumnEntryJSONIntStaysInt(t *testing.T) {
	orig := ColumnEntry{Type: FieldTypeLong, Name: "id", Value: 42}
	body, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ColumnEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Value.(int); !ok {
		t.Fatalf("ColumnEntry.Value: expected int (got %T = %v) — yaml.v3 default returns int for !!int → interface{}, and the integrations-side wire encoder type-asserts .(int); int64 panics it",
			got.Value, got.Value)
	}
	if got.Value.(int) != 42 {
		t.Errorf("Value mismatch: got %d want 42", got.Value.(int))
	}
}

// TestColumnEntryJSONBinaryEnvelope pins the on-disk shape for binary
// values: the wrapper carries a `$type=bin` marker so a base64-string
// payload can be distinguished from a regular string column on read.
// Without the envelope, encoding/json marshals []byte as a base64 JSON
// string indistinguishable from a string column, and the recovered
// Go type would silently flip from []byte to string.
func TestColumnEntryJSONBinaryEnvelope(t *testing.T) {
	orig := ColumnEntry{Type: FieldTypeBLOB, Name: "data", Value: []byte{0xAB, 0xCD}}
	body, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"$type":"bin"`)) {
		t.Fatalf("binary value must carry $type=bin envelope:\n%s", body)
	}
	var got ColumnEntry
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, ok := got.Value.([]byte)
	if !ok {
		t.Fatalf("ColumnEntry.Value: expected []byte (got %T) — $type=bin envelope must round-trip Go-type identity", got.Value)
	}
	if !bytes.Equal(b, []byte{0xAB, 0xCD}) {
		t.Errorf("Value mismatch: got %v want [0xAB 0xCD]", b)
	}
}

// TestColumnEntryJSONFloatColumnPreservesType pins the float-typed
// column round-trip even when the recorded value is a whole number.
// Go's json.Marshal strips trailing zeros from floats — float64(42.0)
// emits as `42`, indistinguishable from int(42) on the wire. If the
// decoder doesn't coerce based on FieldType, the recovered Value is
// int(42); the integrations-side wire encoder then panics in
// EncodeBinaryRow at `ce.Value.(float64)` for FieldTypeDouble or
// `ce.Value.(float32)` for FieldTypeFloat, dropping the MySQL
// connection and breaking every subsequent op on it. This stays as a
// dedicated regression alongside TestColumnEntryJSONIntStaysInt.
func TestColumnEntryJSONFloatColumnPreservesType(t *testing.T) {
	cases := []struct {
		name  string
		ftype FieldType
		in    any
		want  any
	}{
		{"double_whole", FieldTypeDouble, float64(42.0), float64(42.0)},
		{"double_frac", FieldTypeDouble, float64(3.14), float64(3.14)},
		{"float_whole", FieldTypeFloat, float32(42.0), float32(42.0)},
		{"float_frac", FieldTypeFloat, float32(3.5), float32(3.5)},
		{"longlong_int", FieldTypeLongLong, 42, 42},
		{"longlong_int_via_float", FieldTypeLongLong, float64(42.0), 42},
		{"long_int", FieldTypeLong, 7, 7},
		{"tiny_int", FieldTypeTiny, 1, 1},
		{"short_int", FieldTypeShort, 1234, 1234},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := ColumnEntry{Type: tc.ftype, Name: "c", Value: tc.in}
			body, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got ColumnEntry
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v\n--JSON--\n%s", err, body)
			}
			if !reflect.DeepEqual(got.Value, tc.want) {
				t.Errorf("FieldType %v coercion drift:\n got  %#v (%T)\n want %#v (%T)\n--JSON--\n%s",
					tc.ftype, got.Value, got.Value, tc.want, tc.want, body)
			}
		})
	}
}

// valuesEqual is a small DeepEqual wrapper that handles the
// time.Time-vs-time.Time monotonic-clock asymmetry: time.Time values
// that round-trip through RFC3339Nano lose the monotonic clock
// reading, so reflect.DeepEqual reports them unequal even when the
// wall-clock instant is the same. Compare via Equal() for time
// values and DeepEqual for everything else.
func valuesEqual(got, want any) bool {
	if got == nil && want == nil {
		return true
	}
	if g, ok := got.(time.Time); ok {
		w, ok := want.(time.Time)
		if !ok {
			return false
		}
		return g.Equal(w)
	}
	return reflect.DeepEqual(got, want)
}
