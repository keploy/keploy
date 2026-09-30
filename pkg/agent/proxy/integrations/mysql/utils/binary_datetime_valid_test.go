package utils

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// These parsers are reached from three call sites, and only one of them decodes
// bytes the SERVER wrote:
//
//	rowscols/binaryProtocolRowPacket.go  - server -> client resultset rows
//	preparedstmt/stmtExecutePacket.go    - client -> server COM_STMT_EXECUTE params
//	query/queryPacket.go                 - client -> server query attributes
//
// A server row only ever carries DATE length 0 or 4, so a check written against
// rows alone looks correct and still rejects what real client drivers send. The
// cases below are the encodings drivers actually put on the wire; rejecting any
// of them breaks recording for those applications, and no rejection test can
// catch that.
func TestParseBinaryDate_ValidEncodings(t *testing.T) {
	tests := []struct {
		name    string
		hexData string
		want    interface{}
		wantN   int
		wantErr bool
	}{
		{
			name:    "zero date (length 0)",
			hexData: "00",
			want:    ZeroDateString,
			wantN:   1,
		},
		{
			name:    "length 4 - libmysqlclient, mysql-connector-python, Connector/J 8.0.23+",
			hexData: "04e807010f",
			want:    "2024-01-15",
			wantN:   5,
		},
		{
			// MariaDB Connector/J 2.x and 3.x LocalDateCodec, MySQL
			// Connector/J 5.1.x and 8.0.0-8.0.22 setDate. Time bytes zeroed.
			name:    "length 7 - MariaDB Connector/J, MySQL Connector/J <= 8.0.22",
			hexData: "07e807010f000000",
			want:    "2024-01-15",
			wantN:   8,
		},
		{
			// Connector/J 9.x setObject(String, MysqlType.DATE) routes through
			// writeDateTime and can carry time-of-day.
			name:    "length 7 with non-zero time - Connector/J 9 setObject",
			hexData: "07e807010f0a1e00",
			want:    "2024-01-15",
			wantN:   8,
		},
		{
			name:    "length 11 with microseconds",
			hexData: "0be807010f0a1e0040e20100",
			want:    "2024-01-15",
			wantN:   12,
		},
		// Rejections: a length that is not a protocol value must not be trusted
		// as the bytes-consumed count, or the next column reads from the wrong
		// offset.
		{name: "length 5 is not a protocol value", hexData: "05e807010f00", wantErr: true},
		// Widening from one accepted length to three also widens the window for
		// a garbage length byte to be trusted, so the fields are checked too -
		// otherwise this decodes to "65535-99-99" and lands in the mock.
		{name: "valid length but nonsense month and day", hexData: "07ffff6363631700", wantErr: true},
		{name: "month 13", hexData: "04e8070d0f", wantErr: true},
		{name: "day 0", hexData: "04e8070100", wantErr: true},
		// All-zero Y/M/D with a non-zero length is the zero date; still valid.
		{name: "zero date sent with length 4", hexData: "0400000000", want: "0000-00-00", wantN: 5},
		{name: "length 200 is garbage", hexData: "c8010203040506", wantErr: true},
		{name: "length 7 but payload truncated", hexData: "07e807010f00", wantErr: true},
		{name: "length 4 but payload truncated", hexData: "04e80701", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := hex.DecodeString(tt.hexData)
			require.NoError(t, err, "bad test fixture")

			got, n, err := ParseBinaryDate(b)
			if tt.wantErr {
				require.Error(t, err, "expected a decode error")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			// The consumed count is what keeps every later column aligned; a
			// wrong n desyncs the rest of the packet silently.
			require.Equal(t, tt.wantN, n, "bytes consumed")
		})
	}
}

func TestParseBinaryTime_ValidEncodings(t *testing.T) {
	tests := []struct {
		name    string
		hexData string
		want    interface{}
		wantN   int
		wantErr bool
	}{
		{name: "zero time (length 0)", hexData: "00", want: ZeroTimeString, wantN: 1},
		{
			// libmysqlclient store_param_time, Connector/J writeTime,
			// MariaDB Connector/J Duration/LocalTime codecs.
			// len | sign | days(4 LE) | h | m | s
			name:    "length 8 - no microseconds",
			hexData: "08" + "00" + "01000000" + "0a" + "1e" + "2d",
			want:    "1 10:30:45.000000",
			wantN:   9,
		},
		{
			name:    "length 12 - with microseconds",
			hexData: "0c" + "00" + "01000000" + "0a" + "1e" + "2d" + "40e20100",
			want:    "1 10:30:45.123456",
			wantN:   13,
		},
		{
			// The sign byte is its own field; without asserting the value a
			// flipped comparison here silently inverts every recorded TIME.
			name:    "negative interval",
			hexData: "08" + "01" + "01000000" + "0a" + "1e" + "2d",
			want:    "-1 10:30:45.000000",
			wantN:   9,
		},
		{name: "length 10 is not a protocol value", hexData: "0a00010000000a1e2d00", wantErr: true},
		{name: "length 12 but payload truncated", hexData: "0c00010000000a1e2d", wantErr: true},
		{name: "length 8 but payload truncated", hexData: "0800010000", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := hex.DecodeString(tt.hexData)
			require.NoError(t, err, "bad test fixture")

			got, n, err := ParseBinaryTime(b)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.wantN, n, "bytes consumed")
		})
	}
}
