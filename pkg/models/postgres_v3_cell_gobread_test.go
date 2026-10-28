package models

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"math"
	"math/big"
	"math/bits"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// cellReadCorpus is a value of each type a cell holds (every GobEncode tag),
// with the edges of each encoding: the integers' limits, lengths past one
// byte, empty and nil bytes, a UUID with bytes above 127, times in a zone and
// at the zero instant, and floats at their limits, infinities and NaN.
func cellReadCorpus() []any {
	long := strings.Repeat("x", 300)
	return []any{
		nil,
		int64(0), int64(1), int64(-1), int64(127), int64(128), int64(-300), int64(math.MaxInt64), int64(math.MinInt64),
		int32(246), int32(math.MaxInt32), int32(math.MinInt32),
		int16(-7), int16(math.MaxInt16), int16(math.MinInt16),
		int8(-42), int8(math.MaxInt8), int8(math.MinInt8),
		uint64(math.MaxUint64), uint32(math.MaxUint32), uint32(0), uint16(math.MaxUint16),
		42, 0, -1,
		3.14, 0.0, math.Copysign(0, -1), math.Inf(1), math.Inf(-1), math.NaN(), math.MaxFloat64, math.SmallestNonzeroFloat64,
		float32(3.14), float32(math.MaxFloat32), float32(math.Inf(-1)), float32(math.NaN()),
		"", "priority-i23-333b", "café", long,
		true, false,
		[]byte{}, []byte(nil), []byte{0x01, 0x02, 0x03}, []byte(long),
		time.Time{}, time.Date(2026, 4, 24, 14, 25, 37, 580669000, time.UTC),
		time.Date(2026, 10, 27, 9, 1, 2, 3, time.FixedZone("IST", 19800)),
		PostgresV3CellRaw{Format: 1, Bytes: []byte{0x00, 0x00, 0x00, 0x01}},
		[16]uint8{0x11, 0x11, 0x11, 0x11, 0x22, 0x22, 0x33, 0x33, 0x44, 0x44, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55},
		[16]uint8{0, 1, 0x7f, 0x80, 0xff, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xfe, 0x10, 0x20, 0x30, 0x40, 0x50},
		[]interface{}{"vip", "churn-risk", "active"},
		[]interface{}{int32(42), "hello", true, nil, []byte{0x01, 0x02}, time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)},
		[]interface{}{},
		[]interface{}{[]interface{}{"a", "b"}, []interface{}{"c", "d"}},
		[]interface{}{[16]uint8{0xaa, 0xaa}, [16]uint8{0xbb}},
		map[string]interface{}{"name": "vip", "active": true, "count": int32(42)},
		map[string]interface{}{},
		map[string]interface{}{"smtp": map[string]interface{}{"host": "mail.example.com", "port": int32(25)}, "tags": []interface{}{"a"}},
		pgtype.Numeric{Int: big.NewInt(123456), Exp: -2, Valid: true},
		pgtype.Numeric{Int: new(big.Int).Lsh(big.NewInt(1), 200), Exp: 3, Valid: true},
		pgtype.Numeric{NaN: true, Valid: true},
		pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
		pgtype.Interval{Microseconds: 3600_000_000, Days: 7, Months: 1, Valid: true},
		pgtype.Time{Microseconds: 12345, Valid: true},
		pgtype.Bits{Bytes: []byte{0xAB, 0xCD}, Len: 16, Valid: true},
		pgtype.Point{P: pgtype.Vec2{X: 1.5, Y: 2.5}, Valid: true},
		pgtype.Line{A: 1, B: -1, C: 0, Valid: true},
		pgtype.Lseg{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 3, Y: 4}}, Valid: true},
		pgtype.Box{P: [2]pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}}, Valid: true},
		pgtype.Path{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 0}}, Closed: true, Valid: true},
		pgtype.Polygon{P: []pgtype.Vec2{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}}, Valid: true},
		pgtype.Circle{P: pgtype.Vec2{X: 5, Y: 5}, R: 2.5, Valid: true},
		pgtype.TID{BlockNumber: 42, OffsetNumber: 7, Valid: true},
		pgtype.TSVector{Lexemes: []pgtype.TSVectorLexeme{
			{Word: "cat", Positions: []pgtype.TSVectorPosition{{Position: 1, Weight: pgtype.TSVectorWeightD}}},
			{Word: "fish", Positions: []pgtype.TSVectorPosition{{Position: 300, Weight: pgtype.TSVectorWeightA}}},
		}, Valid: true},
		netip.MustParsePrefix("192.168.1.10/32"), netip.MustParsePrefix("2001:db8::/64"),
		net.HardwareAddr{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF},
		pgtype.Hstore{"key1": stringPtr("value1"), "key2": nil, "empty": stringPtr("")},
		pgtype.Range[any]{Lower: int32(1), Upper: int32(10), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		pgtype.Range[any]{Lower: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Upper: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		pgtype.Range[any]{LowerType: pgtype.Empty, UpperType: pgtype.Empty, Valid: true},
		pgtype.Multirange[pgtype.Range[any]]{
			{Lower: int32(1), Upper: int32(5), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true},
		},
	}
}

