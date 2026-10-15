package query

import (
	"context"
	"math/rand"
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
		"a value's length past MaxInt64": {queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00,
			0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'), "unexpected EOF"},
		// Five parameters' types take 10 bytes; 7 follow the counts.
		"parameter_count more than its types can fill": {queryAttrPacket(0x05, 0x01, 0x00, 0x01, 0xfd, 0x00, 0xfd, 0x00, 'x'), "more than the packet can hold"},
	} {
		p, err := DecodeQuery(context.Background(), zap.NewNop(), tc.data, mysql.CLIENT_QUERY_ATTRIBUTES)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decoded %+v (%v), want an error saying %q", name, p, err, tc.want)
		}
	}
}

// A query with no attributes decodes as it did.
func TestDecodeQuery_AQueryWithNoAttributes(t *testing.T) {
	p, err := DecodeQuery(context.Background(), zap.NewNop(), queryAttrPacket(0x00, 0x01, 'S', 'E', 'L', 'E', 'C', 'T', '\t', '1'), mysql.CLIENT_QUERY_ATTRIBUTES)
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
			_, _ = DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES)
		}()
	}
}

// FuzzDecodeQuery: no input panics the decoder (go test runs the seeds; go
// test -fuzz explores from them).
func FuzzDecodeQuery(f *testing.F) {
	f.Add(queryAttrPacket(0x00, 0x01, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'))
	f.Add(queryAttrPacket(0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x01, 0x00, 0x01, 'x'))
	f.Add(queryAttrPacket(0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeVarString), 0x00, 0xfe, 0, 0, 0, 0, 0, 0, 0, 0x80, 'x'))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = DecodeQuery(context.Background(), zap.NewNop(), data, mysql.CLIENT_QUERY_ATTRIBUTES)
	})
}
