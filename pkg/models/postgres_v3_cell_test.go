package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"math/big"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	yaml "gopkg.in/yaml.v3"
)

func stringPtr(s string) *string { return &s }

// The logical-value PostgresV3Cell round-trips native Go values via
// YAML. Key invariants:
//
//  1. NULL (Value=nil) serializes as YAML null and decodes back to nil.
//  2. Empty string (Value="") serializes as plain "" and stays a
//     distinct value from NULL on decode.
//  3. int64 values round-trip as YAML integers (no type coercion to
//     int or float).
//  4. time.Time values round-trip via YAML !!timestamp.
//  5. []byte values round-trip via !!binary.
//  6. Sequences (PostgresV3Cells) dispatch through the custom
//     UnmarshalYAML for every element, including null ones.

func TestPostgresV3Cell_NullVsEmptyString(t *testing.T) {
	cells := PostgresV3Cells{NullCell(), NewValueCell("")}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: `- null\n- ""\n` (or similar — yaml.v3 may use `- ~`).
	if !bytes.Contains(buf, []byte("null")) && !bytes.Contains(buf, []byte("~")) {
		t.Errorf("marshalled NULL missing from YAML:\n%s", buf)
	}

	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cells, want 2", len(got))
	}
	if got[0].Value != nil {
		t.Errorf("cell[0] (NULL) decoded as %v, want nil", got[0].Value)
	}
	if got[1].IsNull() {
		t.Errorf("cell[1] (empty string) decoded as NULL")
	}
	if got[1].Value != "" {
		t.Errorf("cell[1] decoded as %v (type %T), want empty string", got[1].Value, got[1].Value)
	}
}

func TestPostgresV3Cell_IntRoundtrip(t *testing.T) {
	cells := PostgresV3Cells{NewValueCell(int64(246)), NewValueCell(int64(-1)), NewValueCell(int64(9223372036854775807))}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal:\n%s\nerr: %v", buf, err)
	}
	want := []int64{246, -1, 9223372036854775807}
	for i, w := range want {
		n, ok := got[i].Value.(int64)
		if !ok {
			t.Errorf("cell[%d] type = %T, want int64", i, got[i].Value)
			continue
		}
		if n != w {
			t.Errorf("cell[%d] = %d, want %d", i, n, w)
		}
	}
}

func TestPostgresV3Cell_StringRoundtrip(t *testing.T) {
	cases := []string{"flow", "priority-i23-333b", "", "ünïcödé", "with spaces", "203"}
	cells := make(PostgresV3Cells, len(cases))
	for i, s := range cases {
		cells[i] = NewValueCell(s)
	}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	for i, want := range cases {
		s, ok := got[i].Value.(string)
		if !ok {
			t.Errorf("cell[%d] type = %T, want string (yaml was: %s)", i, got[i].Value, buf)
			continue
		}
		if s != want {
			t.Errorf("cell[%d] = %q, want %q", i, s, want)
		}
	}
}

func TestPostgresV3Cell_TimeRoundtrip(t *testing.T) {
	tm := time.Date(2026, 4, 24, 14, 25, 37, 580669000, time.UTC)
	cells := PostgresV3Cells{NewValueCell(tm)}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	gotTime, ok := got[0].Value.(time.Time)
	if !ok {
		t.Fatalf("cell[0] type = %T, want time.Time (yaml was: %s)", got[0].Value, buf)
	}
	if !gotTime.Equal(tm) {
		t.Errorf("cell[0] = %v, want %v", gotTime, tm)
	}
}

func TestPostgresV3Cell_BinaryRoundtrip(t *testing.T) {
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01}
	cells := PostgresV3Cells{NewValueCell(raw)}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	// Should use !!binary tag for arbitrary bytes.
	if !bytes.Contains(buf, []byte("!!binary")) {
		t.Errorf("expected !!binary tag in YAML:\n%s", buf)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	gotBytes, ok := got[0].Value.([]byte)
	if !ok {
		t.Fatalf("cell[0] type = %T, want []byte", got[0].Value)
	}
	if !bytes.Equal(gotBytes, raw) {
		t.Errorf("cell[0] = %x, want %x", gotBytes, raw)
	}
}

