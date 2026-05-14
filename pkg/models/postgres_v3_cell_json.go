package models

// JSON marshalling for PostgresV3Cell. Mirrors MarshalYAML / UnmarshalYAML
// in postgres_v3_cell.go but emits JSON. The two formats are independent
// (keploy reads back whatever it wrote — there is no cross-format mock
// compatibility constraint), so the JSON shape is chosen for clean
// round-trip rather than parallel byte equivalence with YAML.
//
// Why this exists: pre-fix, json.Marshal fell back to the default
// reflective struct encoder, which emitted every cell as `{"Value": x}`
// (the literal Go field name) instead of the unwrapped logical value.
// The postgres-v3 replay matcher reconstructs DataRow bytes by
// type-switching on Cell.Value, so a struct-wrapped cell decoded back
// as `map[string]any{"Value": ...}` produced empty rows from
// `INSERT … RETURNING *`, surfacing as `ArrayIndexOutOfBoundsException`
// in JDBC clients (pgjdbc's `ByteConverter.int8`). All 270 test-set-1
// failures on samples-java/spring-petclinic-rest fanned out from this
// single seam.
//
// Round-trip strategy:
//   * Unambiguous pgtype shapes (Numeric, Interval, Time, Bits, Line,
//     Path, Circle, TID, TSVector, Range, netip.Prefix, HardwareAddr,
//     PostgresV3CellRaw) use canonical-key-set probing — same dispatch
//     as decodePgUntaggedMapping in the YAML path.
//   * Ambiguous shapes (Point/Lseg/Box/Polygon all match `{p, valid}`;
//     Hstore is open-keyed; Multirange is a sequence; binary and
//     timestamp scalars are indistinguishable from string in JSON) carry
//     a `$pgtype` discriminator so the decoder can route without a tag
//     system. The discriminator is keploy-internal and not visible in
//     YAML output.
//   * json.Decoder.UseNumber preserves int-vs-float identity through the
//     untyped parse — without it every JSON number returns as float64
//     and integer cells lose their Go-type identity.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// jsonPgTypeKey is the discriminator field for ambiguous shapes whose
// canonical-key set isn't uniquely identifying on its own. The leading
// dollar sign keeps it lexically distinct from any pgtype field name
// (none of which use `$` in YAML's emitted form either).
const jsonPgTypeKey = "$pgtype"

// jsonPgType discriminator values. These are keploy-internal — only the
// JSON path emits / consumes them — so they don't have a YAML twin.
const (
	jsonPgTypeBinary     = "binary"
	jsonPgTypeTimestamp  = "timestamp"
	jsonPgTypePoint      = "point"
	jsonPgTypeLseg       = "lseg"
	jsonPgTypeBox        = "box"
	jsonPgTypePolygon    = "polygon"
	jsonPgTypeHstore     = "hstore"
	jsonPgTypeMultirange = "multirange"
)

// MarshalJSON dispatches by the cell's logical value type, building a
// json-friendly Go value (map[string]any / []any / scalar) and handing
// it to encoding/json. The output for unambiguous pgtype shapes mirrors
// YAML's canonical-key mapping; ambiguous shapes carry a `$pgtype`
// discriminator (see file-level docstring).
func (c PostgresV3Cell) MarshalJSON() ([]byte, error) {
	return json.Marshal(cellValueToJSON(c.Value))
}

