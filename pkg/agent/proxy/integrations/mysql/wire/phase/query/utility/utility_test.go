package utility

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

func TestDecodeInitDb_DecodesTheSchemaName(t *testing.T) {
	// COM_INIT_DB (0x02), then the schema name to the end of the packet.
	got, err := DecodeInitDb(context.Background(), []byte{0x02, 't', 'e', 's', 't', 'd', 'b'})
	require.NoError(t, err)
	assert.Equal(t, mysql.InitDBPacket{Command: 0x02, Schema: "testdb"}, *got)
}

func TestDecodeSetOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want mysql.SetOptionPacket
	}{
		// COM_SET_OPTION (0x1b), then the option as a 2-byte integer.
		{"MYSQL_OPTION_MULTI_STATEMENTS_ON", []byte{0x1b, 0x00, 0x00}, mysql.SetOptionPacket{Status: 0x1b, Option: 0}},
		{"MYSQL_OPTION_MULTI_STATEMENTS_OFF", []byte{0x1b, 0x01, 0x00}, mysql.SetOptionPacket{Status: 0x1b, Option: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeSetOption(context.Background(), tc.data)
			require.NoError(t, err)
			assert.Equal(t, tc.want, *got)
		})
	}
}

func TestDecodeSetOption_AShortPayloadIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"the command alone", []byte{0x1b}},
		{"the option cut to one byte", []byte{0x1b, 0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeSetOption(context.Background(), tc.data)
			assert.Error(t, err)
		})
	}
}
