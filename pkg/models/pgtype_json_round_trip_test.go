package models

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestPgtypeJSONRoundTrip pins the JSON round-trip for every pgtype-
// shaped cell value the recorder can hand to MarshalJSON. Each case
// goes through:
//
//  1. Marshal → Unmarshal via the cell schema, DeepEqual against the
//     original value. The encoder builds canonical-key mappings (the
//     same shape MarshalYAML uses, just JSON-quoted) for unambiguous
//     types and adds a `$pgtype` discriminator for the ambiguous ones
//     (Point/Lseg/Box/Polygon all share `{p, valid}`; Hstore has open
//     keys; Multirange is a sequence — see postgres_v3_cell_json.go).
//  2. The marshaled JSON does NOT begin with the `{"Value":` struct
//     wrapper that the default reflective encoder emits. Pre-fix that
//     wrapper was the root cause of the postgres-v3 mock-replay
//     failures (every cell decoded back as map[string]any{"Value":...}
//     instead of the unwrapped logical value, so `INSERT … RETURNING *`
//     returned zero rows during replay → JDBC/pgjdbc blew up in
//     ByteConverter.int8 with ArrayIndexOutOfBoundsException). All 270
//     test-set-1 failures on the spring-petclinic-rest sample fanned
//     out from this single seam.
func TestPgtypeJSONRoundTrip(t *testing.T) {
	type roundTripCase struct {
		name string
		in   any
	}
	cases := []roundTripCase{
		// ===== Unambiguous canonical-key shapes =====
		{
			name: "numeric_decimal",
			in:   pgtype.Numeric{Int: big.NewInt(123456), Exp: -2, Valid: true},
		},
		{
			name: "numeric_nan",
			in:   pgtype.Numeric{NaN: true, Valid: true},
		},
		{
			name: "numeric_infinity",
			in:   pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true},
		},
		{
			name: "numeric_listmonk_legacy_shape",
			in:   pgtype.Numeric{Int: big.NewInt(1), Exp: 0, Valid: true},
		},
		{
			name: "interval",
			in:   pgtype.Interval{Microseconds: 3600_000_000, Days: 7, Months: 1, Valid: true},
		},
		{
			name: "pgtime",
			in:   pgtype.Time{Microseconds: 12345, Valid: true},
		},
		{
			name: "bits",
			in:   pgtype.Bits{Bytes: []byte{0xAB, 0xCD}, Len: 16, Valid: true},
		},
		{
			name: "line",
			in:   pgtype.Line{A: 1, B: -1, C: 0, Valid: true},
		},
		{
			name: "path_open",
			in:   pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 0}}, Closed: false, Valid: true},
		},
		{
			name: "path_closed",
			in:   pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}}, Closed: true, Valid: true},
		},
		{
			name: "circle",
			in:   pgtype.Circle{P: pgtype.Vec2{X: 5, Y: 5}, R: 2.5, Valid: true},
		},
		{
			name: "tid",
			in:   pgtype.TID{BlockNumber: 42, OffsetNumber: 7, Valid: true},
		},
		{
			name: "tsvector",
			in: pgtype.TSVector{Lexemes: []pgtype.TSVectorLexeme{
				{Word: "cat", Positions: []pgtype.TSVectorPosition{{Position: 1, Weight: pgtype.TSVectorWeightD}}},
				{Word: "fish", Positions: []pgtype.TSVectorPosition{{Position: 2, Weight: pgtype.TSVectorWeightA}}},
			}, Valid: true},
		},
		{
			name: "range_int4",
			// Bound element types widen to int64 through the JSON
			// reload path (json.Number.Int64() on integer literals),
			// matching the YAML widen documented in
			// pgtype_yaml_round_trip_test.go's range_int4 case.
			in: pgtype.Range[any]{Lower: int64(1), Upper: int64(10), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		},
		{
			name: "range_empty",
			in:   pgtype.Range[any]{LowerType: pgtype.Empty, UpperType: pgtype.Empty, Valid: true},
		},
		{
			name: "range_numeric_bounds",
			// Nested pgtype values inside a range round-trip through
			// the same cell dispatch (the encoder calls
			// cellValueToJSON recursively on Lower/Upper).
			in: pgtype.Range[any]{
				Lower:     pgtype.Numeric{Int: big.NewInt(1), Exp: 0, Valid: true},
				Upper:     pgtype.Numeric{Int: big.NewInt(100), Exp: 0, Valid: true},
				LowerType: pgtype.Inclusive,
				UpperType: pgtype.Exclusive,
				Valid:     true,
			},
		},
		{
			name: "netip_prefix_v4",
			in:   netip.MustParsePrefix("192.168.1.0/24"),
		},
		{
			name: "netip_prefix_v6",
			in:   netip.MustParsePrefix("2001:db8::/32"),
		},
		{
			name: "macaddr",
			in:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		},
		{
			name: "macaddr8",
			in:   net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77},
		},
		{
			name: "raw_cell",
			in:   PostgresV3CellRaw{Format: 1, Bytes: []byte{0x01, 0x02, 0x03}},
		},

		// ===== Ambiguous shapes (require $pgtype discriminator) =====
		{
			name: "point",
			in:   pgtype.Point{P: pgtype.Vec2{X: 1.5, Y: 2.5}, Valid: true},
		},
		{
			name: "lseg",
			in:   pgtype.Lseg{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 3, Y: 4}}, Valid: true},
		},
		{
			name: "box",
			in:   pgtype.Box{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}}, Valid: true},
		},
		{
			name: "polygon",
			in:   pgtype.Polygon{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}}, Valid: true},
		},
		{
			name: "hstore",
			in: pgtype.Hstore{
				"key1":  stringPtr("value1"),
				"key2":  nil,
				"empty": stringPtr(""),
			},
		},
		{
			name: "multirange_int4",
			in: pgtype.Multirange[pgtype.Range[any]]{
				{Lower: int64(1), Upper: int64(5), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
				{Lower: int64(10), Upper: int64(20), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
			},
		},

		// ===== Native cell types =====
		{
			name: "null",
			in:   nil,
		},
		{
			name: "bool_true",
			in:   true,
		},
		{
			name: "int_value",
			in:   int64(42),
		},
		{
			name: "negative_int",
			in:   int64(-12345),
		},
		{
			name: "float_value",
			in:   3.14,
		},
		{
			name: "string_value",
			in:   "Lizard",
		},
		{
			name: "binary_value",
			in:   []byte{0x01, 0x02, 0x03, 0xFF},
		},
		{
			name: "timestamp_value",
			in:   time.Date(2026, 5, 3, 12, 30, 45, 123_456_789, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := NewValueCell(tc.in)
			body, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Regression guard: the default reflective encoder emits
			// the cell's lone exported field as `{"Value":...}` —
			// that's exactly the shape that broke postgres-v3 replay.
			// Even cells whose Value happens to be a struct with its
			// own `Valid` field (Numeric, Range, …) must NOT begin
			// with `"Value":` because the cell-level marshaller
			// dispatches before encoding/json sees the wrapper.
			if bytes.HasPrefix(bytes.TrimSpace(body), []byte(`{"Value":`)) {
				t.Errorf("MarshalJSON emitted struct wrapper {\"Value\":...}; expected unwrapped value:\n%s", body)
			}
			var got PostgresV3Cell
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal: %v\n--JSON--\n%s", err, body)
			}
			if !pgtypeEqual(t, got.Value, tc.in) {
				t.Errorf("round-trip drift:\n got  %#v\n want %#v\n--JSON--\n%s", got.Value, tc.in, body)
			}
		})
	}
}

