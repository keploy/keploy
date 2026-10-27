package query

import (
	"context"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// queryAttrPacket is a COM_QUERY under CLIENT_QUERY_ATTRIBUTES, framed by
// hand: the command byte, then raw (parameter_count onward).
func queryAttrPacket(raw ...byte) []byte {
	return append([]byte{mysql.COM_QUERY}, raw...)
}

// A COM_QUERY under CLIENT_QUERY_ATTRIBUTES whose count or lengths the packet
// cannot hold is an error, never a panic: a parameter_count near MaxInt64
// overflowed the NULL bitmap's length, one past it converted negative, and a
// value's length past MaxInt64 turned negative and passed the bounds check. A
// count is bounded by the types it needs (2 bytes each), which also bounds the
// parameters allocated. mysql2 and Connector/J send their queries this way.
func TestDecodeQuery_ACountOrLengthThePacketCannotHoldIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"parameter_count near MaxInt64": {queryAttrPacket(0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f, 0x01, 0x00, 0x01, 'x'), "more than the packet can hold"},
		"parameter_count past MaxInt64": {queryAttrPacket(0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x01, 0x00, 0x01, 'x'), "more than the packet can hold"},
		"a name's length past MaxInt64": {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00,
			0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'), "unexpected EOF"},
		"a value's length past MaxInt64": {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0x01, 'a',
			0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'), "unexpected EOF"},
		// A NULL is in the bitmap: a name or value length is never one.
		"a NULL marker for a name's length":  {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0xfb, 'x'), "has no name length"},
		"a NULL marker for a value's length": {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0x01, 'a', 0xfb, 'x'), "has no value length"},
		"a name cut short":                   {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0x05, 'a', 'b'), "unexpected EOF"},
		// Five parameters' types take 10 bytes; 7 follow the counts.
		"parameter_count more than its types can fill": {queryAttrPacket(0x05, 0x01, 0x00, 0x01, 0xfd, 0x00, 0xfd, 0x00, 'x'), "more than the packet can hold"},
	} {
		p, err := DecodeQuery(context.Background(), zap.NewNop(), tc.data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decoded %+v (%v), want an error saying %q", name, p, err, tc.want)
		}
	}
}

// connectorJTraceparent is a COM_QUERY as MySQL Connector/J 8.4 sends one with
// the caller's OpenTelemetry trace (JdbcStatement.setAttribute("traceparent",
// tp)): one attribute, a string, named traceparent.
func connectorJTraceparent(tp, sql string) []byte {
	data := queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeString), 0x00)
	data = append(data, byte(len("traceparent")))
	data = append(data, "traceparent"...)
	data = append(data, byte(len(tp)))
	data = append(data, tp...)
	return append(data, sql...)
}

// The query attributes are read as the protocol lays them out: each one's type
// and name, then the values, a NULL one (in the bitmap) sending none. A keploy
// that did not read the names took an attribute's name for its value and glued
// its value to the head of the query: what it recorded from a packet is kept
// as the packet's LegacyQuery and LegacyParameters, and is empty for a packet
// it could not read (it recorded nothing).
func TestDecodeQuery_ReadsTheAttributesAsTheProtocolLaysThemOut(t *testing.T) {
	const tp = "00-a5ff4ff820b14ad28693c1b155015340-0000000000000001-01"

	t.Run("Connector/J's traceparent", func(t *testing.T) {
		p, err := DecodeQuery(context.Background(), zap.NewNop(), connectorJTraceparent(tp, "SELECT 1 AS v"), mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err != nil {
			t.Fatal(err)
		}
		want := []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Name: "traceparent", Value: tp}}
		if p.Query != "SELECT 1 AS v" || !reflect.DeepEqual(p.Parameters, want) {
			t.Fatalf("decoded %q %+v, want SELECT 1 AS v %+v", p.Query, p.Parameters, want)
		}
		// What keploy v3.6.126 recorded from this packet (the java lane's
		// recording of Connector/J 8.4 with this traceparent).
		wantLegacy := []mysql.Parameter{{Type: uint16(mysql.FieldTypeString), Value: "traceparent"}}
		if p.LegacyQuery != "7"+tp+"SELECT 1 AS v" || !reflect.DeepEqual(p.LegacyParameters, wantLegacy) {
			t.Fatalf("legacy reading %q %+v, want %q %+v", p.LegacyQuery, p.LegacyParameters, "7"+tp+"SELECT 1 AS v", wantLegacy)
		}
	})

	t.Run("two attributes, the second NULL", func(t *testing.T) {
		data := queryAttrPacket(0x02, 0x01, 0x02, 0x01,
			byte(mysql.FieldTypeVarString), 0x00, 0x01, 'a',
			byte(mysql.FieldTypeLongLong), 0x00, 0x01, 'b',
			0x01, 'x')
		data = append(data, "SELECT\t2"...)
		p, err := DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err != nil {
			t.Fatal(err)
		}
		want := []mysql.Parameter{
			{Type: uint16(mysql.FieldTypeVarString), Name: "a", Value: "x"},
			{Type: uint16(mysql.FieldTypeLongLong), Name: "b"},
		}
		if p.Query != "SELECT    2" || p.ParameterCount != 2 || !reflect.DeepEqual(p.Parameters, want) {
			t.Fatalf("decoded %q %d %+v, want SELECT 2 %+v", p.Query, p.ParameterCount, p.Parameters, want)
		}
		// Read without names, the second type is a's name (a TINY, its type
		// read as a byte), the first value the next 8 bytes, and the second
		// one byte of the query: the older keploy recorded its tail.
		if p.LegacyQuery != "CT    2" || len(p.LegacyParameters) != 2 || p.LegacyParameters[0].Value != "\x00\x01b\x01xSEL" || p.LegacyParameters[1].Value != int8('E') {
			t.Fatalf("legacy reading %q %+v, want what an older keploy read", p.LegacyQuery, p.LegacyParameters)
		}
	})

	t.Run("a NULL attribute", func(t *testing.T) {
		data := queryAttrPacket(0x01, 0x01, 0x01, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0x01, 'n')
		data = append(data, "SELECT 3"...)
		p, err := DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err != nil {
			t.Fatal(err)
		}
		want := []mysql.Parameter{{Type: uint16(mysql.FieldTypeVarString), Name: "n"}}
		if p.Query != "SELECT 3" || !reflect.DeepEqual(p.Parameters, want) {
			t.Fatalf("decoded %q %+v, want SELECT 3 %+v", p.Query, p.Parameters, want)
		}
	})

	t.Run("an attribute and no query", func(t *testing.T) {
		if p, err := DecodeQuery(context.Background(), zap.NewNop(), connectorJTraceparent(tp, ""), mysql.CLIENT_QUERY_ATTRIBUTES, true); err == nil {
			t.Fatalf("decoded %+v, want no query refused", p)
		}
	})
}

