// postgres_v3_cell.go — logical-value representation for PG row cells.
//
// PRINCIPLE: wire format is an encoding concern, not a capture
// concern. A cell stores the LOGICAL value a PG server would have
// returned — int64 246, time.Time 2026-04-24T14:25:37Z, string
// "priority-i23-333b" — NOT the format-coupled wire bytes. At emit
// time, the v3 replayer looks up the column's TypeOID and the live
// client's requested format, then encodes via the codec registry
// (see integrations/pkg/postgres/v3/codec).
//
// Record and replay decouple: a query captured in one format can
// satisfy a live request in the other because format is derived at
// emit time, not stored in the mock.
//
// Encoding (on-disk YAML/JSON): cells serialize as their native Go
// value. Integer cells are plain YAML integers; string cells are
// plain strings; timestamps are ISO 8601; byte slices use yaml.v3's
// native !!binary tag. The result is diffable, reviewable, and
// hand-editable.
//
// NULL is cell.Value == nil. An empty-string value ("") is distinct
// from NULL — both round-trip correctly.
package models

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// PostgresV3Cell is ONE column in ONE row of a recorded SELECT
// response (or ONE parameter in a Bind message). The Value field
// holds the canonical Go representation for the column's type per
// its TypeOID — see integrations/pkg/postgres/v3/codec for the
// per-OID contract.
//
// NULL is cell.Value == nil; prefer the direct `== nil` check over
// the IsNull helper in hot paths.
type PostgresV3Cell struct {
	// Value is the logical column value. Go type is dictated by the
	// column's TypeOID, matching what pgtype hands back when the
	// scan target is *any:
	//   int2            → int16
	//   int4            → int32
	//   int8            → int64
	//   float4/8        → float64
	//   bool            → bool
	//   text/varchar/…  → string
	//   timestamp[tz]   → time.Time
	//   date            → time.Time (time-of-day zero)
	//   bytea           → []byte
	//   uuid            → string (canonical 8-4-4-4-12 form)
	//   json/jsonb      → string (canonical UTF-8 JSON)
	//   unknown OIDs    → PostgresV3CellRaw (format-tagged bytes,
	//                     carried through the mock as-is; the codec
	//                     shim on the integration side duck-types on
	//                     RawBytesAndFormat and re-emits without
	//                     transcoding across text ↔ binary since no
	//                     codec is registered for the OID).
	Value any
}

// legacyNullSentinel is the printable string that pre-PostgresV3Cell
// recordings wrote into [][]string Rows to denote SQL NULL. New
// recordings never emit this string (NULL goes through native YAML
// null / Cell.IsNull); UnmarshalYAML translates it back to nil so
// legacy mocks keep distinguishing NULL from empty-string.
const legacyNullSentinel = "~~KEPLOY_PG_NULL~~"

// NullCell returns a cell representing SQL NULL.
func NullCell() PostgresV3Cell {
	return PostgresV3Cell{Value: nil}
}

// NewValueCell wraps an already-decoded logical Go value.
func NewValueCell(v any) PostgresV3Cell {
	return PostgresV3Cell{Value: v}
}

// IsNull reports whether the cell is SQL NULL. Readable over
// `cell.Value == nil` when context matters.
func (c PostgresV3Cell) IsNull() bool {
	return c.Value == nil
}

// MarshalYAML emits the cell's logical value directly. A row is a
// YAML sequence of cells and each cell is just its value on disk.
// NULL → YAML null; []byte → !!binary base64 (we build the node
// explicitly because yaml.v3's any-wrapped []byte goes through the
// generic slice encoder and produces a sequence-of-ints instead of
// the binary tag).
//
// String values get routed through yamlSafeStringNode so the emitter
// never picks a block-scalar style for content that would re-parse
// back as invalid YAML — yaml.v3 has a long-standing bug where
// strings starting with "\n\t" or containing embedded tabs emit as
// `|4-` block scalars whose content tab disrupts indent detection
// when the scalar lives inside a sequence (the shape PostgresV3Cells
// produces), so the same mock file the recorder just wrote back fails
// to load on replay. Routing strings through a double-quoted scalar
// sidesteps the bug and is stable across yaml.v3 versions.
func (c PostgresV3Cell) MarshalYAML() (any, error) {
	switch v := c.Value.(type) {
	case []byte:
		return &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!binary",
			Value: base64.StdEncoding.EncodeToString(v),
		}, nil
	case string:
		if stringNeedsDoubleQuoted(v) {
			return &yaml.Node{
				Kind:  yaml.ScalarNode,
				Style: yaml.DoubleQuotedStyle,
				Value: v,
			}, nil
		}
		return v, nil
	}
	return c.Value, nil
}

