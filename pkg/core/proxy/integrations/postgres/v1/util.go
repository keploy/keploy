package v1

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func postgresDecoderFrontend(response models.Frontend) ([]byte, error) {
	// println("Inside PostgresDecoderFrontend")
	var resbuffer []byte
	// list of packets available in the buffer
	packets := response.PacketTypes
	var cc, dtr, ps int = 0, 0, 0
	for _, packet := range packets {
		var msg pgproto3.BackendMessage

		switch packet {
		case string('1'):
			msg = &pgproto3.ParseComplete{}
		case string('2'):
			msg = &pgproto3.BindComplete{}
		case string('3'):
			msg = &pgproto3.CloseComplete{}
		case string('A'):
			msg = &pgproto3.NotificationResponse{
				PID:     response.NotificationResponse.PID,
				Channel: response.NotificationResponse.Channel,
				Payload: response.NotificationResponse.Payload,
			}
		case string('c'):
			msg = &pgproto3.CopyDone{}
		case string('C'):
			if len(response.CommandCompletes) == 0 {
				cc++
				continue
			}
			msg = &pgproto3.CommandComplete{
				CommandTag:     response.CommandCompletes[cc].CommandTag,
				CommandTagType: response.CommandCompletes[cc].CommandTagType,
			}
			cc++
		case string('d'):
			msg = &pgproto3.CopyData{
				Data: response.CopyData.Data,
			}
		case string('D'):
			msg = &pgproto3.DataRow{
				RowValues: response.DataRows[dtr].RowValues,
				Values:    response.DataRows[dtr].Values,
			}
			dtr++
		case string('E'):
			msg = &pgproto3.ErrorResponse{
				Severity:         response.ErrorResponse.Severity,
				Code:             response.ErrorResponse.Code,
				Message:          response.ErrorResponse.Message,
				Detail:           response.ErrorResponse.Detail,
				Hint:             response.ErrorResponse.Hint,
				Position:         response.ErrorResponse.Position,
				InternalPosition: response.ErrorResponse.InternalPosition,
				InternalQuery:    response.ErrorResponse.InternalQuery,
				Where:            response.ErrorResponse.Where,
				SchemaName:       response.ErrorResponse.SchemaName,
				TableName:        response.ErrorResponse.TableName,
				ColumnName:       response.ErrorResponse.ColumnName,
				DataTypeName:     response.ErrorResponse.DataTypeName,
				ConstraintName:   response.ErrorResponse.ConstraintName,
				File:             response.ErrorResponse.File,
				Line:             response.ErrorResponse.Line,
				Routine:          response.ErrorResponse.Routine,
			}
		case string('G'):
			msg = &pgproto3.CopyInResponse{
				OverallFormat:     response.CopyInResponse.OverallFormat,
				ColumnFormatCodes: response.CopyInResponse.ColumnFormatCodes,
			}
		case string('H'):
			msg = &pgproto3.CopyOutResponse{
				OverallFormat:     response.CopyOutResponse.OverallFormat,
				ColumnFormatCodes: response.CopyOutResponse.ColumnFormatCodes,
			}
		case string('I'):
			msg = &pgproto3.EmptyQueryResponse{}
		case string('K'):
			msg = &pgproto3.BackendKeyData{
				ProcessID: response.BackendKeyData.ProcessID,
				SecretKey: response.BackendKeyData.SecretKey,
			}
		case string('n'):
			msg = &pgproto3.NoData{}
		case string('N'):
			msg = &pgproto3.NoticeResponse{
				Severity:         response.NoticeResponse.Severity,
				Code:             response.NoticeResponse.Code,
				Message:          response.NoticeResponse.Message,
				Detail:           response.NoticeResponse.Detail,
				Hint:             response.NoticeResponse.Hint,
				Position:         response.NoticeResponse.Position,
				InternalPosition: response.NoticeResponse.InternalPosition,
				InternalQuery:    response.NoticeResponse.InternalQuery,
				Where:            response.NoticeResponse.Where,
				SchemaName:       response.NoticeResponse.SchemaName,
				TableName:        response.NoticeResponse.TableName,
				ColumnName:       response.NoticeResponse.ColumnName,
				DataTypeName:     response.NoticeResponse.DataTypeName,
				ConstraintName:   response.NoticeResponse.ConstraintName,
				File:             response.NoticeResponse.File,
				Line:             response.NoticeResponse.Line,
				Routine:          response.NoticeResponse.Routine,
			}

		case string('R'):
			switch response.AuthType {
			case AuthTypeOk:
				msg = &pgproto3.AuthenticationOk{}
			case AuthTypeCleartextPassword:
				msg = &pgproto3.AuthenticationCleartextPassword{}
			case AuthTypeMD5Password:
				msg = &pgproto3.AuthenticationMD5Password{}
			case AuthTypeSCMCreds:
				return nil, errors.New("AuthTypeSCMCreds is unimplemented")
			case AuthTypeGSS:
				return nil, errors.New("AuthTypeGSS is unimplemented")
			case AuthTypeGSSCont:
				msg = &pgproto3.AuthenticationGSSContinue{}
			case AuthTypeSSPI:
				return nil, errors.New("AuthTypeSSPI is unimplemented")
			case AuthTypeSASL:
				msg = &pgproto3.AuthenticationSASL{}
			case AuthTypeSASLContinue:
				msg = &pgproto3.AuthenticationSASLContinue{}
			case AuthTypeSASLFinal:
				msg = &pgproto3.AuthenticationSASLFinal{}
			default:
				return nil, fmt.Errorf("unknown authentication type: %d", response.AuthType)
			}

		case string('s'):
			msg = &pgproto3.PortalSuspended{}
		case string('S'):
			msg = &pgproto3.ParameterStatus{
				Name:  response.ParameterStatusCombined[ps].Name,
				Value: response.ParameterStatusCombined[ps].Value,
			}
			ps++

		case string('t'):
			msg = &pgproto3.ParameterDescription{
				ParameterOIDs: response.ParameterDescription.ParameterOIDs,
			}
		case string('T'):
			msg = &pgproto3.RowDescription{
				Fields: response.RowDescription.Fields,
			}
		case string('V'):
			msg = &pgproto3.FunctionCallResponse{
				Result: response.FunctionCallResponse.Result,
			}
		case string('W'):
			msg = &pgproto3.CopyBothResponse{
				OverallFormat:     response.CopyBothResponse.OverallFormat,
				ColumnFormatCodes: response.CopyBothResponse.ColumnFormatCodes,
			}
		case string('Z'):
			msg = &pgproto3.ReadyForQuery{
				TxStatus: response.ReadyForQuery.TxStatus,
			}
		default:
			return nil, fmt.Errorf("unknown message type: %q", packet)
		}

		encoded := msg.Encode([]byte{})
		resbuffer = append(resbuffer, encoded...)
	}
	return resbuffer, nil
}

