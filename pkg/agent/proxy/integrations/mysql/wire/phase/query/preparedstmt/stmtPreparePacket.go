package preparedstmt

import (
	"context"
	"strings"

	"github.com/keploy/keploy-integrations-shared/pkg/models/mysql"
)

//COM_STMT_PREPARE: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html

func DecodeStmtPrepare(_ context.Context, data []byte) (*mysql.StmtPreparePacket, error) {
	packet := &mysql.StmtPreparePacket{
		Command: data[0],
	}

	query := string(data[1:])
	packet.Query = strings.ReplaceAll(query, "\t", "")
	return packet, nil
}
