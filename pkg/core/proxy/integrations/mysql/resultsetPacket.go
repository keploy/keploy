//go:build linux

package mysql

import (
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

func parseResultSet(b []byte, isBinary bool) (*models.MySQLResultSet, error) {

	columns := make([]*models.ColumnDefinition, 0)
	rows := make([]*models.Row, 0)
	var err error
	var eofPresent, isBinaryResultSet bool
	var eofAfterColumns []byte
	var eofAfterRows []byte

	// Parse the column count packet
	columnCount, _, n := readLengthEncodedInteger(b)
	if n == 0 {
		return nil, errors.New("invalid column count")
	}

	// Move the buffer forward by the length of the column count packet
	b = b[n:]

	// Parse the columns
	for i := uint64(0); i < columnCount; i++ {
		var columnPacket *models.ColumnDefinition
		columnPacket, pos, err := parseColumnDefinitionPacket(b)
		if err != nil {
			return nil, err
		}
		b = b[pos:]
		columns = append(columns, columnPacket)
	}

	// Check for EOF packet after columns
	if isEOFPacket(b) {
		eofPresent = true
		eofAfterColumns = b[:9]
		b = b[9:] // Skip the EOF packet
	}

	// Parse the rows
	for len(b) > 0 {
		// Check for EOF packet after columns
		if isEOFPacket(b) {
			eofAfterRows = b[:9]
			// b = b[9:] // Skip the EOF packet
			println("Detected EOF packet after rows")
			break
		}

		var row *models.Row
		if isBinary {
			isBinaryResultSet = true
			println("\n ----- (Binary Row Parsing) STARTED ---- \n")
			row, n, err = parseBinaryRow(b, columns)
			println("\n ----- (Binary Row Parsing) ENDED ---- \n")
		} else {
			row, n, err = parseTextRow(b, columns)
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
		b = b[n:]
	}

	return &models.MySQLResultSet{
		Columns:               columns,
		EOFPresentAfterColumn: eofPresent,
		EOFAfterColumns:       eofAfterColumns,
		EOFAfterRows:          eofAfterRows,
		Rows:                  rows,
		IsBinaryResultSet:     isBinaryResultSet,
	}, nil
}
