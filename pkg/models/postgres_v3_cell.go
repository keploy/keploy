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
	"net"
	"net/netip"
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
	case pgtype.Numeric:
		return marshalPgNumericYAML(v)
	case pgtype.Interval:
		return marshalPgIntervalYAML(v), nil
	case pgtype.Time:
		return marshalPgTimeYAML(v), nil
	case pgtype.Bits:
		return marshalPgBitsYAML(v), nil
	case pgtype.Point:
		return marshalPgPointYAML(v), nil
	case pgtype.Line:
		return marshalPgLineYAML(v), nil
	case pgtype.Lseg:
		return marshalPgLsegYAML(v), nil
	case pgtype.Box:
		return marshalPgBoxYAML(v), nil
	case pgtype.Path:
		return marshalPgPathYAML(v), nil
	case pgtype.Polygon:
		return marshalPgPolygonYAML(v), nil
	case pgtype.Circle:
		return marshalPgCircleYAML(v), nil
	case pgtype.TID:
		return marshalPgTIDYAML(v), nil
	case pgtype.TSVector:
		return marshalPgTSVectorYAML(v), nil
	case pgtype.Hstore:
		return marshalPgHstoreYAML(v), nil
	case pgtype.Range[any]:
		return marshalPgRangeYAML(v)
	case pgtype.Multirange[pgtype.Range[any]]:
		return marshalPgMultirangeYAML(v)
	case netip.Prefix:
		// PG `inet` / `cidr`. Without explicit dispatch, yaml.v3's
		// reflective encoder emits this as a plain string scalar
		// (netip.Prefix implements encoding.TextMarshaler) — which
		// round-trips to a Go `string` on decode, losing the type
		// fidelity the integrations-side codec needs to dispatch
		// `inet`/`cidr` encode plans. Emit a single-key mapping
		// `{prefix: "<canonical>"}` so the decode side recovers the
		// concrete type via the same canonical-key-set probe used
		// for the 16 pgtype shapes.
		return marshalNetipPrefixYAML(v), nil
	case net.HardwareAddr:
		// PG `macaddr` / `macaddr8`. HardwareAddr is a *named*
		// `[]byte`, so the `case []byte:` arm above doesn't match
		// (Go type-switch matches the unnamed type only). Without
		// explicit dispatch yaml.v3 emits it via the generic slice
		// encoder as a sequence of ints, which round-trips to
		// `[]any` and breaks the codec's encode-plan dispatch for
		// `macaddr`. Emit a single-key mapping
		// `{macaddr: !!binary <base64>}` so the decode side
		// recovers the named type.
		return marshalHardwareAddrYAML(v), nil
	case [16]uint8:
		// PG `uuid` scanned into a `[16]byte`-shaped destination
		// (the underlying type of uuid.UUID, and what pgx's
		// UUIDArrayCodec hands back for each `uuid[]` element).
		// Without explicit dispatch yaml.v3 reflects on the fixed
		// array and emits it as a sequence of ints, which decodes
		// back as `[]any{int64...}` — same fidelity-loss class as
		// HardwareAddr, breaking the codec's encode-plan dispatch
		// for `uuid`. Emit a single-key mapping
		// `{uuid: !!binary <base64>}` so the decode side recovers
		// the fixed-array shape symmetrically with the gob path.
		return marshalUUIDBytesYAML(v), nil
	}
	return c.Value, nil
}

// YAML local tags for the pgtype-typed cell shapes. The current
// MarshalYAML emits *untagged* mappings (matching the released
// keploy's reflection-based emit, which is what cross-version replay
// in the GHA matrix encounters on Docker Hub); the primary read path
// is decodePgUntaggedMapping below, which probes the canonical key
// set. These tags exist for backward-compat decode of any recordings
// from the brief window when MarshalYAML did emit them — see
// decodePgTaggedNode.
const (
	pgYAMLTagNumeric    = "!pg/numeric"
	pgYAMLTagInterval   = "!pg/interval"
	pgYAMLTagTime       = "!pg/time"
	pgYAMLTagBits       = "!pg/bits"
	pgYAMLTagPoint      = "!pg/point"
	pgYAMLTagLine       = "!pg/line"
	pgYAMLTagLseg       = "!pg/lseg"
	pgYAMLTagBox        = "!pg/box"
	pgYAMLTagPath       = "!pg/path"
	pgYAMLTagPolygon    = "!pg/polygon"
	pgYAMLTagCircle     = "!pg/circle"
	pgYAMLTagTID        = "!pg/tid"
	pgYAMLTagTSVector   = "!pg/tsvector"
	pgYAMLTagHstore     = "!pg/hstore"
	pgYAMLTagRange      = "!pg/range"
	pgYAMLTagMultirange = "!pg/multirange"
	pgYAMLTagPrefix     = "!pg/prefix"
	pgYAMLTagMACAddr    = "!pg/macaddr"
)

// scalarBoolNode / scalarIntNode / scalarStrNode build the lightweight
// yaml.Node primitives used to assemble the pgtype mapping bodies. Pulled
// out so each per-type MarshalYAML helper stays a flat list of key/value
// pairs and the field encoding stays uniform with how the reflective
// encoder would emit a struct field of the same Go type.
func scalarBoolNode(b bool) *yaml.Node {
	v := "false"
	if b {
		v = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
}

func scalarIntNode(n int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", n)}
}

func scalarFloatNode(f float64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: fmt.Sprintf("%g", f)}
}

func scalarStrNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func scalarKeyNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}

func marshalVec2Node(v pgtype.Vec2) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("x"), scalarFloatNode(v.X),
			scalarKeyNode("y"), scalarFloatNode(v.Y),
		},
	}
}

func marshalVec2SeqNode(ps []pgtype.Vec2) *yaml.Node {
	out := &yaml.Node{Kind: yaml.SequenceNode}
	out.Content = make([]*yaml.Node, 0, len(ps))
	for _, p := range ps {
		out.Content = append(out.Content, marshalVec2Node(p))
	}
	return out
}

// marshalPgNumericYAML emits Numeric as `{int: "<digits>", exp: N,
// nan: bool, infinitymodifier: N, valid: bool}`. Int is rendered as a
// string scalar (matching what yaml.v3 does for *big.Int via its
// TextMarshaler), and an absent Int is emitted as `int: ""` so the
// nil-vs-zero distinction round-trips. NaN / ±infinity numerics
// legitimately carry Int=nil and the codec on the integrations side
// dispatches on that nil-ness when re-emitting.
func marshalPgNumericYAML(v pgtype.Numeric) (*yaml.Node, error) {
	intStr := ""
	if v.Int != nil {
		intStr = v.Int.String()
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("int"), {Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: intStr},
			scalarKeyNode("exp"), scalarIntNode(int64(v.Exp)),
			scalarKeyNode("nan"), scalarBoolNode(v.NaN),
			scalarKeyNode("infinitymodifier"), scalarIntNode(int64(v.InfinityModifier)),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}, nil
}

func marshalPgIntervalYAML(v pgtype.Interval) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("microseconds"), scalarIntNode(v.Microseconds),
			scalarKeyNode("days"), scalarIntNode(int64(v.Days)),
			scalarKeyNode("months"), scalarIntNode(int64(v.Months)),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgTimeYAML(v pgtype.Time) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("microseconds"), scalarIntNode(v.Microseconds),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgBitsYAML(v pgtype.Bits) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("bytes"), {Kind: yaml.ScalarNode, Tag: "!!binary", Value: base64.StdEncoding.EncodeToString(v.Bytes)},
			scalarKeyNode("len"), scalarIntNode(int64(v.Len)),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgPointYAML(v pgtype.Point) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2Node(v.P),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgLineYAML(v pgtype.Line) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("a"), scalarFloatNode(v.A),
			scalarKeyNode("b"), scalarFloatNode(v.B),
			scalarKeyNode("c"), scalarFloatNode(v.C),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgLsegYAML(v pgtype.Lseg) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2SeqNode(v.P[:]),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgBoxYAML(v pgtype.Box) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2SeqNode(v.P[:]),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgPathYAML(v pgtype.Path) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2SeqNode(v.P),
			scalarKeyNode("closed"), scalarBoolNode(v.Closed),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgPolygonYAML(v pgtype.Polygon) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2SeqNode(v.P),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgCircleYAML(v pgtype.Circle) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("p"), marshalVec2Node(v.P),
			scalarKeyNode("r"), scalarFloatNode(v.R),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgTIDYAML(v pgtype.TID) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("blocknumber"), scalarIntNode(int64(v.BlockNumber)),
			scalarKeyNode("offsetnumber"), scalarIntNode(int64(v.OffsetNumber)),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

