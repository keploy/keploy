package models

import (
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	yaml "gopkg.in/yaml.v3"
)

// pgxText is txt, a value of type oid in text, as pgx decodes it at record
// time.
func pgxText(t *testing.T, oid uint32, txt string) (any, error) {
	t.Helper()
	m := pgtype.NewMap()
	typ, ok := m.TypeForOID(oid)
	if !ok {
		t.Fatalf("no type %d", oid)
	}
	return typ.Codec.DecodeValue(m, oid, pgtype.TextFormatCode, []byte(txt))
}

// An array column's elements (at any depth: numeric, interval, time, range,
// point, lseg, box, polygon, path, line, circle, tid, tsvector, inet and
// cidr), a range column's bounds, a multirange column's ranges and a
// point column come back from mocks.yaml as the pgtype values pgx decoded
// them into at record time, by the column's type: as mappings of keploy's
// field words (or strings), no replay could encode them for the column
// ("unable to encode map[...] into binary format for _numeric") and no
// reader could read them as the values. A statement's bound values a
// recording kept decoded likewise, by ParamOIDs.
func TestAPostgresAnswersArraysAndRangesComeBackFromYAMLByTheirColumnsType(t *testing.T) {
	from := time.Date(2026, 10, 24, 10, 30, 45, 0, time.UTC)
	num := pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}
	nan := pgtype.Numeric{NaN: true, Valid: true}
	span := pgtype.Range[any]{Lower: from, Upper: from.Add(time.Hour), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}
	nums := pgtype.Range[any]{Lower: pgtype.Numeric{Int: big.NewInt(15), Exp: -1, Valid: true}, Upper: num, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}
	ints := pgtype.Range[any]{Lower: int64(3), Upper: int64(9), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}
	cols := []struct {
		oid uint32
		v   any
	}{
		{pgtype.NumericArrayOID, []any{num, nil, nan}},
		{pgtype.NumericArrayOID, []any{[]any{num}, []any{nil}}},
		{pgtype.IntervalArrayOID, []any{pgtype.Interval{Microseconds: 90e6, Days: 1, Valid: true}}},
		{pgtype.TimeArrayOID, []any{pgtype.Time{Microseconds: 3600e6, Valid: true}, nil}},
		{pgtype.TsrangeArrayOID, []any{span}},
		{pgtype.NumrangeArrayOID, []any{nums}},
		{pgtype.NumrangeOID, nums},
		{pgtype.Int4multirangeOID, pgtype.Multirange[pgtype.Range[any]]{ints, ints}},
		{pgtype.TsmultirangeOID, pgtype.Multirange[pgtype.Range[any]]{span}},
	}
	// Each as pgx decodes it at record time.
	for oid, txt := range map[uint32]string{
		pgtype.PointOID:          `(1.5,2)`,
		pgtype.PointArrayOID:     `{"(1.5,2)",NULL}`,
		pgtype.LsegArrayOID:      `{"[(1,2),(3.5,4)]"}`,
		pgtype.BoxArrayOID:       `{"(3.5,4),(1,2)"}`,
		pgtype.PolygonArrayOID:   `{"((0,0),(1,0),(1,1))"}`,
		pgtype.PathArrayOID:      `{"[(0,0),(1,1)]"}`,
		pgtype.LineArrayOID:      `{"{1,-1,0.5}"}`,
		pgtype.CircleArrayOID:    `{"<(1,2),3.5>"}`,
		pgtype.TIDArrayOID:       `{"(17,3)"}`,
		pgtype.InetArrayOID:      `{10.0.0.1/32,::1/128}`,
		pgtype.CIDRArrayOID:      `{10.0.0.0/8}`,
		pgtype.TSVectorArrayOID:  `{"'a':1 'cat':2A"}`,
		pgtype.NumericArrayOID:   `{{1.5,NaN},{Infinity,NULL}}`,
		pgtype.TimeArrayOID:      `{10:30:45}`,
		pgtype.IntervalArrayOID:  `{"1 day 00:01:30"}`,
		pgtype.Int8multirangeOID: `{[3,9),[12,20)}`,
	} {
		v, err := pgxText(t, oid, txt)
		if err != nil {
			t.Fatalf("%d %s: %v", oid, txt, err)
		}
		cols = append(cols, struct {
			oid uint32
			v   any
		}{oid, v})
	}
	resp := PostgresV3Response{Rows: []PostgresV3Cells{{}}}
	q := PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)"}
	for _, c := range cols {
		resp.RowDescription = append(resp.RowDescription, PostgresV3ColumnDescriptor{Name: "c", TypeOID: c.oid})
		resp.Rows[0] = append(resp.Rows[0], NewValueCell(c.v))
		q.ParamOIDs = append(q.ParamOIDs, c.oid)
		q.BindValues = append(q.BindValues, NewValueCell(c.v))
	}
	q.Response = &resp
	body, err := yaml.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PostgresV3QuerySpec
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	for i, c := range cols {
		if got := back.Response.Rows[0][i].Value; !reflect.DeepEqual(got, c.v) {
			t.Errorf("column %d (type %d) came back as %#v, want %#v", i, c.oid, got, c.v)
		}
		if got := back.BindValues[i].Value; !reflect.DeepEqual(got, c.v) {
			t.Errorf("bound value %d (type %d) came back as %#v, want %#v", i, c.oid, got, c.v)
		}
		if _, err := back.Response.Rows[0][i].GobEncode(); err != nil {
			t.Errorf("column %d does not gob-encode for the agent after its reload: %v", i, err)
		}
	}
}

