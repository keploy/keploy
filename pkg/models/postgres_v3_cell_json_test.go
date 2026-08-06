package models

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestPgtypeJSONNarrowingRejectsOutOfRange pins the range checks on
// every JSON field that lands in a pgtype struct member narrower than
// the int64 asInt64 returns.
//
// Go's integer conversions truncate silently — int32(1<<31) is
// -2147483648, uint16(65536) is 0, uint8(-1) is 255 — so before the
// range checks a hand-edited (or corrupted) mock carrying an
// out-of-range literal decoded into a *different, wrong* value and
// replayed as if it were what the user wrote. The YAML path never had
// this hole: yaml.v3 decodes straight into the target width and errors
// on overflow (see decodePgNumericMapping, which decodes `exp` into an
// int32 and `infinitymodifier` into an int8). These cases give the JSON
// path the same guarantee.
//
// Each case asserts a decode ERROR, not a clamped value: silently
// rewriting a user's mock is exactly the failure being fixed.
func TestPgtypeJSONNarrowingRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		json string
		// wantErrSubstr keeps the existing per-field error prefix
		// recognizable ("pg/numeric exp: ...") so callers grepping
		// mock-decode failures don't lose the field name.
		wantErrSubstr string
	}{
		{
			name:          "numeric_exp_over_int32",
			json:          `{"int":"1","exp":2147483648,"nan":false,"infinitymodifier":0,"valid":true}`,
			wantErrSubstr: "pg/numeric exp:",
		},
		{
			name:          "numeric_exp_under_int32",
			json:          `{"int":"1","exp":-2147483649,"nan":false,"infinitymodifier":0,"valid":true}`,
			wantErrSubstr: "pg/numeric exp:",
		},
		{
			name:          "numeric_infinitymodifier_over_int8",
			json:          `{"int":"1","exp":0,"nan":false,"infinitymodifier":200,"valid":true}`,
			wantErrSubstr: "pg/numeric infinitymodifier:",
		},
		{
			name:          "interval_days_over_int32",
			json:          `{"microseconds":0,"days":4294967296,"months":0,"valid":true}`,
			wantErrSubstr: "pg/interval days:",
		},
		{
			name:          "interval_months_under_int32",
			json:          `{"microseconds":0,"days":0,"months":-2147483649,"valid":true}`,
			wantErrSubstr: "pg/interval months:",
		},
		{
			name:          "bits_len_over_int32",
			json:          `{"bytes":"","len":2147483648,"valid":true}`,
			wantErrSubstr: "pg/bits len:",
		},
		{
			name:          "bits_bytes_element_over_uint8",
			json:          `{"bytes":[1,256],"len":16,"valid":true}`,
			wantErrSubstr: "pg/bits bytes[1]:",
		},
		{
			name:          "bits_bytes_element_negative",
			json:          `{"bytes":[-1],"len":8,"valid":true}`,
			wantErrSubstr: "pg/bits bytes[0]:",
		},
		{
			name:          "tid_blocknumber_over_uint32",
			json:          `{"blocknumber":4294967296,"offsetnumber":0,"valid":true}`,
			wantErrSubstr: "pg/tid blocknumber:",
		},
		{
			name:          "tid_blocknumber_negative",
			json:          `{"blocknumber":-1,"offsetnumber":0,"valid":true}`,
			wantErrSubstr: "pg/tid blocknumber:",
		},
		{
			name:          "tid_offsetnumber_over_uint16",
			json:          `{"blocknumber":0,"offsetnumber":65536,"valid":true}`,
			wantErrSubstr: "pg/tid offsetnumber:",
		},
		{
			name:          "tsvector_position_over_uint16",
			json:          `{"lexemes":[{"word":"cat","positions":[{"position":65536,"weight":0}]}],"valid":true}`,
			wantErrSubstr: "positions[0] position:",
		},
		{
			name:          "tsvector_weight_over_uint8",
			json:          `{"lexemes":[{"word":"cat","positions":[{"position":1,"weight":256}]}],"valid":true}`,
			wantErrSubstr: "positions[0] weight:",
		},
		{
			name:          "range_lowertype_over_uint8",
			json:          `{"lower":1,"upper":2,"lowertype":256,"uppertype":0,"valid":true}`,
			wantErrSubstr: "pg/range lowertype:",
		},
		{
			name:          "range_uppertype_negative",
			json:          `{"lower":1,"upper":2,"lowertype":0,"uppertype":-1,"valid":true}`,
			wantErrSubstr: "pg/range uppertype:",
		},
		{
			name:          "raw_format_over_int16",
			json:          `{"format":32768,"bytes":""}`,
			wantErrSubstr: "pg/raw format:",
		},
		{
			name:          "raw_format_under_int16",
			json:          `{"format":-32769,"bytes":""}`,
			wantErrSubstr: "pg/raw format:",
		},
		{
			// The multirange arm reaches rangeFromJSON through the
			// discriminator envelope — the bound-type checks have to
			// hold on that path too, not just the bare-range probe.
			name:          "multirange_nested_range_bound_over_uint8",
			json:          `{"$pgtype":"multirange","values":[{"lower":1,"upper":2,"lowertype":300,"uppertype":0,"valid":true}]}`,
			wantErrSubstr: "pg/multirange[0]: pg/range lowertype:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c PostgresV3Cell
			err := json.Unmarshal([]byte(tc.json), &c)
			if err == nil {
				// Print the decoded value so a regression shows the
				// silently-rewritten field, not just "expected error".
				t.Fatalf("out-of-range field decoded silently instead of erroring; got %#v", c.Value)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("error lost its field prefix: want substring %q, got %q", tc.wantErrSubstr, err.Error())
			}
			// The offending value has to appear so a user can find the
			// bad literal in a multi-megabyte mock file.
			if !strings.Contains(err.Error(), "out of range") {
				t.Fatalf("error does not name the range violation: %q", err.Error())
			}
		})
	}
}

