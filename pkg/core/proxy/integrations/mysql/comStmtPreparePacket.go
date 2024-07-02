//go:build linux

package mysql

import (
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComStmtPrepare(data []byte) (*models.MySQLComStmtPreparePacket, error) {
	if len(data) < 1 {
		return nil, errors.New("data too short for COM_STMT_PREPARE")
	}
	// data[1:] will skip the command byte and leave the query string
	query := string(data[1:])
	query = strings.ReplaceAll(query, "\t", "")
	return &models.MySQLComStmtPreparePacket{Query: query}, nil
}
