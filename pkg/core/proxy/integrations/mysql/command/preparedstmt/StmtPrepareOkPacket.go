//go:build linux

package preparedstmt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/rowscols"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// COM_STMT_PREPARE_OK: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_stmt_prepare.html#sect_protocol_com_stmt_prepare_response_ok

func DecodePrepareOk(ctx context.Context, logger *zap.Logger, data []byte) (*mysql.StmtPrepareOkPacket, error) {
	if len(data) < 12 {
		return nil, errors.New("data length is not enough for COM_STMT_PREPARE_OK")
	}

	offset := 0

	response := &mysql.StmtPrepareOkPacket{}

	response.Status = data[offset]
	offset++

	response.StatementID = binary.LittleEndian.Uint32(data[offset : offset+4])
	offset += 4

	response.NumColumns = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	response.NumParams = binary.LittleEndian.Uint16(data[offset : offset+2])

	offset += 2

	//data[10] is reserved byte ([00] filler)
	response.Filler = data[offset]
	offset++

	response.WarningCount = binary.LittleEndian.Uint16(data[offset : offset+2])
	offset += 2

	data = data[offset:]
	fmt.Printf("NumColumns: %d\n", response.NumColumns)
	fmt.Printf("NumParams: %d\n", response.NumParams)
	fmt.Printf("Filler: %x\n", response.Filler)
	fmt.Printf("WarningCount: %d\n", response.WarningCount)
	if response.NumParams > 0 {

		for i := uint16(0); i < response.NumParams; i++ {
			column, n, err := rowscols.DecodeColumn(ctx, logger, data)
			if err != nil {
				return nil, err
			}
			fmt.Printf("From column: %x\n", column.Header)

			response.ParamDefs = append(response.ParamDefs, *column)
			data = data[n:]
		}
		offset = 0
		response.EOFAfterParamDefs = data[offset : offset+9]
		fmt.Printf("EOFAfterParamDefs %d: %x\n", response.EOFAfterParamDefs)

		offset += 9 //skip EOF packet for Parameter Definition
		fmt.Printf("params data", data[:offset])
		data = data[offset:]
	}

	if response.NumColumns > 0 {
		offset = 0
		for i := uint16(0); i < response.NumColumns; i++ {
			column, n, err := rowscols.DecodeColumn(ctx, logger, data)
			if err != nil {
				return nil, err
			}
			response.ColumnDefs = append(response.ColumnDefs, *column)
			offset += n
			fmt.Printf("Decoded ColumnDef %d: %+v\n", i, column)

		}
		fmt.Printf("data for eof %d", data[offset:])
		response.EOFAfterColumnDefs = data[offset:]
		fmt.Printf("response.EOFAfterColumnDefs %d", response.EOFAfterColumnDefs)

		// offset += 9 //skip EOF packet for Column Definitions
		// data = data[offset:]
	}
	fmt.Println("\n"+"---------------", response.ParamDefs, "\n", "---------------")
	return response, nil
}

// EncodePrepareOk encodes a StmtPrepareOkPacket back into a byte slice.
func EncodePrepareOk(ctx context.Context, logger *zap.Logger, packet *mysql.StmtPrepareOkPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Status
	if err := buf.WriteByte(packet.Status); err != nil {
		return nil, fmt.Errorf("failed to write Status: %w", err)
	}

	// Write Statement ID
	if err := binary.Write(buf, binary.LittleEndian, packet.StatementID); err != nil {
		return nil, fmt.Errorf("failed to write StatementID: %w", err)
	}

	// Write Number of Columns
	if err := binary.Write(buf, binary.LittleEndian, packet.NumColumns); err != nil {
		return nil, fmt.Errorf("failed to write NumColumns: %w", err)
	}

	// Write Number of Parameters
	if err := binary.Write(buf, binary.LittleEndian, packet.NumParams); err != nil {
		return nil, fmt.Errorf("failed to write NumParams: %w", err)
	}

	// Write Filler
	if err := buf.WriteByte(packet.Filler); err != nil {
		return nil, fmt.Errorf("failed to write Filler: %w", err)
	}

	// Write Warning Count
	if err := binary.Write(buf, binary.LittleEndian, packet.WarningCount); err != nil {
		return nil, fmt.Errorf("failed to write WarningCount: %w", err)
	}

	// Write ParamDefs if NumParams > 0
	if packet.NumParams > 0 {
		for _, param := range packet.ParamDefs {
			fmt.Println("Param def", param)
			paramBytes, err := rowscols.EncodeColumn(ctx, logger, &param)
			if err != nil {
				return nil, fmt.Errorf("failed to encode ParamDef: %w", err)
			}
			if _, err := buf.Write(paramBytes); err != nil {
				return nil, fmt.Errorf("failed to write ParamDef: %w", err)
			}
			fmt.Printf("Encoded Param %d: %v\n", paramBytes)

		}

		// Write EOF packet for Parameter Definition
		if _, err := buf.Write(packet.EOFAfterParamDefs); err != nil {
			return nil, fmt.Errorf("failed to write EOF packet for Parameter Definition: %w", err)
		}
	}

	// Write ColumnDefs if NumColumns > 0
	if packet.NumColumns > 0 {
		for i, column := range packet.ColumnDefs {
			columnBytes, err := rowscols.EncodeColumn(ctx, logger, &column)
			if err != nil {
				return nil, fmt.Errorf("failed to encode ColumnDef: %w", err)
			}
			if _, err := buf.Write(columnBytes); err != nil {
				return nil, fmt.Errorf("failed to write ColumnDef: %w", err)
			}
			// Add logging for each encoded column
			fmt.Printf("Encoded ColumnDef %d: %v\n", i, columnBytes)
		}

		// Write EOF packet for Column Definitions
		if _, err := buf.Write(packet.EOFAfterColumnDefs); err != nil {
			return nil, fmt.Errorf("failed to write EOF packet for Column Definitions: %w", err)
		}
		// Add logging for EOF packet
		fmt.Printf("EOF After ColumnDefs: %v\n", packet.EOFAfterColumnDefs)
	}

	return buf.Bytes(), nil
}