func TestPostgresV3Cell_BoolRoundtrip(t *testing.T) {
	cells := PostgresV3Cells{NewValueCell(true), NewValueCell(false)}
	buf, err := yaml.Marshal(cells)
	if err != nil {
		t.Fatal(err)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	if got[0].Value != true || got[1].Value != false {
		t.Errorf("bool round-trip: got %v, %v", got[0].Value, got[1].Value)
	}
}

func TestPostgresV3Cell_MixedRow(t *testing.T) {
	// Realistic customer_tag row: id int8, created_at timestamptz,
	// created_by varchar, customer_id varchar, tag varchar.
	tm := time.Date(2026, 4, 24, 14, 25, 37, 580669000, time.UTC)
	row := PostgresV3Cells{
		NewValueCell(int64(246)),
		NewValueCell(tm),
		NewValueCell("flow"),
		NewValueCell("11"),
		NewValueCell("priority-i23-333b"),
	}
	buf, err := yaml.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	// This is the human-readable win: no !!binary, no base64.
	if bytes.Contains(buf, []byte("!!binary")) {
		t.Errorf("mixed row unexpectedly used !!binary:\n%s", buf)
	}
	var got PostgresV3Cells
	if err := yaml.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d cells, want 5", len(got))
	}
	if n, _ := got[0].Value.(int64); n != 246 {
		t.Errorf("cell[0] (id) = %v (%T), want int64(246)", got[0].Value, got[0].Value)
	}
	if tt, _ := got[1].Value.(time.Time); !tt.Equal(tm) {
		t.Errorf("cell[1] (created_at) = %v, want %v", got[1].Value, tm)
	}
	for i, want := range []string{"flow", "11", "priority-i23-333b"} {
		if s, _ := got[2+i].Value.(string); s != want {
			t.Errorf("cell[%d] = %v, want %q", 2+i, got[2+i].Value, want)
		}
	}
}

// JSON round-trip is also needed for MockOutgoing / syncMock paths
// that serialize via JSON. The default encoding/json handles any
// natively — integers become json.Number or float64 depending on
// UseNumber, time.Time becomes RFC3339 string, etc. Pin a minimal
// shape to surface any breakage early.
func TestPostgresV3Cell_JSONRoundtrip_Int(t *testing.T) {
	cell := NewValueCell(int64(246))
	buf, err := json.Marshal(cell.Value)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "246" {
		t.Errorf("JSON int = %s, want 246", buf)
	}
}

func TestPostgresV3Cell_JSONRoundtrip_Null(t *testing.T) {
	cell := NullCell()
	buf, err := json.Marshal(cell.Value)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "null" {
		t.Errorf("JSON null = %s", buf)
	}
}

// ---- Gob round-trip — sidecar-to-agent stream path -------------------

