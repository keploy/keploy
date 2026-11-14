package phase

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The capability sets that change how OK, ERR and EOF packets are laid out:
// a 4.1 client's (which also sets CLIENT_TRANSACTIONS), a pre-4.1 client's
// with CLIENT_TRANSACTIONS, and one with neither.
const (
	protocol41   = mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_TRANSACTIONS
	transactions = mysql.CLIENT_TRANSACTIONS
	neither      = uint32(0)
)

const (
	okInfo     = "Records: 1  Duplicates: 0  Warnings: 1"
	errMessage = "Table 'testdb.missing' doesn't exist"
)

func TestDecodeOk_DecodesEachFieldAndEncodesTheSameBytes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities uint32
		data         []byte
		want         mysql.OKPacket
	}{
		{
			name:         "protocol 41",
			capabilities: protocol41,
			// header, affected rows 1, last insert ID 42, status flags
			// SERVER_STATUS_AUTOCOMMIT, warnings 1, info
			data: append([]byte{0x00, 0x01, 0x2a, 0x02, 0x00, 0x01, 0x00}, okInfo...),
			want: mysql.OKPacket{Header: 0x00, AffectedRows: 1, LastInsertID: 42, StatusFlags: 0x0002, Warnings: 1, Info: okInfo},
		},
		{
			name:         "transactions without protocol 41: status flags, no warnings",
			capabilities: transactions,
			data:         append([]byte{0x00, 0x01, 0x2a, 0x02, 0x00}, okInfo...),
			want:         mysql.OKPacket{Header: 0x00, AffectedRows: 1, LastInsertID: 42, StatusFlags: 0x0002, Info: okInfo},
		},
		{
			name:         "neither: no status flags or warnings",
			capabilities: neither,
			data:         append([]byte{0x00, 0x01, 0x2a}, okInfo...),
			want:         mysql.OKPacket{Header: 0x00, AffectedRows: 1, LastInsertID: 42, Info: okInfo},
		},
		{
			name:         "affected rows in the 3-byte form",
			capabilities: protocol41,
			data:         []byte{0x00, 0xfd, 0x56, 0x34, 0x12, 0x2a, 0x02, 0x00, 0x00, 0x00},
			want:         mysql.OKPacket{Header: 0x00, AffectedRows: 0x123456, LastInsertID: 42, StatusFlags: 0x0002},
		},
		{
			name:         "affected rows in the 8-byte form",
			capabilities: protocol41,
			data:         []byte{0x00, 0xfe, 0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x2a, 0x02, 0x00, 0x00, 0x00},
			want:         mysql.OKPacket{Header: 0x00, AffectedRows: 0x0102030405060708, LastInsertID: 42, StatusFlags: 0x0002},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeOk(context.Background(), tc.data, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)

			encoded, err := EncodeOk(context.Background(), got, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.data, encoded)
		})
	}
}

func TestDecodeERR_DecodesEachFieldAndEncodesTheSameBytes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities uint32
		data         []byte
		want         mysql.ERRPacket
	}{
		{
			name:         "protocol 41: SQL state marker and state",
			capabilities: protocol41,
			// header, error code 1146 (ER_NO_SUCH_TABLE), marker, state, message
			data: append([]byte{0xff, 0x7a, 0x04, '#', '4', '2', 'S', '0', '2'}, errMessage...),
			want: mysql.ERRPacket{Header: 0xff, ErrorCode: 1146, SQLStateMarker: "#", SQLState: "42S02", ErrorMessage: errMessage},
		},
		{
			name:         "without protocol 41: no SQL state",
			capabilities: neither,
			data:         append([]byte{0xff, 0x7a, 0x04}, errMessage...),
			want:         mysql.ERRPacket{Header: 0xff, ErrorCode: 1146, ErrorMessage: errMessage},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeERR(context.Background(), tc.data, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)

			encoded, err := EncodeErr(context.Background(), got, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.data, encoded)
		})
	}
}

func TestDecodeEOF_DecodesEachFieldAndEncodesTheSameBytes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capabilities uint32
		data         []byte
		want         mysql.EOFPacket
	}{
		{
			name:         "protocol 41",
			capabilities: protocol41,
			// header, warnings 0, status flags SERVER_MORE_RESULTS_EXISTS
			data: []byte{0xfe, 0x00, 0x00, 0x08, 0x00},
			want: mysql.EOFPacket{Header: 0xfe, Warnings: 0, StatusFlags: 0x0008},
		},
		{
			name:         "without protocol 41: the header alone",
			capabilities: neither,
			data:         []byte{0xfe},
			want:         mysql.EOFPacket{Header: 0xfe},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeEOF(context.Background(), tc.data, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)

			encoded, err := EncodeEOF(context.Background(), got, tc.capabilities)
			require.NoError(t, err)
			assert.Equal(t, tc.data, encoded)
		})
	}
}

// A packet cut short where one of its fields ends is an error, never a panic.
// Each cut's capacity is its length, so a read past the end panics instead of
// reading the rest of the packet.
func TestDecodeGenericPackets_CutShortIsAnError(t *testing.T) {
	ctx := context.Background()
	decodeOk := func(data []byte, capabilities uint32) error {
		_, err := DecodeOk(ctx, data, capabilities)
		return err
	}
	decodeERR := func(data []byte, capabilities uint32) error {
		_, err := DecodeERR(ctx, data, capabilities)
		return err
	}
	decodeEOF := func(data []byte, capabilities uint32) error {
		_, err := DecodeEOF(ctx, data, capabilities)
		return err
	}

	// Affected rows in the 8-byte form make every cut past them longer than
	// the shortest OK packet, 7 bytes. Header (0), affected rows (1-9), last
	// insert ID (10), status flags (11-12), warnings (13-14).
	ok := []byte{0x00, 0xfe, 0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x2a, 0x02, 0x00, 0x00, 0x00}
	// Header (0), error code (1-2), marker (3), state (4-8), message.
	err41 := append([]byte{0xff, 0x7a, 0x04, '#', '4', '2', 'S', '0', '2'}, errMessage...)
	errPre41 := append([]byte{0xff, 0x7a, 0x04}, errMessage...)
	// Header (0), warnings (1-2), status flags (3-4).
	eof := []byte{0xfe, 0x00, 0x00, 0x08, 0x00}

	for _, tc := range []struct {
		name         string
		decode       func([]byte, uint32) error
		capabilities uint32
		packet       []byte
		cuts         []int
	}{
		// Before and after each field but the last, and inside the
		// affected rows' 8-byte value.
		{"OK under protocol 41", decodeOk, protocol41, ok, []int{0, 1, 9, 10, 11, 13}},
		{"OK under transactions", decodeOk, transactions, ok[:13], []int{0, 1, 10, 11}},
		{"OK under neither", decodeOk, neither, ok[:11], []int{0, 1, 10}},
		{"ERR under protocol 41", decodeERR, protocol41, err41, []int{0, 1, 3, 4}},
		{"ERR without protocol 41", decodeERR, neither, errPre41, []int{0, 1}},
		{"EOF under protocol 41", decodeEOF, protocol41, eof, []int{0, 1, 3}},
		{"EOF without protocol 41", decodeEOF, neither, eof[:1], []int{0}},
	} {
		for _, n := range tc.cuts {
			t.Run(fmt.Sprintf("%s cut to %d bytes", tc.name, n), func(t *testing.T) {
				data := tc.packet[:n:n]
				var err error
				require.NotPanics(t, func() { err = tc.decode(data, tc.capabilities) })
				assert.Error(t, err)
			})
		}
	}
}
