package models

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	yaml "gopkg.in/yaml.v3"
)

// PostgresV3Cell holds one row or bind cell captured from a Postgres
// wire exchange. The on-disk form picks the cheapest human-readable
// representation that round-trips the raw wire bytes losslessly.
//
// YAML encoding rules:
//
//	SQL NULL          →  YAML null           (Cell.IsNull = true)
//	empty ""          →  plain YAML string    (Cell.Bytes = nil / [])
//	UTF-8 safe text   →  plain YAML string    (Cell.Bytes = the UTF-8 bytes)
//	anything else     →  !!binary + base64    (Cell.Bytes = raw wire bytes)
//
// JSON encoding rules (no native binary type in JSON):
//
//	SQL NULL          →  null
//	empty / text      →  "" / "string value"
//	anything else     →  {"$b64": "<base64>"}
//
// "UTF-8 safe" means valid UTF-8 AND no ASCII control bytes below
// 0x20 except \t, \n, \r AND no DEL (0x7F). Those constraints are
// enough to guarantee yaml.v3 accepts the scalar without escape
// surprises and that diffs/greps on the file work as a developer
// would expect.
//
// The distinction between SQL NULL and a zero-length text cell is
// preserved: `IsNull=true, Bytes=nil` is NULL; `IsNull=false,
// Bytes=nil` (or `[]byte{}`) is the empty string, which Postgres
// treats as distinct data.
//
// This type is NOT backward compatible with the pre-cell "base64
// string with ~~KEPLOY_PG_NULL~~ sentinel" encoding. Recordings
// captured before this change must be re-captured.
type PostgresV3Cell struct {
	// IsNull marks the cell as SQL NULL. When true, Bytes is ignored
	// on write and must be nil on read.
	IsNull bool
	// Bytes is the raw cell payload when IsNull is false. A nil or
	// empty slice represents the empty text value (distinct from NULL).
	Bytes []byte
}

// NewTextCell is a convenience constructor for a UTF-8 text cell.
func NewTextCell(s string) PostgresV3Cell {
	return PostgresV3Cell{Bytes: []byte(s)}
}

// NewBinaryCell is a convenience constructor for an arbitrary byte cell.
// Callers pass the raw wire bytes; on-disk encoding is chosen by the
// marshaller (plain YAML string if UTF-8-safe, !!binary otherwise).
func NewBinaryCell(b []byte) PostgresV3Cell {
	return PostgresV3Cell{Bytes: b}
}

// NullCell is the zero-value SQL-NULL cell.
func NullCell() PostgresV3Cell {
	return PostgresV3Cell{IsNull: true}
}

// IsYAMLSafeString reports whether b can be emitted as a plain or
// double-quoted YAML scalar without lossy escapes or parser surprises.
// Exported so the integrations recorder can make the same choice
// when deciding whether to stamp text-format cells as plain strings.
//
// Requirements:
//   - valid UTF-8
//   - no ASCII control bytes below 0x20 except \t (0x09), \n (0x0A),
//     \r (0x0D)
//   - no DEL (0x7F)
//
// Everything else — including any Unicode code point above 0x7F —
// round-trips cleanly through yaml.v3's string scalars.
func IsYAMLSafeString(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r < 0x20 {
			if r == '\t' || r == '\n' || r == '\r' {
				continue
			}
			return false
		}
		if r == 0x7F {
			return false
		}
	}
	return true
}

// MarshalYAML implements yaml.Marshaler.
func (c PostgresV3Cell) MarshalYAML() (interface{}, error) {
	if c.IsNull {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	if IsYAMLSafeString(c.Bytes) {
		// Returning a bare string lets yaml.v3 choose the smallest
		// quoting style (plain / single-quoted / double-quoted)
		// that preserves the content. We explicitly do NOT force a
		// tag here — yaml.v3 will stamp !!str automatically when it
		// emits the scalar.
		return string(c.Bytes), nil
	}
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!binary",
		Value: base64.StdEncoding.EncodeToString(c.Bytes),
	}, nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
//
// Accepts four YAML tags:
//
//	!!null               → NULL
//	!!str (or untagged)  → text, Bytes = UTF-8 of the scalar value
//	!!binary             → binary, Bytes = base64-decoded payload
//	(nothing else is accepted — unknown tags surface as an error so
//	malformed recordings don't silently round-trip as empty cells)
func (c *PostgresV3Cell) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return fmt.Errorf("PostgresV3Cell: nil YAML node")
	}
	// yaml.v3 leaves the tag field empty on implicitly-typed scalars
	// (plain strings that look like strings). Treat empty + !!str as
	// the same path.
	switch node.Tag {
	case "", "!!str":
		c.IsNull = false
		c.Bytes = []byte(node.Value)
		return nil
	case "!!null":
		c.IsNull = true
		c.Bytes = nil
		return nil
	case "!!binary":
		// yaml.v3 preserves !!binary payloads verbatim in node.Value,
		// including any whitespace/newlines from block-scalar line
		// wraps. TrimSpace + strip internal whitespace keeps this
		// robust to editor reflows.
		raw := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
				return -1
			}
			return r
		}, node.Value)
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return fmt.Errorf("PostgresV3Cell: invalid !!binary payload at line %d: %w", node.Line, err)
		}
		c.IsNull = false
		c.Bytes = decoded
		return nil
	default:
		return fmt.Errorf("PostgresV3Cell: unsupported YAML tag %q at line %d (expected !!null, !!str, or !!binary)", node.Tag, node.Line)
	}
}