func TestPostgresV3Cell_Gob_AllTypes(t *testing.T) {
	// The recorder sidecar streams mocks to the agent via encoding/gob.
	// Interface{} fields require either gob.Register or a custom
	// (GobEncoder, GobDecoder) pair. PostgresV3Cell implements the
	// pair — this test pins each Go type that can appear in Value.
	cases := []struct {
		name string
		in   any
	}{
		{"null", nil},
		{"int16", int16(246)},
		{"int32", int32(246)},
		{"int64", int64(246)},
		{"uint16", uint16(65000)},
		{"uint32", uint32(4294967295)}, // max oid
		{"uint64", uint64(18446744073709551615)},
		// PG arrays — pgtype hands back []interface{} whose elements
		// are per-OID logical Go types. Mixed primitive types pin the
		// recursive cell encoding (each element re-enters GobEncode
		// via its own tag byte). Listmonk's INSERT INTO lists (...,
		// tags TEXT[], ...) is the regression case: pre-fix, the
		// encoder rejected []interface{} and the recorder dropped
		// the entire mock including its ParameterDescription, so
		// replay served a 0-arg ParseComplete to a 7-arg INSERT.
		{"slice_text_array", []interface{}{"vip", "churn-risk", "active"}},
		{"slice_mixed_types", []interface{}{int32(42), "hello", true, nil, []byte{0x01, 0x02}}},
		{"slice_empty", []interface{}{}},
		{"slice_nested_2d", []interface{}{
			[]interface{}{"a", "b"},
			[]interface{}{"c", "d"},
		}},
		// PG json/jsonb columns: pgtype scans into map[string]interface{}.
		// Listmonk's `settings` table is the regression case — its
		// `value` column is jsonb and the bootstrap query was dropping
		// every mock pre-fix.
		{"jsonb_simple", map[string]interface{}{"name": "vip", "active": true, "count": int32(42)}},
		{"jsonb_empty", map[string]interface{}{}},
		{"jsonb_nested", map[string]interface{}{
			"smtp":     map[string]interface{}{"host": "mail.example.com", "port": int32(25)},
			"app.lang": "en",
			"tags":     []interface{}{"transactional", "marketing"},
		}},
		// Tag 15 — Go untyped `int`. Reachable on yaml.v3 reload of
		// nested integers inside slice / jsonb cells; the recursive
		// PostgresV3Cell.GobEncode would fail-loud without this case.
		{"int_native", 42},
		{"int_zero", 0},
		// Tag 16 — float32 (PG float4). Distinct from float64 above.
		{"float32", float32(3.14)},
		// Tag 17 — pgtype.Numeric. Three flavours pin the wire shape:
		// a typical decimal value, NaN, and an infinity sentinel.
		{"numeric_decimal", pgtype.Numeric{Int: big.NewInt(123456), Exp: -2, Valid: true}},
		{"numeric_nan", pgtype.Numeric{NaN: true, Valid: true}},
		{"numeric_inf", pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}},
		// Tags 18-29 — proactive close of the §D audit (every type
		// pgtype hands back from a default-registered OID into *any).
		// Fixtures cover the canonical wire shape of each PG type so
		// recorder→replay never fail-louds on a workload we haven't
		// seen yet.
		{"int8", int8(-42)},
		{"interval", pgtype.Interval{Microseconds: 3600_000_000, Days: 7, Months: 1, Valid: true}},
		{"pgtime", pgtype.Time{Microseconds: 12345, Valid: true}},
		{"bits", pgtype.Bits{Bytes: []byte{0xAB, 0xCD}, Len: 16, Valid: true}},
		{"point", pgtype.Point{P: pgtype.Vec2{X: 1.5, Y: 2.5}, Valid: true}},
		{"line", pgtype.Line{A: 1, B: -1, C: 0, Valid: true}},
		{"lseg", pgtype.Lseg{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 3, Y: 4}}, Valid: true}},
		{"box", pgtype.Box{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}}, Valid: true}},
		{"path_open", pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 0}}, Closed: false, Valid: true}},
		{"path_closed", pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}}, Closed: true, Valid: true}},
		{"polygon", pgtype.Polygon{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}}, Valid: true}},
		{"circle", pgtype.Circle{P: pgtype.Vec2{X: 5, Y: 5}, R: 2.5, Valid: true}},
		{"tid", pgtype.TID{BlockNumber: 42, OffsetNumber: 7, Valid: true}},
		{"tsvector", pgtype.TSVector{Lexemes: []pgtype.TSVectorLexeme{
			{Word: "cat", Positions: []pgtype.TSVectorPosition{{Position: 1, Weight: pgtype.TSVectorWeightD}}},
			{Word: "fish", Positions: []pgtype.TSVectorPosition{{Position: 2, Weight: pgtype.TSVectorWeightA}}},
		}, Valid: true}},
		{"netip_prefix_v4_host", netip.MustParsePrefix("192.168.1.10/32")},
		{"netip_prefix_v4_net", netip.MustParsePrefix("10.0.0.0/8")},
		{"netip_prefix_v6", netip.MustParsePrefix("2001:db8::/64")},
		{"macaddr_6", net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}},
		{"macaddr_8", net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00, 0x11}},
		{"hstore", pgtype.Hstore{
			"key1":    stringPtr("value1"),
			"key2":    nil, // SQL NULL inside the hstore
			"empty":   stringPtr(""),
			"unicode": stringPtr("café"),
		}},
		{"range_int4", pgtype.Range[any]{Lower: int32(1), Upper: int32(10), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}},
		{"range_empty", pgtype.Range[any]{LowerType: pgtype.Empty, UpperType: pgtype.Empty, Valid: true}},
		{"multirange_int4", pgtype.Multirange[pgtype.Range[any]]{
			{Lower: int32(1), Upper: int32(5), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
			{Lower: int32(10), Upper: int32(20), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		}},
		{"float64", 3.14},
		{"string", "priority-i23-333b"},
		{"bool", true},
		{"bytes", []byte{0x01, 0x02, 0x03}},
		{"time", time.Date(2026, 4, 24, 14, 25, 37, 580669000, time.UTC)},
		{"raw", PostgresV3CellRaw{Format: 1, Bytes: []byte{0x00, 0x00, 0x00, 0x01}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := NewValueCell(tc.in)
			var buf bytes.Buffer
			enc := gob.NewEncoder(&buf)
			if err := enc.Encode(orig); err != nil {
				t.Fatalf("encode: %v", err)
			}
			var got PostgresV3Cell
			if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// time.Time equality must use Equal.
			switch want := tc.in.(type) {
			case time.Time:
				tgot, ok := got.Value.(time.Time)
				if !ok || !tgot.Equal(want) {
					t.Errorf("time round-trip: got %v, want %v", got.Value, want)
				}
			case []byte:
				gbs, ok := got.Value.([]byte)
				if !ok || !bytes.Equal(gbs, want) {
					t.Errorf("bytes round-trip: got %v, want %v", got.Value, want)
				}
			case PostgresV3CellRaw:
				gr, ok := got.Value.(PostgresV3CellRaw)
				if !ok || gr.Format != want.Format || !bytes.Equal(gr.Bytes, want.Bytes) {
					t.Errorf("raw round-trip: got %+v, want %+v", got.Value, want)
				}
			case []interface{}:
				// Slice cells (PG arrays) need DeepEqual — `==` on
				// interface-typed slices panics. Each element should
				// have round-tripped via the recursive nested cell
				// encoding, so the rebuilt []interface{} matches the
				// input element-for-element including primitive types,
				// nil, and nested slices for multi-dim arrays.
				if !reflect.DeepEqual(got.Value, want) {
					t.Errorf("slice round-trip: got %#v, want %#v", got.Value, want)
				}
			case map[string]interface{}:
				// JSONB cells — same DeepEqual story as slices, with
				// the additional sort-on-encode determinism property
				// (the gob bytes for the same input map must be
				// stable across runs because Go's map iteration order
				// is randomised).
				if !reflect.DeepEqual(got.Value, want) {
					t.Errorf("jsonb round-trip: got %#v, want %#v", got.Value, want)
				}
			case pgtype.Interval, pgtype.Time, pgtype.Bits, pgtype.Point,
				pgtype.Line, pgtype.Lseg, pgtype.Box, pgtype.Path,
				pgtype.Polygon, pgtype.Circle, pgtype.TID,
				pgtype.TSVector, netip.Prefix, net.HardwareAddr,
				pgtype.Hstore, pgtype.Range[any],
				pgtype.Multirange[pgtype.Range[any]]:
				if !reflect.DeepEqual(got.Value, want) {
					t.Errorf("%T round-trip drift:\n got  %+v\n want %+v", want, got.Value, want)
				}
			case pgtype.Numeric:
				// Numeric needs DeepEqual too — *big.Int can't be
				// compared with `==`. Both sides must agree on Int
				// (numeric value, not pointer identity), Exp, NaN,
				// InfinityModifier, and Valid.
				gn, ok := got.Value.(pgtype.Numeric)
				if !ok {
					t.Fatalf("numeric round-trip: got %T, want pgtype.Numeric", got.Value)
				}
				if gn.NaN != want.NaN || gn.Valid != want.Valid ||
					gn.Exp != want.Exp || gn.InfinityModifier != want.InfinityModifier {
					t.Errorf("numeric meta drift: got %+v, want %+v", gn, want)
				}
				if (gn.Int == nil) != (want.Int == nil) {
					t.Errorf("numeric Int nil-ness drift: got %v, want %v", gn.Int, want.Int)
				}
				if gn.Int != nil && want.Int != nil && gn.Int.Cmp(want.Int) != 0 {
					t.Errorf("numeric Int drift: got %s, want %s", gn.Int.String(), want.Int.String())
				}
			default:
				if got.Value != tc.in {
					t.Errorf("round-trip: got %v (%T), want %v (%T)", got.Value, got.Value, tc.in, tc.in)
				}
			}
		})
	}
}

