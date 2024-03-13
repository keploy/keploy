package mysqlparser

import (
	"errors"
	"strings"
)

type ComStmtPreparePacket struct {
	Query string `json:"query,omitempty" yaml:"query,omitempty,flow"`
}

func decodeComStmtPrepare(data []byte) (*ComStmtPreparePacket, error) {
	if len(data) < 1 {
		return nil, errors.New("data too short for COM_STMT_PREPARE")
	}
	// data[1:] will skip the command byte and leave the query string
	query := string(data[1:])
	query = strings.ReplaceAll(query, "\t", "")
	return &ComStmtPreparePacket{Query: query}, nil
}
