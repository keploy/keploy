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
// Encoding (on-disk YAML): cells serialize as their native Go
// value. Integer cells are plain YAML integers; string cells are
// plain strings; timestamps are ISO 8601; byte slices use yaml.v3's
// native !!binary tag. The result is diffable, reviewable, and
// hand-editable.
//
// JSON is a secondary path used by syncMock / MockOutgoing to ferry
// recorded values through encoding/json. PostgresV3Cell deliberately
// does NOT implement MarshalJSON / UnmarshalJSON — encoding/json's
// reflection emits the struct as `{"Value": ...}` and consumers on
// the receive side decode with the same struct shape. The native-
// scalar guarantee above applies only to the on-disk YAML form.
//
// NULL is cell.Value == nil. An empty-string value ("") is distinct
// from NULL — both round-trip correctly.
package models

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
	//
	//   int2 / int4 / int8         → int16 / int32 / int64
	//   oid / xid / cid            → uint32 (pg_catalog metadata)
	//   xid8                       → uint64 (PG ≥ 13)
	//   float4 / float8            → float32 / float64
	//   numeric / decimal          → pgtype.Numeric (arbitrary
	//                                precision; struct fields go
	//                                through gob-friendly paths)
	//   bool                       → bool
	//   text / varchar / name /
	//     bpchar / char             → string
	//   timestamp / timestamptz    → time.Time
	//   date / time / timetz       → time.Time (time-of-day zero on
	//                                date; pgtype.Time fallback on
	//                                some pgx versions)
	//   bytea / xml                → []byte
	//   uuid                       → canonical 8-4-4-4-12 string
	//   text[] / int4[] / composite-row / multi-dim arrays
	//                              → []interface{} of per-element
	//                                logical type
	//   json / jsonb               → map[string]interface{} for
	//                                top-level objects, []interface{}
	//                                for top-level arrays. Nested
	//                                values use the same recursive
	//                                cell encoding.
	//   yaml-reloaded nested ints  → int (yaml.v3 produces native
	//                                Go int when destination is `any`,
	//                                which is the recursive decode
	//                                path inside slice / jsonb cells)
	//   unknown OIDs               → PostgresV3CellRaw (format-tagged
	//                                bytes, carried through verbatim;
	//                                codec on integrations side duck-
	//                                types on RawBytesAndFormat to
	//                                re-emit without transcoding)
	//
	// Width caveat: yaml.v3's resolver decodes every !!int back to
	// int64, so int16 / int32 cells round-trip through the gob path
	// (sidecar → agent stream) with their source Go width but widen
	// to int64 through the YAML path (mocks.yaml on disk). That's
	// intentional — the codec on the integrations side encodes onto
	// the wire using the column OID, not the Go width, so the bytes
	// emitted are correct in both cases. Downstream code that type-
	// switches on Cell.Value should accept int16/int32/int64 for "an
	// integer column" or use a helper that widens.
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
//
// A typed-nil pointer (e.g. (*PostgresV3CellRaw)(nil) handed in by a
// nil-coalesced raw extractor) is collapsed to plain nil so the
// resulting cell behaves consistently across IsNull(), MarshalYAML,
// and GobEncode — without this, the cell would test as not-NULL via
// IsNull but serialize as NULL through gob, which silently breaks
// NULL-vs-empty diffs at replay time.
func NewValueCell(v any) PostgresV3Cell {
	if isTypedNilPointer(v) {
		v = nil
	}
	return PostgresV3Cell{Value: v}
}

// IsNull reports whether the cell is SQL NULL. Readable over
// `cell.Value == nil` when context matters. Also returns true for a
// typed-nil pointer (e.g. (*PostgresV3CellRaw)(nil)) so the predicate
// matches the on-disk encoding GobEncode picks for the same shape.
func (c PostgresV3Cell) IsNull() bool {
	if c.Value == nil {
		return true
	}
	return isTypedNilPointer(c.Value)
}

// isTypedNilPointer reports whether v is a typed-nil pointer, i.e.
// v != nil at the interface level but the underlying pointer is nil.
// We restrict the check to the one pointer type that GobEncode
// special-cases (*PostgresV3CellRaw). Any other typed-nil arriving
// here is a programmer error and is left alone so it surfaces in
// the GobEncode default branch with a clear "unsupported Value
// type" message.
func isTypedNilPointer(v any) bool {
	if raw, ok := v.(*PostgresV3CellRaw); ok && raw == nil {
		return true
	}
	return false
}