// readCellBy reads data with GobDecode, by gob from each cell's first message
// when byGob (as before cellStream), and returns the cell, the error's text and
// the gob decoders the read built.
func readCellBy(data []byte, byGob bool) (PostgresV3Cell, string, int64) {
	cellReadByGob.Store(byGob)
	defer cellReadByGob.Store(false)
	before := cellGobDecoders.Load()
	var c PostgresV3Cell
	err := c.GobDecode(data)
	built := cellGobDecoders.Load() - before
	if err != nil {
		return c, err.Error(), built
	}
	return c, "", built
}

// sameCell reports whether two cells read from one stream hold the same value:
// of one type, deeply equal, or (a NaN is unequal to itself) printed alike.
func sameCell(a, b PostgresV3Cell) bool {
	if reflect.TypeOf(a.Value) != reflect.TypeOf(b.Value) {
		return false
	}
	return reflect.DeepEqual(a.Value, b.Value) || fmt.Sprintf("%#v", a.Value) == fmt.Sprintf("%#v", b.Value)
}

// Every cell is read as a gob decoder over its stream reads it (the reference:
// GobDecode before cellStream), into the same value of the same type, and the
// value read encodes to the bytes it was read from.
func TestACellIsReadAsAGobDecoderReadsIt(t *testing.T) {
	for _, v := range cellReadCorpus() {
		enc, err := NewValueCell(v).GobEncode()
		if err != nil {
			t.Fatalf("%T %v: GobEncode: %v", v, v, err)
		}
		want, wantErr, _ := readCellBy(enc, true)
		got, gotErr, _ := readCellBy(enc, false)
		if gotErr != "" || wantErr != "" {
			t.Errorf("%T %v: read with error %q, gob's reference %q", v, v, gotErr, wantErr)
			continue
		}
		if !sameCell(got, want) {
			t.Errorf("%T %v: read %#v, gob's reference read %#v", v, v, got.Value, want.Value)
		}
		again, err := got.GobEncode()
		if err != nil || !bytes.Equal(again, enc) {
			t.Errorf("%T %v: the value read encodes to % x (%v), read from % x", v, v, again, err, enc)
		}
	}
}

// gobUintBytes is x as gob writes an unsigned integer.
func gobUintBytes(x uint64) []byte {
	if x <= 0x7f {
		return []byte{byte(x)}
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], x)
	lead := bits.LeadingZeros64(x) / 8
	return append([]byte{byte(-(8 - lead))}, b[lead:]...)
}

// padMessage is the cell enc with extra appended inside its k-th gob message
// (the last when k is -1), the message's length counting them.
func padMessage(t *testing.T, enc []byte, k int, extra ...byte) []byte {
	t.Helper()
	var msgs [][]byte
	for b := enc[1:]; len(b) > 0; {
		n, rest, ok := gobUint(b)
		if !ok || n > uint64(len(rest)) {
			t.Fatalf("stream % x does not split into messages", enc)
		}
		msgs, b = append(msgs, rest[:n]), rest[n:]
	}
	if k < 0 {
		k = len(msgs) - 1
	}
	if k >= len(msgs) {
		t.Fatalf("stream % x has %d messages, none at %d", enc, len(msgs), k)
	}
	out := []byte{enc[0]}
	for i, m := range msgs {
		if i == k {
			m = append(append([]byte(nil), m...), extra...)
		}
		out = append(append(out, gobUintBytes(uint64(len(m)))...), m...)
	}
	return out
}