// cellValueToJSON converts an arbitrary cell value into a Go value
// whose json.Marshal output is the canonical JSON shape for that
// pgtype. Recursive in pgtype.Range / Multirange so nested cells go
// through the same dispatch.
func cellValueToJSON(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		// Top-level binary cell. JSON's reflective default for []byte
		// is a base64 string indistinguishable from a regular string,
		// so wrap with a discriminator that survives the round trip.
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeBinary,
			"data":        base64.StdEncoding.EncodeToString(x),
		}
	case time.Time:
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeTimestamp,
			"value":       x.Format(time.RFC3339Nano),
		}
	case pgtype.Numeric:
		intStr := ""
		if x.Int != nil {
			intStr = x.Int.String()
		}
		return map[string]any{
			"int":              intStr,
			"exp":              int64(x.Exp),
			"nan":              x.NaN,
			"infinitymodifier": int64(x.InfinityModifier),
			"valid":            x.Valid,
		}
	case pgtype.Interval:
		return map[string]any{
			"microseconds": x.Microseconds,
			"days":         int64(x.Days),
			"months":       int64(x.Months),
			"valid":        x.Valid,
		}
	case pgtype.Time:
		return map[string]any{
			"microseconds": x.Microseconds,
			"valid":        x.Valid,
		}
	case pgtype.Bits:
		return map[string]any{
			"bytes": base64.StdEncoding.EncodeToString(x.Bytes),
			"len":   int64(x.Len),
			"valid": x.Valid,
		}
	case pgtype.Point:
		return map[string]any{
			jsonPgTypeKey: jsonPgTypePoint,
			"p":           vec2ToJSON(x.P),
			"valid":       x.Valid,
		}
	case pgtype.Line:
		return map[string]any{
			"a":     x.A,
			"b":     x.B,
			"c":     x.C,
			"valid": x.Valid,
		}
	case pgtype.Lseg:
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeLseg,
			"p":           vec2SeqToJSON(x.P[:]),
			"valid":       x.Valid,
		}
	case pgtype.Box:
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeBox,
			"p":           vec2SeqToJSON(x.P[:]),
			"valid":       x.Valid,
		}
	case pgtype.Path:
		return map[string]any{
			"p":      vec2SeqToJSON(x.P),
			"closed": x.Closed,
			"valid":  x.Valid,
		}
	case pgtype.Polygon:
		return map[string]any{
			jsonPgTypeKey: jsonPgTypePolygon,
			"p":           vec2SeqToJSON(x.P),
			"valid":       x.Valid,
		}
	case pgtype.Circle:
		return map[string]any{
			"p":     vec2ToJSON(x.P),
			"r":     x.R,
			"valid": x.Valid,
		}
	case pgtype.TID:
		return map[string]any{
			"blocknumber":  int64(x.BlockNumber),
			"offsetnumber": int64(x.OffsetNumber),
			"valid":        x.Valid,
		}
	case pgtype.TSVector:
		lex := make([]map[string]any, 0, len(x.Lexemes))
		for _, l := range x.Lexemes {
			pos := make([]map[string]any, 0, len(l.Positions))
			for _, p := range l.Positions {
				pos = append(pos, map[string]any{
					"position": int64(p.Position),
					"weight":   int64(p.Weight),
				})
			}
			lex = append(lex, map[string]any{
				"word":      l.Word,
				"positions": pos,
			})
		}
		return map[string]any{
			"lexemes": lex,
			"valid":   x.Valid,
		}
	case pgtype.Hstore:
		// Hstore keys are open-set so the canonical-key probe can't
		// distinguish it from a generic JSON object. Wrap with the
		// discriminator and preserve the `*string` nil-vs-empty
		// distinction (nil → JSON null, "" → JSON empty string).
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		inner := make(map[string]any, len(keys))
		for _, k := range keys {
			val := x[k]
			if val == nil {
				inner[k] = nil
			} else {
				inner[k] = *val
			}
		}
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeHstore,
			"values":      inner,
		}
	case pgtype.Range[any]:
		return map[string]any{
			"lower":     cellValueToJSON(x.Lower),
			"upper":     cellValueToJSON(x.Upper),
			"lowertype": int64(x.LowerType),
			"uppertype": int64(x.UpperType),
			"valid":     x.Valid,
		}
	case pgtype.Multirange[pgtype.Range[any]]:
		// Bare JSON arrays can't be distinguished from generic []any
		// cells, so the multirange carries the same `$pgtype`
		// discriminator wrapper as Hstore.
		ranges := make([]any, 0, len(x))
		for _, r := range x {
			ranges = append(ranges, cellValueToJSON(r))
		}
		return map[string]any{
			jsonPgTypeKey: jsonPgTypeMultirange,
			"values":      ranges,
		}
	case netip.Prefix:
		return map[string]any{"prefix": x.String()}
	case net.HardwareAddr:
		// Macaddr nests the binary discriminator so the bytes round-
		// trip via the same $pgtype:binary envelope as a top-level
		// []byte cell. Keeps decode logic symmetric.
		return map[string]any{
			"macaddr": map[string]any{
				jsonPgTypeKey: jsonPgTypeBinary,
				"data":        base64.StdEncoding.EncodeToString(x),
			},
		}
	case PostgresV3CellRaw:
		return map[string]any{
			"format": int64(x.Format),
			"bytes":  base64.StdEncoding.EncodeToString(x.Bytes),
		}
	}
	// Default: emit value as-is. Numbers, strings, bools, generic
	// jsonb-shaped maps/slices all flow through encoding/json's
	// reflective path correctly.
	return v
}