// PostgresV3Cells is a named slice type so YAML decoding can dispatch
// through our custom UnmarshalYAML on every element, including null
// ones. yaml.v3's default sequence decoder short-circuits `null`
// scalars to the zero-value of the element type WITHOUT calling the
// element's UnmarshalYAML — which would silently collapse SQL NULL
// into an empty-bytes cell and lose the Postgres-semantic distinction
// between NULL and ''. Owning the sequence walk ourselves restores
// that distinction.
type PostgresV3Cells []PostgresV3Cell

// MarshalYAML implements yaml.Marshaler. Delegates to the element
// type's marshaler; the only reason this exists is to keep the
// encoder and decoder symmetric (ownership of both walks).
func (cs PostgresV3Cells) MarshalYAML() (interface{}, error) {
	if cs == nil {
		// A truly-absent BindValues / Rows field uses omitempty;
		// an empty (but non-nil) slice should still emit as `[]`
		// so an edit that drops all binds remains round-trippable.
		return []PostgresV3Cell{}, nil
	}
	return []PostgresV3Cell(cs), nil
}

// UnmarshalYAML implements yaml.Unmarshaler. Walks the sequence by
// hand so every element — including YAML nulls — routes through
// PostgresV3Cell.UnmarshalYAML and preserves IsNull.
func (cs *PostgresV3Cells) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return nil
	}
	// yaml.v3 uses an alias node shell when the sequence itself is
	// referenced from an anchor. Deref once.
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.SequenceNode:
		// fall through
	case yaml.ScalarNode:
		// An explicit null for the whole sequence means "no cells".
		if node.Tag == "!!null" || (node.Tag == "" && node.Value == "") {
			*cs = nil
			return nil
		}
		return fmt.Errorf("PostgresV3Cells: expected sequence, got scalar %q at line %d", node.Value, node.Line)
	default:
		return fmt.Errorf("PostgresV3Cells: expected sequence, got kind %v at line %d", node.Kind, node.Line)
	}
	out := make(PostgresV3Cells, len(node.Content))
	for i, child := range node.Content {
		if err := out[i].UnmarshalYAML(child); err != nil {
			return fmt.Errorf("PostgresV3Cells[%d]: %w", i, err)
		}
	}
	*cs = out
	return nil
}

// MarshalYAML at Rows-of-Cells level for PostgresV3Response/DataSpec/
// MigrationTable: [][]PostgresV3Cell needs each inner slice to be
// a PostgresV3Cells so the null-preserving decode kicks in. Callers
// declare the outer type as []PostgresV3Cells; helpers here convert
// to/from the natural [][]PostgresV3Cell for ergonomics.
//
// (no new helper methods needed — a []PostgresV3Cells slice of the
// named type yields the right YAML shape out of the box.)

// MarshalJSON implements json.Marshaler.
//
// JSON has no native binary representation, so binary cells encode
// as {"$b64": "<base64>"} — the $-prefix mimics MongoDB's $binary
// convention so mongosh and other Mongo-aware tools render it
// sensibly. Text cells emit as plain JSON strings.
func (c PostgresV3Cell) MarshalJSON() ([]byte, error) {
	if c.IsNull {
		return []byte("null"), nil
	}
	if IsYAMLSafeString(c.Bytes) {
		return json.Marshal(string(c.Bytes))
	}
	return json.Marshal(struct {
		B64 string `json:"$b64"`
	}{B64: base64.StdEncoding.EncodeToString(c.Bytes)})
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *PostgresV3Cell) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		c.IsNull = true
		c.Bytes = nil
		return nil
	}
	// Try plain string first — this is the hot path for text cells.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.IsNull = false
		c.Bytes = []byte(s)
		return nil
	}
	// Fall back to the binary envelope.
	var obj struct {
		B64 string `json:"$b64"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("PostgresV3Cell: cannot unmarshal %s: %w", data, err)
	}
	if obj.B64 == "" {
		return fmt.Errorf("PostgresV3Cell: JSON object missing $b64 field: %s", data)
	}
	decoded, err := base64.StdEncoding.DecodeString(obj.B64)
	if err != nil {
		return fmt.Errorf("PostgresV3Cell: invalid $b64 payload: %w", err)
	}
	c.IsNull = false
	c.Bytes = decoded
	return nil
}