// PostgresV3SafeString wraps string for fields that may carry text
// containing embedded newlines, tabs, or other whitespace that yaml.v3
// v3.0.1's emitter mishandles when the value lives inside a sequence
// or nested mapping. The custom MarshalYAML routes problematic values
// through a DoubleQuotedStyle scalar (escapes \t / \n) so the document
// the recorder writes always re-parses on the replayer side. Use this
// type for any v3 schema field that holds free-form server- or user-
// generated content (SQL text, NoticeResponse messages, user-supplied
// table data going into mocks). Fields like enum-like Severity / Code
// don't strictly need it but using it consistently across the Notice
// shape keeps the type contract uniform.
type PostgresV3SafeString string

// MarshalYAML emits the string with DoubleQuotedStyle when the value
// would trip yaml.v3's plain/block-style emitter; otherwise emits as
// a plain scalar so common short values stay greppable.
func (s PostgresV3SafeString) MarshalYAML() (any, error) {
	v := string(s)
	if stringNeedsDoubleQuoted(v) {
		return &yaml.Node{
			Kind:  yaml.ScalarNode,
			Style: yaml.DoubleQuotedStyle,
			Value: v,
		}, nil
	}
	return v, nil
}

// UnmarshalYAML accepts both plain and quoted scalars — the on-disk
// shape is just a string, only the WRITE side cares about styling.
func (s *PostgresV3SafeString) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*s = ""
		return nil
	}
	*s = PostgresV3SafeString(node.Value)
	return nil
}

// stringNeedsDoubleQuoted reports whether yaml.v3's plain/block style
// heuristic would mis-emit a round-trip-unsafe representation for s.
// Reproduced in isolation against yaml.v3 v3.0.1, the landmines are:
//
//   - any string containing an embedded newline picks a literal block
//     scalar (|…) whose continuation-line indentation indicator
//     yaml.v3 itself sometimes mis-computes inside a sequence (the
//     shape PostgresV3Cells produces), so the same document fails to
//     re-parse with "did not find expected key" or "found a tab
//     character where an indentation space is expected";
//
//   - strings starting with whitespace (space/tab/newline/CR) trip
//     the indent-indicator race even when single-line, because the
//     plain-style scanner chops the leading whitespace and the parser
//     then can't recover the original;
//
//   - strings with a trailing space or tab lose the trailing byte
//     under plain style (yaml.v3 trims them).
//
// Forcing double-quoted style escapes \t / \n / leading-whitespace as
// C-style sequences and makes the emitted YAML round-trip cleanly,
// regardless of whether the scalar lives at the top level or under a
// sequence. The trade-off is readability — quoted strings are slightly
// noisier than plain — but those strings already had control bytes,
// so the loss is small in practice.
func stringNeedsDoubleQuoted(s string) bool {
	if s == "" {
		return false
	}
	switch s[0] {
	case ' ', '\t', '\n', '\r':
		return true
	}
	switch s[len(s)-1] {
	case ' ', '\t':
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\t', '\n', '\r':
			return true
		}
	}
	return false
}