// foreignCellStreams are cells whose tag names one type and whose stream holds
// another's value, or a value out of the tag's range, or more than the value,
// or a value spelled with more bytes than it needs.
func foreignCellStreams(t *testing.T) [][]byte {
	t.Helper()
	cell := func(tag byte, vs ...any) []byte {
		var buf bytes.Buffer
		buf.WriteByte(tag)
		enc := gob.NewEncoder(&buf)
		for _, v := range vs {
			if err := enc.Encode(v); err != nil {
				t.Fatal(err)
			}
		}
		return buf.Bytes()
	}
	// A known type's definitions followed by a message of another type
	// whose body reads as the known type's value.
	tb, err := time.Unix(5, 0).UTC().GobEncode()
	if err != nil {
		t.Fatal(err)
	}
	afterDefs := func(tag byte, k cellKnownType, v any) []byte {
		if len(k.defs) == 0 {
			t.Fatalf("no definitions known for tag %d", tag)
		}
		return append(append([]byte{tag}, k.defs...), cell(tag, v)[1:]...)
	}
	known := cellKnownTypes()
	// A UUID's value message after this process's definitions, spelled as
	// gob spells one (checked below) but for the count, the elements and the
	// bytes after them.
	uuidValue := func(count uint64, elems []uint64, extra ...byte) []byte {
		body := append(gobUintBytes(uint64(known.uuid.id)<<1), 0x00)
		body = append(body, gobUintBytes(count)...)
		for _, e := range elems {
			body = append(body, gobUintBytes(e)...)
		}
		body = append(body, extra...)
		msg := append(gobUintBytes(uint64(len(body))), body...)
		return append(append([]byte{cellTagUUIDBytes}, known.uuid.defs...), msg...)
	}
	uuid := [16]uint8{1, 2, 3, 0x7f, 0x80, 0xff, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	elems := make([]uint64, len(uuid))
	for i, e := range uuid {
		elems[i] = uint64(e)
	}
	if got, want := uuidValue(16, elems), cell(cellTagUUIDBytes, uuid); !bytes.Equal(got, want) {
		t.Fatalf("a UUID spelled by hand is % x, gob spells it % x", got, want)
	}
	wide := append([]uint64(nil), elems...)
	wide[3] = 300
	edge := append([]uint64(nil), elems...)
	edge[3] = math.MaxUint8 + 1
	// A point whose discriminator is the point's kind plus 256, then the
	// point's own messages.
	pt, err := NewValueCell(pgtype.Point{P: pgtype.Vec2{X: 1.5, Y: 2.5}, Valid: true}).GobEncode()
	if err != nil {
		t.Fatal(err)
	}
	_, _, ptRest, ok := gobMessage(pt[1:])
	if !ok {
		t.Fatalf("a point's stream % x has no first message", pt)
	}
	enc := func(v any) []byte {
		b, err := NewValueCell(v).GobEncode()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return [][]byte{
		uuidValue(16, wide),              // an element out of uint8's range
		uuidValue(16, edge),              // an element one past uint8's range
		uuidValue(15, elems),             // a count of 15 before 16 elements
		uuidValue(17, elems),             // a count of 17 before 16 elements
		uuidValue(16, elems, 0x07, 0x08), // bytes after the elements, in the value's message
		cell(cellTagGeom, uint64(300)),   // a discriminator out of uint8's range
		append(cell(cellTagGeom, uint64(256)+uint64(geomKindPoint)), ptRest...),
		{cellTagInt64, 0x0c, 0x04, 0x00, 0xf7, 1, 2, 3, 4, 5, 6, 7, 8, 9}, // an integer in 9 bytes
		// Bytes after the value inside a message: the first of a
		// multi-value cell, a key string, a time's bytes.
		padMessage(t, enc(pgtype.Interval{Microseconds: 3600_000_000, Days: 7, Months: 1, Valid: true}), 0, 0x05),
		padMessage(t, enc(pgtype.Hstore{"key1": stringPtr("value1")}), 1, 0x61, 0x62),
		padMessage(t, enc(time.Date(2026, 10, 27, 9, 1, 2, 3, time.UTC)), -1, 0x01),
		padMessage(t, enc([]byte{1, 2, 3}), -1, 0x04),
		afterDefs(cellTagTime, known.time, tb),
		afterDefs(cellTagUUIDBytes, known.uuid, bytes.Repeat([]byte{0x10}, 16)),
		cell(cellTagInt32, int64(1)<<40), cell(cellTagInt32, int64(math.MinInt32)-1), cell(cellTagInt16, int64(1)<<20),
		cell(cellTagInt8, int64(300)), cell(cellTagInt8, int64(-129)), cell(cellTagUint16, uint64(1)<<20),
		cell(cellTagUint32, uint64(1)<<40), cell(cellTagFloat32, 1e300), cell(cellTagFloat32, -1e39),
		// Each range's edges: the last value it holds and the first it
		// does not, on both sides (gob refuses the second as an overflow).
		cell(cellTagInt8, int64(math.MaxInt8)), cell(cellTagInt8, int64(math.MaxInt8)+1),
		cell(cellTagInt8, int64(math.MinInt8)), cell(cellTagInt8, int64(math.MinInt8)-1),
		cell(cellTagInt16, int64(math.MaxInt16)), cell(cellTagInt16, int64(math.MaxInt16)+1),
		cell(cellTagInt16, int64(math.MinInt16)), cell(cellTagInt16, int64(math.MinInt16)-1),
		cell(cellTagInt32, int64(math.MaxInt32)), cell(cellTagInt32, int64(math.MaxInt32)+1),
		cell(cellTagInt32, int64(math.MinInt32)),
		cell(cellTagUint16, uint64(math.MaxUint16)), cell(cellTagUint16, uint64(math.MaxUint16)+1),
		cell(cellTagUint32, uint64(math.MaxUint32)), cell(cellTagUint32, uint64(math.MaxUint32)+1),
		cell(cellTagGeom, uint64(math.MaxUint8)), cell(cellTagGeom, uint64(math.MaxUint8)+1),
		cell(cellTagFloat32, math.MaxFloat32), cell(cellTagFloat32, -math.MaxFloat32),
		cell(cellTagFloat32, math.Nextafter(math.MaxFloat32, math.Inf(1))),
		cell(cellTagFloat32, math.MaxFloat64), cell(cellTagFloat32, -math.MaxFloat64),
		cell(cellTagFloat32, math.Inf(1)), cell(cellTagFloat32, math.Inf(-1)),
		cell(cellTagInt64, uint64(5)), cell(cellTagUint64, int64(5)), cell(cellTagString, []byte("x")),
		cell(cellTagBytes, "x"), cell(cellTagBool, int64(1)), cell(cellTagBool, uint64(2)), cell(cellTagFloat64, int64(2)),
		cell(cellTagTime, int64(1)), cell(cellTagTime, "2026"), cell(cellTagUUIDBytes, [15]uint8{1}),
		cell(cellTagUUIDBytes, []byte{1, 2}), cell(cellTagUUIDBytes, [16]uint16{1}), cell(cellTagTime, time.Time{}, int64(1)),
		cell(cellTagInt64, int64(1), int64(2)), cell(cellTagString, "a", "b"),
		append(cell(cellTagUUIDBytes, [16]uint8{9}), 0x00), append(cell(cellTagTime, time.Unix(5, 0).UTC()), 0x03, 0x04, 0x00, 0x02),
		append(cell(cellTagInt64, int64(5)), 0xff),
		{cellTagInt64, 0x04, 0xff, 0x04, 0x00, 0x0a},       // a message length of 4 in one byte more than it needs
		{cellTagInt64, 0x04, 0x04, 0x00, 0xff, 0x0a},       // the value 5 in two bytes
		{cellTagInt64, 0x04, 0x04, 0x01, 0x0a},             // a field delta of 1
		{cellTagInt64, 0x04, 0x04, 0x00, 0x0a, 0x0b},       // a byte more than the value in its message
		{cellTagString, 0x04, 0x0c, 0x00, 0x02, 0x61},      // a string longer than its message
		{cellTagString, 0x04, 0x0c, 0x00, 0x00, 0x61},      // a string shorter than its message
		{cellTagString, 0x05, 0x0c, 0x00, 0x00, 0x61},      // a message longer than the stream
		{cellTagBytes, 0x04, 0x0a, 0x00, 0x01, 0x61, 0x62}, // a byte after the message
		{cellTagInt64, 0x00}, {cellTagInt64}, {cellTagNull, 0x01, 0x02},
	}
}

// A cell's stream cut short or with any one byte changed, or one that holds
// what its tag does not name (foreignCellStreams), is read as gob reads it:
// the same error, or the same value.
func TestACorruptCellIsReadAsAGobDecoderReadsIt(t *testing.T) {
	compared := 0
	for _, data := range foreignCellStreams(t) {
		want, wantErr, _ := readCellBy(data, true)
		got, gotErr, _ := readCellBy(data, false)
		compared++
		if gotErr != wantErr || (gotErr == "" && !sameCell(got, want)) {
			t.Errorf("stream % x: read %#v (error %q), gob's reference %#v (error %q)", data, got.Value, gotErr, want.Value, wantErr)
		}
	}
	for _, v := range cellReadCorpus() {
		enc, err := NewValueCell(v).GobEncode()
		if err != nil {
			t.Fatal(err)
		}
		var variants [][]byte
		for n := 1; n < len(enc); n++ {
			variants = append(variants, enc[:n])
		}
		for i := 1; i < len(enc) && i < 64; i++ {
			for _, b := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff, enc[i] ^ 0x01, enc[i] + 1, enc[i] - 1} {
				if b == enc[i] {
					continue
				}
				m := append([]byte(nil), enc...)
				m[i] = b
				variants = append(variants, m)
			}
		}
		for _, data := range variants {
			want, wantErr, _ := readCellBy(data, true)
			got, gotErr, _ := readCellBy(data, false)
			compared++
			if gotErr != wantErr || (gotErr == "" && !sameCell(got, want)) {
				t.Errorf("%T %v, stream % x: read %#v (error %q), gob's reference %#v (error %q)",
					v, v, data, got.Value, gotErr, want.Value, wantErr)
			}
		}
	}
	t.Logf("%d corrupt streams compared", compared)
}

// A time or a UUID is read by hand only as the whole rest of its stream: a
// second value of its type from the same encoder carries no definitions, so a
// gob decoder started at it could not read it. Each value is read as a gob
// decoder over the whole stream reads it.
func TestAKnownTypeIsReadByHandOnlyAsTheWholeRestOfItsStream(t *testing.T) {
	t1 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2027, 1, 2, 3, 4, 5, 6, time.FixedZone("IST", 19800))
	u1, u2 := [16]uint8{1, 2, 3}, [16]uint8{0xff, 0x80}
	for _, vs := range [][]any{{t1, t2}, {u1, u2}, {t1, int64(7)}, {u1, t1}, {int64(7), t1, t2}} {
		var buf bytes.Buffer
		enc := gob.NewEncoder(&buf)
		for _, v := range vs {
			if err := enc.Encode(v); err != nil {
				t.Fatal(err)
			}
		}
		ref := gob.NewDecoder(bytes.NewReader(buf.Bytes()))
		s := &cellStream{b: buf.Bytes()}
		for i, v := range vs {
			want, got := reflect.New(reflect.TypeOf(v)), reflect.New(reflect.TypeOf(v))
			if err := ref.Decode(want.Interface()); err != nil {
				t.Fatalf("%v: gob read value %d: %v", vs, i, err)
			}
			if err := s.Decode(got.Interface()); err != nil {
				t.Errorf("%v: value %d read with error %v, gob read %v", vs, i, err, want.Elem())
				break
			}
			if !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface()) {
				t.Errorf("%v: value %d read %v, gob read %v", vs, i, got.Elem(), want.Elem())
			}
		}
	}
}

