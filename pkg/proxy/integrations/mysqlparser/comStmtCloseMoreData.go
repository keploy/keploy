package mysqlparser

import (
	"encoding/binary"
	"errors"
	"strings"
)

type ComStmtPreparePacket1 struct {
	Header []byte `json:"header,omitempty" yaml:"header,omitempty"`
	Query  string `json:"query,omitempty" yaml:"query,omitempty"`
}

type ComStmtCloseAndPrepare struct {
	StmtClose   ComStmtClosePacket    `json:"stmt_close,omitempty" yaml:"stmt_close,omitempty"`
	StmtPrepare ComStmtPreparePacket1 `json:"stmt_prepare,omitempty" yaml:"stmt_prepare,omitempty"`
}

func decodeComStmtCloseMoreData(data []byte) (*ComStmtCloseAndPrepare, error) {
	if len(data) < 10 {
		return nil, errors.New("data too short for COM_STMT_CLOSE and COM_STMT_PREPARE with header")
	}
	status := data[0]

	// Decode statement ID for COM_STMT_CLOSE
	statementID := binary.LittleEndian.Uint32(data[1:])

	// Extract the header for COM_STMT_PREPARE
	prepareHeader := data[5:9]

	// Get the query string after the header
	query := string(data[10:])
	query = strings.ReplaceAll(query, "\t", "")

	return &ComStmtCloseAndPrepare{
		StmtClose: ComStmtClosePacket{
			Status:      status,
			StatementID: statementID,
		},
		StmtPrepare: ComStmtPreparePacket1{
			Header: prepareHeader,
			Query:  query,
		},
	}, nil
}