func postgresDecoderBackend(request models.Backend) ([]byte, error) {
	// take each object , try to make it frontend or backend message so that it can call it's corresponding encode function
	// and then append it to the buffer, for a particular mock ..

	var reqbuffer []byte
	// list of packets available in the buffer
	var b, e, p = 0, 0, 0
	packets := request.PacketTypes
	for _, packet := range packets {
		var msg pgproto3.FrontendMessage
		switch packet {
		case string('B'):
			msg = &pgproto3.Bind{
				DestinationPortal:    request.Binds[b].DestinationPortal,
				PreparedStatement:    request.Binds[b].PreparedStatement,
				ParameterFormatCodes: request.Binds[b].ParameterFormatCodes,
				Parameters:           request.Binds[b].Parameters,
				ResultFormatCodes:    request.Binds[b].ResultFormatCodes,
			}
			b++
		case string('C'):
			msg = &pgproto3.Close{
				Object_Type: request.Close.Object_Type,
				Name:        request.Close.Name,
			}
		case string('D'):
			msg = &pgproto3.Describe{
				ObjectType: request.Describe.ObjectType,
				Name:       request.Describe.Name,
			}
		case string('E'):
			msg = &pgproto3.Execute{
				Portal:  request.Executes[e].Portal,
				MaxRows: request.Executes[e].MaxRows,
			}
			e++
		case string('F'):
			// *msg.(*pgproto3.Flush) = request.Flush
			msg = &pgproto3.Flush{}
		case string('f'):
			// *msg.(*pgproto3.FunctionCall) = request.FunctionCall
			msg = &pgproto3.FunctionCall{
				Function:         request.FunctionCall.Function,
				Arguments:        request.FunctionCall.Arguments,
				ArgFormatCodes:   request.FunctionCall.ArgFormatCodes,
				ResultFormatCode: request.FunctionCall.ResultFormatCode,
			}
		case string('d'):
			msg = &pgproto3.CopyData{
				Data: request.CopyData.Data,
			}
		case string('c'):
			msg = &pgproto3.CopyDone{}
		case string('H'):
			msg = &pgproto3.CopyFail{
				Message: request.CopyFail.Message,
			}
		case string('P'):
			msg = &pgproto3.Parse{
				Name:          request.Parses[p].Name,
				Query:         request.Parses[p].Query,
				ParameterOIDs: request.Parses[p].ParameterOIDs,
			}
			p++
		case string('p'):
			switch request.AuthType {
			case pgproto3.AuthTypeSASL:
				msg = &pgproto3.SASLInitialResponse{
					AuthMechanism: request.SASLInitialResponse.AuthMechanism,
					Data:          request.SASLInitialResponse.Data,
				}
			case pgproto3.AuthTypeSASLContinue:
				msg = &pgproto3.SASLResponse{
					Data: request.SASLResponse.Data,
				}
			case pgproto3.AuthTypeSASLFinal:
				msg = &pgproto3.SASLResponse{
					Data: request.SASLResponse.Data,
				}
			case pgproto3.AuthTypeGSS, pgproto3.AuthTypeGSSCont:
				msg = &pgproto3.GSSResponse{
					Data: []byte{}, // TODO: implement
				}
			case pgproto3.AuthTypeCleartextPassword, pgproto3.AuthTypeMD5Password:
				fallthrough
			default:
				// to maintain backwards compatability
				// println("Here is decoded PASSWORD", request.PasswordMessage.Password)
				msg = &pgproto3.PasswordMessage{Password: request.PasswordMessage.Password}
			}
		case string('Q'):
			msg = &pgproto3.Query{
				String: request.Query.String,
			}
		case string('S'):
			msg = &pgproto3.Sync{}
		case string('X'):
			// *msg.(*pgproto3.Terminate) = request.Terminate
			msg = &pgproto3.Terminate{}
		default:
			return nil, fmt.Errorf("unknown message type: %q", packet)
		}
		if msg == nil {
			return nil, errors.New("msg is nil")
		}
		encoded := msg.Encode([]byte{})

		reqbuffer = append(reqbuffer, encoded...)
	}
	return reqbuffer, nil
}