func marshalPgTSVectorYAML(v pgtype.TSVector) *yaml.Node {
	lexemes := &yaml.Node{Kind: yaml.SequenceNode}
	lexemes.Content = make([]*yaml.Node, 0, len(v.Lexemes))
	for _, lex := range v.Lexemes {
		positions := &yaml.Node{Kind: yaml.SequenceNode}
		positions.Content = make([]*yaml.Node, 0, len(lex.Positions))
		for _, p := range lex.Positions {
			positions.Content = append(positions.Content, &yaml.Node{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					scalarKeyNode("position"), scalarIntNode(int64(p.Position)),
					scalarKeyNode("weight"), scalarIntNode(int64(p.Weight)),
				},
			})
		}
		lexemes.Content = append(lexemes.Content, &yaml.Node{
			Kind: yaml.MappingNode,
			Content: []*yaml.Node{
				scalarKeyNode("word"), scalarStrNode(lex.Word),
				scalarKeyNode("positions"), positions,
			},
		})
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("lexemes"), lexemes,
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}
}

// marshalPgHstoreYAML emits an hstore as an untagged mapping carrying
// the raw key/value pairs. SQL-NULL values inside the hstore (the
// `*string` being nil) are emitted as YAML null so the round-trip
// preserves the nil-vs-empty-string distinction the codec dispatches
// on.
func marshalPgHstoreYAML(v pgtype.Hstore) *yaml.Node {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := &yaml.Node{Kind: yaml.MappingNode}
	out.Content = make([]*yaml.Node, 0, 2*len(keys))
	for _, k := range keys {
		val := v[k]
		var valNode *yaml.Node
		if val == nil {
			valNode = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
		} else if StringNeedsDoubleQuoted(*val) {
			valNode = &yaml.Node{Kind: yaml.ScalarNode, Style: yaml.DoubleQuotedStyle, Value: *val}
		} else {
			valNode = scalarStrNode(*val)
		}
		out.Content = append(out.Content, scalarKeyNode(k), valNode)
	}
	return out
}

// marshalPgRangeYAML emits Range[any] as an untagged mapping. Lower and
// Upper recurse through PostgresV3Cell.MarshalYAML so the bound element
// type stays per-cohort (int4 → int32, tstzrange → time.Time, numrange
// → pgtype.Numeric, etc.) — same recursion pattern as the gob path.
func marshalPgRangeYAML(v pgtype.Range[any]) (*yaml.Node, error) {
	lowerAny, err := PostgresV3Cell{Value: v.Lower}.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("PostgresV3Cell.MarshalYAML range lower: %w", err)
	}
	upperAny, err := PostgresV3Cell{Value: v.Upper}.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("PostgresV3Cell.MarshalYAML range upper: %w", err)
	}
	lowerNode, err := toYAMLNode(lowerAny)
	if err != nil {
		return nil, fmt.Errorf("PostgresV3Cell.MarshalYAML range lower node: %w", err)
	}
	upperNode, err := toYAMLNode(upperAny)
	if err != nil {
		return nil, fmt.Errorf("PostgresV3Cell.MarshalYAML range upper node: %w", err)
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("lower"), lowerNode,
			scalarKeyNode("upper"), upperNode,
			scalarKeyNode("lowertype"), scalarIntNode(int64(v.LowerType)),
			scalarKeyNode("uppertype"), scalarIntNode(int64(v.UpperType)),
			scalarKeyNode("valid"), scalarBoolNode(v.Valid),
		},
	}, nil
}

func marshalPgMultirangeYAML(v pgtype.Multirange[pgtype.Range[any]]) (*yaml.Node, error) {
	out := &yaml.Node{Kind: yaml.SequenceNode}
	out.Content = make([]*yaml.Node, 0, len(v))
	for i, r := range v {
		n, err := marshalPgRangeYAML(r)
		if err != nil {
			return nil, fmt.Errorf("PostgresV3Cell.MarshalYAML multirange[%d]: %w", i, err)
		}
		out.Content = append(out.Content, n)
	}
	return out, nil
}

// marshalNetipPrefixYAML emits a netip.Prefix as a single-key mapping
// `{prefix: "<canonical>"}`. The canonical string form
// (`Prefix.String()`) round-trips losslessly through
// `netip.ParsePrefix` for both IPv4 and IPv6, including host-only
// forms like `1.2.3.4/32` and `::1/128`. The single-key mapping is
// unambiguous against the other pgtype shapes (none use a sole
// `prefix` key), so the canonical-key-set probe in
// decodePgUntaggedMapping picks it up cleanly without a `!pg/<name>`
// tag (cross-version compat with released keploy).
func marshalNetipPrefixYAML(v netip.Prefix) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("prefix"), scalarStrNode(v.String()),
		},
	}
}

// marshalHardwareAddrYAML emits a net.HardwareAddr as a single-key
// mapping `{macaddr: !!binary <base64>}`. The raw bytes (rather than
// the canonical colon-separated text form) preserve the macaddr8 vs
// macaddr distinction by length without a separate field, matching
// how the gob path stores them. The single-key mapping disambiguates
// against the unnamed `[]byte` cell, which goes through the
// `case []byte:` arm in MarshalYAML and emits a bare `!!binary`
// scalar — so the cell-level decode never confuses a `bytea` column
// with a `macaddr` column.
func marshalHardwareAddrYAML(v net.HardwareAddr) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("macaddr"), {
				Kind:  yaml.ScalarNode,
				Tag:   "!!binary",
				Value: base64.StdEncoding.EncodeToString(v),
			},
		},
	}
}

// marshalUUIDBytesYAML emits a `[16]uint8` as a single-key mapping
// `{uuid: !!binary <base64>}`. Same shape rationale as
// marshalHardwareAddrYAML: the single-key mapping disambiguates
// against the unnamed `[]byte` cell (which goes through the
// `case []byte:` arm and emits a bare `!!binary` scalar) so the
// cell-level decode never confuses a `bytea` column with a `uuid`
// column. The 16-byte length is implicit in the payload.
func marshalUUIDBytesYAML(v [16]uint8) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			scalarKeyNode("uuid"), {
				Kind:  yaml.ScalarNode,
				Tag:   "!!binary",
				Value: base64.StdEncoding.EncodeToString(v[:]),
			},
		},
	}
}

// toYAMLNode converts a value (which may already be a *yaml.Node from a
// nested MarshalYAML call, or a plain Go value yaml.v3 will encode via
// reflection) to a *yaml.Node. Used by the range/multirange marshallers
// that need to splice the per-bound MarshalYAML output back into a
// MappingNode.Content slice.
func toYAMLNode(v any) (*yaml.Node, error) {
	if n, ok := v.(*yaml.Node); ok {
		return n, nil
	}
	out := &yaml.Node{}
	if err := out.Encode(v); err != nil {
		return nil, err
	}
	return out, nil
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
// Non-scalar nodes (sequences / mappings) are rejected with an
// explicit error: their .Value is empty, so the previous "just copy
// node.Value" path would have silently produced "" and corrupted
// notice / error fields without surfacing the malformed input.
func (s *PostgresV3SafeString) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		*s = ""
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("PostgresV3SafeString: expected scalar node, got kind=%d", node.Kind)
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
	// Backward-compat: a `!pg/<name>` local tag on the node selects a
	// specific reconstructor without depending on key-set heuristics.
	// MarshalYAML no longer emits these — the cross-version GHA matrix
	// (PR-built recorder, released-keploy replayer) cannot tolerate
	// custom tags the released binary doesn't register — but recordings
	// from the brief window when the tag-driven encoder shipped still
	// decode through this path.
	if v, ok, err := decodePgTaggedNode(node); err != nil {
		return err
	} else if ok {
		c.Value = v
		return nil
	}
	// Primary read path: untagged mapping that looks like one of the
	// pgtype shapes (the on-disk shape released keploy emits and the
	// shape this version emits since the !pg/<name> tags were dropped).
	// Probe the canonical key set and route to the right reconstructor;
	// if nothing matches, fall through to the generic any-decode path.
	//
	// `!!map` is yaml.v3's auto-resolved tag for any plain mapping
	// without an explicit local tag — that's the shape current and
	// pre-tagged recordings both carry on disk, so we treat it the
	// same as an empty tag string for the canonical-key-set probe.
	if node.Kind == yaml.MappingNode && (node.Tag == "" || node.Tag == "!!map") {
		if v, ok, err := decodePgUntaggedMapping(node); err != nil {
			return err
		} else if ok {
			c.Value = v
			return nil
		}
	}
	var v any
	if err := node.Decode(&v); err != nil {
		return fmt.Errorf("PostgresV3Cell: decode node (kind=%d, tag=%q): %w", node.Kind, node.Tag, err)
	}
	c.Value = v
	return nil
}

