package postgresparser

import (
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"time"

	"errors"
	"fmt"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func PostgresDecoder(encoded string) ([]byte, error) {
	// decode the base 64 encoded string to buffer ..
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func PostgresDecoderFrontend(response models.Frontend) ([]byte, error) {
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
			msg = &pgproto3.CommandComplete{
				CommandTag: response.CommandCompletes[cc].CommandTag,
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
			if response.DataRows[dtr].Values != nil {
				fmt.Println("-----", response.DataRows[dtr].Values)
				fmt.Println("-----", response.DataRows[dtr].RowValues)
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

func PostgresDecoderBackend(request models.Backend) ([]byte, error) {
	// take each object , try to make it frontend or backend message so that it can call it's corresponding encode function
	// and then append it to the buffer, for a particular mock ..

	var reqbuffer []byte
	// list of packets available in the buffer
	var b, e, p int = 0, 0, 0
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

func PostgresEncoder(buffer []byte) string {
	// encode the buffer to base 64 string ..
	encoded := base64.StdEncoding.EncodeToString(buffer)
	return encoded
}

func findBinaryStreamMatch(tcsMocks []*models.Mock, requestBuffers [][]byte, logger *zap.Logger, h *hooks.Hook, isSorted bool) int {

	mxSim := -1.0
	mxIdx := -1

	for idx, mock := range tcsMocks {

		if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
			for requestIndex, reqBuff := range requestBuffers {

				expectedPgReq := mock.Spec.PostgresRequests[requestIndex]
				encoded, err := PostgresDecoderBackend(expectedPgReq)
				if err != nil {
					logger.Debug("Error while decoding postgres request", zap.Error(err))
				}
				var encoded64 []byte
				if expectedPgReq.Payload != "" {
					encoded64, err = PostgresDecoder(mock.Spec.PostgresRequests[requestIndex].Payload)
					if err != nil {
						logger.Debug("Error while decoding postgres request", zap.Error(err))
						return -1
					}
				}
				var similarity1, similarity2 float64
				if len(encoded) > 0 {
					similarity1 = FuzzyCheck(encoded, reqBuff)
				}
				if len(encoded64) > 0 {
					similarity2 = FuzzyCheck(encoded64, reqBuff)
				}

				// calculate the jaccard similarity between the two buffers one with base64 encoding and another via that ..
				similarity := max(similarity1, similarity2)
				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
					continue
				}
			}
		}
	}

	if isSorted {
		if mxIdx != -1 && mxSim >= 0.78 {
			logger.Debug("Matched with Sorted Stream", zap.Float64("similarity", mxSim))
		} else {
			mxIdx = -1
		}
	} else {
		if mxIdx != -1 {
			logger.Debug("Matched with Unsorted Stream", zap.Float64("similarity", mxSim))
		}
	}
	return mxIdx
}

func CheckValidEncode(tcsMocks []*models.Mock, h *hooks.Hook, log *zap.Logger) {
	for _, mock := range tcsMocks {
		for _, reqBuff := range mock.Spec.PostgresRequests {
			encode, err := PostgresDecoderBackend(reqBuff)
			if err != nil {
				log.Debug("Error in decoding")
			}
			actual_encode, err := PostgresDecoder(reqBuff.Payload)
			if err != nil {
				log.Debug("Error in decoding")
			}

			if len(encode) != len(actual_encode) {
				log.Debug("Not Equal Length of buffer in request", zap.Any("payload", reqBuff.Payload))
				log.Debug("Length of encode", zap.Int("length", len(encode)), zap.Int("length", len(actual_encode)))
				log.Debug("Encode via readable", zap.Any("encode", encode))
				log.Debug("Actual Encode", zap.Any("actual_encode", actual_encode))
				log.Debug("Payload", zap.Any("payload", reqBuff.Payload))
				continue
			}
			log.Debug("Matched")

		}
		for _, resBuff := range mock.Spec.PostgresResponses {
			encode, err := PostgresDecoderFrontend(resBuff)
			if err != nil {
				log.Debug("Error in decoding")
			}
			actual_encode, err := PostgresDecoder(resBuff.Payload)
			if err != nil {
				log.Debug("Error in decoding")
			}
			if len(encode) != len(actual_encode) {
				log.Debug("Not Equal Length of buffer in response")
				log.Debug("Length of encode", zap.Any("length", len(encode)), zap.Any("length", len(actual_encode)))
				log.Debug("Encode via readable", zap.Any("encode", encode))
				log.Debug("Actual Encode", zap.Any("actual_encode", actual_encode))
				log.Debug("Payload", zap.Any("payload", resBuff.Payload))
				continue
			}
			log.Debug("Matched")
		}
	}
	h.SetTcsMocks(tcsMocks)
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

func sliceCommandTag(mock *models.Mock, logger *zap.Logger, prep []QueryData, actualPgReq *models.Backend, ps_case int) *models.Mock {

	switch ps_case {
	case 1:

		copyMock := *mock
		fmt.Println("Inside Slice Command Tag for ", ps_case)
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
		fmt.Println("Inside Slice Command Tag for ", ps_case)
		mockPackets := copyMock.Spec.PostgresResponses[0].PacketTypes
		for idx, v := range mockPackets {
			if v == "1" || v == "T" {
				mockPackets = append(mockPackets[:idx], mockPackets[idx+1:]...)
			}
		}
		copyMock.Spec.PostgresResponses[0].Payload = ""
		copyMock.Spec.PostgresResponses[0].PacketTypes = mockPackets
		rsFormat := actualPgReq.Bind.ResultFormatCodes
		fmt.Println("Result Format Codes for mock ", copyMock.Name, "*** ", len(rsFormat), rsFormat)

		for idx, datarow := range copyMock.Spec.PostgresResponses[0].DataRows {
			for column, row_value := range datarow.RowValues {
				// fmt.Println("datarow.RowValues", len(datarow.RowValues))
				if rsFormat[column] == 1 {
					// datarows := make([]byte, 4)
					new_row, _ := getChandedDataRow(row_value)
					// logger.Info("New Row Value", zap.String("new_row", new_row))
					copyMock.Spec.PostgresResponses[0].DataRows[idx].RowValues[column] = new_row
				}
			}
		}
		return &copyMock
	case 3:
		// "B", "E", "P", "B", "D", "E" => "B", "E", "B",  "E"
		fmt.Println("Inside Slice Command Tag for ", ps_case)
		fmt.Println("Inside Execute Command Tag 3")
	default:
	}
	return nil
}

func getChandedDataRow(input string) (string, error) {
	// Convert input1 (integer input as string) to integer
	buffer := make([]byte, 4)
	if intValue, err := strconv.Atoi(input); err == nil {

		binary.BigEndian.PutUint32(buffer, uint32(intValue))
		return ("b64:" + PostgresEncoder(buffer)), nil
	} else if dateValue, err := time.Parse("2006-01-02", input); err == nil {
		// Perform additional operations on the date
		epoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		difference := dateValue.Sub(epoch).Hours() / 24
		fmt.Printf("Difference in days from epoch: %.2f days\n", difference)
		binary.BigEndian.PutUint32(buffer, uint32(difference))
		return ("b64:" + PostgresEncoder(buffer)), nil
	} else {
		return "b64:AAAAAA==", err
	}
}

func decodePgResponse(bufStr string, logger *zap.Logger) *models.Frontend {

	// bufStr = "MQAAAAQyAAAABEMAAAAKQkVHSU4AMQAAAAQyAAAABFQAAAF/AA9pZAAAAGMuAAEAAAAXAAT/////AABiaXJ0aF9kYXRlAAAAYy4AAwAABDoABP////8AAG5hbWUAAABjLgACAAAEE///AAABAwAAaWQAAABjJQABAAAAFwAE/////wAAYWRkcmVzcwAAAGMlAAQAAAQT//8AAAEDAABjaXR5AAAAYyUABQAABBP//wAAAQMAAGZpcnN0X25hbWUAAABjJQACAAAEE///AAABAwAAbGFzdF9uYW1lAAAAYyUAAwAABBP//wAAAQMAAHRlbGVwaG9uZQAAAGMlAAYAAAQT//8AAAEDAABpZAAAAGNFAAEAAAAXAAT/////AABuYW1lAAAAY0UAAgAABBP//wAAAQMAAHBldF9pZAAAAGNhAAQAAAAXAAT/////AABpZAAAAGNhAAEAAAAXAAT/////AAB2aXNpdF9kYXRlAAAAY2EAAgAABDoABP////8AAGRlc2NyaXB0aW9uAAAAY2EAAwAABBP//wAAAQMAAEQAAACTAA8AAAABMQAAAAoyMDIxLTA3LTA3AAAABkRleHRlcgAAAAEzAAAAH0hOTyBBIC01MDQgU0VDVE9SLTIgQU5NT0wgQVBQVFQAAAAJTkVXIERFTEhJAAAABVJpdGlrAAAABEphaW4AAAAKOTk1ODE3ODU0OQAAAAExAAAAA0RvZ/////////////////////9DAAAADVNFTEVDVCAxAFoAAAAFVA==" //PostgresEncoder(buffer)
	pg := NewFrontend()
	buffer, err := PostgresDecoder(bufStr)
	fmt.Println("Buffer is ", buffer)
	if err != nil {
		logger.Error("failed to decode pg response from the buffered string")
	}
	if !isStartupPacket(buffer) && len(buffer) > 5 && bufStr != "Tg==" {
		bufferCopy := buffer

		//Saving list of packets in case of multiple packets in a single buffer steam
		ps := make([]pgproto3.ParameterStatus, 0)
		dataRows := []pgproto3.DataRow{}
		// here 5 is taken to skip first byte as header
		for i := 0; i < len(bufferCopy)-5; {
			pg.FrontendWrapper.MsgType = buffer[i]
			pg.FrontendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
			msg, err := pg.TranslateToReadableResponse(buffer[i:(i+pg.FrontendWrapper.BodyLen+5)], logger)
			if err != nil {
				logger.Error("failed to translate the response message to readable", zap.Error(err))
				break
			}

			pg.FrontendWrapper.PacketTypes = append(pg.FrontendWrapper.PacketTypes, string(pg.FrontendWrapper.MsgType))
			i += (5 + pg.FrontendWrapper.BodyLen)
			if pg.FrontendWrapper.ParameterStatus.Name != "" {
				ps = append(ps, pg.FrontendWrapper.ParameterStatus)
			}
			if pg.FrontendWrapper.MsgType == 'C' {
				pg.FrontendWrapper.CommandComplete = *msg.(*pgproto3.CommandComplete)
				pg.FrontendWrapper.CommandCompletes = append(pg.FrontendWrapper.CommandCompletes, pg.FrontendWrapper.CommandComplete)
			}
			if pg.FrontendWrapper.MsgType == 'D' && pg.FrontendWrapper.DataRow.RowValues != nil {
				// Create a new slice for each DataRow
				valuesCopy := make([]string, len(pg.FrontendWrapper.DataRow.RowValues))
				fmt.Println("Values Copy", valuesCopy, "----++=--- ", pg.FrontendWrapper.DataRow.RowValues)
				copy(valuesCopy, pg.FrontendWrapper.DataRow.RowValues)
				fmt.Println("Values Copy", valuesCopy)
				row := pgproto3.DataRow{
					RowValues: valuesCopy, // Use the copy of the values
					Values:    pg.FrontendWrapper.DataRow.Values,
				}
				// fmt.Println("row is ", row)
				dataRows = append(dataRows, row)
				// newDataRows = append(newDataRows, string(row.Values[]))
			}
		}

		if len(ps) > 0 {
			pg.FrontendWrapper.ParameterStatusCombined = ps
		}
		if len(dataRows) > 0 {
			pg.FrontendWrapper.DataRows = dataRows
		}

		// from here take the msg and append its readabable form to the pgResponses
		pg_mock := &models.Frontend{
			PacketTypes: pg.FrontendWrapper.PacketTypes,
			Identfier:   "ServerResponse",
			Length:      uint32(len(buffer)),
			// Payload:                         bufStr,
			AuthenticationOk:                pg.FrontendWrapper.AuthenticationOk,
			AuthenticationCleartextPassword: pg.FrontendWrapper.AuthenticationCleartextPassword,
			AuthenticationMD5Password:       pg.FrontendWrapper.AuthenticationMD5Password,
			AuthenticationGSS:               pg.FrontendWrapper.AuthenticationGSS,
			AuthenticationGSSContinue:       pg.FrontendWrapper.AuthenticationGSSContinue,
			AuthenticationSASL:              pg.FrontendWrapper.AuthenticationSASL,
			AuthenticationSASLContinue:      pg.FrontendWrapper.AuthenticationSASLContinue,
			AuthenticationSASLFinal:         pg.FrontendWrapper.AuthenticationSASLFinal,
			BackendKeyData:                  pg.FrontendWrapper.BackendKeyData,
			BindComplete:                    pg.FrontendWrapper.BindComplete,
			CloseComplete:                   pg.FrontendWrapper.CloseComplete,
			CommandComplete:                 pg.FrontendWrapper.CommandComplete,
			CommandCompletes:                pg.FrontendWrapper.CommandCompletes,
			CopyData:                        pg.FrontendWrapper.CopyData,
			CopyDone:                        pg.FrontendWrapper.CopyDone,
			CopyInResponse:                  pg.FrontendWrapper.CopyInResponse,
			CopyOutResponse:                 pg.FrontendWrapper.CopyOutResponse,
			DataRow:                         pg.FrontendWrapper.DataRow,
			DataRows:                        pg.FrontendWrapper.DataRows,
			EmptyQueryResponse:              pg.FrontendWrapper.EmptyQueryResponse,
			ErrorResponse:                   pg.FrontendWrapper.ErrorResponse,
			FunctionCallResponse:            pg.FrontendWrapper.FunctionCallResponse,
			NoData:                          pg.FrontendWrapper.NoData,
			NoticeResponse:                  pg.FrontendWrapper.NoticeResponse,
			NotificationResponse:            pg.FrontendWrapper.NotificationResponse,
			ParameterDescription:            pg.FrontendWrapper.ParameterDescription,
			ParameterStatusCombined:         pg.FrontendWrapper.ParameterStatusCombined,
			ParseComplete:                   pg.FrontendWrapper.ParseComplete,
			PortalSuspended:                 pg.FrontendWrapper.PortalSuspended,
			ReadyForQuery:                   pg.FrontendWrapper.ReadyForQuery,
			RowDescription:                  pg.FrontendWrapper.RowDescription,
			MsgType:                         pg.FrontendWrapper.MsgType,
			AuthType:                        pg.FrontendWrapper.AuthType,
		}
		after_encoded, err := PostgresDecoderFrontend(*pg_mock)
		if err != nil {
			logger.Info("failed to decode the response message in proxy for postgres dependency", zap.Error(err))
		}
		fmt.Println("AFTER ENCODED", after_encoded)
		fmt.Println("DATA ROWS 1", pg_mock.DataRows[0].RowValues)
		fmt.Println("DATA ROWS 2", pg_mock.DataRows[0].Values)
		// fmt.Println("DATA ROWS 3", pg_mock.DataRows[1].RowValues)
		// fmt.Println("DATA ROWS 4", pg_mock.DataRows[1].Values)

		if len(after_encoded) != len(buffer) {
			logger.Info("the length of the encoded buffer is not equal to the length of the original buffer", zap.Any("after_encoded", len(after_encoded)), zap.Any("buffer", len(buffer)))

			// pg_mock.Payload = bufStr
		}
		return pg_mock
	}
	return nil
}

func decodePgRequest(buffer []byte, logger *zap.Logger) *models.Backend {

	pg := NewBackend()

	if !isStartupPacket(buffer) && len(buffer) > 5 {
		bufferCopy := buffer
		for i := 0; i < len(bufferCopy)-5; {
			logger.Debug("Inside the if condition")
			pg.BackendWrapper.MsgType = buffer[i]
			pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
			if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
				logger.Debug("failed to translate the postgres request message due to shorter network packet buffer")
				break
			}
			msg, err := pg.TranslateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
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

			i += (5 + pg.BackendWrapper.BodyLen)
		}

		pg_mock := &models.Backend{
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
		return pg_mock
	}

	return nil
}