func checkIfps(array []string) bool {
	n := len(array)
	if n%2 != 0 {
		// If the array length is odd, it cannot match the pattern
		return false
	}

	for i := 0; i < n; i += 2 {
		// Check if consecutive elements are "B" and "E"
		if array[i] != "B" || array[i+1] != "E" {
			return false
		}
	}

	return true
}

func sliceCommandTag(mock *models.Mock, logger *zap.Logger, prep []QueryData, actualPgReq *models.Backend, psCase int) *models.Mock {

	logger.Debug("Inside Slice Command Tag for ", zap.Int("psCase", psCase))
	logger.Debug("Prep Query Data", zap.Any("prep", prep))
	switch psCase {
	case 1:

		copyMock := *mock
		// fmt.Println("Inside Slice Command Tag for ", psCase)
		mockPackets := copyMock.Spec.PostgresResponses[0].PacketTypes
		for idx, v := range mockPackets {
			if v == "1" {
				mockPackets = append(mockPackets[:idx], mockPackets[idx+1:]...)
			}
		}
		copyMock.Spec.PostgresResponses[0].Payload = ""
		copyMock.Spec.PostgresResponses[0].PacketTypes = mockPackets

		return &copyMock
	case 2:
		// ["2", D, C, Z]
		copyMock := *mock
		// fmt.Println("Inside Slice Command Tag for ", psCase)
		mockPackets := copyMock.Spec.PostgresResponses[0].PacketTypes
		for idx, v := range mockPackets {
			if v == "1" || v == "T" {
				mockPackets = append(mockPackets[:idx], mockPackets[idx+1:]...)
			}
		}
		copyMock.Spec.PostgresResponses[0].Payload = ""
		copyMock.Spec.PostgresResponses[0].PacketTypes = mockPackets
		rsFormat := actualPgReq.Bind.ResultFormatCodes

		for idx, datarow := range copyMock.Spec.PostgresResponses[0].DataRows {
			for column, rowVal := range datarow.RowValues {
				// fmt.Println("datarow.RowValues", len(datarow.RowValues))
				if rsFormat[column] == 1 {
					// datarows := make([]byte, 4)
					newRow, _ := getChandedDataRow(rowVal)
					// logger.Info("New Row Value", zap.String("newRow", newRow))
					copyMock.Spec.PostgresResponses[0].DataRows[idx].RowValues[column] = newRow
				}
			}
		}
		return &copyMock
	default:
	}
	return nil
}

func getChandedDataRow(input string) (string, error) {
	// Convert input1 (integer input as string) to integer
	buffer := make([]byte, 4)
	if intValue, err := strconv.Atoi(input); err == nil {

		binary.BigEndian.PutUint32(buffer, uint32(intValue))
		return "b64:" + util.EncodeBase64(buffer), nil
	} else if dateValue, err := time.Parse("2006-01-02", input); err == nil {
		// Perform additional operations on the date
		epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		difference := dateValue.Sub(epoch).Hours() / 24
		// fmt.Printf("Difference in days from epoch: %.2f days\n", difference)
		binary.BigEndian.PutUint32(buffer, uint32(difference))
		return "b64:" + util.EncodeBase64(buffer), nil
	}
	return "b64:AAAAAA==", errors.New("Invalid input")

}