// decodePgTaggedNode handles the `!pg/<name>` tag dispatch. Returns
// (value, true, nil) on a match, (nil, false, nil) if the node has a
// different (or empty) tag, or (nil, false, err) on a decode failure.
func decodePgTaggedNode(node *yaml.Node) (any, bool, error) {
	switch node.Tag {
	case pgYAMLTagNumeric:
		v, err := decodePgNumericMapping(node)
		return v, true, err
	case pgYAMLTagInterval:
		v, err := decodePgIntervalMapping(node)
		return v, true, err
	case pgYAMLTagTime:
		v, err := decodePgTimeMapping(node)
		return v, true, err
	case pgYAMLTagBits:
		v, err := decodePgBitsMapping(node)
		return v, true, err
	case pgYAMLTagPoint:
		v, err := decodePgPointMapping(node)
		return v, true, err
	case pgYAMLTagLine:
		v, err := decodePgLineMapping(node)
		return v, true, err
	case pgYAMLTagLseg:
		v, err := decodePgLsegMapping(node)
		return v, true, err
	case pgYAMLTagBox:
		v, err := decodePgBoxMapping(node)
		return v, true, err
	case pgYAMLTagPath:
		v, err := decodePgPathMapping(node)
		return v, true, err
	case pgYAMLTagPolygon:
		v, err := decodePgPolygonMapping(node)
		return v, true, err
	case pgYAMLTagCircle:
		v, err := decodePgCircleMapping(node)
		return v, true, err
	case pgYAMLTagTID:
		v, err := decodePgTIDMapping(node)
		return v, true, err
	case pgYAMLTagTSVector:
		v, err := decodePgTSVectorMapping(node)
		return v, true, err
	case pgYAMLTagHstore:
		v, err := decodePgHstoreMapping(node)
		return v, true, err
	case pgYAMLTagRange:
		v, err := decodePgRangeMapping(node)
		return v, true, err
	case pgYAMLTagMultirange:
		v, err := decodePgMultirangeNode(node)
		return v, true, err
	case pgYAMLTagPrefix:
		v, err := decodeNetipPrefixMapping(node)
		return v, true, err
	case pgYAMLTagMACAddr:
		v, err := decodeHardwareAddrMapping(node)
		return v, true, err
	}
	return nil, false, nil
}

// decodePgUntaggedMapping inspects an untagged MappingNode for one of
// the canonical pgtype key sets and routes to the matching
// reconstructor. This is the primary read path: MarshalYAML emits
// untagged mappings (for cross-version compat with released keploy
// on Docker Hub, whose YAML library doesn't know the `!pg/<name>`
// local tags), and listmonk's pre-fix mocks.yaml shape — Numeric as a
// bare `{int, exp, nan, infinitymodifier, valid}` mapping — flows
// through the same path. Only key sets that uniquely identify a
// single pgtype shape are probed; ambiguous shapes (Point/Lseg/Box/
// Polygon all match `{p, valid}`) cannot be recovered to their
// concrete Go type from an untagged mapping alone — those decode as
// `map[string]any`, matching how released keploy's reflection emit
// also rehydrates them.
//
// Invariant: every key set in the switch below must be pairwise
// distinct from — and not a subset of — every other key set in the
// switch. `keysEqual` requires exact equality, so a subset relation
// (e.g. Time's {microseconds, valid} is contained in Interval's
// {microseconds, days, months, valid}) does not cause silent
// corruption today; but the moment anyone weakens the predicate to a
// "contains-all-required-keys" match, the smaller set would shadow
// the larger one and silently misroute the larger type's recordings.
// The disjointness audit is pinned by
// TestPgtypeYAMLKeySetsMutuallyDisjoint in
// pgtype_yaml_disambiguation_test.go.
func decodePgUntaggedMapping(node *yaml.Node) (any, bool, error) {
	keys := pgMappingKeySet(node)
	if keys == nil {
		return nil, false, nil
	}
	switch {
	case keysEqual(keys, []string{"int", "exp", "nan", "infinitymodifier", "valid"}):
		v, err := decodePgNumericMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"microseconds", "days", "months", "valid"}):
		v, err := decodePgIntervalMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"microseconds", "valid"}):
		v, err := decodePgTimeMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"bytes", "len", "valid"}):
		v, err := decodePgBitsMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"a", "b", "c", "valid"}):
		v, err := decodePgLineMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"p", "closed", "valid"}):
		v, err := decodePgPathMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"p", "r", "valid"}):
		v, err := decodePgCircleMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"blocknumber", "offsetnumber", "valid"}):
		v, err := decodePgTIDMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"lexemes", "valid"}):
		v, err := decodePgTSVectorMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"lower", "upper", "lowertype", "uppertype", "valid"}):
		v, err := decodePgRangeMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"prefix"}):
		v, err := decodeNetipPrefixMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"macaddr"}):
		v, err := decodeHardwareAddrMapping(node)
		return v, true, err
	case keysEqual(keys, []string{"uuid"}):
		v, err := decodeUUIDBytesMapping(node)
		return v, true, err
	}
	return nil, false, nil
}

// pgMappingKeySet returns the lowercased keys of a mapping node, or nil
// if any key is non-scalar (in which case backward-compat probing must
// not match — the node is a generic map).
func pgMappingKeySet(node *yaml.Node) []string {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]string, 0, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		k := node.Content[i]
		if k.Kind != yaml.ScalarNode {
			return nil
		}
		out = append(out, k.Value)
	}
	return out
}

func keysEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	gs := make([]string, len(got))
	copy(gs, got)
	ws := make([]string, len(want))
	copy(ws, want)
	sort.Strings(gs)
	sort.Strings(ws)
	for i := range gs {
		if gs[i] != ws[i] {
			return false
		}
	}
	return true
}

// pgMappingFields returns the value node for each lowercased key in a
// MappingNode. Per-type decoders look up the fields they care about
// rather than walking the slice manually for every type.
func pgMappingFields(node *yaml.Node) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node, got kind=%d", node.Kind)
	}
	out := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("non-scalar mapping key (kind=%d)", k.Kind)
		}
		out[k.Value] = node.Content[i+1]
	}
	return out, nil
}

func decodePgNumericMapping(node *yaml.Node) (pgtype.Numeric, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Numeric{}, fmt.Errorf("pg/numeric: %w", err)
	}
	out := pgtype.Numeric{}
	if intNode, ok := fields["int"]; ok && intNode != nil {
		// pgtype.Numeric.Int is *big.Int rendered via its TextMarshaler
		// — yaml.v3 emits "<digits>" or "" for nil. Decode the string
		// form back into *big.Int; an empty string keeps Int=nil so
		// NaN / ±infinity numerics preserve their nil-Int identity.
		var s string
		if intNode.Kind == yaml.ScalarNode {
			s = intNode.Value
		} else if err := intNode.Decode(&s); err != nil {
			return pgtype.Numeric{}, fmt.Errorf("pg/numeric int: %w", err)
		}
		if s != "" {
			bi := new(big.Int)
			if _, ok := bi.SetString(s, 10); !ok {
				return pgtype.Numeric{}, fmt.Errorf("pg/numeric int: invalid integer literal %q", s)
			}
			out.Int = bi
		}
	}
	if expNode, ok := fields["exp"]; ok && expNode != nil {
		var n int32
		if err := expNode.Decode(&n); err != nil {
			return pgtype.Numeric{}, fmt.Errorf("pg/numeric exp: %w", err)
		}
		out.Exp = n
	}
	if nanNode, ok := fields["nan"]; ok && nanNode != nil {
		if err := nanNode.Decode(&out.NaN); err != nil {
			return pgtype.Numeric{}, fmt.Errorf("pg/numeric nan: %w", err)
		}
	}
	if imNode, ok := fields["infinitymodifier"]; ok && imNode != nil {
		var n int8
		if err := imNode.Decode(&n); err != nil {
			return pgtype.Numeric{}, fmt.Errorf("pg/numeric infinitymodifier: %w", err)
		}
		out.InfinityModifier = pgtype.InfinityModifier(n)
	}
	if validNode, ok := fields["valid"]; ok && validNode != nil {
		if err := validNode.Decode(&out.Valid); err != nil {
			return pgtype.Numeric{}, fmt.Errorf("pg/numeric valid: %w", err)
		}
	}
	return out, nil
}

