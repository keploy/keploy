package models

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

// TestPostgresV3Cell_YAMLRoundTrip exercises every branch of the
// YAML encoding rules: NULL, empty string, ASCII text, UTF-8 text,
// text containing yaml-permitted control chars, binary (NUL byte),
// binary (non-UTF-8), and "confusable" text that looks like a tagged
// scalar but should round-trip as plain string.
//
// All cells are tested INSIDE a PostgresV3Cells sequence. Cells
// never appear as a top-level YAML document in real recordings —
// they live inside BindValues / Rows arrays — and yaml.v3's
// top-level null short-circuit (which bypasses UnmarshalYAML on
// value receivers) means a standalone null document would decode as
// the zero value no matter what we do on the struct. The named-
// slice type's UnmarshalYAML owns the walk so sequence-element nulls
// do reach PostgresV3Cell.UnmarshalYAML intact — that's the real
// path we care about.
func TestPostgresV3Cell_YAMLRoundTrip(t *testing.T) {
	cases := []struct {
		name          string
		cell          PostgresV3Cell
		wantCellYAML  string // how the cell renders as a sequence element
	}{
		{"SQL NULL", NullCell(), "- null\n"},
		{"empty text", NewTextCell(""), "- \"\"\n"},
		{"ASCII text", NewTextCell("public"), "- public\n"},
		{"UTF-8 text", NewTextCell("flyway_schema_history"), "- flyway_schema_history\n"},
		{"CJK UTF-8 text", NewTextCell("你好世界"), "- 你好世界\n"},
		{"text with newline and tab — YAML-permitted controls stay text",
			NewTextCell("line1\nline2\ttabbed"), "- |-\n  line1\n  line2\ttabbed\n"},
		{"text that LOOKS numeric — still UTF-8 text", NewTextCell("202"), "- \"202\"\n"},
		{"text that looks like int — yaml.v3 quotes it", NewTextCell("0"), "- \"0\"\n"},
		{"binary with NUL byte", NewBinaryCell([]byte{0x00, 0x01, 0x02, 0x03}), "- !!binary AAECAw==\n"},
		{"binary non-UTF-8 (0xFF)", NewBinaryCell([]byte{0xFF, 0xFE, 0xFD}), "- !!binary //79\n"},
		{"binary valid-UTF-8-but-DEL", NewBinaryCell([]byte{0x7F}), "- !!binary fw==\n"},
		{"int4 binary (-1)", NewBinaryCell([]byte{0xFF, 0xFF, 0xFF, 0xFF}), "- !!binary /////w==\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cells := PostgresV3Cells{tc.cell}
			gotYAML, err := yamlLib.Marshal(&cells)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(gotYAML) != tc.wantCellYAML {
				t.Fatalf("YAML marshal drift:\n  want: %q\n  got:  %q", tc.wantCellYAML, string(gotYAML))
			}

			// Round-trip through the named-slice decoder.
			var got PostgresV3Cells
			if err := yamlLib.Unmarshal(gotYAML, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("round-trip produced %d cells, want 1 (payload: %q)", len(got), gotYAML)
			}
			if got[0].IsNull != tc.cell.IsNull {
				t.Fatalf("IsNull mismatch: want %v got %v", tc.cell.IsNull, got[0].IsNull)
			}
			if !bytes.Equal(got[0].Bytes, tc.cell.Bytes) {
				t.Fatalf("Bytes mismatch:\n  want: %x\n  got:  %x", tc.cell.Bytes, got[0].Bytes)
			}

			// Re-marshal must be byte-identical (stability check).
			reYAML, err := yamlLib.Marshal(&got)
			if err != nil {
				t.Fatalf("re-Marshal: %v", err)
			}
			if !bytes.Equal(gotYAML, reYAML) {
				t.Fatalf("re-marshal drift:\n  first:  %q\n  second: %q", gotYAML, reYAML)
			}
		})
	}
}

