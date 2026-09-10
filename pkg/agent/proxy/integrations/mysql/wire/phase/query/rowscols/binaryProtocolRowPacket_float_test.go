package rowscols

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// FLOAT and DOUBLE result-set columns carried two independent defects that
// together made any schema with a real-typed column unusable.
//
//  1. RECORD side: readBinaryValue cast the wire bytes numerically
//     (float64(binary.LittleEndian.Uint64(...))) instead of reinterpreting
//     them as an IEEE-754 bit pattern. A DOUBLE column holding 9.99 was
//     written to mocks.yaml as 4.621813488089437e+18. Because the damage
//     happens at record time it survives into the artifact — re-recording
//     does not help, and nothing downstream can recover the original.
//
//  2. REPLAY side: EncodeBinaryRow type-asserted ce.Value.(float32) for
//     FieldTypeFloat. Values arrive from a serializer, and YAML resolves an
//     untagged scalar by shape: "9.99" is float64, "10" is int. Neither is
//     float32, so the assertion panicked. That panic is not a failed row —
//     it unwinds the connection handler and the application under replay
//     sees "invalid connection" with nothing naming the cause.
//
// The two interact: DOUBLE's assertion (.(float64)) happened to succeed on
// YAML's float64, so DOUBLE corrupted silently while FLOAT crashed loudly.
// That asymmetry is why the record-side bug went unnoticed.

// buildBinaryRow lays out a binary protocol result-set row:
// 4-byte header, 0x00 marker, null bitmap, then packed column values.
func buildBinaryRow(t *testing.T, values []byte, numCols int) []byte {
	t.Helper()
	bitmapLen := (numCols + 7 + 2) / 8
	body := make([]byte, 0, 1+bitmapLen+len(values))
	body = append(body, 0x00)                       // packet header marker
	body = append(body, make([]byte, bitmapLen)...) // no NULLs
	body = append(body, values...)

	pkt := make([]byte, 4, 4+len(body))
	pkt[0] = byte(len(body))
	pkt[1] = byte(len(body) >> 8)
	pkt[2] = byte(len(body) >> 16)
	pkt[3] = 1 // sequence id
	return append(pkt, body...)
}

func floatCols() []*mysql.ColumnDefinition41 {
	return []*mysql.ColumnDefinition41{
		{Name: "price", Type: byte(mysql.FieldTypeFloat)},
		{Name: "ratio", Type: byte(mysql.FieldTypeDouble)},
	}
}

// TestDecodeBinaryRow_FloatDoubleAreBitPatterns pins the record-side fix.
// The literals are the exact corruptions observed before it: MySQL sending
// 9.99 produced 1.0926057e+09 for FLOAT and 4.621813488089437e+18 for
// DOUBLE, which is what the uint bit pattern reads as when cast as a number.
func TestDecodeBinaryRow_FloatDoubleAreBitPatterns(t *testing.T) {
	const wantF32 = float32(9.99)
	const wantF64 = float64(9.99)

	vals := make([]byte, 12)
	binary.LittleEndian.PutUint32(vals[0:4], math.Float32bits(wantF32))
	binary.LittleEndian.PutUint64(vals[4:12], math.Float64bits(wantF64))

	row, _, err := DecodeBinaryRow(context.Background(), zap.NewNop(),
		buildBinaryRow(t, vals, 2), floatCols())
	if err != nil {
		t.Fatalf("DecodeBinaryRow: %v", err)
	}

	got32, ok := row.Values[0].Value.(float32)
	if !ok {
		t.Fatalf("FLOAT: expected float32, got %T", row.Values[0].Value)
	}
	if got32 != wantF32 {
		t.Errorf("FLOAT: got %v, want %v (numeric cast would give 1.0926057e+09)", got32, wantF32)
	}

	got64, ok := row.Values[1].Value.(float64)
	if !ok {
		t.Fatalf("DOUBLE: expected float64, got %T", row.Values[1].Value)
	}
	if got64 != wantF64 {
		t.Errorf("DOUBLE: got %v, want %v (numeric cast would give 4.621813488089437e+18)", got64, wantF64)
	}
}

