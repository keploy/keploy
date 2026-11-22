package models

import (
	"net/netip"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"
	yaml "gopkg.in/yaml.v3"
)

// A cell's YAML holds no type. A numeric, interval, time, geometric value,
// tid, tsvector or range is written as a mapping of its fields, and an
// inet or cidr inside an array as its text. The cell's own decode reads a
// mapping back by its keys alone (decodePgUntaggedMapping), whatever its
// column's type; an element of an array, and a multirange's range, it
// leaves as yaml.v3's generic decode makes it, since a json or jsonb array
// of the application's objects with the same keys is written the same way
// and must read back as those objects. Such an element is then a
// map[string]any of keploy's field words (or a string), which no replay
// can encode for the column ("unable to encode map[...] into binary format
// for _numeric") and no reader can read as the value. The column's type,
// which the answer's RowDescription and a statement's ParamOIDs state,
// tells them apart: each is read back by it (pgTypedValue).

// UnmarshalYAML reads the answer as yaml.v3 does, then each row's cells by
// the type its RowDescription states for their column (pgTypedValue).
func (r *PostgresV3Response) UnmarshalYAML(node *yaml.Node) error {
	type plain PostgresV3Response
	if err := node.Decode((*plain)(r)); err != nil {
		return err
	}
	for _, row := range r.Rows {
		for i := range row {
			if i < len(r.RowDescription) {
				row[i].Value = pgTypedValue(r.RowDescription[i].TypeOID, row[i].Value)
			}
		}
	}
	return nil
}

// UnmarshalYAML reads the statement as yaml.v3 does, then each bound value
// a recording kept decoded by the type its ParamOIDs state (pgTypedValue).
func (q *PostgresV3QuerySpec) UnmarshalYAML(node *yaml.Node) error {
	type plain PostgresV3QuerySpec
	if err := node.Decode((*plain)(q)); err != nil {
		return err
	}
	for i := range q.BindValues {
		if i < len(q.ParamOIDs) {
			q.BindValues[i].Value = pgTypedValue(q.ParamOIDs[i], q.BindValues[i].Value)
		}
	}
	return nil
}

var (
	pgTypesOnce sync.Once
	pgTypes     *pgtype.Map
)

// pgTypedValue is v, a value of type oid as yaml.v3 decoded it, read back as
// the value pgx decodes a value of the type into, where keploy's YAML wrote
// it in a form of its own: each element of an array at any depth (a
// numeric[] as []any of pgtype.Numeric, an inet[] as []any of
// netip.Prefix), each range of a multirange (as a
// pgtype.Multirange[pgtype.Range[any]]), and a point, lseg, box or polygon
// cell, whose mapping's keys are shared, so its own decode leaves it a map.
// Anything else is v as it is: a value of another type (a json or jsonb
// document), a mapping that is not its type's shape or does not decode,
// and one already read as its value. v's slices are changed in place.
func pgTypedValue(oid uint32, v any) any {
	switch v.(type) {
	case []any, map[string]any:
	default:
		return v
	}
	pgTypesOnce.Do(func() { pgTypes = pgtype.NewMap() })
	return pgTypedAs(pgTypes, oid, v)
}

// pgTypedAs is pgTypedValue with the type map m.
func pgTypedAs(m *pgtype.Map, oid uint32, v any) any {
	t, ok := m.TypeForOID(oid)
	if !ok {
		return v
	}
	switch c := t.Codec.(type) {
	case *pgtype.ArrayCodec:
		s, ok := v.([]any)
		if !ok || c.ElementType == nil {
			return v
		}
		for i, e := range s {
			if _, sub := e.([]any); sub {
				// A sub-array of a multi-dimensional array.
				s[i] = pgTypedAs(m, oid, e)
				continue
			}
			s[i] = pgTypedAs(m, c.ElementType.OID, e)
		}
		return s
	case *pgtype.MultirangeCodec:
		s, ok := v.([]any)
		if !ok || c.ElementType == nil {
			return v
		}
		out := make(pgtype.Multirange[pgtype.Range[any]], 0, len(s))
		for _, e := range s {
			r, ok := pgTypedAs(m, c.ElementType.OID, e).(pgtype.Range[any])
			if !ok {
				return v
			}
			out = append(out, r)
		}
		return out
	case pgtype.InetCodec:
		if txt, ok := v.(string); ok {
			if p, err := netip.ParsePrefix(txt); err == nil {
				return p
			}
		}
		return v
	case *pgtype.RangeCodec:
		return pgShapeIs[pgtype.Range[any]](v)
	case pgtype.NumericCodec:
		return pgShapeIs[pgtype.Numeric](v)
	case pgtype.IntervalCodec:
		return pgShapeIs[pgtype.Interval](v)
	case pgtype.TimeCodec:
		return pgShapeIs[pgtype.Time](v)
	case pgtype.PathCodec:
		return pgShapeIs[pgtype.Path](v)
	case pgtype.LineCodec:
		return pgShapeIs[pgtype.Line](v)
	case pgtype.CircleCodec:
		return pgShapeIs[pgtype.Circle](v)
	case pgtype.TIDCodec:
		return pgShapeIs[pgtype.TID](v)
	case pgtype.TSVectorCodec:
		return pgShapeIs[pgtype.TSVector](v)
	case pgtype.PointCodec:
		return pgShapeBy(v, decodePgPointMapping)
	case pgtype.LsegCodec:
		return pgShapeBy(v, decodePgLsegMapping)
	case pgtype.BoxCodec:
		return pgShapeBy(v, decodePgBoxMapping)
	case pgtype.PolygonCodec:
		return pgShapeBy(v, decodePgPolygonMapping)
	}
	return v
}

// pgShapeIs is v, a map[string]any, read as the pgtype shape its keys are
// (decodePgUntaggedMapping) where that shape is a T, or else v.
func pgShapeIs[T any](v any) any {
	n := pgNodeOf(v)
	if n == nil {
		return v
	}
	if d, ok, err := decodePgUntaggedMapping(n); ok && err == nil {
		if x, ok := d.(T); ok {
			return x
		}
	}
	return v
}

// pgShapeBy is v, a map[string]any with exactly the keys a point, lseg, box
// and polygon are written with ({p, valid}), read by decode, or else v.
func pgShapeBy[T any](v any, decode func(*yaml.Node) (T, error)) any {
	n := pgNodeOf(v)
	if n == nil || !keysEqual(pgMappingKeySet(n), []string{"p", "valid"}) {
		return v
	}
	if d, err := decode(n); err == nil {
		return d
	}
	return v
}

// pgNodeOf is v, a map[string]any, as a YAML mapping node, or nil.
func pgNodeOf(v any) *yaml.Node {
	mp, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	var n yaml.Node
	if err := n.Encode(mp); err != nil {
		return nil
	}
	return &n
}