func decodePgIntervalMapping(node *yaml.Node) (pgtype.Interval, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Interval{}, fmt.Errorf("pg/interval: %w", err)
	}
	out := pgtype.Interval{}
	if n, ok := fields["microseconds"]; ok && n != nil {
		if err := n.Decode(&out.Microseconds); err != nil {
			return out, fmt.Errorf("pg/interval microseconds: %w", err)
		}
	}
	if n, ok := fields["days"]; ok && n != nil {
		if err := n.Decode(&out.Days); err != nil {
			return out, fmt.Errorf("pg/interval days: %w", err)
		}
	}
	if n, ok := fields["months"]; ok && n != nil {
		if err := n.Decode(&out.Months); err != nil {
			return out, fmt.Errorf("pg/interval months: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/interval valid: %w", err)
		}
	}
	return out, nil
}

func decodePgTimeMapping(node *yaml.Node) (pgtype.Time, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Time{}, fmt.Errorf("pg/time: %w", err)
	}
	out := pgtype.Time{}
	if n, ok := fields["microseconds"]; ok && n != nil {
		if err := n.Decode(&out.Microseconds); err != nil {
			return out, fmt.Errorf("pg/time microseconds: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/time valid: %w", err)
		}
	}
	return out, nil
}

func decodePgBitsMapping(node *yaml.Node) (pgtype.Bits, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Bits{}, fmt.Errorf("pg/bits: %w", err)
	}
	out := pgtype.Bits{}
	if n, ok := fields["bytes"]; ok && n != nil {
		// Bytes may arrive as `!!binary` (the new tagged emitter) or as
		// a `[]int` sequence (yaml.v3's reflective fallback). Try the
		// binary scalar path first, then fall back to the int-slice
		// decode so backward-compat with hand-edited fixtures works.
		if n.Kind == yaml.ScalarNode && n.Tag == "!!binary" {
			raw := n.Value
			if strings.ContainsAny(raw, " \t\r\n") {
				raw = stripBase64Whitespace(raw)
			}
			b, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				return out, fmt.Errorf("pg/bits bytes: %w", err)
			}
			out.Bytes = b
		} else if err := n.Decode(&out.Bytes); err != nil {
			return out, fmt.Errorf("pg/bits bytes: %w", err)
		}
	}
	if n, ok := fields["len"]; ok && n != nil {
		if err := n.Decode(&out.Len); err != nil {
			return out, fmt.Errorf("pg/bits len: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/bits valid: %w", err)
		}
	}
	return out, nil
}

func decodeVec2Mapping(node *yaml.Node) (pgtype.Vec2, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Vec2{}, fmt.Errorf("pg vec2: %w", err)
	}
	out := pgtype.Vec2{}
	if n, ok := fields["x"]; ok && n != nil {
		if err := n.Decode(&out.X); err != nil {
			return out, fmt.Errorf("pg vec2 x: %w", err)
		}
	}
	if n, ok := fields["y"]; ok && n != nil {
		if err := n.Decode(&out.Y); err != nil {
			return out, fmt.Errorf("pg vec2 y: %w", err)
		}
	}
	return out, nil
}

func decodeVec2SeqNode(node *yaml.Node) ([]pgtype.Vec2, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("pg vec2 seq: expected sequence, got kind=%d", node.Kind)
	}
	out := make([]pgtype.Vec2, 0, len(node.Content))
	for i, child := range node.Content {
		v, err := decodeVec2Mapping(child)
		if err != nil {
			return nil, fmt.Errorf("pg vec2 seq[%d]: %w", i, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func decodePgPointMapping(node *yaml.Node) (pgtype.Point, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Point{}, fmt.Errorf("pg/point: %w", err)
	}
	out := pgtype.Point{}
	if n, ok := fields["p"]; ok && n != nil {
		v, err := decodeVec2Mapping(n)
		if err != nil {
			return out, fmt.Errorf("pg/point p: %w", err)
		}
		out.P = v
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/point valid: %w", err)
		}
	}
	return out, nil
}

func decodePgLineMapping(node *yaml.Node) (pgtype.Line, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Line{}, fmt.Errorf("pg/line: %w", err)
	}
	out := pgtype.Line{}
	if n, ok := fields["a"]; ok && n != nil {
		if err := n.Decode(&out.A); err != nil {
			return out, fmt.Errorf("pg/line a: %w", err)
		}
	}
	if n, ok := fields["b"]; ok && n != nil {
		if err := n.Decode(&out.B); err != nil {
			return out, fmt.Errorf("pg/line b: %w", err)
		}
	}
	if n, ok := fields["c"]; ok && n != nil {
		if err := n.Decode(&out.C); err != nil {
			return out, fmt.Errorf("pg/line c: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/line valid: %w", err)
		}
	}
	return out, nil
}

func decodePgLsegMapping(node *yaml.Node) (pgtype.Lseg, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Lseg{}, fmt.Errorf("pg/lseg: %w", err)
	}
	out := pgtype.Lseg{}
	if n, ok := fields["p"]; ok && n != nil {
		ps, err := decodeVec2SeqNode(n)
		if err != nil {
			return out, fmt.Errorf("pg/lseg p: %w", err)
		}
		if len(ps) != 2 {
			return out, fmt.Errorf("pg/lseg p: expected 2 vec2, got %d", len(ps))
		}
		out.P = [2]pgtype.Vec2{ps[0], ps[1]}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/lseg valid: %w", err)
		}
	}
	return out, nil
}

func decodePgBoxMapping(node *yaml.Node) (pgtype.Box, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Box{}, fmt.Errorf("pg/box: %w", err)
	}
	out := pgtype.Box{}
	if n, ok := fields["p"]; ok && n != nil {
		ps, err := decodeVec2SeqNode(n)
		if err != nil {
			return out, fmt.Errorf("pg/box p: %w", err)
		}
		if len(ps) != 2 {
			return out, fmt.Errorf("pg/box p: expected 2 vec2, got %d", len(ps))
		}
		out.P = [2]pgtype.Vec2{ps[0], ps[1]}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/box valid: %w", err)
		}
	}
	return out, nil
}

func decodePgPathMapping(node *yaml.Node) (pgtype.Path, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Path{}, fmt.Errorf("pg/path: %w", err)
	}
	out := pgtype.Path{}
	if n, ok := fields["p"]; ok && n != nil {
		ps, err := decodeVec2SeqNode(n)
		if err != nil {
			return out, fmt.Errorf("pg/path p: %w", err)
		}
		out.P = ps
	}
	if n, ok := fields["closed"]; ok && n != nil {
		if err := n.Decode(&out.Closed); err != nil {
			return out, fmt.Errorf("pg/path closed: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/path valid: %w", err)
		}
	}
	return out, nil
}

func decodePgPolygonMapping(node *yaml.Node) (pgtype.Polygon, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Polygon{}, fmt.Errorf("pg/polygon: %w", err)
	}
	out := pgtype.Polygon{}
	if n, ok := fields["p"]; ok && n != nil {
		ps, err := decodeVec2SeqNode(n)
		if err != nil {
			return out, fmt.Errorf("pg/polygon p: %w", err)
		}
		out.P = ps
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/polygon valid: %w", err)
		}
	}
	return out, nil
}

