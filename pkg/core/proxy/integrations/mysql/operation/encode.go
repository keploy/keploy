//go:build linux

package operation

import (
	"context"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

/*
    1.  MySQLStructToBytes
	2.	EncodeMySQLStruct
	3.	MySQLPacketToBytes
	4.	MarshalMySQLPacket
	5.	ConvertMySQLToBytes
	6.	SerializeMySQLPacket
	7.	EncodeMySQLData
	8.	MySQLDataToBytes
	9.	PackMySQLBytes
	10.	StructToMySQLBytes
*/

func EncodeToBinary(ctx context.Context, logger *zap.Logger, packet *mysql.PacketBundle) ([]byte, error) {

	
	return nil, nil
}