// TestPostgresV3Cell_Gob_CellsSlice — round-trip a slice of cells.
// The top-level path the recorder hits on every row.
func TestPostgresV3Cell_Gob_CellsSlice(t *testing.T) {
	row := PostgresV3Cells{
		NewValueCell(int64(246)),
		NewValueCell(time.Date(2026, 4, 24, 14, 25, 37, 0, time.UTC)),
		NewValueCell("flow"),
		NullCell(),
		NewValueCell("priority-i24-2bd6"),
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(row); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got PostgresV3Cells
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != len(row) {
		t.Fatalf("arity: got %d, want %d", len(got), len(row))
	}
	if got[0].Value.(int64) != 246 || got[2].Value.(string) != "flow" || !got[3].IsNull() {
		t.Errorf("mismatch: %+v", got)
	}
}

// TestPostgresV3Cell_RawValue_YAMLtoGobRoundtrip pins the fix for the
// sap_demo_java INSERT ... RETURNING id crash. The recorder stores
// binary int cells that pgtype can't decode (e.g. unknown-OID columns
// where no RowDescription was captured) as PostgresV3CellRaw. YAML
// marshals the struct as `{format: N, bytes: [...]}`. Before the
// UnmarshalYAML fix, the load path decoded this mapping as
// map[string]any, and the subsequent gob round-trip's default
// stringify turned the cell value into a fmt.Sprint of the map —
// which the codec then refused to re-encode, emitting 0 bytes where
// 8-byte binary int8 was expected. This test pins the round-trip
// through both YAML and gob to prevent regressions.
func TestPostgresV3Cell_RawValue_YAMLtoGobRoundtrip(t *testing.T) {
	orig := NewValueCell(PostgresV3CellRaw{Format: 1, Bytes: []byte{0, 0, 0, 0, 0, 0, 0, 6}})

	// YAML marshal + unmarshal — the on-disk mocks.yaml path.
	yb, err := yaml.Marshal(orig)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	var afterYAML PostgresV3Cell
	if err := yaml.Unmarshal(yb, &afterYAML); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	raw, ok := afterYAML.Value.(PostgresV3CellRaw)
	if !ok {
		t.Fatalf("yaml round-trip: Value is %T %+v, want PostgresV3CellRaw", afterYAML.Value, afterYAML.Value)
	}
	if raw.Format != 1 || !bytes.Equal(raw.Bytes, []byte{0, 0, 0, 0, 0, 0, 0, 6}) {
		t.Errorf("yaml round-trip mismatch: %+v", raw)
	}

	// Gob encode + decode — the sidecar-to-agent stream path.
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(afterYAML); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	var afterGob PostgresV3Cell
	if err := gob.NewDecoder(&buf).Decode(&afterGob); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	raw2, ok := afterGob.Value.(PostgresV3CellRaw)
	if !ok {
		t.Fatalf("gob round-trip: Value is %T %+v, want PostgresV3CellRaw", afterGob.Value, afterGob.Value)
	}
	if raw2.Format != 1 || !bytes.Equal(raw2.Bytes, []byte{0, 0, 0, 0, 0, 0, 0, 6}) {
		t.Errorf("gob round-trip mismatch: %+v", raw2)
	}
}

// Pre-PostgresV3Cell recordings encoded SQL NULL as the printable
// string "~~KEPLOY_PG_NULL~~" inside [][]string Rows. When the new
// binary loads those mocks for replay the sentinel must translate back
// to a proper NULL — otherwise every legacy NULL cell silently turns
// into a real text string, breaking row-level comparisons.
func TestPostgresV3Cell_LegacyNullSentinel_DecodedAsNull(t *testing.T) {
	for _, body := range []string{
		// Double-quoted: yaml.v3 tags as !!str.
		"\"~~KEPLOY_PG_NULL~~\"\n",
		// Plain untagged scalar — same path, different tag branch.
		"~~KEPLOY_PG_NULL~~\n",
	} {
		var c PostgresV3Cell
		if err := yaml.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("legacy null sentinel %q unmarshal: %v", body, err)
		}
		if !c.IsNull() {
			t.Errorf("legacy null sentinel %q: Value=%v (type %T), want NULL", body, c.Value, c.Value)
		}
	}
}