// UnmarshalYAML decodes a YAML node into Value. Tag-driven type
// dispatch ensures ints come out as int64 (not Go's platform-
// dependent int), strings as string, etc., so downstream codec
// dispatch is uniform.
//
//	!!null       → nil
//	!!bool       → bool
//	!!int        → int64
//	!!float      → float64
//	!!str        → string
//	!!binary     → []byte
//	!!timestamp  → time.Time
//	other        → interface{} default (maps/seqs reserved for future array support)
func (c *PostgresV3Cell) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		c.Value = nil
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		switch node.Tag {
		case "!!null":
			c.Value = nil
			return nil
		case "!!bool":
			var b bool
			if err := node.Decode(&b); err != nil {
				return err
			}
			c.Value = b
			return nil
		case "!!int":
			var n int64
			if err := node.Decode(&n); err != nil {
				return err
			}
			c.Value = n
			return nil
		case "!!float":
			var f float64
			if err := node.Decode(&f); err != nil {
				return err
			}
			c.Value = f
			return nil
		case "!!str":
			// Legacy pre-PostgresV3Cell recordings stored SQL NULL as
			// the printable sentinel "~~KEPLOY_PG_NULL~~" inside the
			// [][]string Rows shape (earlier revisions used
			// "\x00NULL\x00", but yaml.v3 rejects NUL bytes so the
			// printable form shipped). When the new binary reads those
			// old mocks for backward compatibility, translate the
			// sentinel back to a proper NULL — otherwise every legacy
			// NULL cell would collapse to a real string of the
			// sentinel's text, corrupting row-level comparisons.
			if node.Value == legacyNullSentinel {
				c.Value = nil
				return nil
			}
			c.Value = node.Value
			return nil
		case "!!binary":
			// yaml.v3 exposes the base64 payload as node.Value for
			// !!binary scalars; decoding into []byte directly errors
			// with "cannot unmarshal !!binary into []uint8" because
			// the resolver doesn't round-trip its own tag into a
			// concrete Go type. Decode the base64 ourselves.
			//
			// yaml.v3 folds long !!binary payloads onto continuation
			// lines, and the emitter keeps the embedded newlines in
			// node.Value when the line exceeds ~80 columns. StdEncoding
			// rejects those, so strip ASCII whitespace (space, tab,
			// CR, LF) before decoding. yaml.v3 itself never inserts
			// anything outside that set into the scalar, and real
			// base64 payloads never contain those characters either.
			raw := node.Value
			if strings.ContainsAny(raw, " \t\r\n") {
				raw = stripBase64Whitespace(raw)
			}
			b, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				return fmt.Errorf("PostgresV3Cell: !!binary base64 decode: %w", err)
			}
			c.Value = b
			return nil
		case "!!timestamp":
			var t time.Time
			if err := node.Decode(&t); err != nil {
				return err
			}
			c.Value = t
			return nil
		}
		// Untagged scalar: the resolver picks the type based on content.
		// Empty untagged scalar in plain style (Style==0) is NULL per
		// YAML 1.2. yaml.v3 has no exported PlainStyle constant; style
		// value 0 is the "no explicit style" (plain) encoding.
		if node.Value == "" && node.Style == 0 {
			c.Value = nil
			return nil
		}
		// Legacy NULL sentinel (see !!str branch above) when the node
		// came through un-tagged — e.g. read from a bare `- ~~KEPLOY_PG_NULL~~`
		// entry in a legacy Rows fixture. Same translation applies.
		if node.Value == legacyNullSentinel {
			c.Value = nil
			return nil
		}
	}
	// Mapping nodes are the serialized form of PostgresV3CellRaw — the
	// recorder's MarshalYAML returns the struct directly, which yaml.v3
	// encodes as `{format: N, bytes: [...]}`. Without this reconstruction
	// the default path below decodes into `map[string]any`, which then
	// trips the gob default-stringify branch when the agent streams the
	// cell to the proxy: the live client sees a string whose content is
	// the fmt.Sprint of the map, the codec rejects it, emitDataRow emits
	// 0 bytes, and pgjdbc crashes in ByteConverter.int8 with
	// ArrayIndexOutOfBoundsException.
	if node.Kind == yaml.MappingNode && isPostgresV3CellRawNode(node) {
		var raw PostgresV3CellRaw
		if err := node.Decode(&raw); err != nil {
			return fmt.Errorf("PostgresV3Cell: decode raw-cell node: %w", err)
		}
		c.Value = raw
		return nil
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return fmt.Errorf("PostgresV3Cell: decode node (kind=%d, tag=%q): %w", node.Kind, node.Tag, err)
	}
	c.Value = v
	return nil
}

// isPostgresV3CellRawNode reports whether a YAML mapping node has
// exactly the `format` and `bytes` keys that `PostgresV3CellRaw`
// marshals to. A mapping with only these two keys is the unambiguous
// signature of a raw-OID fallback cell; any other shape (e.g. future
// composite types, ad-hoc maps from custom recorders) is left to the
// generic any-decode path.
func isPostgresV3CellRawNode(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content) != 4 {
		return false
	}
	seenFormat, seenBytes := false, false
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode {
			return false
		}
		switch key.Value {
		case "format":
			seenFormat = true
		case "bytes":
			seenBytes = true
		}
	}
	return seenFormat && seenBytes
}