// func writeEOFPacket(buf *bytes.Buffer) error {
// 	// Write EOF Packet
// 	eofPacket := []byte{0xfe, 0x00, 0x00, 0x02, 0x00} // {0xfe, warning_count (2 bytes), status_flags (2 bytes)}
// 	if _, err := buf.Write(eofPacket); err != nil {
// 		return errors.New("failed to write EOF packet")
// 	}
// 	return nil
// }

// TestDecodeEncode tests the decoding and encoding of a StmtPrepareOkPacket.
func TestDecodeEncode(ctx context.Context, logger *zap.Logger, original []byte, decodeFunc func(context.Context, *zap.Logger, []byte) (*mysql.StmtPrepareOkPacket, error), encodeFunc func(context.Context, *zap.Logger, *mysql.StmtPrepareOkPacket) ([]byte, error)) bool {
	// Decode the original data
	decoded, err := decodeFunc(ctx, logger, original)
	if err != nil {
		fmt.Printf("Decoding failed: %v\n", err)
		return false
	}

	// Encode the decoded data
	encoded, err := encodeFunc(ctx, logger, decoded)
	if err != nil {
		fmt.Printf("Encoding failed: %v\n", err)
		return false
	}

	// Compare the original and encoded data
	if bytes.Equal(original, encoded) {
		//fmt.Println("Test passed: Decoded and encoded values match")
		//fmt.Printf("Decoded: %+v\nEncoded: %v\n", decoded, encoded)
		return true
	} else {
		//fmt.Println("Test failed: Decoded and encoded values do not match")
		fmt.Printf("Original: %v\nEncoded: %v\n", original, encoded)
		return false
	}
}

func main() {
	logger, _ := zap.NewDevelopment()
	ctx := context.Background()

	// Original StmtPrepareOkPacket data (example, you need to replace it with actual data)
	originalData := []byte{
		0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x00, 0x00, 0x00, 0x00, 0x17, 0x00, 0x00, 0x02, 0x03, 0x64, 0x65, 0x66, 0x00, 0x00, 0x00, 0x01,
		0x3f, 0x00, 0x0c, 0x21, 0x00, 0xfd, 0xbf, 0x00, 0x00, 0xfd, 0x00, 0x00, 0x1f, 0x00, 0x00, 0x17,
		0x00, 0x00, 0x03, 0x03, 0x64, 0x65, 0x66, 0x00, 0x00, 0x00, 0x01, 0x3f, 0x00, 0x0c, 0x21, 0x00,
		0xfd, 0xbf, 0x00, 0x00, 0xfd, 0x00, 0x00, 0x1f, 0x00, 0x00, 0x17, 0x00, 0x00, 0x04, 0x03, 0x64,
		0x65, 0x66, 0x00, 0x00, 0x00, 0x01, 0x3f, 0x00, 0x0c, 0x21, 0x00, 0x4e, 0x00, 0x00, 0x00, 0x0c,
		0x00, 0x00, 0x06, 0x00, 0x00, 0x05, 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x03, 0x00,
	}

	// Use the TestDecodeEncode function
	testResult := TestDecodeEncode(ctx, logger, originalData, DecodePrepareOk, EncodePrepareOk)
	fmt.Println("Test result:", testResult)
}
