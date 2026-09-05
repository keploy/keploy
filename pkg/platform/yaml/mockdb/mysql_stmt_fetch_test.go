package mockdb

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestMySQLStmtFetchRoundTrip(t *testing.T) {
	t.Parallel()

	mock := &models.Mock{Name: "stmt-fetch", Kind: models.MySQL}
	mock.Spec.Metadata = map[string]string{"type": "mocks"}
	mock.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 9},
			Type:   mysql.CommandStatusToString(mysql.COM_STMT_FETCH),
		},
		Message: &mysql.StmtFetchPacket{Status: mysql.COM_STMT_FETCH, StatementID: 4, NumRows: 2},
	}}}
	mock.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: 6, SequenceID: 1},
			Type:   mysql.COM_STMT_FETCH_RESPONSE,
		},
		Message: &mysql.StmtFetchResponse{
			Rows: []*mysql.BinaryRow{{
				Header:        mysql.Header{PayloadLength: 6, SequenceID: 1},
				RowNullBuffer: []byte{0},
				Values:        []mysql.ColumnEntry{{Type: mysql.FieldTypeLong, Name: "id", Value: int32(7)}},
			}},
			FinalResponse: &mysql.GenericResponse{
				Type: mysql.StatusToString(mysql.EOF),
				Data: []byte{0x05, 0x00, 0x00, 0x02, 0xfe, 0x00, 0x00, 0x82, 0x00},
			},
		},
	}}}

	t.Run("yaml", func(t *testing.T) {
		doc, err := EncodeMock(mock, zap.NewNop())
		require.NoError(t, err)
		got, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
		require.NoError(t, err)
		assertStmtFetchRoundTrip(t, got)
	})

	t.Run("json", func(t *testing.T) {
		doc, ok, err := EncodeMockJSON(mock, zap.NewNop())
		require.NoError(t, err)
		require.True(t, ok)
		got, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, zap.NewNop())
		require.NoError(t, err)
		assertStmtFetchRoundTrip(t, got)
	})
}

func assertStmtFetchRoundTrip(t *testing.T, mocks []*models.Mock) {
	t.Helper()
	require.Len(t, mocks, 1)
	require.Len(t, mocks[0].Spec.MySQLRequests, 1)
	require.Len(t, mocks[0].Spec.MySQLResponses, 1)

	req, ok := mocks[0].Spec.MySQLRequests[0].Message.(*mysql.StmtFetchPacket)
	require.Truef(t, ok, "request type = %T", mocks[0].Spec.MySQLRequests[0].Message)
	require.Equal(t, uint32(4), req.StatementID)
	require.Equal(t, uint32(2), req.NumRows)

	resp, ok := mocks[0].Spec.MySQLResponses[0].Message.(*mysql.StmtFetchResponse)
	require.Truef(t, ok, "response type = %T", mocks[0].Spec.MySQLResponses[0].Message)
	require.Len(t, resp.Rows, 1)
	require.EqualValues(t, 7, resp.Rows[0].Values[0].Value)
	require.Equal(t, []byte{0x05, 0x00, 0x00, 0x02, 0xfe, 0x00, 0x00, 0x82, 0x00}, resp.FinalResponse.Data)
}