// A typed-nil *PostgresV3CellRaw must not crash GobEncode and must
// be treated as SQL NULL across IsNull(), MarshalYAML, and
// GobEncode — otherwise the cell would test as not-NULL via IsNull
// but serialize as NULL through gob, silently breaking NULL-vs-
// empty diffs at replay time.
//
// Defence is two-layered: NewValueCell normalises typed-nil to
// plain nil at construction, and IsNull defensively reports true
// for cells whose Value field still holds a typed-nil (e.g. mocks
// constructed via the struct literal). Both gates must hold.
func TestPostgresV3Cell_TypedNilRawValue_NormalizedToNull(t *testing.T) {
	t.Run("NewValueCell collapses typed-nil to plain nil", func(t *testing.T) {
		cell := NewValueCell((*PostgresV3CellRaw)(nil))
		if cell.Value != nil {
			t.Errorf("NewValueCell typed-nil: Value=%v (type %T), want plain nil", cell.Value, cell.Value)
		}
		if !cell.IsNull() {
			t.Errorf("NewValueCell typed-nil: IsNull()=false, want true")
		}
	})

	t.Run("IsNull catches struct-literal typed-nil", func(t *testing.T) {
		cell := PostgresV3Cell{Value: (*PostgresV3CellRaw)(nil)}
		if !cell.IsNull() {
			t.Error("struct-literal typed-nil cell: IsNull()=false, want true")
		}
	})

	t.Run("Gob round-trip emits as NULL", func(t *testing.T) {
		cell := PostgresV3Cell{Value: (*PostgresV3CellRaw)(nil)}

		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(cell); err != nil {
			t.Fatalf("gob encode of typed-nil *PostgresV3CellRaw must not error: %v", err)
		}
		var got PostgresV3Cell
		if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
			t.Fatalf("gob decode: %v", err)
		}
		if !got.IsNull() || got.Value != nil {
			t.Errorf("typed-nil *PostgresV3CellRaw round-tripped as %v (type %T), want plain-nil NULL", got.Value, got.Value)
		}
	})
}

