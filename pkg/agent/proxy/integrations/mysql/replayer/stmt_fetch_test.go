package replayer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestSimulateCommandPhaseReplaysStmtFetchRowsWithoutMetadata(t *testing.T) {
	const (
		query          = "SELECT id FROM cursor_rows ORDER BY id"
		otherQuery     = "SELECT id FROM other_rows ORDER BY id"
		recordedStmtID = uint32(7)
		otherStmtID    = uint32(8)
		runtimeStmtID  = uint32(99)
	)

	otherPrepare := stmtFetchPrepareMock("prepare-other", "conn-1", otherQuery, otherStmtID)
	otherFetch := stmtFetchMock("fetch-other", "conn-1", otherStmtID, 2, int32(99), 0x0082)
	prepare := stmtFetchPrepareMock("prepare", "conn-1", query, recordedStmtID)
	firstFetch := stmtFetchMock("fetch-1", "conn-1", recordedStmtID, 2, int32(7), 0x0042)
	secondFetch := stmtFetchMock("fetch-2", "conn-1", recordedStmtID, 2, int32(8), 0x0082)
	db := &fakeMockDb{perTest: []*models.Mock{otherPrepare, otherFetch, prepare, firstFetch, secondFetch}}

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	decodeCtx := &wire.DecodeContext{
		Mode:               models.MODE_TEST,
		LastOp:             wire.NewLastOpMap(),
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{runtimeStmtID: {StatementID: runtimeStmtID}},
		ServerGreetings:    wire.NewGreetings(),
		StmtIDToQuery:      map[uint32]string{runtimeStmtID: query},
	}
	decodeCtx.LastOp.Store(serverConn, wire.RESET)
	decodeCtx.ServerGreetings.Store(serverConn, &mysql.HandshakeV10Packet{
		CapabilityFlags: mysql.CLIENT_PROTOCOL_41,
	})

	done := make(chan error, 1)
	go func() {
		done <- simulateCommandPhase(context.Background(), zap.NewNop(), serverConn, db, decodeCtx, models.OutgoingOptions{})
	}()

	require.NoError(t, clientConn.SetDeadline(time.Now().Add(2*time.Second)))
	writeFetchAndRequireResponse(t, clientConn, runtimeStmtID, 2, []byte{
		0x06, 0x00, 0x00, 0x01, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x02, 0xfe, 0x00, 0x00, 0x42, 0x00,
	})
	writeFetchAndRequireResponse(t, clientConn, runtimeStmtID, 2, []byte{
		0x06, 0x00, 0x00, 0x01, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x02, 0xfe, 0x00, 0x00, 0x82, 0x00,
	})
	require.Equal(t, []string{"fetch-1", "fetch-2"}, db.deletedFil)

	require.NoError(t, clientConn.Close())
	commandErr := <-done
	require.True(t, errors.Is(commandErr, io.EOF) || errors.Is(commandErr, io.ErrClosedPipe), "command phase error = %v", commandErr)
}

func writeFetchAndRequireResponse(t *testing.T, conn net.Conn, statementID, numRows uint32, want []byte) {
	t.Helper()
	command := make([]byte, 13)
	command[0] = 0x09
	command[4] = mysql.COM_STMT_FETCH
	binary.LittleEndian.PutUint32(command[5:9], statementID)
	binary.LittleEndian.PutUint32(command[9:13], numRows)
	_, err := conn.Write(command)
	require.NoError(t, err)

	got := make([]byte, len(want))
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func stmtFetchPrepareMock(name, connID, query string, stmtID uint32) *models.Mock {
	mock := &models.Mock{Name: name, Kind: models.MySQL}
	mock.TestModeInfo.Lifetime = models.LifetimeConnection
	mock.Spec.Metadata = map[string]string{"type": "connection", "connID": connID}
	mock.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(query) + 1)}, Type: mysql.CommandStatusToString(mysql.COM_STMT_PREPARE)},
		Message: &mysql.StmtPreparePacket{Command: mysql.COM_STMT_PREPARE, Query: query},
	}}}
	mock.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
		Message: &mysql.StmtPrepareOkPacket{StatementID: stmtID},
	}}}
	return mock
}

func stmtFetchMock(name, connID string, stmtID, numRows uint32, value int32, finalStatus uint16) *models.Mock {
	mock := &models.Mock{Name: name, Kind: models.MySQL}
	mock.TestModeInfo.Lifetime = models.LifetimePerTest
	mock.Spec.Metadata = map[string]string{"type": "mocks", "connID": connID}
	mock.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 9},
			Type:   mysql.CommandStatusToString(mysql.COM_STMT_FETCH),
		},
		Message: &mysql.StmtFetchPacket{Status: mysql.COM_STMT_FETCH, StatementID: stmtID, NumRows: numRows},
	}}}
	mock.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 6, SequenceID: 1},
			Type:   mysql.COM_STMT_FETCH_RESPONSE,
		},
		Message: &mysql.StmtFetchResponse{
			Rows: []*mysql.BinaryRow{{
				Header:        mysql.Header{PayloadLength: 6, SequenceID: 1},
				RowNullBuffer: []byte{0x00},
				Values: []mysql.ColumnEntry{{
					Type:  mysql.FieldTypeLong,
					Name:  "id",
					Value: value,
				}},
			}},
			FinalResponse: &mysql.GenericResponse{
				Type: mysql.StatusToString(mysql.EOF),
				Data: []byte{0x05, 0x00, 0x00, 0x02, 0xfe, 0x00, 0x00, byte(finalStatus), byte(finalStatus >> 8)},
			},
		},
	}}}
	return mock
}
