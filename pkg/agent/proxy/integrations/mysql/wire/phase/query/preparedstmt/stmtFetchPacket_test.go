package preparedstmt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestDecodeStmtFetch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    []byte
		want    *mysql.StmtFetchPacket
		wantErr string
	}{
		{
			name: "valid packet",
			data: []byte{0x1c, 0x78, 0x56, 0x34, 0x12, 0x20, 0x00, 0x00, 0x00},
			want: &mysql.StmtFetchPacket{
				Status:      0x1c,
				StatementID: 0x12345678,
				NumRows:     32,
			},
		},
		{
			name:    "truncated packet",
			data:    []byte{0x1c, 0x01, 0x00, 0x00, 0x00},
			wantErr: "data too short for COM_STMT_FETCH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := DecodeStmtFetch(context.Background(), zap.NewNop(), tt.data)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
