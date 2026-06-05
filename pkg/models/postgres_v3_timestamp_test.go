package models

import (
	"testing"
	"time"
)

// Regression for the postgres fuzzer failure: a 6-digit-year timestamp
// ("149206-12-15T16:39:16.394721Z") round-trips through the RFC3339Nano encoder
// but failed to decode because Go's "2006" layout token only accepts 4 digits.
func TestDecodeJSONTimestamp_WideAndNormalYears(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{
			name: "4-digit year (fast path)",
			in:   "2024-06-15T10:11:12.345678Z",
			want: time.Date(2024, time.June, 15, 10, 11, 12, 345678000, time.UTC),
		},
		{
			name: "6-digit year UTC (the fuzzer case)",
			in:   "149206-12-15T16:39:16.394721Z",
			want: time.Date(149206, time.December, 15, 16, 39, 16, 394721000, time.UTC),
		},
		{
			name: "5-digit year with offset preserved",
			in:   "12345-01-02T03:04:05.6789+05:30",
			want: time.Date(12345, time.January, 2, 3, 4, 5, 678900000, time.FixedZone("", 5*3600+30*60)),
		},
		{
			name: "postgres max year 294276",
			in:   "294276-12-31T23:59:59Z",
			want: time.Date(294276, time.December, 31, 23, 59, 59, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeJSONTimestamp(tc.in)
			if err != nil {
				t.Fatalf("decodeJSONTimestamp(%q) error: %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("decodeJSONTimestamp(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Encoding a wide-year time.Time via the same RFC3339Nano layout and decoding it
// back must be lossless — the round-trip the postgres replay path relies on.
func TestDecodeJSONTimestamp_WideYearRoundTrip(t *testing.T) {
	for _, want := range []time.Time{
		time.Date(149206, time.December, 15, 16, 39, 16, 394721000, time.UTC),
		time.Date(99999, time.July, 4, 1, 2, 3, 0, time.UTC),
	} {
		enc := want.Format(time.RFC3339Nano)
		got, err := decodeJSONTimestamp(enc)
		if err != nil {
			t.Fatalf("round-trip %q: %v", enc, err)
		}
		if !got.Equal(want) {
			t.Fatalf("round-trip %q = %v, want %v", enc, got, want)
		}
	}
}

// Genuinely malformed input must still error (we don't want the wide-year
// fallback to mask real corruption).
func TestDecodeJSONTimestamp_Invalid(t *testing.T) {
	for _, in := range []string{"not-a-time", "2024-13-99T99:99:99Z", ""} {
		if _, err := decodeJSONTimestamp(in); err == nil {
			t.Fatalf("decodeJSONTimestamp(%q): expected error, got nil", in)
		}
	}
}
