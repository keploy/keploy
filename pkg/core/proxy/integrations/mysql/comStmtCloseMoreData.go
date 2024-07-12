//go:build linux

package mysql

import (
	"encoding/binary"
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtCloseAndPrepare(data []byte) (*models.ComStmtCloseAndPrepare, error) {
	if len(data) < 10 {
		return nil, errors.New("data too short for COM_STMT_CLOSE and COM_STMT_PREPARE with header")
	}
	status := data[0]

	// Decode statement ID for COM_STMT_CLOSE
	statementID := binary.LittleEndian.Uint32(data[1:5])

	// Extract the header for COM_STMT_PREPARE
	prepareHeader := data[5:9]

	// Get the query string after the header
	query := string(data[10:])
	query = strings.ReplaceAll(query, "\t", "")

	return &models.ComStmtCloseAndPrepare{
		StmtClose: models.ComStmtClosePacket{
			Status:      status,
			StatementID: statementID,
		},
		StmtPrepare: models.ComStmtPreparePacket1{
			Header: prepareHeader,
			Query:  query,
		},
	}, nil
}

func decodeComStmtCloseAndQuery(data []byte) (*models.ComStmtCloseAndQuery, error) {

	if len(data) < 10 {
		return nil, errors.New("data too short for COM_STMT_CLOSE and COM_QUERY with header")
	}
	status := data[0]

	// Decode statement ID for COM_STMT_CLOSE
	statementID := binary.LittleEndian.Uint32(data[1:5])

	// Extract the header for COM_STMT_PREPARE
	//[5:9]

	packet := &models.MySQLQueryPacket{}
	packet.Command = data[9]
	packet.Query = string(data[10:])

	return &models.ComStmtCloseAndQuery{
		StmtClose: models.ComStmtClosePacket{
			Status:      status,
			StatementID: statementID,
		},
		StmtQuery: models.MySQLQueryPacket{
			Command: packet.Command,
			Query:   packet.Query,
		},
	}, nil

}
