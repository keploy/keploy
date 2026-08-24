package mysql

import (
	"encoding/json"
	"math"
	"testing"
)

// A BIGINT UNSIGNED above MaxInt64 fails json.Number.Int64(). Falling
// straight through to Float64() from there keeps only the top 53 bits,
// and coercing that float back to an integer is implementation-defined
// in Go — on amd64 it saturates, so every value above MaxInt64 landed
// on the same wrong number. yaml.v3 yields uint64 for these, so the
// JSON mock format has to as well or the two formats replay the same
// mock as different rows.
func TestColumnEntryJSONBigintUnsignedKeepsValue(t *testing.T) {
	for _, want := range []uint64{
		math.MaxInt64 + 1,
		math.MaxUint64 - 1,
		math.MaxUint64,
		18446744073709551000,
	} {
		in := ColumnEntry{Type: FieldTypeLongLong, Name: "big", Value: want, Unsigned: true}
		blob, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal %d: %v", want, err)
		}
		var out ColumnEntry
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", blob, err)
		}
		got, ok := out.Value.(uint64)
		if !ok {
			t.Fatalf("%d: got %v (%T), want a uint64 — json was %s",
				want, out.Value, out.Value, blob)
		}
		if got != want {
			t.Errorf("%d: round-tripped to %d — json was %s", want, got, blob)
		}
	}
}

// Two distinct BIGINT UNSIGNED values must not collapse onto each
// other. The saturating int conversion made 18446744073709551614 and
// 9223372036854775808 both become -9223372036854775808. Note this
// property is restored by the exactInt guard alone — it passes with a
// float64 on both sides too. TestColumnEntryJSONBigintUnsignedKeepsValue
// is what pins the uint64 rung itself.
func TestColumnEntryJSONBigintUnsignedStaysDistinct(t *testing.T) {
	roundTrip := func(v uint64) any {
		t.Helper()
		blob, err := json.Marshal(ColumnEntry{Type: FieldTypeLongLong, Value: v, Unsigned: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var out ColumnEntry
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return out.Value
	}

	a := roundTrip(math.MaxUint64 - 1)
	b := roundTrip(math.MaxInt64 + 1)
	if a == b {
		t.Errorf("distinct BIGINT UNSIGNED values collapsed onto %v", a)
	}
}

// An integer literal that still fits int must keep its existing Go
// type — the uint64 rung is a fallback, not a replacement. Bounds are
// spelled at int32 width so the file still compiles on a 32-bit host;
// json_test.go covers the MaxInt64 case on 64-bit.
func TestColumnEntryJSONInRangeIntegerStaysInt(t *testing.T) {
	for _, want := range []int{0, 1, -1, math.MaxInt32, math.MinInt32} {
		blob, err := json.Marshal(ColumnEntry{Type: FieldTypeLongLong, Value: want})
		if err != nil {
			t.Fatalf("marshal %d: %v", want, err)
		}
		var out ColumnEntry
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", blob, err)
		}
		got, ok := out.Value.(int)
		if !ok {
			t.Fatalf("%d: got %v (%T), want int", want, out.Value, out.Value)
		}
		if got != want {
			t.Errorf("%d: round-tripped to %d", want, got)
		}
	}
}

// coerceValueForFieldType must not narrow a float it cannot represent
// as an int. Saturating here invents a plausible-looking number; the
// value is passed through instead so the wire encoder can reject it by
// column name.
func TestCoerceValueForFieldTypeLeavesUnrepresentableFloats(t *testing.T) {
	for _, v := range []float64{1e300, -1e300, 9.5, math.NaN(), math.Inf(1)} {
		got := coerceValueForFieldType(v, FieldTypeLongLong)
		if _, narrowed := got.(int); narrowed {
			t.Errorf("%v was narrowed to int %v, want passed through", v, got)
		}
	}
	// Integral and in range still narrows, as before.
	if got := coerceValueForFieldType(float64(42), FieldTypeLongLong); got != 42 {
		t.Errorf("float64(42) coerced to %v (%T), want int 42", got, got)
	}
}

// encoding/json writes a float64 in 'f' format for any magnitude below
// 1e21, so a DOUBLE column holding 1e19 is marshalled as the integer
// literal 10000000000000000000 — above MaxInt64, so recoverJSONValue
// hands back a uint64. The FLOAT/DOUBLE arms have to accept that or the
// column silently changes Go type across the round trip and gets
// re-serialized as an integer by UpdateMocks.
func TestColumnEntryJSONLargeDoubleStaysFloat(t *testing.T) {
	for _, ftype := range []FieldType{FieldTypeDouble, FieldTypeFloat} {
		blob, err := json.Marshal(ColumnEntry{Type: ftype, Name: "d", Value: 1e19})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if want := `1e+19`; string(blob) == want {
			t.Fatalf("premise broken: encoding/json wrote %s, expected an integer literal", blob)
		}

		var out ColumnEntry
		if err := json.Unmarshal(blob, &out); err != nil {
			t.Fatalf("unmarshal %s: %v", blob, err)
		}
		switch ftype {
		case FieldTypeDouble:
			got, ok := out.Value.(float64)
			if !ok {
				t.Errorf("DOUBLE: got %v (%T), want float64 — json was %s", out.Value, out.Value, blob)
			} else if got != 1e19 {
				t.Errorf("DOUBLE: got %v, want 1e19", got)
			}
		case FieldTypeFloat:
			if _, ok := out.Value.(float32); !ok {
				t.Errorf("FLOAT: got %v (%T), want float32 — json was %s", out.Value, out.Value, blob)
			}
		}
	}
}

// exactInt's bound is width-dependent: on 64-bit float64(MaxInt) rounds
// up to 2^63 and MaxInt is unreachable from a float64, while on 32-bit
// float64(MaxInt) is exact and must be accepted. Deriving the cases
// from math.MaxInt keeps this honest at both widths rather than
// asserting one platform's answer.
func TestExactIntBoundaries(t *testing.T) {
	if got, ok := exactInt(float64(math.MinInt)); !ok || got != math.MinInt {
		t.Errorf("exactInt(float64(MinInt)) = %d, %v; want %d, true", got, ok, math.MinInt)
	}

	// One float64 step past MaxInt is always out of range.
	if got, ok := exactInt(float64(math.MaxInt) * 2); ok {
		t.Errorf("exactInt(2*float64(MaxInt)) = %d, true; want rejected", got)
	}

	// float64(MaxInt) is exactly MaxInt only when int is 32-bit; on
	// 64-bit it rounds up to 2^63, which int cannot hold. Compare the
	// widths rather than the values — float64(math.MaxInt) ==
	// math.MaxInt is a constant expression that converts the untyped
	// constant to float64 and is therefore true at both widths.
	const intIs64Bit = math.MaxInt > math.MaxInt32
	got, ok := exactInt(float64(math.MaxInt))
	switch {
	case intIs64Bit && ok:
		t.Errorf("exactInt(float64(MaxInt)) = %d, true; want rejected (rounds above MaxInt)", got)
	case !intIs64Bit && (!ok || got != math.MaxInt):
		t.Errorf("exactInt(float64(MaxInt)) = %d, %v; want %d, true", got, ok, math.MaxInt)
	}

	for _, f := range []float64{9.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got, ok := exactInt(f); ok {
			t.Errorf("exactInt(%v) = %d, true; want rejected", f, got)
		}
	}
}