// TestPgtypeJSONNarrowingAcceptsBoundaryValues is the other half of the
// range checks: every extreme value that DOES fit the target width must
// still decode, unchanged. An over-eager bound (off-by-one, or a signed
// check applied to an unsigned field) would truncate the legitimate
// extremes Postgres actually emits — TID block numbers run the full
// uint32 space, tsvector positions the full uint16 space.
func TestPgtypeJSONNarrowingAcceptsBoundaryValues(t *testing.T) {
	cases := []struct {
		name string
		json string
		want any
	}{
		{
			name: "numeric_exp_int32_extremes",
			json: `{"int":"1","exp":2147483647,"nan":false,"infinitymodifier":-1,"valid":true}`,
			want: pgtype.Numeric{Int: big.NewInt(1), Exp: math.MaxInt32, InfinityModifier: pgtype.NegativeInfinity, Valid: true},
		},
		{
			name: "interval_int32_extremes",
			json: `{"microseconds":9223372036854775807,"days":-2147483648,"months":2147483647,"valid":true}`,
			want: pgtype.Interval{Microseconds: math.MaxInt64, Days: math.MinInt32, Months: math.MaxInt32, Valid: true},
		},
		{
			name: "bits_len_and_byte_extremes",
			json: `{"bytes":[0,255],"len":2147483647,"valid":true}`,
			want: pgtype.Bits{Bytes: []byte{0, 255}, Len: math.MaxInt32, Valid: true},
		},
		{
			name: "tid_unsigned_extremes",
			json: `{"blocknumber":4294967295,"offsetnumber":65535,"valid":true}`,
			want: pgtype.TID{BlockNumber: math.MaxUint32, OffsetNumber: math.MaxUint16, Valid: true},
		},
		{
			name: "tsvector_unsigned_extremes",
			json: `{"lexemes":[{"word":"cat","positions":[{"position":65535,"weight":255}]}],"valid":true}`,
			want: pgtype.TSVector{
				Lexemes: []pgtype.TSVectorLexeme{{
					Word:      "cat",
					Positions: []pgtype.TSVectorPosition{{Position: math.MaxUint16, Weight: pgtype.TSVectorWeight(255)}},
				}},
				Valid: true,
			},
		},
		{
			name: "range_bound_type_extremes",
			json: `{"lower":1,"upper":2,"lowertype":255,"uppertype":0,"valid":true}`,
			want: pgtype.Range[any]{Lower: int64(1), Upper: int64(2), LowerType: pgtype.BoundType(255), UpperType: pgtype.BoundType(0), Valid: true},
		},
		{
			name: "raw_format_int16_extremes",
			json: `{"format":-32768,"bytes":""}`,
			want: PostgresV3CellRaw{Format: math.MinInt16, Bytes: []byte{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c PostgresV3Cell
			if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
				t.Fatalf("in-range value rejected: %v", err)
			}
			if !reflect.DeepEqual(c.Value, tc.want) {
				t.Fatalf("decoded value mismatch:\n got %#v\nwant %#v", c.Value, tc.want)
			}
			// Round-trip: re-encoding and re-decoding the extreme must
			// be a fixed point, so the range checks can't quietly
			// degrade a mock across record → replay rewrites.
			b, err := json.Marshal(PostgresV3Cell{Value: c.Value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back PostgresV3Cell
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("re-decode of %s: %v", b, err)
			}
			if !reflect.DeepEqual(back.Value, tc.want) {
				t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", back.Value, tc.want)
			}
		})
	}
}

// TestAsInt64RejectsUnrepresentable covers the narrowing conversions
// inside asInt64 itself, which no JSON literal can reach
// (json.Number.Int64 rejects them first) but Go-built map fixtures can:
// a float64 outside int64's range — where the Go spec leaves the
// conversion result implementation-defined — and the unsigned shapes as
// wide as int64 (uint64 AND uint), which wrap to a negative int64.
func TestAsInt64RejectsUnrepresentable(t *testing.T) {
	type unrepresentable struct {
		name string
		in   any
	}
	cases := []unrepresentable{
		{"float64_over_int64", 1e30},
		{"float64_under_int64", -1e30},
		{"float64_nan", math.NaN()},
		{"float64_positive_inf", math.Inf(1)},
		{"float64_negative_inf", math.Inf(-1)},
		{"float32_over_int64", float32(1e30)},
		{"uint64_over_int64", uint64(math.MaxInt64) + 1},
	}
	// uint is 64 bits wide on every 64-bit build, i.e. exactly as wide as
	// uint64, and wraps identically. The constant guard keeps a 32-bit
	// build compiling and passing, where no uint value exceeds int64.
	if math.MaxUint > math.MaxInt64 {
		cases = append(cases,
			unrepresentable{"uint_max", ^uint(0)},
			unrepresentable{"uint_max_int64_plus_one", ^uint(0)/2 + 1},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := asInt64(tc.in)
			if err == nil {
				t.Fatalf("unrepresentable %T(%v) converted silently to %d", tc.in, tc.in, got)
			}
		})
	}

	// Values that do fit must keep their existing behaviour, including
	// the truncation of a fractional float that hand-built fixtures
	// have always relied on.
	fits := []struct {
		name string
		in   any
		want int64
	}{
		{"float64_fraction_truncates", 1.9, 1},
		{"float64_negative_fraction_truncates", -1.9, -1},
		{"uint64_max_int64", uint64(math.MaxInt64), math.MaxInt64},
		{"json_number", json.Number("-42"), -42},
		{"string", "1234", 1234},
	}
	for _, tc := range fits {
		t.Run(tc.name, func(t *testing.T) {
			got, err := asInt64(tc.in)
			if err != nil {
				t.Fatalf("representable %T(%v) rejected: %v", tc.in, tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("asInt64(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