// stripBase64Whitespace removes space / tab / CR / LF from s. Used to
// un-fold !!binary payloads the yaml.v3 emitter wraps across multiple
// lines for long scalars; StdEncoding.DecodeString can't tolerate the
// embedded whitespace but the character set we strip is disjoint from
// the base64 alphabet so no legitimate payload byte is lost.
func stripBase64Whitespace(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}

// Gob round-trip — the recorder sidecar streams mocks to the agent
// via encoding/gob, and gob cannot round-trip interface{} fields
// without either gob.Register for every concrete value type or a
// custom (GobEncoder, GobDecoder) pair on the enclosing struct. We
// take the second path here: the set of types that can appear in
// Value is dictated by the codec package (int64, float64, string,
// bool, []byte, time.Time, and a small raw-bytes fallback for
// unknown OIDs), and enumerating them with a discriminator byte
// keeps the gob wire format decoupled from any upstream library's
// Register calls. Same shape as MarshalYAML/UnmarshalYAML.
//
// Tag byte assignments (do NOT renumber — mocks on disk and in the
// sidecar stream depend on stability):
//
//	0  → NULL        (no payload)
//	1  → int64       (gob int64 — int8 columns)
//	2  → float64     (gob float64)
//	3  → string      (gob string)
//	4  → bool        (gob bool)
//	5  → []byte      (gob []byte)
//	6  → time.Time   (gob time.Time — uses time's own GobEncode)
//	7  → rawBytes    (format int16 + []byte payload; for unknown OIDs
//	                  that the codec stored verbatim as *RawValue.
//	                  We carry just the (format, bytes) tuple and
//	                  decode back to the codec's RawValue on read if
//	                  the caller needs the typed form.)
//	8  → int32       (gob int32 — pgtype hands int4 columns back as
//	                  Go int32 when the destination is *any)
//	9  → int16       (gob int16 — same for int2 columns)
const (
	cellTagNull    byte = 0
	cellTagInt64   byte = 1
	cellTagFloat64 byte = 2
	cellTagString  byte = 3
	cellTagBool    byte = 4
	cellTagBytes   byte = 5
	cellTagTime    byte = 6
	cellTagRaw     byte = 7
	// Tags 8 and 9 cover the narrower integer types pgtype hands
	// back for int4 / int2 columns when the destination is *any.
	// Allocated after the original closed set, so older mocks (gob
	// streams that only carry tags 0-7) still decode unchanged.
	cellTagInt32 byte = 8
	cellTagInt16 byte = 9
)

// PostgresV3CellRaw is the wire form for a raw-OID fallback cell.
// Mirrors codec.RawValue but lives in pkg/models so the gob path
// doesn't depend on the codec package (which lives in integrations,
// a separate module). Consumers that need a codec.RawValue convert
// at the call site via RawBytesAndFormat() (the codec's encode path
// duck-types on this method to avoid an import cycle back into
// models from codec).
type PostgresV3CellRaw struct {
	Format int16
	Bytes  []byte
}

// RawBytesAndFormat exposes the (format, bytes) tuple without
// requiring codec to import models. Returned bytes alias the struct
// — callers must not retain a reference past the cell's lifetime.
func (r PostgresV3CellRaw) RawBytesAndFormat() (int16, []byte) {
	return r.Format, r.Bytes
}