// A query with no attributes decodes as it did.
func TestDecodeQuery_AQueryWithNoAttributes(t *testing.T) {
	p, err := DecodeQuery(context.Background(), zap.NewNop(), queryAttrPacket(0x00, 0x01, 'S', 'E', 'L', 'E', 'C', 'T', '\t', '1'), mysql.CLIENT_QUERY_ATTRIBUTES, true)
	if err != nil || p.Query != "SELECT    1" || p.ParameterCount != 0 {
		t.Fatalf("decoded %+v (%v), want SELECT 1 and no parameters", p, err)
	}
}

// No packet panics the decoder: random packets, their bytes weighted toward
// the length-encoded markers (0xfb-0xff), decode or are refused.
func TestDecodeQuery_NoPacketPanics(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	types := []byte{byte(mysql.FieldTypeVarString), byte(mysql.FieldTypeLong), byte(mysql.FieldTypeLongLong), byte(mysql.FieldTypeTiny),
		byte(mysql.FieldTypeDouble), byte(mysql.FieldTypeDate), byte(mysql.FieldTypeDateTime), byte(mysql.FieldTypeTime), byte(mysql.FieldTypeBLOB)}
	pick := func() byte {
		switch r.Intn(4) {
		case 0:
			return byte(0xfb + r.Intn(5))
		case 1:
			return types[r.Intn(len(types))]
		case 2:
			return byte(r.Intn(4))
		default:
			return byte(r.Intn(256))
		}
	}
	for i := 0; i < 200000; i++ {
		data := []byte{mysql.COM_QUERY}
		for n := r.Intn(40); n > 0; n-- {
			data = append(data, pick())
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("packet %x panicked the decoder: %v", data, p)
				}
			}()
			_, _ = DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		}()
	}
}

// FuzzDecodeQuery: no input panics the decoder (go test runs the seeds; go
// test -fuzz explores from them).
func FuzzDecodeQuery(f *testing.F) {
	f.Add(queryAttrPacket(0x00, 0x01, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'))
	f.Add(queryAttrPacket(0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x01, 0x00, 0x01, 'x'))
	f.Add(queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'))
	f.Add(connectorJTraceparent("00-a5ff4ff820b14ad28693c1b155015340-0000000000000001-01", "SELECT 1"))
	f.Add(queryAttrPacket(0x02, 0x01, 0x02, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0x01, 'a', byte(mysql.FieldTypeLongLong), 0x00, 0x01, 'b', 0x01, 'x', 'S'))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES, true)
		if err != nil {
			return
		}
		// A decoded packet holds a query, and an attribute for its count,
		// each NULL in the bitmap holding no value.
		if p.Query == "" || len(p.Parameters) != p.ParameterCount {
			t.Fatalf("packet %x decoded %+v", data, p)
		}
		for i, param := range p.Parameters {
			if p.NullBitmap[i/8]&(1<<(i%8)) != 0 && param.Value != nil {
				t.Fatalf("packet %x: NULL attribute %d decoded a value %+v", data, i, param)
			}
		}
	})
}