// yaml.v3 v3.0.1 has an emitter bug for plain strings containing
// embedded tab characters: when the string also has a newline, the
// emitter picks a literal block scalar (`|4-`) and writes the tab byte
// verbatim on a continuation line, which the parser then refuses with
// "found a tab character where an indentation space is expected" once
// the scalar lives inside a sequence (the shape our Rows uses).
// PostgresV3Cell.MarshalYAML has to route those strings through a
// double-quoted scalar instead; this test pins the fix by running a
// catalogue of known-problematic values through a full sequence-of-
// cells round-trip — the exact shape mocks.yaml stores for a row.
func TestPostgresV3Cell_RowRoundTrip_TabInStringSurvives(t *testing.T) {
	tricky := []string{
		"\thello",                 // leading tab
		"hello\tworld",            // embedded tab
		"\n\thello",               // leading newline + tab — the echo-sql failure
		"line1\n\tline2\n\tline3", // block-scalar with tabs
		"trailing\t",              // trailing tab
		" leading space",          // leading whitespace
		"trailing space ",         // trailing whitespace
		"plain no weirdness",      // baseline (should still round-trip)
		"",                        // empty string (distinct from NULL)
	}

	row := make(PostgresV3Cells, len(tricky))
	for i, s := range tricky {
		row[i] = NewValueCell(s)
	}

	buf, err := yaml.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal row with tab-bearing strings: %v", err)
	}
	var decoded PostgresV3Cells
	if err := yaml.Unmarshal(buf, &decoded); err != nil {
		t.Fatalf("Unmarshal re-read failed (yaml.v3 emitter regression?): %v\n--YAML--\n%s", err, buf)
	}
	if len(decoded) != len(tricky) {
		t.Fatalf("cell count: got %d, want %d", len(decoded), len(tricky))
	}
	for i, want := range tricky {
		got, ok := decoded[i].Value.(string)
		if !ok {
			t.Errorf("cell[%d] Value type: got %T, want string (value=%v)", i, decoded[i].Value, decoded[i].Value)
			continue
		}
		if got != want {
			t.Errorf("cell[%d] round-trip: got %q, want %q", i, got, want)
		}
	}
}

// yaml.v3 folds long !!binary scalars onto continuation lines and the
// embedded whitespace bleeds into node.Value; base64.StdEncoding
// rejects that, so UnmarshalYAML strips it before decoding. This test
// feeds a hand-wrapped payload to pin the fix.
func TestPostgresV3Cell_BinaryWithFoldedWhitespace_Decodes(t *testing.T) {
	// yaml-authored payload with a real line wrap inside the scalar —
	// identical in effect to what yaml.v3 emits for long binary cells.
	// The original bytes are the 12-byte sequence below; base64 is
	// "AAECAwQFBgcICQoL", wrapped at position 6 across two lines. With
	// the `|` indicator the newline becomes whitespace in the scalar.
	body := "!!binary |\n  AAECAwQF\n  BgcICQoL\n"

	var c PostgresV3Cell
	if err := yaml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("folded !!binary unmarshal: %v", err)
	}
	got, ok := c.Value.([]byte)
	if !ok {
		t.Fatalf("folded !!binary: Value=%v (type %T), want []byte", c.Value, c.Value)
	}
	want := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B}
	if !bytes.Equal(got, want) {
		t.Errorf("folded !!binary decode: got %x, want %x", got, want)
	}
}