// GobEncode writes the cell to a gob stream using the tag-byte
// format documented above. Equivalent of MarshalYAML for the gob
// transport path (sidecar → agent).
func (c PostgresV3Cell) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	switch v := c.Value.(type) {
	case nil:
		if err := buf.WriteByte(cellTagNull); err != nil {
			return nil, err
		}
	case int64:
		buf.WriteByte(cellTagInt64)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case int32:
		// pgtype scans int4 into a Go int32 when the destination is
		// `*any`, so this is the on-wire shape for every smallint/
		// integer column the recorder captures. We preserve the
		// concrete type rather than widening to int64 because the
		// codec's encode path matches against the source type when
		// re-emitting and an int64 ↔ int4 mismatch reads as a
		// driver-level type tag drift on replay.
		buf.WriteByte(cellTagInt32)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case int16:
		// Same rationale as int32 — pgtype maps int2 to Go int16.
		buf.WriteByte(cellTagInt16)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case float64:
		buf.WriteByte(cellTagFloat64)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case string:
		buf.WriteByte(cellTagString)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case bool:
		buf.WriteByte(cellTagBool)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case []byte:
		buf.WriteByte(cellTagBytes)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case time.Time:
		buf.WriteByte(cellTagTime)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case PostgresV3CellRaw:
		buf.WriteByte(cellTagRaw)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case *PostgresV3CellRaw:
		buf.WriteByte(cellTagRaw)
		if err := enc.Encode(*v); err != nil {
			return nil, err
		}
	default:
		// Fail loud rather than silently downgrade. An earlier revision
		// of this branch routed unknown types through fmt.Sprint and
		// stored the result as a string cell, but that is silently
		// lossy: pgtype.Numeric, pgtype.Interval, array decodes, etc.
		// all stringify to forms the codec cannot re-encode under the
		// original OID, so the cell would emit zero bytes on replay
		// and pgjdbc would crash in ByteConverter with an
		// ArrayIndexOutOfBoundsException — the kind of failure that
		// surfaces deep in the test set with no obvious link back to
		// the recorder. Surfacing the type at record time gives an
		// actionable signal: the missing case is named, the OID can
		// be added to the codec catalogue, and a tag byte can be
		// allocated in this switch.
		return nil, fmt.Errorf("PostgresV3Cell.GobEncode: unsupported Value type %T — add a tag byte and an explicit case to GobEncode/GobDecode (and a codec entry in integrations/pkg/postgres/v3/codec) for new pgtype return types", v)
	}
	return buf.Bytes(), nil
}

// GobDecode restores the cell from the gob stream produced by
// GobEncode. Symmetric dispatch on the tag byte.
func (c *PostgresV3Cell) GobDecode(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("PostgresV3Cell.GobDecode: empty buffer")
	}
	tag := data[0]
	dec := gob.NewDecoder(bytes.NewReader(data[1:]))
	switch tag {
	case cellTagNull:
		c.Value = nil
	case cellTagInt64:
		var v int64
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode int64: %w", err)
		}
		c.Value = v
	case cellTagFloat64:
		var v float64
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode float64: %w", err)
		}
		c.Value = v
	case cellTagString:
		var v string
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode string: %w", err)
		}
		c.Value = v
	case cellTagBool:
		var v bool
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode bool: %w", err)
		}
		c.Value = v
	case cellTagBytes:
		var v []byte
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode bytes: %w", err)
		}
		c.Value = v
	case cellTagTime:
		var v time.Time
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode time: %w", err)
		}
		c.Value = v
	case cellTagRaw:
		var v PostgresV3CellRaw
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode raw: %w", err)
		}
		c.Value = v
	case cellTagInt32:
		var v int32
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode int32: %w", err)
		}
		c.Value = v
	case cellTagInt16:
		var v int16
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode int16: %w", err)
		}
		c.Value = v
	default:
		return fmt.Errorf("PostgresV3Cell.GobDecode: unknown tag %d", tag)
	}
	return nil
}

// PostgresV3Cells is a row (sequence of cells). Named type so
// sequence-element dispatch goes through our UnmarshalYAML — yaml.v3's
// default sequence-of-nil short-circuit otherwise bypasses
// UnmarshalYAML for elements whose YAML representation is null.
type PostgresV3Cells []PostgresV3Cell

// UnmarshalYAML walks the sequence node by hand so every element's
// UnmarshalYAML is invoked (including null-valued scalars that
// yaml.v3 would otherwise short-circuit).
func (cs *PostgresV3Cells) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.SequenceNode {
		return fmt.Errorf("PostgresV3Cells: expected sequence, got kind=%d", node.Kind)
	}
	out := make(PostgresV3Cells, len(node.Content))
	for i, child := range node.Content {
		if err := (&out[i]).UnmarshalYAML(child); err != nil {
			return fmt.Errorf("PostgresV3Cells[%d]: %w", i, err)
		}
	}
	*cs = out
	return nil
}