// TestPostgresV3Cell_NullVsEmptyStringAreDistinct is the single most
// important invariant of the new type: SQL NULL and the empty text
// value must never collide after a sequence round-trip. Postgres
// treats them as semantically different (`IS NULL` vs `= ''`), and
// the legacy sentinel-string encoding has been known to conflate
// them in edge cases. Tested inside a PostgresV3Cells sequence —
// the only context cells ever appear in on disk.
func TestPostgresV3Cell_NullVsEmptyStringAreDistinct(t *testing.T) {
	cells := PostgresV3Cells{NullCell(), NewTextCell("")}
	out, err := yamlLib.Marshal(&cells)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Emitted YAML must visually distinguish the two entries so a
	// human diffing the file can see which is which.
	if !bytes.Contains(out, []byte("- null")) {
		t.Fatalf("NULL entry not emitted as `- null`:\n%s", out)
	}
	if !bytes.Contains(out, []byte("- \"\"")) {
		t.Fatalf("empty entry not emitted as `- \"\"`:\n%s", out)
	}

	var round PostgresV3Cells
	if err := yamlLib.Unmarshal(out, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(round) != 2 {
		t.Fatalf("round-trip produced %d cells, want 2", len(round))
	}
	if !round[0].IsNull {
		t.Fatalf("NULL round-trip lost IsNull: %+v", round[0])
	}
	if round[1].IsNull {
		t.Fatalf("empty text round-trip gained IsNull: %+v", round[1])
	}
	if len(round[1].Bytes) != 0 {
		t.Fatalf("empty text round-trip corrupted Bytes: %x", round[1].Bytes)
	}

	// Same invariant on the JSON side — cells marshalled individually
	// (JSON doesn't have yaml.v3's top-level-null short-circuit, so
	// single-cell round-trip works fine there).
	n := NullCell()
	e := NewTextCell("")
	nullJSON, _ := json.Marshal(&n)
	emptyJSON, _ := json.Marshal(&e)
	if bytes.Equal(nullJSON, emptyJSON) {
		t.Fatalf("NULL and empty-string serialise identically in JSON:\n  NULL:  %s\n  EMPTY: %s", nullJSON, emptyJSON)
	}
}

// TestPostgresV3Cell_LegacySentinelIsNotNull pins the no-magic
// guarantee: an old recording that stored "~~KEPLOY_PG_NULL~~" as
// a plain string under the previous sentinel scheme decodes as the
// literal string, NOT as SQL NULL. This is the "no backward compat"
// break the user asked for — the sentinel no longer has meaning, and
// the test documents that.
func TestPostgresV3Cell_LegacySentinelIsNotNull(t *testing.T) {
	const legacyYAML = "- \"~~KEPLOY_PG_NULL~~\"\n"
	var got PostgresV3Cells
	if err := yamlLib.Unmarshal([]byte(legacyYAML), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 cell, got %d", len(got))
	}
	if got[0].IsNull {
		t.Fatalf("legacy sentinel string must NOT decode as NULL — the schema no longer reads it specially")
	}
	if string(got[0].Bytes) != "~~KEPLOY_PG_NULL~~" {
		t.Fatalf("legacy sentinel: want literal string, got %q", got[0].Bytes)
	}
}

// TestPostgresV3Cell_UnsupportedTagIsError asserts that an unknown
// YAML tag (e.g. !!int, !!bool) is rejected. Silent accept here would
// let malformed recordings look fine while meaning the wrong thing.
func TestPostgresV3Cell_UnsupportedTagIsError(t *testing.T) {
	const badYAML = "- !!int 42\n"
	var got PostgresV3Cells
	err := yamlLib.Unmarshal([]byte(badYAML), &got)
	if err == nil {
		t.Fatalf("want error on !!int tag, got nil and cells=%+v", got)
	}
	if !strings.Contains(err.Error(), "unsupported YAML tag") {
		t.Fatalf("error message not informative: %v", err)
	}
}

// TestPostgresV3Cell_JSONRoundTrip covers the JSON encoding variants:
// null, text, binary (object form), empty string.
func TestPostgresV3Cell_JSONRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		cell     PostgresV3Cell
		wantJSON string
	}{
		{"SQL NULL", NullCell(), `null`},
		{"empty text", NewTextCell(""), `""`},
		{"ASCII text", NewTextCell("public"), `"public"`},
		{"UTF-8 text", NewTextCell("你好"), `"你好"`},
		{"binary NUL", NewBinaryCell([]byte{0x00, 0x01}), `{"$b64":"AAE="}`},
		{"binary 0xFF", NewBinaryCell([]byte{0xFF, 0xFE}), `{"$b64":"//4="}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotJSON, err := json.Marshal(&tc.cell)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(gotJSON) != tc.wantJSON {
				t.Fatalf("JSON marshal:\n  want: %s\n  got:  %s", tc.wantJSON, gotJSON)
			}

			var got PostgresV3Cell
			if err := json.Unmarshal(gotJSON, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.IsNull != tc.cell.IsNull {
				t.Fatalf("IsNull: want %v got %v", tc.cell.IsNull, got.IsNull)
			}
			if !bytes.Equal(got.Bytes, tc.cell.Bytes) {
				t.Fatalf("Bytes: want %x got %x", tc.cell.Bytes, got.Bytes)
			}
		})
	}
}

// TestPostgresV3Cell_BindValues_SliceIntegration asserts the
// replacement BindValues type slots into PostgresV3QuerySpec
// cleanly and round-trips through YAML unmarshal on a real-shape
// fragment that mixes text and binary binds.
func TestPostgresV3Cell_BindValues_SliceIntegration(t *testing.T) {
	const yaml = `
sqlAstHash: sha256:abc
sqlNormalized: "select * from t where id = $1 and tag = $2"
invocationId: "0:42"
bindValues:
  - "202"
  - public
  - !!binary /////w==
  - null
bindFormats: [0, 0, 1, 0]
`
	var spec PostgresV3QuerySpec
	if err := yamlLib.Unmarshal([]byte(yaml), &spec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(spec.BindValues) != 4 {
		t.Fatalf("BindValues len: want 4 got %d", len(spec.BindValues))
	}
	want := PostgresV3Cells{
		NewTextCell("202"),
		NewTextCell("public"),
		NewBinaryCell([]byte{0xFF, 0xFF, 0xFF, 0xFF}),
		NullCell(),
	}
	for i, w := range want {
		got := spec.BindValues[i]
		if got.IsNull != w.IsNull {
			t.Fatalf("BindValues[%d] IsNull: want %v got %v", i, w.IsNull, got.IsNull)
		}
		if !bytes.Equal(got.Bytes, w.Bytes) {
			t.Fatalf("BindValues[%d] Bytes: want %x got %x", i, w.Bytes, got.Bytes)
		}
	}
	if !reflect.DeepEqual(spec.BindFormats, []int{0, 0, 1, 0}) {
		t.Fatalf("BindFormats drift: %v", spec.BindFormats)
	}

	// Re-marshal and decode again — shape stable.
	reYAML, err := yamlLib.Marshal(&spec)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var round PostgresV3QuerySpec
	if err := yamlLib.Unmarshal(reYAML, &round); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !reflect.DeepEqual([]PostgresV3Cell(spec.BindValues), []PostgresV3Cell(round.BindValues)) {
		t.Fatalf("BindValues drift after second round-trip:\n  before: %+v\n  after:  %+v", spec.BindValues, round.BindValues)
	}
}

// TestPostgresV3Cell_Rows_Integration covers the same for
// PostgresV3Response.Rows — the larger of the two cell-typed fields
// by volume in a typical recording.
func TestPostgresV3Cell_Rows_Integration(t *testing.T) {
	const yaml = `
rowDescription:
  - name: id
    typeOid: 23
  - name: name
    typeOid: 1043
  - name: tag_bytes
    typeOid: 17
rows:
  - - "1"
    - acme-corp
    - !!binary AAECAw==
  - - "2"
    - null
    - !!binary //79
commandComplete: "SELECT 2"
`
	var resp PostgresV3Response
	if err := yamlLib.Unmarshal([]byte(yaml), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(resp.Rows) != 2 || len(resp.Rows[0]) != 3 {
		t.Fatalf("shape drift: %+v", resp.Rows)
	}
	if string(resp.Rows[0][1].Bytes) != "acme-corp" {
		t.Fatalf("row 0 col 1: want \"acme-corp\", got %q", resp.Rows[0][1].Bytes)
	}
	if !bytes.Equal(resp.Rows[0][2].Bytes, []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Fatalf("row 0 col 2 binary: got %x", resp.Rows[0][2].Bytes)
	}
	if !resp.Rows[1][1].IsNull {
		t.Fatalf("row 1 col 1: want NULL, got %+v", resp.Rows[1][1])
	}
}

// TestIsYAMLSafeString covers the helper used by the recorder to pick
// plain-string vs !!binary at capture time.
func TestIsYAMLSafeString(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", []byte{}, true},
		{"nil", nil, true},
		{"ASCII", []byte("hello world"), true},
		{"UTF-8 CJK", []byte("你好"), true},
		{"tab", []byte("col1\tcol2"), true},
		{"newline", []byte("line1\nline2"), true},
		{"CR", []byte("line1\rline2"), true},
		{"NUL byte", []byte{0x00}, false},
		{"BEL", []byte{0x07}, false},
		{"control 0x1F", []byte{0x1F}, false},
		{"DEL 0x7F", []byte{0x7F}, false},
		{"invalid UTF-8", []byte{0xFF, 0xFE}, false},
		{"int4 -1 binary", []byte{0xFF, 0xFF, 0xFF, 0xFF}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsYAMLSafeString(tc.in)
			if got != tc.want {
				t.Fatalf("IsYAMLSafeString(%x): want %v got %v", tc.in, tc.want, got)
			}
		})
	}
}
