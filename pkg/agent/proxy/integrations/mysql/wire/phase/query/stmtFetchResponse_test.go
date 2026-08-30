package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestEncodeStmtFetchResponsePreservesRowOnlyFraming(t *testing.T) {
	t.Parallel()

	response := &mysql.StmtFetchResponse{
		Rows: []*mysql.BinaryRow{
			{
				Header:        mysql.Header{SequenceID: 1},
				RowNullBuffer: []byte{0x00},
				Values: []mysql.ColumnEntry{{
					Type:  mysql.FieldTypeVarString,
					Name:  "name",
					Value: "seven",
				}},
			},
			{
				Header:        mysql.Header{SequenceID: 2},
				RowNullBuffer: []byte{0x00},
				Values: []mysql.ColumnEntry{{
					Type:  mysql.FieldTypeVarString,
					Name:  "name",
					Value: "eight",
				}},
			},
		},
		FinalResponse: &mysql.GenericResponse{
			Type: mysql.StatusToString(mysql.EOF),
			Data: []byte{0x05, 0x00, 0x00, 0x03, 0xfe, 0x00, 0x00, 0x82, 0x00},
		},
	}

	got, err := EncodeStmtFetchResponse(context.Background(), zap.NewNop(), response)
	require.NoError(t, err)

	// COM_STMT_FETCH returns binary rows directly. There is no column-count or
	// column-definition prefix because those were sent by COM_STMT_EXECUTE.
	want := []byte{
		0x08, 0x00, 0x00, 0x01, 0x00, 0x00, 0x05, 's', 'e', 'v', 'e', 'n',
		0x08, 0x00, 0x00, 0x02, 0x00, 0x00, 0x05, 'e', 'i', 'g', 'h', 't',
		0x05, 0x00, 0x00, 0x03, 0xfe, 0x00, 0x00, 0x82, 0x00,
	}
	require.Equal(t, want, got)
}

func TestEncodeStmtFetchResponseRequiresTerminator(t *testing.T) {
	t.Parallel()

	_, err := EncodeStmtFetchResponse(context.Background(), zap.NewNop(), &mysql.StmtFetchResponse{})
	require.EqualError(t, err, "encode COM_STMT_FETCH response: final response is missing")
}