// MarshalYAML emits the cell's logical value directly. A row is a
// YAML sequence of cells and each cell is just its value on disk.
// NULL → YAML null; []byte → !!binary base64 (we build the node
// explicitly because yaml.v3's any-wrapped []byte goes through the
// generic slice encoder and produces a sequence-of-ints instead of
// the binary tag).
//
// String values are screened through StringNeedsDoubleQuoted; when
// that predicate matches, MarshalYAML returns an explicit yaml.Node
// with yaml.DoubleQuotedStyle instead of the raw string. The escape
// keeps the emitter out of the literal-block-scalar branch — yaml.v3
// has a long-standing bug where strings starting with "\n\t" or
// containing embedded tabs emit as `|4-` block scalars whose content
// tab disrupts indent detection when the scalar lives inside a
// sequence (the shape PostgresV3Cells produces), so the same mock
// file the recorder just wrote back fails to load on replay. The
// double-quoted form sidesteps that path entirely and is stable
// across yaml.v3 versions.
func (c PostgresV3Cell) MarshalYAML() (any, error) {
	switch v := c.Value.(type) {
	case []byte:
		return &yaml.Node{
			Kind:  yaml.ScalarNode,
			Tag:   "!!binary",
			Value: base64.StdEncoding.EncodeToString(v),
		}, nil
	case string:
		if StringNeedsDoubleQuoted(v) {
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
	if StringNeedsDoubleQuoted(v) {
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

// StringNeedsDoubleQuoted reports whether yaml.v3's plain/block style
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
func StringNeedsDoubleQuoted(s string) bool {
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
//	                  represented in this repo as PostgresV3CellRaw.
//	                  We carry just the (format, bytes) tuple and
//	                  GobDecode restores a PostgresV3CellRaw value on
//	                  read if the caller needs the typed form. The
//	                  integrations-side codec consumes that struct via
//	                  RawBytesAndFormat() — there is no conversion
//	                  back to a separate codec RawValue type in this
//	                  package.)
//	8  → int32       (gob int32 — pgtype hands int4 columns back as
//	                  Go int32 when the destination is *any)
//	9  → int16       (gob int16 — same for int2 columns)
//	10 → uint32      (gob uint32 — pgtype's mapping for `oid`,
//	                  `xid`, `cid`. Required for pg_catalog metadata
//	                  queries every JPA / ORM driver runs at boot.)
//	11 → uint16      (gob uint16 — rare but kept for catalogue
//	                  closure)
//	12 → uint64      (gob uint64 — `xid8` on PG ≥ 13)
//	13 → []interface{}  (length-prefixed sequence of nested cells —
//	                  PG ARRAY columns: text[], int4[], composite-row,
//	                  multi-dim arrays. Each element is encoded
//	                  recursively through this very GobEncode so the
//	                  type closure stays stable across nesting depth.)
//	14 → map[string]interface{}  (length-prefixed sorted (key, nested-
//	                  cell) pairs — PG `json` / `jsonb` columns. Each
//	                  value re-enters GobEncode recursively so nested
//	                  objects / arrays land in the map (this) / slice
//	                  (tag 13) cases. Keys sorted on encode for
//	                  deterministic gob bytes.)
//	15 → int         (gob int64 on the wire, decoded back to int —
//	                  yaml.v3 produces Go's untyped int when reloading
//	                  nested integers inside slice / jsonb cells.)
//	16 → float32     (gob float32 — pgtype hands float4 columns back
//	                  as Go float32; distinct from tag 2 float64.)
//	17 → pgtype.Numeric  (constituent fields: *big.Int via its own
//	                  GobEncode, Exp int32, InfinityModifier int8, NaN
//	                  bool, Valid bool. Required for PG `numeric` /
//	                  `decimal` columns — listmonk dashboard regression
//	                  surfaced this on /api/settings.)
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
	// Tags 10-12 cover the unsigned-integer types pgtype hands back
	// for PG's `oid`, `xid`, `cid`, and `xid8` types. `oid` (uint32)
	// is the most common — every pg_catalog metadata query that
	// Hibernate / SQLAlchemy / pgjdbc issue at startup returns oid
	// columns, and without these tags those mocks fail to gob-encode
	// in the recorder sidecar, get dropped on the syncMock channel,
	// and the entire app boot wedges on a "no recorded invocation
	// matched" against `SELECT … FROM pg_class …`.
	cellTagUint32 byte = 10
	cellTagUint16 byte = 11
	cellTagUint64 byte = 12
	// Tag 13 covers PG ARRAY columns: pgtype scans `text[]`, `int4[]`,
	// composite-row, etc. into a Go `[]interface{}` whose elements are
	// the per-cell logical Go type (string for text[], int32 for int4[],
	// nested []interface{} for multi-dim arrays). Encoding strategy:
	// length-prefixed sequence of nested PostgresV3Cells, each going
	// through this cell's own GobEncode/GobDecode recursively. That
	// keeps the type closure stable (no new tag per element type) and
	// gracefully nests for multi-dimensional arrays.
	cellTagSlice byte = 13
	// Tag 14 covers PG JSON / JSONB columns: pgtype scans `jsonb` /
	// `json` into a Go `map[string]interface{}` whose values are
	// per-key logical types (string for strings, float64 for numbers,
	// bool for booleans, nested map[string]interface{} for objects,
	// []interface{} for arrays). Encoding strategy: length-prefixed
	// sequence of (key, nested-cell) pairs — each value goes through
	// PostgresV3Cell.GobEncode recursively, so nested objects + arrays
	// re-enter the slice case (tag 13) and the value-type closure stays
	// stable. Required for listmonk's `settings` table (its bootstrap
	// `SELECT … FROM settings` was the regression case): pre-fix the
	// recorder dropped every JSONB-bearing mock and replay couldn't
	// satisfy the install path.
	cellTagJSONB byte = 14
	// Tag 15 covers Go's untyped `int`. Reachable in two cases the
	// principal-engineer audit pinned: (1) yaml.v3 decodes nested
	// `!!int` values inside slice/jsonb cells into Go's native `int`
	// when the destination is `any` (the recursive node.Decode(&v)
	// path that handles tag-13 slice / tag-14 jsonb shapes), so a
	// fresh-from-disk mock can carry int values inside its array /
	// object cells; (2) some app-level helpers wrap counts in plain
	// `int` before stuffing them into bind parameters. Width is
	// platform-dependent in Go but PG ints are bounded; we narrow to
	// int64 on the wire and decode back into int so the round-trip
	// is shape-stable.
	cellTagInt byte = 15
	// Tag 16 covers float4 columns: pgtype hands those back as Go
	// `float32` when destination is *any. Distinct from tag 2 which
	// is float64 — keeping the source width through gob preserves
	// what pgtype.Encode wants on emit (binary float4 is 4 bytes,
	// not 8).
	cellTagFloat32 byte = 16
	// Tag 17 covers `pgtype.Numeric` — PG's arbitrary-precision
	// `numeric` / `decimal` type. The struct's fields (Int *big.Int,
	// Exp int32, NaN bool, InfinityModifier int8, Valid bool) are
	// gob-friendly natively (big.Int has its own GobEncoder). Without
	// this tag, listmonk's `GET /api/settings` endpoint dropped its
	// recorded mock on every dashboard query that surfaces a numeric
	// (counts, rate-limit thresholds, configurable monetary values).
	cellTagNumeric byte = 17
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
	case uint32:
		// PostgreSQL's `oid`, `xid`, and `cid` types all surface as
		// Go uint32 from pgtype.Map.Scan — and they show up
		// constantly in the pg_catalog metadata queries every JPA /
		// ORM driver runs at boot (Hibernate's
		// `SELECT n.nspname,c.relname,a.attname,a.atttypid,...
		// FROM pg_class c JOIN pg_attribute a ON …` is the load-
		// bearing one). Drop a tag here so those mocks gob-encode
		// successfully and the recorder doesn't lose the entire
		// boot-time mock cohort.
		buf.WriteByte(cellTagUint32)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case uint16:
		// pgtype emits uint16 for some attlen / typmod-aligned
		// columns; rare but keeping the catalogue closed is
		// cheap.
		buf.WriteByte(cellTagUint16)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case uint64:
		// xid8 (PG 13+ 64-bit transaction ids) — pgtype maps it to
		// Go uint64. Required for any txid_current() or advisory-
		// lock workload running on a recent PG.
		buf.WriteByte(cellTagUint64)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case int:
		// Reachable when a slice / jsonb cell is reloaded from
		// mocks.yaml: yaml.v3 decodes nested `!!int` into Go's
		// platform-dependent `int` whenever the destination is
		// `any`, which is the recursive node.Decode(&v) path
		// inside this cell's slice (tag 13) and jsonb (tag 14)
		// branches. Without this case, GobEncode would fail-loud
		// on the rebuilt cell and the storemocks step would abort
		// the entire replay. Width is platform-dependent in Go but
		// the wire form is fixed at int64 — we don't lose range on
		// either 32- or 64-bit platforms because PG ints are
		// bounded.
		buf.WriteByte(cellTagInt)
		if err := enc.Encode(int64(v)); err != nil {
			return nil, err
		}
	case float32:
		// pgtype hands float4 columns back as Go float32. Distinct
		// from cellTagFloat64 (tag 2) so the source width survives
		// on emit — the binary float4 wire format is 4 bytes, not 8,
		// and pgtype.Encode for OIDFloat4 expects float32.
		buf.WriteByte(cellTagFloat32)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case pgtype.Numeric:
		// PG `numeric` / `decimal` — arbitrary-precision. Encode the
		// constituent fields directly: *big.Int via its own GobEncode
		// (handles nil → empty-bytes), Exp / InfinityModifier / NaN /
		// Valid via gob's primitive emit. We canonicalise nil Int to
		// big.NewInt(0) so the gob stream is deterministic for the
		// "valid Numeric with zero/null payload" case (rare but
		// reachable on degenerate inputs). Decode reconstructs the
		// struct verbatim so pgtype.Encode on emit gets the exact
		// shape it expects.
		buf.WriteByte(cellTagNumeric)
		// Encode an Int-presence flag separately from the bytes:
		// big.Int.GobEncode() of a fresh big.NewInt(0) produces a
		// non-empty payload that's indistinguishable from "absent
		// Int" if we encoded nil → zero-int. NaN / ±infinity numerics
		// legitimately carry Int=nil and the round-trip must preserve
		// that nil-ness so pgtype.Encode picks the right wire shape
		// on emit. Tag the presence explicitly.
		hasInt := v.Int != nil
		if err := enc.Encode(hasInt); err != nil {
			return nil, err
		}
		if hasInt {
			intBytes, err := v.Int.GobEncode()
			if err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode numeric Int: %w", err)
			}
			if err := enc.Encode(intBytes); err != nil {
				return nil, err
			}
		}
		if err := enc.Encode(v.Exp); err != nil {
			return nil, err
		}
		if err := enc.Encode(int8(v.InfinityModifier)); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.NaN); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case map[string]interface{}:
		// PG `json` / `jsonb` columns: pgtype.Map.Scan resolves these
		// into a Go `map[string]interface{}` whose values are the per-
		// key logical Go type (string, float64, bool, nested
		// map[string]interface{}, []interface{} for arrays). Encode
		// strategy: length-prefix + sorted (key, nested-cell) pairs
		// — keys sorted so the gob byte stream is deterministic for a
		// given map regardless of Go's randomized map-iteration order.
		// Each value re-enters GobEncode recursively so nested objects
		// + arrays land in the slice (tag 13) / map (this) cases.
		// Without this case listmonk's bootstrap `SELECT … FROM
		// settings` (settings.value is jsonb) dropped its mock and
		// the install-time replay served 0-arg ParseComplete to a
		// 7-arg INSERT INTO lists, crashing the app.
		buf.WriteByte(cellTagJSONB)
		if err := enc.Encode(int32(len(v))); err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := enc.Encode(k); err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode jsonb key %q: %w", k, err)
			}
			elemBytes, err := PostgresV3Cell{Value: v[k]}.GobEncode()
			if err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode jsonb value at %q (%T): %w", k, v[k], err)
			}
			if err := enc.Encode(elemBytes); err != nil {
				return nil, err
			}
		}
	case []interface{}:
		// PG ARRAY columns: pgtype.Map.Scan resolves text[] / int4[] /
		// composite-row / multi-dim arrays into a `[]interface{}` whose
		// elements are the per-cell logical Go type. Encode as a length-
		// prefixed sequence of nested cells so element types stay
		// per-position and multi-dim nesting works recursively. Without
		// this case, listmonk's install-time INSERT INTO lists (...,
		// tags TEXT[], ...) recorded a no-PD mock (because the gob
		// failure dropped the entire mock including its
		// ParameterDescription) and replay served a 0-arg ParseComplete
		// to a 7-arg INSERT — pgx aborted with "expected 0 arguments,
		// got 7" and listmonk crashed before any user traffic.
		buf.WriteByte(cellTagSlice)
		if err := enc.Encode(int32(len(v))); err != nil {
			return nil, err
		}
		for i, elem := range v {
			elemBytes, err := PostgresV3Cell{Value: elem}.GobEncode()
			if err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode slice elem %d (%T): %w", i, elem, err)
			}
			if err := enc.Encode(elemBytes); err != nil {
				return nil, err
			}
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
		// A typed-nil pointer survives the `case nil:` arm above
		// because (*PostgresV3CellRaw)(nil) has a non-nil type
		// descriptor; without this guard `enc.Encode(*v)` would
		// panic on the nil dereference. Treat it the same as a
		// SQL NULL — the call site clearly intended an absent
		// value, and writing tagRaw with a zero RawValue would
		// emit garbled bytes on replay.
		if v == nil {
			buf.WriteByte(cellTagNull)
			break
		}
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
	case cellTagUint32:
		var v uint32
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode uint32: %w", err)
		}
		c.Value = v
	case cellTagUint16:
		var v uint16
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode uint16: %w", err)
		}
		c.Value = v
	case cellTagUint64:
		var v uint64
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode uint64: %w", err)
		}
		c.Value = v
	case cellTagInt:
		var v int64
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode int: %w", err)
		}
		// Decode back into Go's untyped int — the round-trip target
		// is the same shape yaml.v3 produced on load. int64 → int
		// narrows on 32-bit platforms but PG int values fit; bigger
		// numerics live in pgtype.Numeric (tag 17).
		c.Value = int(v)
	case cellTagFloat32:
		var v float32
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode float32: %w", err)
		}
		c.Value = v
	case cellTagNumeric:
		var (
			hasInt    bool
			expVal    int32
			infMod    int8
			nanFlag   bool
			validFlag bool
		)
		if err := dec.Decode(&hasInt); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode numeric hasInt flag: %w", err)
		}
		recovered := pgtype.Numeric{}
		if hasInt {
			var intBytes []byte
			if err := dec.Decode(&intBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode numeric Int bytes: %w", err)
			}
			recovered.Int = new(big.Int)
			if err := recovered.Int.GobDecode(intBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode numeric Int gob: %w", err)
			}
		}
		if err := dec.Decode(&expVal); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode numeric Exp: %w", err)
		}
		if err := dec.Decode(&infMod); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode numeric InfinityModifier: %w", err)
		}
		if err := dec.Decode(&nanFlag); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode numeric NaN: %w", err)
		}
		if err := dec.Decode(&validFlag); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode numeric Valid: %w", err)
		}
		recovered.Exp = expVal
		recovered.InfinityModifier = pgtype.InfinityModifier(infMod)
		recovered.NaN = nanFlag
		recovered.Valid = validFlag
		c.Value = recovered
	case cellTagJSONB:
		// Symmetric to the GobEncode map case: read length, then
		// (key, nested-cell-bytes) pairs, decode each value through
		// PostgresV3Cell.GobDecode recursively. Map iteration on
		// the encode side is sorted so the gob stream is
		// deterministic; rebuild does not need to preserve order
		// (Go's map type is unordered).
		var n int32
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode jsonb length: %w", err)
		}
		out := make(map[string]interface{}, n)
		for i := int32(0); i < n; i++ {
			var k string
			if err := dec.Decode(&k); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode jsonb key %d: %w", i, err)
			}
			var elemBytes []byte
			if err := dec.Decode(&elemBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode jsonb value bytes for %q: %w", k, err)
			}
			var elem PostgresV3Cell
			if err := elem.GobDecode(elemBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode jsonb value cell for %q: %w", k, err)
			}
			out[k] = elem.Value
		}
		c.Value = out
	case cellTagSlice:
		// Symmetric to the GobEncode []interface{} branch: read the
		// length, then each element as a self-contained nested cell
		// gob blob, and decode it back to its underlying Value. The
		// rebuild produces a fresh []interface{} the caller can
		// type-assert per its own per-OID expectations (text[]
		// elements come back as string, int4[] as int32, multi-dim
		// arrays as nested []interface{} via the recursive case).
		var n int32
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode slice length: %w", err)
		}
		out := make([]interface{}, n)
		for i := int32(0); i < n; i++ {
			var elemBytes []byte
			if err := dec.Decode(&elemBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode slice elem %d bytes: %w", i, err)
			}
			var elem PostgresV3Cell
			if err := elem.GobDecode(elemBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode slice elem %d cell: %w", i, err)
			}
			out[i] = elem.Value
		}
		c.Value = out
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