// TestEncodeBinaryRow_SurvivesYAMLRoundTrip pins the replay-side fix, through
// the serializer that actually causes it. Encoding the in-memory row is not
// enough: the bug only appears once the row has been through mocks.yaml,
// because that is what erases float32 and collapses 10.0 to an int.
func TestEncodeBinaryRow_SurvivesYAMLRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f32  float32
		f64  float64
	}{
		// Fractional: YAML keeps a decimal point, so both come back float64.
		{"fractional", 9.99, 9.99},
		// Whole: yaml.v3 emits float64(10) as "10", which resolves back to
		// int, not float64. The DOUBLE assertion panicked here too.
		{"whole", 10, 10},
		// Negative and zero, for the sign bit and the all-zero bit pattern.
		{"negative", -0.5, -1234.5678},
		{"zero", 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vals := make([]byte, 12)
			binary.LittleEndian.PutUint32(vals[0:4], math.Float32bits(tc.f32))
			binary.LittleEndian.PutUint64(vals[4:12], math.Float64bits(tc.f64))
			wire := buildBinaryRow(t, vals, 2)

			row, _, err := DecodeBinaryRow(context.Background(), zap.NewNop(), wire, floatCols())
			if err != nil {
				t.Fatalf("DecodeBinaryRow: %v", err)
			}

			// The round trip through the mock store.
			blob, err := yaml.Marshal(row)
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			var restored mysql.BinaryRow
			if err := yaml.Unmarshal(blob, &restored); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}

			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EncodeBinaryRow panicked on YAML-restored row: %v\nyaml was:\n%s", r, blob)
				}
			}()

			got, err := EncodeBinaryRow(context.Background(), zap.NewNop(), &restored, floatCols())
			if err != nil {
				t.Fatalf("EncodeBinaryRow: %v\nyaml was:\n%s", err, blob)
			}
			if !bytes.Equal(got, wire) {
				t.Errorf("wire bytes changed across record/store/replay:\n got %x\nwant %x\nyaml was:\n%s",
					got, wire, blob)
			}
		})
	}
}

// TestEncodeBinaryRow_IntegerColumnsSurviveYAML covers the same serializer
// hazard on the integer columns, which used a naked ce.Value.(int). YAML
// happens to give int for small integers, so these did not panic in practice
// — but a BIGINT UNSIGNED above MaxInt64 resolves to uint64 and did.
func TestEncodeBinaryRow_IntegerColumnsSurviveYAML(t *testing.T) {
	cols := []*mysql.ColumnDefinition41{
		{Name: "big", Type: byte(mysql.FieldTypeLongLong), Flags: 0x0020}, // UNSIGNED_FLAG
	}
	vals := make([]byte, 8)
	binary.LittleEndian.PutUint64(vals, math.MaxUint64-1)
	wire := buildBinaryRow(t, vals, 1)

	row, _, err := DecodeBinaryRow(context.Background(), zap.NewNop(), wire, cols)
	if err != nil {
		t.Fatalf("DecodeBinaryRow: %v", err)
	}
	blob, err := yaml.Marshal(row)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var restored mysql.BinaryRow
	if err := yaml.Unmarshal(blob, &restored); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EncodeBinaryRow panicked on BIGINT UNSIGNED: %v\nyaml was:\n%s", r, blob)
		}
	}()
	got, err := EncodeBinaryRow(context.Background(), zap.NewNop(), &restored, cols)
	if err != nil {
		t.Fatalf("EncodeBinaryRow: %v\nyaml was:\n%s", err, blob)
	}
	if !bytes.Equal(got, wire) {
		t.Errorf("wire bytes changed:\n got %x\nwant %x\nyaml was:\n%s", got, wire, blob)
	}
}
