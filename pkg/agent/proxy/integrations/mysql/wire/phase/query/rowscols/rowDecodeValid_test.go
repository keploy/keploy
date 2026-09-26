package rowscols

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func col(name string, t byte) *mysql.ColumnDefinition41 {
	return &mysql.ColumnDefinition41{Name: name, Type: t}
}

// buildRow wraps buildBinaryRow (binaryProtocolRowPacket_float_test.go) with an
// explicit NULL bitmap. Hand-writing the 4-byte header is not worth it: the
// payload length has to count the 0x00 marker and the bitmap as well as the
// values, and getting it wrong is invisible today only because neither decoder
// validates it yet.
func buildRow(t *testing.T, bitmap []byte, values []byte) []byte {
	t.Helper()
	body := make([]byte, 0, 1+len(bitmap)+len(values))
	body = append(body, 0x00)
	body = append(body, bitmap...)
	body = append(body, values...)

	pkt := make([]byte, 4, 4+len(body))
	pkt[0] = byte(len(body))
	pkt[1] = byte(len(body) >> 8)
	pkt[2] = byte(len(body) >> 16)
	pkt[3] = 1 // sequence id
	return append(pkt, body...)
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err, "bad test fixture")
	return b
}

// The bounds checks added for truncated packets sit on the same code path as
// every valid row, so a well-formed row still has to decode. Asserting the
// returned offset equals len(data) is the part that matters most: a check that
// consumes the wrong number of bytes leaves the rest of the resultset
// misaligned, which is quieter and worse than a panic.
func TestDecodeBinaryRow_ValidRows(t *testing.T) {
	tests := []struct {
		name   string
		bitmap []byte
		values string
		cols   []*mysql.ColumnDefinition41
		want   []interface{}
	}{
		{
			name:   "DATE length 4 then TINY",
			bitmap: []byte{0x00},
			values: "04e807010f" + "2a",
			cols:   []*mysql.ColumnDefinition41{col("d", byte(mysql.FieldTypeDate)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{"2024-01-15", int8(42)},
		},
		{
			// The 7-byte form real drivers send; only reachable on the client
			// parameter path in practice, but the row decoder shares the parser.
			name:   "DATE length 7 then TINY",
			bitmap: []byte{0x00},
			values: "07e807010f000000" + "2a",
			cols:   []*mysql.ColumnDefinition41{col("d", byte(mysql.FieldTypeDate)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{"2024-01-15", int8(42)},
		},
		{
			name:   "zero DATE then TINY",
			bitmap: []byte{0x00},
			values: "00" + "2a",
			cols:   []*mysql.ColumnDefinition41{col("d", byte(mysql.FieldTypeDate)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{"0000-00-00", int8(42)},
		},
		{
			name:   "TIME length 8 then TINY",
			bitmap: []byte{0x00},
			values: "0800010000000a1e2d" + "2a",
			cols:   []*mysql.ColumnDefinition41{col("tm", byte(mysql.FieldTypeTime)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{"1 10:30:45.000000", int8(42)},
		},
		{
			name:   "TIME length 12 with microseconds then TINY",
			bitmap: []byte{0x00},
			values: "0c00010000000a1e2d40e20100" + "2a",
			cols:   []*mysql.ColumnDefinition41{col("tm", byte(mysql.FieldTypeTime)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{"1 10:30:45.123456", int8(42)},
		},
		{
			// Bit 2 of the bitmap - isNull offsets by 2 - marks column 0 NULL.
			// A NULL consumes no value bytes, so TINY must still land correctly.
			name:   "NULL first column then TINY",
			bitmap: []byte{0x04},
			values: "2a",
			cols:   []*mysql.ColumnDefinition41{col("d", byte(mysql.FieldTypeDate)), col("t", byte(mysql.FieldTypeTiny))},
			want:   []interface{}{nil, int8(42)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildRow(t, tt.bitmap, mustHex(t, tt.values))

			row, n, err := DecodeBinaryRow(context.Background(), zap.NewNop(), data, tt.cols)
			require.NoError(t, err)
			require.Equal(t, len(data), n, "consumed count must cover the whole row or the resultset desyncs")
			require.Len(t, row.Values, len(tt.cols))
			for i, w := range tt.want {
				require.Equal(t, w, row.Values[i].Value, "column %d (%s)", i, tt.cols[i].Name)
			}
		})
	}
}

func TestDecodeTextRow_ValidRows(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		cols    []*mysql.ColumnDefinition41
		want    []interface{}
	}{
		{
			name:    "single short string",
			payload: "0161",
			cols:    []*mysql.ColumnDefinition41{col("a", byte(mysql.FieldTypeString))},
			want:    []interface{}{"a"},
		},
		{
			name:    "empty string then short string",
			payload: "00" + "0162",
			cols:    []*mysql.ColumnDefinition41{col("a", byte(mysql.FieldTypeString)), col("b", byte(mysql.FieldTypeString))},
			want:    []interface{}{"", "b"},
		},
		{
			// 0xfb is the text-protocol NULL marker. DecodeTextRow stores a nil
			// Value for it, not an empty string - the two are different on the
			// wire and must stay different in the mock.
			name:    "trailing NULL column",
			payload: "0161" + "fb",
			cols:    []*mysql.ColumnDefinition41{col("a", byte(mysql.FieldTypeString)), col("b", byte(mysql.FieldTypeString))},
			want:    []interface{}{"a", nil},
		},
		{
			name:    "leading NULL column",
			payload: "fb" + "0162",
			cols:    []*mysql.ColumnDefinition41{col("a", byte(mysql.FieldTypeString)), col("b", byte(mysql.FieldTypeString))},
			want:    []interface{}{nil, "b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := mustHex(t, tt.payload)
			data := make([]byte, 4, 4+len(payload))
			data[0] = byte(len(payload))
			data[3] = 1 // sequence id
			data = append(data, payload...)

			row, n, err := DecodeTextRow(context.Background(), zap.NewNop(), data, tt.cols)
			require.NoError(t, err)
			require.Equal(t, len(data), n, "consumed count must cover the whole row")
			require.Len(t, row.Values, len(tt.cols))
			for i, w := range tt.want {
				require.Equal(t, w, row.Values[i].Value, "column %d (%s)", i, tt.cols[i].Name)
			}
		})
	}
}
