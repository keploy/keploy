package preparedstmt

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_EXECUTE carries parameters the CLIENT DRIVER encoded, so the
// accepted DATE encodings are the drivers', not the server's. A server binary
// resultset row only ever uses length 0 or 4; several mainstream drivers bind a
// DATE parameter with the 7-byte form. Decoding these at the packet layer is
// what makes a regression here visible: when this decode fails during record,
// recorder/record_v2.go logs at Debug and continues WITHOUT draining the
// server's response, so every later mock on the connection is paired with the
// wrong response and nothing surfaces above Debug.
//
// Payloads below are hand-assembled from the protocol and match captures taken
// against MySQL 8.3 with useServerPrepStmts=true. They contain no customer data.
func TestDecodeStmtExecute_DateParamEncodings(t *testing.T) {
	const stmtID = 2

	// COM_STMT_EXECUTE header for one parameter:
	//   17            COM_STMT_EXECUTE
	//   02000000      statement id = 2
	//   00            flags
	//   01000000      iteration count = 1
	//   00            NULL bitmap (1 param, not null)
	//   01            new-params-bound flag
	//   0a00          param type = 0x0a MYSQL_TYPE_DATE, unsigned = 0
	const header = "170200000000010000000001" + "0a00"

	tests := []struct {
		name    string
		payload string
		want    interface{}
	}{
		{
			name:    "length 4 - libmysqlclient / Connector/J 8.0.23+",
			payload: "04e807010f",
			want:    "2024-01-15",
		},
		{
			name:    "length 7 - MariaDB Connector/J LocalDate, MySQL Connector/J <= 8.0.22 setDate",
			payload: "07e807010f000000",
			want:    "2024-01-15",
		},
		{
			name:    "length 7 with time - Connector/J 9 setObject(String, DATE)",
			payload: "07e807010f0a1e00",
			want:    "2024-01-15",
		},
		{
			name:    "length 11 with microseconds",
			payload: "0be807010f0a1e0040420f00",
			want:    "2024-01-15",
		},
		{
			name:    "length 0 - zero date",
			payload: "00",
			want:    "0000-00-00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := hex.DecodeString(header + tt.payload)
			require.NoError(t, err, "bad test fixture")

			pkt, err := DecodeStmtExecute(
				context.Background(),
				zap.NewNop(),
				data,
				map[uint32]*mysql.StmtPrepareOkPacket{
					stmtID: {StatementID: stmtID, NumParams: 1},
				},
				0,
				map[uint16]bool{},
			)
			require.NoError(t, err, "a DATE parameter real drivers send must decode, not error")
			require.Len(t, pkt.Parameters, 1)
			require.Equal(t, tt.want, pkt.Parameters[0].Value)
		})
	}
}

// A DATE followed by another parameter catches a wrong bytes-consumed count,
// which a single-parameter test cannot: the wrong n only shows up as the NEXT
// parameter reading from the wrong offset.
func TestDecodeStmtExecute_DateParamKeepsOffsetAligned(t *testing.T) {
	const stmtID = 3

	//   17 03000000 00 01000000   header
	//   00                        NULL bitmap (2 params)
	//   01                        new-params-bound
	//   0a00 0300                 types: DATE, MYSQL_TYPE_LONG
	//   07 e807010f 000000        DATE, length 7
	//   d2040000                  LONG = 1234
	data, err := hex.DecodeString("17030000000001000000" + "00" + "01" + "0a00" + "0300" +
		"07e807010f000000" + "d2040000")
	require.NoError(t, err)

	pkt, err := DecodeStmtExecute(
		context.Background(), zap.NewNop(), data,
		map[uint32]*mysql.StmtPrepareOkPacket{stmtID: {StatementID: stmtID, NumParams: 2}},
		0, map[uint16]bool{},
	)
	require.NoError(t, err)
	require.Len(t, pkt.Parameters, 2)
	require.Equal(t, "2024-01-15", pkt.Parameters[0].Value)
	require.EqualValues(t, 1234, pkt.Parameters[1].Value,
		"the LONG after the DATE decoded from the wrong offset - ParseBinaryDate returned the wrong consumed count")
}
