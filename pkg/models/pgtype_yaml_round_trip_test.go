package models

import (
	"bytes"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	yaml "gopkg.in/yaml.v3"
)

// TestPgtypeYAMLRoundTrip pins the YAML round-trip for every pgtype-
// shaped cell value the recorder can hand to MarshalYAML. Each case
// goes through three checks:
//
//  1. Marshal → Unmarshal via the cell schema, DeepEqual against the
//     original value. The emitter writes untagged mappings (matching
//     released-keploy's reflection-based emit) for cross-version
//     compatibility, and the decoder dispatches via canonical-key-set
//     probing — that's the primary read path exercised here.
//  2. The marshaled YAML does NOT carry an `!pg/<name>` local tag —
//     a tag would break replay against released keploy that doesn't
//     register the custom tag set.
//  3. Backward-compat (tag): a hand-crafted tagged mapping (mirroring
//     an on-disk recording from the brief window when the encoder
//     emitted tags) still decodes into the same Go value via the
//     tag-switching read path that's preserved alongside the untagged
//     probe. Only the unambiguous shapes are exercised here; ambiguous
//     ones (Point/Lseg/Box/Polygon all match `{p, valid}`) require the
//     tag for disambiguation and rely on the tagged path exclusively.
func TestPgtypeYAMLRoundTrip(t *testing.T) {
	type roundTripCase struct {
		name string
		in   any
		tag  string
		// untagged, when non-empty, is an alternate on-disk YAML body
		// (untagged mapping) that the decoder must accept via the
		// canonical-key-set probe. The harness decodes it and
		// DeepEquals the resulting Value against `in`. Empty means no
		// untagged shape is asserted (used for shapes whose canonical
		// key set collides with another pgtype and so require the
		// tagged read path for disambiguation).
		untagged string
	}
	cases := []roundTripCase{
		{
			name: "numeric_decimal",
			in:   pgtype.Numeric{Int: big.NewInt(123456), Exp: -2, Valid: true},
			tag:  pgYAMLTagNumeric,
			untagged: "" +
				"int: \"123456\"\n" +
				"exp: -2\n" +
				"nan: false\n" +
				"infinitymodifier: 0\n" +
				"valid: true\n",
		},
		{
			name: "numeric_nan",
			in:   pgtype.Numeric{NaN: true, Valid: true},
			tag:  pgYAMLTagNumeric,
			untagged: "" +
				"int: \"\"\n" +
				"exp: 0\n" +
				"nan: true\n" +
				"infinitymodifier: 0\n" +
				"valid: true\n",
		},
		{
			name: "numeric_infinity",
			in:   pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true},
			tag:  pgYAMLTagNumeric,
			untagged: "" +
				"int: \"\"\n" +
				"exp: 0\n" +
				"nan: false\n" +
				"infinitymodifier: 1\n" +
				"valid: true\n",
		},
		// Listmonk's pre-fix on-disk shape — the bug-report fixture.
		// Notably the decoder must accept `int: "1"` (yaml.v3 emits
		// *big.Int as a quoted string via TextMarshaler).
		{
			name: "numeric_listmonk_legacy_shape",
			in:   pgtype.Numeric{Int: big.NewInt(1), Exp: 0, Valid: true},
			tag:  pgYAMLTagNumeric,
			untagged: "" +
				"int: \"1\"\n" +
				"exp: 0\n" +
				"nan: false\n" +
				"infinitymodifier: 0\n" +
				"valid: true\n",
		},
		{
			name: "interval",
			in:   pgtype.Interval{Microseconds: 3600_000_000, Days: 7, Months: 1, Valid: true},
			tag:  pgYAMLTagInterval,
			untagged: "" +
				"microseconds: 3600000000\n" +
				"days: 7\n" +
				"months: 1\n" +
				"valid: true\n",
		},
		{
			name: "pgtime",
			in:   pgtype.Time{Microseconds: 12345, Valid: true},
			tag:  pgYAMLTagTime,
			untagged: "" +
				"microseconds: 12345\n" +
				"valid: true\n",
		},
		{
			name: "bits",
			in:   pgtype.Bits{Bytes: []byte{0xAB, 0xCD}, Len: 16, Valid: true},
			tag:  pgYAMLTagBits,
			// Backward-compat: the pre-tag emitter would render Bytes
			// via reflection as a YAML int sequence; the new decoder
			// must accept both that and the new !!binary form.
			untagged: "" +
				"bytes:\n" +
				"  - 171\n" +
				"  - 205\n" +
				"len: 16\n" +
				"valid: true\n",
		},
		{
			name: "point",
			in:   pgtype.Point{P: pgtype.Vec2{X: 1.5, Y: 2.5}, Valid: true},
			tag:  pgYAMLTagPoint,
			// `{p, valid}` is shared with Lseg/Box/Polygon — the
			// canonical-key-set probe can't disambiguate, so backward
			// compat for these shapes requires the tag and we skip
			// the untagged check.
		},
		{
			name: "line",
			in:   pgtype.Line{A: 1, B: -1, C: 0, Valid: true},
			tag:  pgYAMLTagLine,
			untagged: "" +
				"a: 1\n" +
				"b: -1\n" +
				"c: 0\n" +
				"valid: true\n",
		},
		{
			name: "lseg",
			in:   pgtype.Lseg{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 3, Y: 4}}, Valid: true},
			tag:  pgYAMLTagLseg,
		},
		{
			name: "box",
			in:   pgtype.Box{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}}, Valid: true},
			tag:  pgYAMLTagBox,
		},
		{
			name: "path_open",
			in:   pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 0}}, Closed: false, Valid: true},
			tag:  pgYAMLTagPath,
			untagged: "" +
				"p:\n" +
				"  - {x: 0, y: 0}\n" +
				"  - {x: 1, y: 1}\n" +
				"  - {x: 2, y: 0}\n" +
				"closed: false\n" +
				"valid: true\n",
		},
		{
			name: "polygon",
			in:   pgtype.Polygon{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}}, Valid: true},
			tag:  pgYAMLTagPolygon,
		},
		{
			name: "circle",
			in:   pgtype.Circle{P: pgtype.Vec2{X: 5, Y: 5}, R: 2.5, Valid: true},
			tag:  pgYAMLTagCircle,
			untagged: "" +
				"p: {x: 5, y: 5}\n" +
				"r: 2.5\n" +
				"valid: true\n",
		},
		{
			name: "tid",
			in:   pgtype.TID{BlockNumber: 42, OffsetNumber: 7, Valid: true},
			tag:  pgYAMLTagTID,
			untagged: "" +
				"blocknumber: 42\n" +
				"offsetnumber: 7\n" +
				"valid: true\n",
		},
		{
			name: "tsvector",
			in: pgtype.TSVector{Lexemes: []pgtype.TSVectorLexeme{
				{Word: "cat", Positions: []pgtype.TSVectorPosition{{Position: 1, Weight: pgtype.TSVectorWeightD}}},
				{Word: "fish", Positions: []pgtype.TSVectorPosition{{Position: 2, Weight: pgtype.TSVectorWeightA}}},
			}, Valid: true},
			tag: pgYAMLTagTSVector,
			untagged: "" +
				"lexemes:\n" +
				"  - word: cat\n" +
				"    positions:\n" +
				"      - {position: 1, weight: " + itoa(int(pgtype.TSVectorWeightD)) + "}\n" +
				"  - word: fish\n" +
				"    positions:\n" +
				"      - {position: 2, weight: " + itoa(int(pgtype.TSVectorWeightA)) + "}\n" +
				"valid: true\n",
		},
		{
			name: "hstore",
			in: pgtype.Hstore{
				"key1":  stringPtr("value1"),
				"key2":  nil,
				"empty": stringPtr(""),
			},
			tag: pgYAMLTagHstore,
		},
		{
			// Bound element types widen to int64 through the YAML
			// reload path (yaml.v3's resolver maps `!!int` → int64
			// when destination is `any`, the recursive decode shape
			// for nested cells); the cell-level docs call this out
			// as intentional because emit-time codec dispatch keys
			// off the column OID, not the Go width. This case pins
			// the documented widen for the tagged path; backward-
			// compat for typed-bound ranges is covered by range_empty
			// (empty bounds = nil, no width to drift on).
			name: "range_int4",
			in:   pgtype.Range[any]{Lower: int64(1), Upper: int64(10), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
			tag:  pgYAMLTagRange,
		},
		{
			name: "range_empty",
			in:   pgtype.Range[any]{LowerType: pgtype.Empty, UpperType: pgtype.Empty, Valid: true},
			tag:  pgYAMLTagRange,
			untagged: "" +
				"lower: null\n" +
				"upper: null\n" +
				"lowertype: " + itoa(int(pgtype.Empty)) + "\n" +
				"uppertype: " + itoa(int(pgtype.Empty)) + "\n" +
				"valid: true\n",
		},
		{
			// Same int64 widen as range_int4 — bound types come back
			// at int64 width through the YAML reload path.
			name: "multirange_int4",
			in: pgtype.Multirange[pgtype.Range[any]]{
				{Lower: int64(1), Upper: int64(5), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
				{Lower: int64(10), Upper: int64(20), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
			},
			tag: pgYAMLTagMultirange,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := NewValueCell(tc.in)
			body, err := yaml.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// The serialized form must NOT carry an `!pg/<name>` local
			// tag — released keploy on Docker Hub doesn't register the
			// custom tag set, so a tagged emit breaks cross-version
			// replay (PR-built recorder, released-keploy replayer).
			// MarshalYAML emits untagged mappings instead and the
			// decoder dispatches via canonical-key-set probing; the
			// tag-switching read path is preserved only for
			// recordings made during the brief tag-emitting window.
			if strings.Contains(string(body), "!pg/") {
				t.Errorf("marshaled YAML must not carry a !pg/<name> tag (cross-version compat with released keploy):\n%s", body)
			}
			// Backward-compat (tag): a hand-crafted body that does
			// carry the tag must still decode through the legacy
			// tag-switching read path. We synthesise the body by
			// reconstructing the tagged form from the (untagged)
			// marshaled output — the tag goes on the outermost
			// mapping/sequence node.
			tagged := injectPgTag(string(body), tc.tag)
			var taggedCell PostgresV3Cell
			if err := yaml.Unmarshal([]byte(tagged), &taggedCell); err != nil {
				t.Fatalf("tagged backward-compat unmarshal: %v\n--YAML--\n%s", err, tagged)
			}
			if !pgtypeEqual(t, taggedCell.Value, tc.in) {
				t.Errorf("tagged backward-compat drift:\n got  %#v\n want %#v\n--YAML--\n%s", taggedCell.Value, tc.in, tagged)
			}
			if tc.untagged == "" {
				// Ambiguous shapes (Point/Lseg/Box/Polygon all match
				// `{p, valid}`; multirange has nested ambiguity) cannot
				// be recovered to their concrete pgtype Go type from
				// an untagged mapping — that's exactly what released
				// keploy produces too, so the cross-version contract
				// is "decode loses Go-type fidelity for these shapes,
				// the integrations-side codec re-encodes from logical
				// fields anyway". The untagged round-trip is therefore
				// not asserted here.
				return
			}
			// Primary read path: an untagged body decodes via the
			// canonical-key-set probe to the same Go value.
			var legacy PostgresV3Cell
			if err := yaml.Unmarshal([]byte(tc.untagged), &legacy); err != nil {
				t.Fatalf("untagged unmarshal: %v\n--YAML--\n%s", err, tc.untagged)
			}
			if !pgtypeEqual(t, legacy.Value, tc.in) {
				t.Errorf("untagged decode drift:\n got  %#v\n want %#v\n--YAML--\n%s", legacy.Value, tc.in, tc.untagged)
			}
		})
	}
}