// TestPgtypeJSONNoStructWrapper is the focused regression test for the
// production bug: a fresh PostgresV3Cell marshalled through encoding/json
// must NOT carry the `{"Value":...}` reflective-encoder wrapper. This
// stays even if every other test in this file is deleted, so the seam
// the petclinic / echo-mysql / mysql-fuzzer failures all traced back to
// can never silently regress.
func TestPgtypeJSONNoStructWrapper(t *testing.T) {
	cell := NewValueCell(int64(6))
	body, err := json.Marshal(cell)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte(`"Value"`)) {
		t.Fatalf("MarshalJSON must not emit the {\"Value\":...} reflective-encoder wrapper that broke postgres-v3 replay:\n%s", body)
	}
	if string(body) != "6" {
		t.Errorf("MarshalJSON of int cell: got %q want \"6\"", string(body))
	}
}

// TestPgtypeJSONJSONBNumberRecovery pins the recursive number-recovery
// inside generic jsonb-shaped maps. Without the recursive walk in
// recoverPgMappingFromJSON's fall-through branch, json.Number leaks
// through map[string]any cells; PostgresV3Cell.GobEncode then errors
// out with `unsupported Value type json.Number` when the cell flows
// through the agent's gob transport (sidecar → agent), surfacing in
// the postgres-fuzzer sample as `failed to gob-encode request body
// for storemocks` and aborting the test set.
func TestPgtypeJSONJSONBNumberRecovery(t *testing.T) {
	// Hand-craft a jsonb-like cell value: a generic map whose key set
	// doesn't match any pgtype canonical shape.
	body := []byte(`{"n":42, "f":3.14, "nested":{"k":7}, "arr":[1, 2, 3]}`)
	var cell PostgresV3Cell
	if err := cell.UnmarshalJSON(body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, ok := cell.Value.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cell.Value)
	}
	if v, ok := m["n"].(int64); !ok || v != 42 {
		t.Errorf("jsonb integer leaf: got %#v (%T) want int64(42) — json.Number must not leak through generic map fall-through", m["n"], m["n"])
	}
	if v, ok := m["f"].(float64); !ok || v != 3.14 {
		t.Errorf("jsonb float leaf: got %#v (%T) want float64(3.14)", m["f"], m["f"])
	}
	nested, ok := m["nested"].(map[string]any)
	if !ok {
		t.Fatalf("jsonb nested object: got %T want map[string]any", m["nested"])
	}
	if v, ok := nested["k"].(int64); !ok || v != 7 {
		t.Errorf("jsonb nested integer: got %#v (%T) want int64(7)", nested["k"], nested["k"])
	}
	arr, ok := m["arr"].([]any)
	if !ok {
		t.Fatalf("jsonb array: got %T want []any", m["arr"])
	}
	if len(arr) != 3 {
		t.Fatalf("jsonb array len: got %d want 3", len(arr))
	}
	for i, want := range []int64{1, 2, 3} {
		if v, ok := arr[i].(int64); !ok || v != want {
			t.Errorf("jsonb array[%d]: got %#v (%T) want int64(%d)", i, arr[i], arr[i], want)
		}
	}
}

// TestPgtypeJSONRowsRoundTrip mirrors the on-disk shape that was
// actually failing in the petclinic mocks: a row of mixed int+string
// cells inside a sequence. Pre-fix the wire form was
// `[[{"Value":1},{"Value":"Cat"}]]`; post-fix it must be `[[1,"Cat"]]`.
func TestPgtypeJSONRowsRoundTrip(t *testing.T) {
	row := PostgresV3Cells{NewValueCell(int64(1)), NewValueCell("Cat")}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(body) != `[1,"Cat"]` {
		t.Errorf("Cells row JSON shape:\n got  %s\n want %s", string(body), `[1,"Cat"]`)
	}
	var got PostgresV3Cells
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(cells) = %d want 2", len(got))
	}
	if got[0].Value != int64(1) {
		t.Errorf("cell[0]: got %#v want int64(1)", got[0].Value)
	}
	if got[1].Value != "Cat" {
		t.Errorf("cell[1]: got %#v want \"Cat\"", got[1].Value)
	}
}

// stringPtr is shared with the YAML test file; redefining here would
// duplicate the symbol. Instead, reuse the YAML test's helper which
// lives in the same package.