func vec2ToJSON(v pgtype.Vec2) map[string]any {
	return map[string]any{"x": v.X, "y": v.Y}
}

func vec2SeqToJSON(ps []pgtype.Vec2) []any {
	out := make([]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, vec2ToJSON(p))
	}
	return out
}

// UnmarshalJSON parses the JSON form back into the cell's logical
// Go-typed Value. Mirrors UnmarshalYAML's type-recovery shape: scalars
// map directly, mappings dispatch via the canonical-key-set probe (or
// the `$pgtype` discriminator for ambiguous shapes), arrays come back
// as []any with each element recursively recovered.
func (c *PostgresV3Cell) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return fmt.Errorf("PostgresV3Cell: decode JSON: %w", err)
	}
	v, err := recoverCellFromJSON(raw)
	if err != nil {
		return err
	}
	c.Value = v
	return nil
}

// recoverCellFromJSON walks a parsed JSON value and reconstructs the
// Go-typed cell value.
func recoverCellFromJSON(raw any) (any, error) {
	switch x := raw.(type) {
	case nil:
		return nil, nil
	case bool:
		return x, nil
	case json.Number:
		// Prefer int64 — yaml.v3 widens every !!int to int64 through
		// the untyped decode path, so the JSON path matches that
		// width to keep cohort matching's type-switch consistent.
		// Fall through to float64 only if the literal carries a
		// fraction or overflows int64.
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("PostgresV3Cell: invalid number %q: %w", string(x), err)
		}
		return f, nil
	case string:
		if x == legacyNullSentinel {
			return nil, nil
		}
		return x, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := recoverCellFromJSON(e)
			if err != nil {
				return nil, fmt.Errorf("PostgresV3Cell: array[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		return recoverPgMappingFromJSON(x)
	}
	return nil, fmt.Errorf("PostgresV3Cell: unexpected JSON type %T", raw)
}

// recoverPgMappingFromJSON probes a parsed JSON object for the
// `$pgtype` discriminator first (ambiguous shapes), then falls through
// to canonical-key-set probing for unambiguous shapes. Unknown shapes
// pass through as a generic map[string]any so jsonb-shaped payloads
// keep their structure.
func recoverPgMappingFromJSON(m map[string]any) (any, error) {
	if disc, ok := m[jsonPgTypeKey].(string); ok {
		return decodeJSONByDiscriminator(disc, m)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch {
	case len(keys) == 1 && keys[0] == "prefix":
		return decodeJSONPrefix(m["prefix"])
	case len(keys) == 1 && keys[0] == "macaddr":
		return decodeJSONHWAddr(m["macaddr"])
	case keysEq(keys, []string{"exp", "infinitymodifier", "int", "nan", "valid"}):
		return numericFromJSON(m)
	case keysEq(keys, []string{"days", "microseconds", "months", "valid"}):
		return intervalFromJSON(m)
	case keysEq(keys, []string{"microseconds", "valid"}):
		return timeFromJSON(m)
	case keysEq(keys, []string{"bytes", "len", "valid"}):
		return bitsFromJSON(m)
	case keysEq(keys, []string{"a", "b", "c", "valid"}):
		return lineFromJSON(m)
	case keysEq(keys, []string{"closed", "p", "valid"}):
		return pathFromJSON(m)
	case keysEq(keys, []string{"p", "r", "valid"}):
		return circleFromJSON(m)
	case keysEq(keys, []string{"blocknumber", "offsetnumber", "valid"}):
		return tidFromJSON(m)
	case keysEq(keys, []string{"lexemes", "valid"}):
		return tsvectorFromJSON(m)
	case keysEq(keys, []string{"lower", "lowertype", "upper", "uppertype", "valid"}):
		return rangeFromJSON(m)
	case keysEq(keys, []string{"bytes", "format"}):
		return rawCellFromJSON(m)
	}
	// Fall through: generic JSON object (jsonb top-level, ad-hoc map).
	// Walk the values recursively so nested numbers convert from
	// json.Number to int64/float64 — without this, downstream gob
	// transport (PostgresV3Cell.GobEncode) errors out on the
	// json.Number type with `unsupported Value type json.Number` for
	// any jsonb cell that contains an integer leaf. Mirrors the array
	// recursion in recoverCellFromJSON.
	out := make(map[string]any, len(m))
	for k, v := range m {
		r, err := recoverCellFromJSON(v)
		if err != nil {
			return nil, fmt.Errorf("jsonb[%q]: %w", k, err)
		}
		out[k] = r
	}
	return out, nil
}

func decodeJSONByDiscriminator(disc string, m map[string]any) (any, error) {
	switch disc {
	case jsonPgTypeBinary:
		return decodeJSONBinary(m["data"])
	case jsonPgTypeTimestamp:
		return decodeJSONTimestamp(m["value"])
	case jsonPgTypePoint:
		return pointFromJSON(m)
	case jsonPgTypeLseg:
		return lsegFromJSON(m)
	case jsonPgTypeBox:
		return boxFromJSON(m)
	case jsonPgTypePolygon:
		return polygonFromJSON(m)
	case jsonPgTypeHstore:
		return hstoreFromJSON(m)
	case jsonPgTypeMultirange:
		return multirangeFromJSON(m)
	}
	return nil, fmt.Errorf("PostgresV3Cell: unknown $pgtype %q", disc)
}

func keysEq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// ===== Per-pgtype JSON reconstructors =====

func numericFromJSON(m map[string]any) (pgtype.Numeric, error) {
	out := pgtype.Numeric{}
	if v, ok := m["int"]; ok && v != nil {
		s, err := asString(v)
		if err != nil {
			return out, fmt.Errorf("pg/numeric int: %w", err)
		}
		if s != "" {
			bi := new(big.Int)
			if _, ok := bi.SetString(s, 10); !ok {
				return out, fmt.Errorf("pg/numeric int: invalid integer literal %q", s)
			}
			out.Int = bi
		}
	}
	if v, ok := m["exp"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/numeric exp: %w", err)
		}
		out.Exp = int32(n)
	}
	if v, ok := m["nan"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/numeric nan: %w", err)
		}
		out.NaN = b
	}
	if v, ok := m["infinitymodifier"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/numeric infinitymodifier: %w", err)
		}
		out.InfinityModifier = pgtype.InfinityModifier(n)
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/numeric valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func intervalFromJSON(m map[string]any) (pgtype.Interval, error) {
	out := pgtype.Interval{}
	if v, ok := m["microseconds"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/interval microseconds: %w", err)
		}
		out.Microseconds = n
	}
	if v, ok := m["days"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/interval days: %w", err)
		}
		out.Days = int32(n)
	}
	if v, ok := m["months"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/interval months: %w", err)
		}
		out.Months = int32(n)
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/interval valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func timeFromJSON(m map[string]any) (pgtype.Time, error) {
	out := pgtype.Time{}
	if v, ok := m["microseconds"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/time microseconds: %w", err)
		}
		out.Microseconds = n
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/time valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func bitsFromJSON(m map[string]any) (pgtype.Bits, error) {
	out := pgtype.Bits{}
	if v, ok := m["bytes"]; ok && v != nil {
		switch t := v.(type) {
		case string:
			b, err := base64.StdEncoding.DecodeString(t)
			if err != nil {
				return out, fmt.Errorf("pg/bits bytes: %w", err)
			}
			out.Bytes = b
		case []any:
			// Backward-compat with hand-edited fixtures that wrote
			// bytes as a sequence of small ints (mirrors the
			// equivalent fall-through in decodePgBitsMapping).
			b := make([]byte, 0, len(t))
			for i, e := range t {
				n, err := asInt64(e)
				if err != nil {
					return out, fmt.Errorf("pg/bits bytes[%d]: %w", i, err)
				}
				b = append(b, byte(n))
			}
			out.Bytes = b
		default:
			return out, fmt.Errorf("pg/bits bytes: unexpected JSON type %T", v)
		}
	}
	if v, ok := m["len"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/bits len: %w", err)
		}
		out.Len = int32(n)
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/bits valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func vec2FromJSON(v any) (pgtype.Vec2, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return pgtype.Vec2{}, fmt.Errorf("pg vec2: expected object, got %T", v)
	}
	out := pgtype.Vec2{}
	if x, ok := m["x"]; ok && x != nil {
		f, err := asFloat64(x)
		if err != nil {
			return out, fmt.Errorf("pg vec2 x: %w", err)
		}
		out.X = f
	}
	if y, ok := m["y"]; ok && y != nil {
		f, err := asFloat64(y)
		if err != nil {
			return out, fmt.Errorf("pg vec2 y: %w", err)
		}
		out.Y = f
	}
	return out, nil
}

func vec2SeqFromJSON(v any) ([]pgtype.Vec2, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("pg vec2 seq: expected array, got %T", v)
	}
	out := make([]pgtype.Vec2, 0, len(arr))
	for i, e := range arr {
		p, err := vec2FromJSON(e)
		if err != nil {
			return nil, fmt.Errorf("pg vec2 seq[%d]: %w", i, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func pointFromJSON(m map[string]any) (pgtype.Point, error) {
	out := pgtype.Point{}
	if v, ok := m["p"]; ok && v != nil {
		p, err := vec2FromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/point p: %w", err)
		}
		out.P = p
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/point valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func lsegFromJSON(m map[string]any) (pgtype.Lseg, error) {
	out := pgtype.Lseg{}
	if v, ok := m["p"]; ok && v != nil {
		ps, err := vec2SeqFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/lseg p: %w", err)
		}
		if len(ps) != 2 {
			return out, fmt.Errorf("pg/lseg p: expected 2 vec2, got %d", len(ps))
		}
		out.P = [2]pgtype.Vec2{ps[0], ps[1]}
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/lseg valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func boxFromJSON(m map[string]any) (pgtype.Box, error) {
	out := pgtype.Box{}
	if v, ok := m["p"]; ok && v != nil {
		ps, err := vec2SeqFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/box p: %w", err)
		}
		if len(ps) != 2 {
			return out, fmt.Errorf("pg/box p: expected 2 vec2, got %d", len(ps))
		}
		out.P = [2]pgtype.Vec2{ps[0], ps[1]}
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/box valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func lineFromJSON(m map[string]any) (pgtype.Line, error) {
	out := pgtype.Line{}
	for _, k := range []string{"a", "b", "c"} {
		if v, ok := m[k]; ok && v != nil {
			f, err := asFloat64(v)
			if err != nil {
				return out, fmt.Errorf("pg/line %s: %w", k, err)
			}
			switch k {
			case "a":
				out.A = f
			case "b":
				out.B = f
			case "c":
				out.C = f
			}
		}
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/line valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func pathFromJSON(m map[string]any) (pgtype.Path, error) {
	out := pgtype.Path{}
	if v, ok := m["p"]; ok && v != nil {
		ps, err := vec2SeqFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/path p: %w", err)
		}
		out.P = ps
	}
	if v, ok := m["closed"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/path closed: %w", err)
		}
		out.Closed = b
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/path valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func polygonFromJSON(m map[string]any) (pgtype.Polygon, error) {
	out := pgtype.Polygon{}
	if v, ok := m["p"]; ok && v != nil {
		ps, err := vec2SeqFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/polygon p: %w", err)
		}
		out.P = ps
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/polygon valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func circleFromJSON(m map[string]any) (pgtype.Circle, error) {
	out := pgtype.Circle{}
	if v, ok := m["p"]; ok && v != nil {
		p, err := vec2FromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/circle p: %w", err)
		}
		out.P = p
	}
	if v, ok := m["r"]; ok && v != nil {
		f, err := asFloat64(v)
		if err != nil {
			return out, fmt.Errorf("pg/circle r: %w", err)
		}
		out.R = f
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/circle valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func tidFromJSON(m map[string]any) (pgtype.TID, error) {
	out := pgtype.TID{}
	if v, ok := m["blocknumber"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/tid blocknumber: %w", err)
		}
		out.BlockNumber = uint32(n)
	}
	if v, ok := m["offsetnumber"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/tid offsetnumber: %w", err)
		}
		out.OffsetNumber = uint16(n)
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/tid valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func tsvectorFromJSON(m map[string]any) (pgtype.TSVector, error) {
	out := pgtype.TSVector{}
	if v, ok := m["lexemes"]; ok && v != nil {
		arr, ok := v.([]any)
		if !ok {
			return out, fmt.Errorf("pg/tsvector lexemes: expected array, got %T", v)
		}
		out.Lexemes = make([]pgtype.TSVectorLexeme, 0, len(arr))
		for i, e := range arr {
			lm, ok := e.(map[string]any)
			if !ok {
				return out, fmt.Errorf("pg/tsvector lexemes[%d]: expected object, got %T", i, e)
			}
			lex := pgtype.TSVectorLexeme{}
			if w, ok := lm["word"]; ok && w != nil {
				s, err := asString(w)
				if err != nil {
					return out, fmt.Errorf("pg/tsvector lexemes[%d] word: %w", i, err)
				}
				lex.Word = s
			}
			if pv, ok := lm["positions"]; ok && pv != nil {
				parr, ok := pv.([]any)
				if !ok {
					return out, fmt.Errorf("pg/tsvector lexemes[%d] positions: expected array, got %T", i, pv)
				}
				lex.Positions = make([]pgtype.TSVectorPosition, 0, len(parr))
				for j, pe := range parr {
					pm, ok := pe.(map[string]any)
					if !ok {
						return out, fmt.Errorf("pg/tsvector lexemes[%d] positions[%d]: expected object, got %T", i, j, pe)
					}
					pos := pgtype.TSVectorPosition{}
					if pn, ok := pm["position"]; ok && pn != nil {
						n, err := asInt64(pn)
						if err != nil {
							return out, fmt.Errorf("pg/tsvector lexemes[%d] positions[%d] position: %w", i, j, err)
						}
						pos.Position = uint16(n)
					}
					if wn, ok := pm["weight"]; ok && wn != nil {
						n, err := asInt64(wn)
						if err != nil {
							return out, fmt.Errorf("pg/tsvector lexemes[%d] positions[%d] weight: %w", i, j, err)
						}
						pos.Weight = pgtype.TSVectorWeight(n)
					}
					lex.Positions = append(lex.Positions, pos)
				}
			}
			out.Lexemes = append(out.Lexemes, lex)
		}
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/tsvector valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func hstoreFromJSON(m map[string]any) (pgtype.Hstore, error) {
	v, ok := m["values"]
	if !ok || v == nil {
		return pgtype.Hstore{}, nil
	}
	inner, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pg/hstore values: expected object, got %T", v)
	}
	out := make(pgtype.Hstore, len(inner))
	for k, val := range inner {
		if val == nil {
			out[k] = nil
			continue
		}
		s, err := asString(val)
		if err != nil {
			return nil, fmt.Errorf("pg/hstore[%q]: %w", k, err)
		}
		ss := s
		out[k] = &ss
	}
	return out, nil
}

func rangeFromJSON(m map[string]any) (pgtype.Range[any], error) {
	out := pgtype.Range[any]{}
	if v, ok := m["lower"]; ok {
		rec, err := recoverCellFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/range lower: %w", err)
		}
		out.Lower = rec
	}
	if v, ok := m["upper"]; ok {
		rec, err := recoverCellFromJSON(v)
		if err != nil {
			return out, fmt.Errorf("pg/range upper: %w", err)
		}
		out.Upper = rec
	}
	if v, ok := m["lowertype"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/range lowertype: %w", err)
		}
		out.LowerType = pgtype.BoundType(n)
	}
	if v, ok := m["uppertype"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/range uppertype: %w", err)
		}
		out.UpperType = pgtype.BoundType(n)
	}
	if v, ok := m["valid"]; ok && v != nil {
		b, err := asBool(v)
		if err != nil {
			return out, fmt.Errorf("pg/range valid: %w", err)
		}
		out.Valid = b
	}
	return out, nil
}

func multirangeFromJSON(m map[string]any) (pgtype.Multirange[pgtype.Range[any]], error) {
	v, ok := m["values"]
	if !ok || v == nil {
		return pgtype.Multirange[pgtype.Range[any]]{}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("pg/multirange values: expected array, got %T", v)
	}
	out := make(pgtype.Multirange[pgtype.Range[any]], 0, len(arr))
	for i, e := range arr {
		em, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pg/multirange[%d]: expected object, got %T", i, e)
		}
		r, err := rangeFromJSON(em)
		if err != nil {
			return nil, fmt.Errorf("pg/multirange[%d]: %w", i, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func decodeJSONBinary(v any) ([]byte, error) {
	s, err := asString(v)
	if err != nil {
		return nil, fmt.Errorf("$pgtype binary: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("$pgtype binary: %w", err)
	}
	return b, nil
}

func decodeJSONTimestamp(v any) (time.Time, error) {
	s, err := asString(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("$pgtype timestamp: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("$pgtype timestamp: %w", err)
	}
	return t, nil
}

func decodeJSONPrefix(v any) (netip.Prefix, error) {
	s, err := asString(v)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: %w", err)
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("pg/prefix: %w", err)
	}
	return p, nil
}

func decodeJSONHWAddr(v any) (net.HardwareAddr, error) {
	switch t := v.(type) {
	case map[string]any:
		// Discriminator-wrapped: {$pgtype: binary, data: <base64>}.
		if disc, ok := t[jsonPgTypeKey].(string); ok && disc == jsonPgTypeBinary {
			return decodeJSONBinary(t["data"])
		}
		return nil, fmt.Errorf("pg/macaddr: expected {$pgtype: binary, data: ...} envelope")
	case string:
		// Bare base64 string — accepted for hand-edited fixtures.
		b, err := base64.StdEncoding.DecodeString(t)
		if err != nil {
			return nil, fmt.Errorf("pg/macaddr: %w", err)
		}
		return net.HardwareAddr(b), nil
	}
	return nil, fmt.Errorf("pg/macaddr: unexpected JSON type %T", v)
}

func rawCellFromJSON(m map[string]any) (PostgresV3CellRaw, error) {
	out := PostgresV3CellRaw{}
	if v, ok := m["format"]; ok && v != nil {
		n, err := asInt64(v)
		if err != nil {
			return out, fmt.Errorf("pg/raw format: %w", err)
		}
		out.Format = int16(n)
	}
	if v, ok := m["bytes"]; ok && v != nil {
		s, err := asString(v)
		if err != nil {
			return out, fmt.Errorf("pg/raw bytes: %w", err)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return out, fmt.Errorf("pg/raw bytes: %w", err)
		}
		out.Bytes = b
	}
	return out, nil
}

// ===== JSON-side primitive extractors =====

// asInt64 handles every shape a parsed JSON number can take. With
// json.Decoder.UseNumber the wire scalar arrives as json.Number, but
// hand-built map[string]any fixtures from Go code may carry int / int32
// / int64 / float64 directly.
func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case json.Number:
		return t.Int64()
	case float64:
		return int64(t), nil
	case float32:
		return int64(t), nil
	case int:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case int64:
		return t, nil
	case uint:
		return int64(t), nil
	case uint8:
		return int64(t), nil
	case uint16:
		return int64(t), nil
	case uint32:
		return int64(t), nil
	case uint64:
		return int64(t), nil
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid int64 string %q: %w", t, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("expected number, got %T", v)
}

func asFloat64(v any) (float64, error) {
	switch t := v.(type) {
	case json.Number:
		return t.Float64()
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	}
	return 0, fmt.Errorf("expected number, got %T", v)
}

func asBool(v any) (bool, error) {
	if b, ok := v.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("expected bool, got %T", v)
}

func asString(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	return "", fmt.Errorf("expected string, got %T", v)
}