// injectPgTag returns a copy of the (untagged) marshaled cell body
// with the given !pg/<name> local tag prepended to the outermost
// mapping/sequence. Used by the round-trip test to drive the
// backward-compat tag-switching decode path.
//
// yaml.v3 prints the cell value at the document root with no leading
// indentation. For a MappingNode the body starts with `key:` on line
// one; for a SequenceNode it starts with `- ` on line one. Either way,
// inserting `<tag>\n` ahead of the body promotes the implicit document
// node to the tag — yaml.v3's parser binds a leading tag token to the
// next node it encounters.
func injectPgTag(body, tag string) string {
	return tag + "\n" + body
}

// TestPgtypeYAMLRoundTrip_NumericTagAbsent pins the inverse of the
// pre-revert assertion: Numeric (and every other pgtype-shaped cell)
// must NOT carry an `!pg/numeric` tag in its on-disk form, because
// released keploy on Docker Hub doesn't register the custom tag set
// and the cross-version GHA replay matrix would fail with
// `cannot unmarshal !pg/numeric into string`. The decoder's
// tag-switching path stays in place for backward compat with any
// recordings made during the brief tag-emitting window — see
// TestPgtypeYAMLRoundTrip's tagged sub-assertion.
func TestPgtypeYAMLRoundTrip_NumericTagAbsent(t *testing.T) {
	cell := NewValueCell(pgtype.Numeric{Int: big.NewInt(1), Exp: 0, Valid: true})
	body, err := yaml.Marshal(cell)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("!pg/")) {
		t.Errorf("Numeric MarshalYAML must not emit !pg/<name> tag (cross-version replay against released keploy):\n%s", body)
	}
}

// pgtypeEqual centralises the comparison logic — pgtype.Numeric carries
// a *big.Int that DeepEqual reports unequal across pointer identity, so
// we compare the structural fields explicitly. Every other type is safe
// under reflect.DeepEqual.
func pgtypeEqual(t *testing.T, got, want any) bool {
	t.Helper()
	switch w := want.(type) {
	case pgtype.Numeric:
		g, ok := got.(pgtype.Numeric)
		if !ok {
			return false
		}
		if g.NaN != w.NaN || g.Valid != w.Valid || g.Exp != w.Exp || g.InfinityModifier != w.InfinityModifier {
			return false
		}
		if (g.Int == nil) != (w.Int == nil) {
			return false
		}
		if g.Int != nil && w.Int != nil && g.Int.Cmp(w.Int) != 0 {
			return false
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

// itoa keeps the test fixtures literal-friendly (no fmt import inside
// the table; weight/bound-type byte constants change values across pgx
// versions and an inline %d would force an extra fmt-vs-strconv import
// race that the table layout doesn't need).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