// Only a value of a type keploy's YAML writes in a form of its own is read
// by its column's type: an array of json or jsonb documents with a shape's
// keys, a column of no stated type, and a value that is not the column
// type's shape stay what yaml.v3 makes of them, as before. (A top-level
// document with a shape's exact keys and kinds is still read as that shape
// by the cell's own decode, as before.)
func TestAPostgresDocumentWithAShapesKeysComesBackFromYAMLAsTheDocument(t *testing.T) {
	timeDoc := map[string]any{"microseconds": 5, "valid": true}
	numDoc := map[string]any{"int": "1", "exp": 0, "nan": false, "infinitymodifier": 0, "valid": true}
	cols := []struct {
		oid uint32
		v   any
	}{
		{pgtype.JSONBOID, []any{timeDoc, timeDoc}},
		{pgtype.JSONOID, []any{numDoc, numDoc}},
		{pgtype.JSONBArrayOID, []any{timeDoc}},
		{0, []any{timeDoc}},
		{pgtype.NumericArrayOID, []any{timeDoc}},
		{pgtype.TimeArrayOID, []any{numDoc}},
		// An array column whose cell is no array, an element whose shape
		// does not decode, a multirange holding no range, a point column
		// with a lseg's keys plus another, an inet[] element that is no
		// prefix.
		{pgtype.NumericArrayOID, map[string]any{"a": "x"}},
		{pgtype.NumericArrayOID, []any{map[string]any{"int": "1x", "exp": 0, "nan": false, "infinitymodifier": 0, "valid": true}}},
		{pgtype.Int4multirangeOID, []any{"x"}},
		{pgtype.PointOID, map[string]any{"p": "x", "valid": true, "z": 1}},
		// A point column whose p is no point, one with a point's keys
		// plus another, and a multirange column holding a mapping.
		{pgtype.PointOID, map[string]any{"p": "x", "valid": true}},
		{pgtype.PointOID, map[string]any{"p": map[string]any{"x": 1, "y": 2}, "valid": true, "z": 1}},
		{pgtype.Int4multirangeOID, map[string]any{"a": 1}},
		{pgtype.InetArrayOID, []any{"x"}},
	}
	resp := PostgresV3Response{Rows: []PostgresV3Cells{{}, {}}}
	for _, c := range cols {
		resp.RowDescription = append(resp.RowDescription, PostgresV3ColumnDescriptor{Name: "c", TypeOID: c.oid})
		resp.Rows[0] = append(resp.Rows[0], NewValueCell(c.v))
	}
	// A row longer than the description: its extra cell is read as before.
	resp.Rows[1] = append(append(resp.Rows[1], resp.Rows[0]...), NewValueCell([]any{timeDoc}))
	body, err := yaml.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PostgresV3Response
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	for r, row := range back.Rows {
		for i, c := range row {
			var want any = []any{timeDoc}
			if i < len(cols) {
				want = cols[i].v
			}
			if !reflect.DeepEqual(c.Value, want) {
				t.Errorf("row %d column %d came back as %#v, want %#v", r, i, c.Value, want)
			}
		}
	}
}

// A statement whose bound values outnumber its ParamOIDs (no parameter types
// seen) loads, and its values beyond them are read as before.
func TestAPostgresStatementWithMoreBindsThanTypesComesBackFromYAML(t *testing.T) {
	q := PostgresV3QuerySpec{SQLNormalized: "SELECT f($1, $2)", ParamOIDs: []uint32{pgtype.NumericArrayOID},
		BindValues: PostgresV3Cells{NewValueCell([]any{map[string]any{"a": 1}}), NewValueCell([]any{map[string]any{"b": 2}})}}
	body, err := yaml.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back PostgresV3QuerySpec
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if !reflect.DeepEqual(back.BindValues[1].Value, []any{map[string]any{"b": 2}}) {
		t.Errorf("came back as %#v", back.BindValues[1].Value)
	}
}