func decodePgRequest(buffer []byte, logger *zap.Logger) *models.Backend {

	pg := NewBackend()

	if !isStartupPacket(buffer) && len(buffer) > 5 {
		bufferCopy := buffer
		for i := 0; i < len(bufferCopy)-5; {
			pg.BackendWrapper.MsgType = buffer[i]
			pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
			if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
				logger.Debug("failed to translate the postgres request message due to shorter network packet buffer")
				break
			}
			msg, err := pg.translateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
			if err != nil && buffer[i] != 112 {
				logger.Debug("failed to translate the request message to readable", zap.Error(err))
			}
			if pg.BackendWrapper.MsgType == 'p' {
				pg.BackendWrapper.PasswordMessage = *msg.(*pgproto3.PasswordMessage)
			}

			if pg.BackendWrapper.MsgType == 'P' {
				pg.BackendWrapper.Parse = *msg.(*pgproto3.Parse)
				pg.BackendWrapper.Parses = append(pg.BackendWrapper.Parses, pg.BackendWrapper.Parse)
			}

			if pg.BackendWrapper.MsgType == 'B' {
				pg.BackendWrapper.Bind = *msg.(*pgproto3.Bind)
				pg.BackendWrapper.Binds = append(pg.BackendWrapper.Binds, pg.BackendWrapper.Bind)
			}

			if pg.BackendWrapper.MsgType == 'E' {
				pg.BackendWrapper.Execute = *msg.(*pgproto3.Execute)
				pg.BackendWrapper.Executes = append(pg.BackendWrapper.Executes, pg.BackendWrapper.Execute)
			}

			pg.BackendWrapper.PacketTypes = append(pg.BackendWrapper.PacketTypes, string(pg.BackendWrapper.MsgType))

			i += 5 + pg.BackendWrapper.BodyLen
		}

		pgMock := &models.Backend{
			PacketTypes: pg.BackendWrapper.PacketTypes,
			Identfier:   "ClientRequest",
			Length:      uint32(len(buffer)),
			// Payload:             bufStr,
			Bind:                pg.BackendWrapper.Bind,
			Binds:               pg.BackendWrapper.Binds,
			PasswordMessage:     pg.BackendWrapper.PasswordMessage,
			CancelRequest:       pg.BackendWrapper.CancelRequest,
			Close:               pg.BackendWrapper.Close,
			CopyData:            pg.BackendWrapper.CopyData,
			CopyDone:            pg.BackendWrapper.CopyDone,
			CopyFail:            pg.BackendWrapper.CopyFail,
			Describe:            pg.BackendWrapper.Describe,
			Execute:             pg.BackendWrapper.Execute,
			Executes:            pg.BackendWrapper.Executes,
			Flush:               pg.BackendWrapper.Flush,
			FunctionCall:        pg.BackendWrapper.FunctionCall,
			GssEncRequest:       pg.BackendWrapper.GssEncRequest,
			Parse:               pg.BackendWrapper.Parse,
			Parses:              pg.BackendWrapper.Parses,
			Query:               pg.BackendWrapper.Query,
			SSlRequest:          pg.BackendWrapper.SSlRequest,
			StartupMessage:      pg.BackendWrapper.StartupMessage,
			SASLInitialResponse: pg.BackendWrapper.SASLInitialResponse,
			SASLResponse:        pg.BackendWrapper.SASLResponse,
			Sync:                pg.BackendWrapper.Sync,
			Terminate:           pg.BackendWrapper.Terminate,
			MsgType:             pg.BackendWrapper.MsgType,
			AuthType:            pg.BackendWrapper.AuthType,
		}
		return pgMock
	}

	return nil
}

func mergePgRequests(requestBuffers [][]byte, logger *zap.Logger) [][]byte {
	logger.Debug("MERGING REQUESTS")
	// Check for PBDE first
	var mergeBuff []byte
	for _, v := range requestBuffers {
		backend := decodePgRequest(v, logger)

		if backend == nil {
			logger.Debug("Rerurning nil while merging ")
			break
		}
		buf, _ := postgresDecoderBackend(*backend)
		mergeBuff = append(mergeBuff, buf...)
	}
	if len(mergeBuff) > 0 {
		return [][]byte{mergeBuff}
	}

	return requestBuffers
}

func mergeMocks(pgmocks []models.Backend, logger *zap.Logger) []models.Backend {
	logger.Debug("MERGING Mocks")
	if len(pgmocks) == 0 {
		return pgmocks
	}
	// Check for PBDE first
	if len(pgmocks[0].PacketTypes) == 0 || pgmocks[0].PacketTypes[0] != "P" {
		return pgmocks
	}
	var mergeBuff []byte
	for _, v := range pgmocks {
		buf, _ := postgresDecoderBackend(v)
		mergeBuff = append(mergeBuff, buf...)
	}
	if len(mergeBuff) > 0 {
		return []models.Backend{*decodePgRequest(mergeBuff, logger)}
	}

	return pgmocks
}