func decodePgCircleMapping(node *yaml.Node) (pgtype.Circle, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Circle{}, fmt.Errorf("pg/circle: %w", err)
	}
	out := pgtype.Circle{}
	if n, ok := fields["p"]; ok && n != nil {
		v, err := decodeVec2Mapping(n)
		if err != nil {
			return out, fmt.Errorf("pg/circle p: %w", err)
		}
		out.P = v
	}
	if n, ok := fields["r"]; ok && n != nil {
		if err := n.Decode(&out.R); err != nil {
			return out, fmt.Errorf("pg/circle r: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/circle valid: %w", err)
		}
	}
	return out, nil
}

func decodePgTIDMapping(node *yaml.Node) (pgtype.TID, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.TID{}, fmt.Errorf("pg/tid: %w", err)
	}
	out := pgtype.TID{}
	if n, ok := fields["blocknumber"]; ok && n != nil {
		if err := n.Decode(&out.BlockNumber); err != nil {
			return out, fmt.Errorf("pg/tid blocknumber: %w", err)
		}
	}
	if n, ok := fields["offsetnumber"]; ok && n != nil {
		if err := n.Decode(&out.OffsetNumber); err != nil {
			return out, fmt.Errorf("pg/tid offsetnumber: %w", err)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/tid valid: %w", err)
		}
	}
	return out, nil
}

func decodePgTSVectorMapping(node *yaml.Node) (pgtype.TSVector, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.TSVector{}, fmt.Errorf("pg/tsvector: %w", err)
	}
	out := pgtype.TSVector{}
	if n, ok := fields["lexemes"]; ok && n != nil {
		if n.Kind != yaml.SequenceNode {
			return out, fmt.Errorf("pg/tsvector lexemes: expected sequence, got kind=%d", n.Kind)
		}
		out.Lexemes = make([]pgtype.TSVectorLexeme, 0, len(n.Content))
		for i, lexNode := range n.Content {
			lf, err := pgMappingFields(lexNode)
			if err != nil {
				return out, fmt.Errorf("pg/tsvector lex[%d]: %w", i, err)
			}
			lex := pgtype.TSVectorLexeme{}
			if w, ok := lf["word"]; ok && w != nil {
				if err := w.Decode(&lex.Word); err != nil {
					return out, fmt.Errorf("pg/tsvector lex[%d] word: %w", i, err)
				}
			}
			if pn, ok := lf["positions"]; ok && pn != nil {
				if pn.Kind != yaml.SequenceNode {
					return out, fmt.Errorf("pg/tsvector lex[%d] positions: expected sequence, got kind=%d", i, pn.Kind)
				}
				lex.Positions = make([]pgtype.TSVectorPosition, 0, len(pn.Content))
				for j, posNode := range pn.Content {
					pf, err := pgMappingFields(posNode)
					if err != nil {
						return out, fmt.Errorf("pg/tsvector lex[%d] pos[%d]: %w", i, j, err)
					}
					pos := pgtype.TSVectorPosition{}
					if pp, ok := pf["position"]; ok && pp != nil {
						if err := pp.Decode(&pos.Position); err != nil {
							return out, fmt.Errorf("pg/tsvector lex[%d] pos[%d] position: %w", i, j, err)
						}
					}
					if pw, ok := pf["weight"]; ok && pw != nil {
						var w byte
						if err := pw.Decode(&w); err != nil {
							return out, fmt.Errorf("pg/tsvector lex[%d] pos[%d] weight: %w", i, j, err)
						}
						pos.Weight = pgtype.TSVectorWeight(w)
					}
					lex.Positions = append(lex.Positions, pos)
				}
			}
			out.Lexemes = append(out.Lexemes, lex)
		}
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/tsvector valid: %w", err)
		}
	}
	return out, nil
}

func decodePgHstoreMapping(node *yaml.Node) (pgtype.Hstore, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("pg/hstore: expected mapping, got kind=%d", node.Kind)
	}
	out := make(pgtype.Hstore, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		v := node.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("pg/hstore: non-scalar key (kind=%d)", k.Kind)
		}
		if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
			out[k.Value] = nil
			continue
		}
		var s string
		if err := v.Decode(&s); err != nil {
			return nil, fmt.Errorf("pg/hstore value for %q: %w", k.Value, err)
		}
		sv := s
		out[k.Value] = &sv
	}
	return out, nil
}

func decodePgRangeMapping(node *yaml.Node) (pgtype.Range[any], error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return pgtype.Range[any]{}, fmt.Errorf("pg/range: %w", err)
	}
	out := pgtype.Range[any]{}
	if n, ok := fields["lower"]; ok && n != nil {
		var c PostgresV3Cell
		if err := c.UnmarshalYAML(n); err != nil {
			return out, fmt.Errorf("pg/range lower: %w", err)
		}
		out.Lower = c.Value
	}
	if n, ok := fields["upper"]; ok && n != nil {
		var c PostgresV3Cell
		if err := c.UnmarshalYAML(n); err != nil {
			return out, fmt.Errorf("pg/range upper: %w", err)
		}
		out.Upper = c.Value
	}
	if n, ok := fields["lowertype"]; ok && n != nil {
		var b byte
		if err := n.Decode(&b); err != nil {
			return out, fmt.Errorf("pg/range lowertype: %w", err)
		}
		out.LowerType = pgtype.BoundType(b)
	}
	if n, ok := fields["uppertype"]; ok && n != nil {
		var b byte
		if err := n.Decode(&b); err != nil {
			return out, fmt.Errorf("pg/range uppertype: %w", err)
		}
		out.UpperType = pgtype.BoundType(b)
	}
	if n, ok := fields["valid"]; ok && n != nil {
		if err := n.Decode(&out.Valid); err != nil {
			return out, fmt.Errorf("pg/range valid: %w", err)
		}
	}
	return out, nil
}

