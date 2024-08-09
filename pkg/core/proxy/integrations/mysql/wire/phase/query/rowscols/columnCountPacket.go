//go:build linux

package rowscols

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.uber.org/zap"
)

func DecodeColumnCount(_ context.Context, _ *zap.Logger, data []byte) (uint64, error) {

	// offset := 0
	// pkt := &mysql.ColumnCount{
	// 	Header: mysql.Header{
	// 		PayloadLength: utils.GetPayloadLength(data[:3]),
	// 		SequenceID:    data[3],
	// 	},
	// }
	// offset += 4
	// data = data[offset:]

	// Parse the column count packet
	columnCount, _, n := utils.ReadLengthEncodedInteger(data)
	if n == 0 {
		return 0, errors.New("invalid column count")
	}

	return columnCount, nil
}