// Reading a cell builds no gob decoder unless the cell holds a struct or an
// array gob must define (a raw value of an unknown type, a line's
// coefficients): one was built for every cell before, and every cell of a
// recorded set is read whenever a test loads its mocks. A UUID and a time
// are read without one when this process's gob encoded them.
func TestReadingACellBuildsNoGobDecoder(t *testing.T) {
	stillGob := map[string]bool{
		fmt.Sprintf("%T", PostgresV3CellRaw{}): true,
		fmt.Sprintf("%T", pgtype.Line{}):       true,
	}
	var cells, before int64
	for _, v := range cellReadCorpus() {
		enc, err := NewValueCell(v).GobEncode()
		if err != nil {
			t.Fatal(err)
		}
		cells++
		_, _, ref := readCellBy(enc, true)
		before += ref
		_, _, built := readCellBy(enc, false)
		want := int64(0)
		if stillGob[fmt.Sprintf("%T", v)] {
			want = 1
		}
		if built != want {
			t.Errorf("%T %v: reading the cell built %d gob decoders, want %d", v, v, built, want)
		}
	}
	if before < cells {
		t.Errorf("the reference read built %d gob decoders for %d cells, want one or more per cell", before, cells)
	}

	// A row as a mock carries it, read by the mock's own decoder.
	row := PostgresV3Cells{NewValueCell(int64(7)), NewValueCell("ada"), NewValueCell(int32(3)), NewValueCell(true),
		NewValueCell(nil), NewValueCell(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)), NewValueCell([16]uint8{1, 2, 3}),
		NewValueCell(pgtype.Numeric{Int: big.NewInt(1999), Exp: -2, Valid: true})}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(row); err != nil {
		t.Fatal(err)
	}
	start := cellGobDecoders.Load()
	var got PostgresV3Cells
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if built := cellGobDecoders.Load() - start; built != 0 || len(got) != len(row) {
		t.Errorf("a row of %d cells read %d cells and built %d gob decoders for them, want none", len(row), len(got), built)
	}
}