func decodePgMultirangeNode(node *yaml.Node) (pgtype.Multirange[pgtype.Range[any]], error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("pg/multirange: expected sequence, got kind=%d", node.Kind)
	}
	out := make(pgtype.Multirange[pgtype.Range[any]], 0, len(node.Content))
	for i, child := range node.Content {
		r, err := decodePgRangeMapping(child)
		if err != nil {
			return nil, fmt.Errorf("pg/multirange[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// decodeNetipPrefixMapping reconstructs a netip.Prefix from the
// `{prefix: "<canonical>"}` mapping shape emitted by
// marshalNetipPrefixYAML. The string body is parsed with
// netip.ParsePrefix; both IPv4 (`1.2.3.0/24`, `1.2.3.4/32`) and IPv6
// (`2001:db8::/32`, `::1/128`) canonical forms round-trip cleanly.
func decodeNetipPrefixMapping(node *yaml.Node) (netip.Prefix, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: %w", err)
	}
	pn, ok := fields["prefix"]
	if !ok || pn == nil {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: missing 'prefix' field")
	}
	if pn.Kind != yaml.ScalarNode {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: 'prefix' must be a scalar, got kind=%d", pn.Kind)
	}
	p, err := netip.ParsePrefix(pn.Value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: parse %q: %w", pn.Value, err)
	}
	return p, nil
}

// decodeHardwareAddrMapping reconstructs a net.HardwareAddr from the
// `{macaddr: !!binary <base64>}` mapping shape emitted by
// marshalHardwareAddrYAML. The byte slice is wrapped in a
// HardwareAddr named-type cast; the byte length encodes macaddr (6)
// vs macaddr8 (8) without a separate field.
func decodeHardwareAddrMapping(node *yaml.Node) (net.HardwareAddr, error) {
	fields, err := pgMappingFields(node)
	if err != nil {
		return nil, fmt.Errorf("pg/macaddr: %w", err)
	}
	mn, ok := fields["macaddr"]
	if !ok || mn == nil {
		return nil, fmt.Errorf("pg/macaddr: missing 'macaddr' field")
	}
	if mn.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("pg/macaddr: 'macaddr' must be a scalar, got kind=%d", mn.Kind)
	}
	raw := mn.Value
	if strings.ContainsAny(raw, " \t\r\n") {
		raw = stripBase64Whitespace(raw)
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("pg/macaddr: base64 decode: %w", err)
	}
	return net.HardwareAddr(b), nil
}

// decodeUUIDBytesMapping reconstructs a `[16]uint8` from the
// `{uuid: !!binary <base64>}` mapping shape emitted by
// marshalUUIDBytesYAML. The 16-byte length is enforced explicitly
// — fixed-array sizing must be exact, and a wrong-length payload
// would silently truncate or zero-pad if we just `copy`'d.
func decodeUUIDBytesMapping(node *yaml.Node) ([16]uint8, error) {
	var arr [16]uint8
	fields, err := pgMappingFields(node)
	if err != nil {
		return arr, fmt.Errorf("pg/uuid: %w", err)
	}
	mn, ok := fields["uuid"]
	if !ok || mn == nil {
		return arr, fmt.Errorf("pg/uuid: missing 'uuid' field")
	}
	if mn.Kind != yaml.ScalarNode {
		return arr, fmt.Errorf("pg/uuid: 'uuid' must be a scalar, got kind=%d", mn.Kind)
	}
	raw := mn.Value
	if strings.ContainsAny(raw, " \t\r\n") {
		raw = stripBase64Whitespace(raw)
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return arr, fmt.Errorf("pg/uuid: base64 decode: %w", err)
	}
	if len(b) != 16 {
		return arr, fmt.Errorf("pg/uuid: expected 16 bytes, got %d", len(b))
	}
	copy(arr[:], b)
	return arr, nil
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

// encodeGeomVec2s writes a length-prefixed sequence of Vec2 (X, Y
// pairs) for the geometric union (Point as len=1, Lseg/Box as len=2,
// Path/Polygon as len=N). The shared helper keeps the geometric encode
// arms compact and consistent.
func encodeGeomVec2s(enc *gob.Encoder, ps []pgtype.Vec2) error {
	if err := enc.Encode(int32(len(ps))); err != nil {
		return err
	}
	for _, p := range ps {
		if err := enc.Encode(p.X); err != nil {
			return err
		}
		if err := enc.Encode(p.Y); err != nil {
			return err
		}
	}
	return nil
}

// decodeGeomVec2s reads a length-prefixed Vec2 sequence written by
// encodeGeomVec2s. Symmetric inverse.
func decodeGeomVec2s(dec *gob.Decoder) ([]pgtype.Vec2, error) {
	var n int32
	if err := dec.Decode(&n); err != nil {
		return nil, err
	}
	// Same defensive bounds check as the slice-cell GobDecode arm:
	// gob input is untrusted, so a corrupted/hand-edited length
	// must not crash make() or drive an unbounded allocation.
	if n < 0 {
		return nil, fmt.Errorf("decodeGeomVec2s: invalid negative length %d", n)
	}
	out := make([]pgtype.Vec2, int(n))
	for i := 0; i < int(n); i++ {
		if err := dec.Decode(&out[i].X); err != nil {
			return nil, err
		}
		if err := dec.Decode(&out[i].Y); err != nil {
			return nil, err
		}
	}
	return out, nil
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
//	18 → int8        (gob int8 — pgtype.InfinityModifier sentinel for
//	                  ±infinity timestamps, plus literal int8 binds.)
//	19 → pgtype.Interval  (Microseconds, Days, Months, Valid.)
//	20 → pgtype.Time      (Microseconds, Valid.)
//	21 → pgtype.Bits      (Bytes, Len int32, Valid.)
//	22 → geometric union  (1-byte sub-discriminator: 1=Point, 2=Line,
//	                  3=Lseg, 4=Box, 5=Path, 6=Polygon, 7=Circle,
//	                  followed by Vec2 sequence + per-shape extras.)
//	23 → pgtype.TID       (BlockNumber uint32, OffsetNumber uint16,
//	                  Valid.)
//	24 → pgtype.TSVector  (length + sequence of (Word string, length
//	                  + sequence of (Position uint16, Weight byte)),
//	                  Valid.)
//	25 → netip.Prefix     (canonical Prefix.String() form.)
//	26 → net.HardwareAddr (raw bytes, distinct tag from cellTagBytes
//	                  to preserve type identity for pgtype.Encode.)
//	27 → pgtype.Hstore    (length + sorted (key, isNil bool, value
//	                  string-when-not-nil) tuples. *string preserves
//	                  the SQL-NULL-inside-hstore semantic.)
//	28 → pgtype.Range[any]  (Lower nested-cell bytes, Upper nested-
//	                  cell bytes, LowerType byte, UpperType byte,
//	                  Valid. Bound element type recurses through the
//	                  whole catalogue — int4 → int32, tstzrange →
//	                  time.Time, numrange → pgtype.Numeric, etc.)
//	29 → pgtype.Multirange[Range[any]]  (length + sequence of nested
//	                  Range cells through tag 28.)
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
	// Tag 18 — `int8`. Covers PG's `pgtype.InfinityModifier` (int8 alias)
	// returned for `'infinity'` / `'-infinity'` timestamps, plus any
	// caller that supplies a literal int8 bind. Kept distinct from
	// int16/int32/int64 because gob's reflection won't widen across
	// signed integer sizes when the destination is `*any`.
	cellTagInt8 byte = 18
	// Tag 19 — `pgtype.Interval` (PG `interval`).
	cellTagInterval byte = 19
	// Tag 20 — `pgtype.Time` (PG `time`).
	cellTagPgTime byte = 20
	// Tag 21 — `pgtype.Bits` (PG `bit` / `varbit`).
	cellTagBits byte = 21
	// Tag 22 — geometric union (Point / Line / Lseg / Box / Path /
	// Polygon / Circle). One tag with a 1-byte sub-discriminator keeps
	// the wire format readable and avoids burning seven tag bytes on
	// types that share the same Vec2-based shape.
	cellTagGeom byte = 22
	// Tag 23 — `pgtype.TID` (PG `tid` / ctid).
	cellTagTID byte = 23
	// Tag 24 — `pgtype.TSVector` (PG `tsvector`).
	cellTagTSVector byte = 24
	// Tag 25 — `netip.Prefix` (PG `inet` / `cidr`). Encoded as the
	// canonical `Prefix.String()` form so a host-only address (no
	// /N) and a network prefix round-trip distinctly.
	cellTagNetipPrefix byte = 25
	// Tag 26 — `net.HardwareAddr` (PG `macaddr` / `macaddr8`). Stored
	// as raw bytes with a distinct tag from cellTagBytes (5) so the
	// type identity survives the round-trip — pgtype.Encode's plan
	// dispatch picks different paths for `net.HardwareAddr` vs
	// `[]byte`, so collapsing them would emit wrong wire bytes.
	cellTagMACAddr byte = 26
	// Tag 27 — `pgtype.Hstore` (`map[string]*string`). Distinct from
	// cellTagJSONB (14) because hstore values are `*string` (with
	// nil meaning SQL NULL inside the hstore) while jsonb values
	// reach as `interface{}` (with `nil` being JSON null at the
	// Go-typing layer too — but the type identity is what
	// pgtype.Encode dispatches on).
	cellTagHstore byte = 27
	// Tag 28 — `pgtype.Range[any]` (PG int4range / int8range /
	// numrange / tsrange / tstzrange / daterange). Lower and Upper
	// recurse through PostgresV3Cell.GobEncode so the bound element
	// type stays per-cohort (int4 → int32, tstzrange → time.Time,
	// numrange → pgtype.Numeric, etc.).
	cellTagRange byte = 28
	// Tag 29 — `pgtype.Multirange[Range[any]]` (PG int4multirange /
	// int8multirange / nummultirange / tsmultirange / tstzmultirange
	// / datemultirange — added in PG 14). Length-prefixed sequence
	// of nested Range cells through tag 28.
	cellTagMultirange byte = 29
	// Tag 30 — Go `[16]uint8` (the underlying type of `uuid.UUID`).
	// pgx's UUIDArrayCodec returns each `uuid[]` element as a raw
	// `[16]uint8` fixed-array; without an explicit case the encoder
	// falls into the `default:` arm and tears down the entire
	// agent→k8s-proxy mock stream (see record.go HandleOutgoing).
	// Gob-encoded as `[16]uint8` after the tag, same framing as
	// every other case in this catalogue (the gob stream carries
	// the fixed-array shape on the wire; not a 16-byte raw payload).
	cellTagUUIDBytes byte = 30
)

// Geometric sub-discriminators for cellTagGeom. Stable across versions
// — do NOT renumber: existing mocks on disk depend on these.
const (
	geomKindPoint   byte = 1
	geomKindLine    byte = 2
	geomKindLseg    byte = 3
	geomKindBox     byte = 4
	geomKindPath    byte = 5
	geomKindPolygon byte = 6
	geomKindCircle  byte = 7
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
	case int8:
		buf.WriteByte(cellTagInt8)
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	case pgtype.Interval:
		buf.WriteByte(cellTagInterval)
		if err := enc.Encode(v.Microseconds); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Days); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Months); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Time:
		buf.WriteByte(cellTagPgTime)
		if err := enc.Encode(v.Microseconds); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Bits:
		buf.WriteByte(cellTagBits)
		if err := enc.Encode(v.Bytes); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Len); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Point:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindPoint); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, []pgtype.Vec2{v.P}); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Line:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindLine); err != nil {
			return nil, err
		}
		// A·x + B·y + C = 0 — three coefficients, no Vec2.
		if err := enc.Encode([3]float64{v.A, v.B, v.C}); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Lseg:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindLseg); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, v.P[:]); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Box:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindBox); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, v.P[:]); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Path:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindPath); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, v.P); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Closed); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Polygon:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindPolygon); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, v.P); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Circle:
		buf.WriteByte(cellTagGeom)
		if err := enc.Encode(geomKindCircle); err != nil {
			return nil, err
		}
		if err := encodeGeomVec2s(enc, []pgtype.Vec2{v.P}); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.R); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.TID:
		buf.WriteByte(cellTagTID)
		if err := enc.Encode(v.BlockNumber); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.OffsetNumber); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.TSVector:
		buf.WriteByte(cellTagTSVector)
		if err := enc.Encode(int32(len(v.Lexemes))); err != nil {
			return nil, err
		}
		for i, lex := range v.Lexemes {
			if err := enc.Encode(lex.Word); err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode tsvector lex %d word: %w", i, err)
			}
			if err := enc.Encode(int32(len(lex.Positions))); err != nil {
				return nil, err
			}
			for _, p := range lex.Positions {
				if err := enc.Encode(p.Position); err != nil {
					return nil, err
				}
				if err := enc.Encode(byte(p.Weight)); err != nil {
					return nil, err
				}
			}
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case netip.Prefix:
		buf.WriteByte(cellTagNetipPrefix)
		// Canonical string form: a host-only IPv4 reads as "1.2.3.4/32"
		// and a /24 network prefix reads as "1.2.3.0/24" — distinct,
		// round-trip clean via netip.ParsePrefix on decode.
		if err := enc.Encode(v.String()); err != nil {
			return nil, err
		}
	case net.HardwareAddr:
		buf.WriteByte(cellTagMACAddr)
		// Stored as raw bytes; the distinct tag preserves type
		// identity for pgtype.Encode's plan dispatch.
		if err := enc.Encode([]byte(v)); err != nil {
			return nil, err
		}
	case pgtype.Hstore:
		buf.WriteByte(cellTagHstore)
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
				return nil, err
			}
			val := v[k]
			isNil := val == nil
			if err := enc.Encode(isNil); err != nil {
				return nil, err
			}
			if !isNil {
				if err := enc.Encode(*val); err != nil {
					return nil, err
				}
			}
		}
	case pgtype.Range[any]:
		buf.WriteByte(cellTagRange)
		// Bounds recurse through this very GobEncode so the bound
		// element type (int32 for int4range, time.Time for tstzrange,
		// pgtype.Numeric for numrange, etc.) is preserved verbatim.
		lower, err := PostgresV3Cell{Value: v.Lower}.GobEncode()
		if err != nil {
			return nil, fmt.Errorf("PostgresV3Cell.GobEncode range lower (%T): %w", v.Lower, err)
		}
		upper, err := PostgresV3Cell{Value: v.Upper}.GobEncode()
		if err != nil {
			return nil, fmt.Errorf("PostgresV3Cell.GobEncode range upper (%T): %w", v.Upper, err)
		}
		if err := enc.Encode(lower); err != nil {
			return nil, err
		}
		if err := enc.Encode(upper); err != nil {
			return nil, err
		}
		if err := enc.Encode(byte(v.LowerType)); err != nil {
			return nil, err
		}
		if err := enc.Encode(byte(v.UpperType)); err != nil {
			return nil, err
		}
		if err := enc.Encode(v.Valid); err != nil {
			return nil, err
		}
	case pgtype.Multirange[pgtype.Range[any]]:
		buf.WriteByte(cellTagMultirange)
		if err := enc.Encode(int32(len(v))); err != nil {
			return nil, err
		}
		for i, r := range v {
			rangeBytes, err := PostgresV3Cell{Value: r}.GobEncode()
			if err != nil {
				return nil, fmt.Errorf("PostgresV3Cell.GobEncode multirange[%d]: %w", i, err)
			}
			if err := enc.Encode(rangeBytes); err != nil {
				return nil, err
			}
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
	case [16]uint8:
		// pgx's UUIDArrayCodec returns each `uuid[]` element as a raw
		// `[16]uint8` and the bare `uuid` column scan path lands here
		// too when callers Scan into `*[16]byte` instead of pgtype.UUID.
		// Gob-encode the [16]uint8 after the tag (same framing as
		// every other case in this switch — gob carries the
		// fixed-array shape on the wire) so GobDecode can reconstruct
		// it symmetrically.
		buf.WriteByte(cellTagUUIDBytes)
		if err := enc.Encode(v); err != nil {
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
	case cellTagInt8:
		var v int8
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode int8: %w", err)
		}
		c.Value = v
	case cellTagInterval:
		var ivl pgtype.Interval
		if err := dec.Decode(&ivl.Microseconds); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode interval Microseconds: %w", err)
		}
		if err := dec.Decode(&ivl.Days); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode interval Days: %w", err)
		}
		if err := dec.Decode(&ivl.Months); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode interval Months: %w", err)
		}
		if err := dec.Decode(&ivl.Valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode interval Valid: %w", err)
		}
		c.Value = ivl
	case cellTagPgTime:
		var tm pgtype.Time
		if err := dec.Decode(&tm.Microseconds); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode pgtype.Time Microseconds: %w", err)
		}
		if err := dec.Decode(&tm.Valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode pgtype.Time Valid: %w", err)
		}
		c.Value = tm
	case cellTagBits:
		var b pgtype.Bits
		if err := dec.Decode(&b.Bytes); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode bits Bytes: %w", err)
		}
		if err := dec.Decode(&b.Len); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode bits Len: %w", err)
		}
		if err := dec.Decode(&b.Valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode bits Valid: %w", err)
		}
		c.Value = b
	case cellTagGeom:
		var kind byte
		if err := dec.Decode(&kind); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode geom discriminator: %w", err)
		}
		switch kind {
		case geomKindPoint:
			ps, err := decodeGeomVec2s(dec)
			if err != nil || len(ps) != 1 {
				return fmt.Errorf("PostgresV3Cell.GobDecode point Vec2: %v (len=%d)", err, len(ps))
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode point Valid: %w", err)
			}
			c.Value = pgtype.Point{P: ps[0], Valid: valid}
		case geomKindLine:
			var coef [3]float64
			if err := dec.Decode(&coef); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode line coef: %w", err)
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode line Valid: %w", err)
			}
			c.Value = pgtype.Line{A: coef[0], B: coef[1], C: coef[2], Valid: valid}
		case geomKindLseg:
			ps, err := decodeGeomVec2s(dec)
			if err != nil || len(ps) != 2 {
				return fmt.Errorf("PostgresV3Cell.GobDecode lseg Vec2: %v (len=%d)", err, len(ps))
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode lseg Valid: %w", err)
			}
			c.Value = pgtype.Lseg{P: [2]pgtype.Vec2{ps[0], ps[1]}, Valid: valid}
		case geomKindBox:
			ps, err := decodeGeomVec2s(dec)
			if err != nil || len(ps) != 2 {
				return fmt.Errorf("PostgresV3Cell.GobDecode box Vec2: %v (len=%d)", err, len(ps))
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode box Valid: %w", err)
			}
			c.Value = pgtype.Box{P: [2]pgtype.Vec2{ps[0], ps[1]}, Valid: valid}
		case geomKindPath:
			ps, err := decodeGeomVec2s(dec)
			if err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode path Vec2s: %w", err)
			}
			var closed, valid bool
			if err := dec.Decode(&closed); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode path Closed: %w", err)
			}
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode path Valid: %w", err)
			}
			c.Value = pgtype.Path{P: ps, Closed: closed, Valid: valid}
		case geomKindPolygon:
			ps, err := decodeGeomVec2s(dec)
			if err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode polygon Vec2s: %w", err)
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode polygon Valid: %w", err)
			}
			c.Value = pgtype.Polygon{P: ps, Valid: valid}
		case geomKindCircle:
			ps, err := decodeGeomVec2s(dec)
			if err != nil || len(ps) != 1 {
				return fmt.Errorf("PostgresV3Cell.GobDecode circle Vec2: %v (len=%d)", err, len(ps))
			}
			var r float64
			if err := dec.Decode(&r); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode circle R: %w", err)
			}
			var valid bool
			if err := dec.Decode(&valid); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode circle Valid: %w", err)
			}
			c.Value = pgtype.Circle{P: ps[0], R: r, Valid: valid}
		default:
			return fmt.Errorf("PostgresV3Cell.GobDecode geom: unknown kind %d", kind)
		}
	case cellTagTID:
		var tid pgtype.TID
		if err := dec.Decode(&tid.BlockNumber); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode tid BlockNumber: %w", err)
		}
		if err := dec.Decode(&tid.OffsetNumber); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode tid OffsetNumber: %w", err)
		}
		if err := dec.Decode(&tid.Valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode tid Valid: %w", err)
		}
		c.Value = tid
	case cellTagTSVector:
		var n int32
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode tsvector length: %w", err)
		}
		// Bounds-check before allocating: gob input is untrusted (see
		// the cellTagSlice arm below for the full threat-model note).
		// A negative or absurd length must not drive a runtime panic
		// in make() or an unbounded allocation. Symmetric guard.
		if n < 0 {
			return fmt.Errorf("PostgresV3Cell.GobDecode tsvector length: invalid negative length %d", n)
		}
		lexemes := make([]pgtype.TSVectorLexeme, n)
		for i := int32(0); i < n; i++ {
			if err := dec.Decode(&lexemes[i].Word); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode tsvector lex %d word: %w", i, err)
			}
			var pn int32
			if err := dec.Decode(&pn); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode tsvector lex %d pos count: %w", i, err)
			}
			if pn < 0 {
				return fmt.Errorf("PostgresV3Cell.GobDecode tsvector lex %d pos count: invalid negative length %d", i, pn)
			}
			poss := make([]pgtype.TSVectorPosition, pn)
			for j := int32(0); j < pn; j++ {
				if err := dec.Decode(&poss[j].Position); err != nil {
					return fmt.Errorf("PostgresV3Cell.GobDecode tsvector lex %d pos %d Position: %w", i, j, err)
				}
				var w byte
				if err := dec.Decode(&w); err != nil {
					return fmt.Errorf("PostgresV3Cell.GobDecode tsvector lex %d pos %d Weight: %w", i, j, err)
				}
				poss[j].Weight = pgtype.TSVectorWeight(w)
			}
			lexemes[i].Positions = poss
		}
		var valid bool
		if err := dec.Decode(&valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode tsvector Valid: %w", err)
		}
		c.Value = pgtype.TSVector{Lexemes: lexemes, Valid: valid}
	case cellTagNetipPrefix:
		var s string
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode netip.Prefix: %w", err)
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode netip.Prefix parse %q: %w", s, err)
		}
		c.Value = p
	case cellTagMACAddr:
		var b []byte
		if err := dec.Decode(&b); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode net.HardwareAddr: %w", err)
		}
		c.Value = net.HardwareAddr(b)
	case cellTagHstore:
		var n int32
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode hstore length: %w", err)
		}
		// Bounds-check before allocating: gob input is untrusted (see
		// the cellTagSlice arm below for the full threat-model note).
		if n < 0 {
			return fmt.Errorf("PostgresV3Cell.GobDecode hstore length: invalid negative length %d", n)
		}
		out := make(pgtype.Hstore, n)
		for i := int32(0); i < n; i++ {
			var k string
			if err := dec.Decode(&k); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode hstore key %d: %w", i, err)
			}
			var isNil bool
			if err := dec.Decode(&isNil); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode hstore nil-flag for %q: %w", k, err)
			}
			if isNil {
				out[k] = nil
				continue
			}
			var val string
			if err := dec.Decode(&val); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode hstore value for %q: %w", k, err)
			}
			out[k] = &val
		}
		c.Value = out
	case cellTagRange:
		var lowerBytes, upperBytes []byte
		if err := dec.Decode(&lowerBytes); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range lower bytes: %w", err)
		}
		if err := dec.Decode(&upperBytes); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range upper bytes: %w", err)
		}
		var lt, ut byte
		if err := dec.Decode(&lt); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range LowerType: %w", err)
		}
		if err := dec.Decode(&ut); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range UpperType: %w", err)
		}
		var valid bool
		if err := dec.Decode(&valid); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range Valid: %w", err)
		}
		var lower, upper PostgresV3Cell
		if err := lower.GobDecode(lowerBytes); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range lower cell: %w", err)
		}
		if err := upper.GobDecode(upperBytes); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode range upper cell: %w", err)
		}
		c.Value = pgtype.Range[any]{
			Lower:     lower.Value,
			Upper:     upper.Value,
			LowerType: pgtype.BoundType(lt),
			UpperType: pgtype.BoundType(ut),
			Valid:     valid,
		}
	case cellTagMultirange:
		var n int32
		if err := dec.Decode(&n); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode multirange length: %w", err)
		}
		// Bounds-check before allocating: gob input is untrusted (see
		// the cellTagSlice arm below for the full threat-model note).
		if n < 0 {
			return fmt.Errorf("PostgresV3Cell.GobDecode multirange length: invalid negative length %d", n)
		}
		out := make(pgtype.Multirange[pgtype.Range[any]], n)
		for i := int32(0); i < n; i++ {
			var rangeBytes []byte
			if err := dec.Decode(&rangeBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode multirange[%d] bytes: %w", i, err)
			}
			var rcell PostgresV3Cell
			if err := rcell.GobDecode(rangeBytes); err != nil {
				return fmt.Errorf("PostgresV3Cell.GobDecode multirange[%d] cell: %w", i, err)
			}
			r, ok := rcell.Value.(pgtype.Range[any])
			if !ok {
				return fmt.Errorf("PostgresV3Cell.GobDecode multirange[%d]: nested cell is %T, want pgtype.Range[any]", i, rcell.Value)
			}
			out[i] = r
		}
		c.Value = out
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
		// Bounds-check before allocating: gob input is untrusted (see
		// the cellTagSlice arm below for the full threat-model note).
		// A negative length on a map make() doesn't itself panic, but
		// we still reject it eagerly so the loop's `for i < n` doesn't
		// go negative-iterations and surface the corruption later as
		// a confusing nested decode error.
		if n < 0 {
			return fmt.Errorf("PostgresV3Cell.GobDecode jsonb length: invalid negative length %d", n)
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
		// Bounds-check before allocating: gob input is untrusted (it
		// arrives over the recorder→agent socket and from on-disk
		// mocks that may have been hand-edited), so a malformed
		// length must not be allowed to drive a 2-GiB allocation or
		// a panic via make's runtime length check.
		if n < 0 {
			return fmt.Errorf("PostgresV3Cell.GobDecode slice length: invalid negative length %d", n)
		}
		out := make([]interface{}, int(n))
		for i := 0; i < int(n); i++ {
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
	case cellTagUUIDBytes:
		// Symmetric to the [16]uint8 GobEncode case: gob-decode the
		// fixed-array shape pgx originally produced. We decode into a
		// [16]uint8 (not uuid.UUID) so the models package stays free
		// of a uuid dependency; callers that want uuid.UUID can
		// convert with `uuid.UUID(arr)`.
		var arr [16]uint8
		if err := dec.Decode(&arr); err != nil {
			return fmt.Errorf("PostgresV3Cell.GobDecode uuidBytes: %w", err)
		}
		c.Value = arr
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
	// node==nil is a distinct failure mode from "wrong kind": the
	// previous combined guard would dereference node.Kind in the
	// error formatter and panic. Split them so a missing/empty
	// node surfaces a descriptive error instead of crashing the
	// loader on hand-edited YAML.
	if node == nil {
		return fmt.Errorf("PostgresV3Cells: expected sequence, got nil node")
	}
	if node.Kind != yaml.SequenceNode {
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
