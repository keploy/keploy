package postgresparser

import (
	"encoding/base64"
	"encoding/binary"
	"math"

	"errors"
	"fmt"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
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
		// fmt.Println("Encoded packet ", packet, " is ", i, "-----", encoded)
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

func IfBeginOnlyQuery(reqBuff []byte, logger *zap.Logger, expectedPgReq *models.Backend, h *hooks.Hook) (*models.Backend, bool) {
	actualreq := decodePgRequest(reqBuff, logger, h)
	if actualreq == nil {
		return nil, false
	}
	actualPgReq := *actualreq

	if len(actualPgReq.Parses) > 0 && len(expectedPgReq.Parses) > 0 && len(expectedPgReq.Parses) == len(actualPgReq.Parses) {

		if expectedPgReq.Parses[0].Query == "BEGIN READ ONLY" || expectedPgReq.Parses[0].Query == "BEGIN" {
			expectedPgReq.Parses = expectedPgReq.Parses[1:]
			if expectedPgReq.PacketTypes[0] == "P" {
				expectedPgReq.PacketTypes = expectedPgReq.PacketTypes[1:]
			}
		}

		if actualPgReq.Parses[0].Query == "BEGIN READ ONLY" || actualPgReq.Parses[0].Query == "BEGIN" {
			actualPgReq.Parses = actualPgReq.Parses[1:]
			if actualPgReq.PacketTypes[0] == "P" {
				actualPgReq.PacketTypes = actualPgReq.PacketTypes[1:]
			}
		}
		return &actualPgReq, true
	}

	return nil, false
}

func matchingReadablePG(requestBuffers [][]byte, logger *zap.Logger, h *hooks.Hook) (bool, []models.Frontend, error) {
	for {
		tcsMocks, err := h.GetConfigMocks()
		if err != nil {
			return false, nil, fmt.Errorf("error while getting tcs mocks %v", err)
		}

		var isMatched, sortFlag bool = false, true
		var sortedTcsMocks []*models.Mock
		var matchedMock *models.Mock

		for _, mock := range tcsMocks {
			if mock == nil {
				continue
			}

			if sortFlag {
				if mock.TestModeInfo.IsFiltered == false {
					sortFlag = false
				} else {
					sortedTcsMocks = append(sortedTcsMocks, mock)
				}
			}

			if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
				for requestIndex, reqBuff := range requestBuffers {
					bufStr := base64.StdEncoding.EncodeToString(reqBuff)
					encoded, err := PostgresDecoderBackend(mock.Spec.PostgresRequests[requestIndex])
					if err != nil {
						logger.Debug("Error while decoding postgres request", zap.Error(err))
					}
					if mock.Spec.PostgresRequests[requestIndex].Identfier == "StartupRequest" {
						logger.Debug("CHANGING TO MD5 for Response")
						mock.Spec.PostgresResponses[requestIndex].AuthType = 5
						continue
					} else {
						if len(encoded) > 0 && encoded[0] == 'p' {
							logger.Debug("CHANGING TO MD5 for Request and Response")
							mock.Spec.PostgresRequests[requestIndex].PasswordMessage.Password = "md5fe4f2f657f01fa1dd9d111d5391e7c07"

							mock.Spec.PostgresResponses[requestIndex].PacketTypes = []string{"R", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "S", "K", "Z"}
							mock.Spec.PostgresResponses[requestIndex].AuthType = 0
							mock.Spec.PostgresResponses[requestIndex].BackendKeyData = pgproto3.BackendKeyData{
								ProcessID: 2613,
								SecretKey: 824670820,
							}
							mock.Spec.PostgresResponses[requestIndex].ReadyForQuery.TxStatus = 73
							mock.Spec.PostgresResponses[requestIndex].ParameterStatusCombined = []pgproto3.ParameterStatus{
								{
									Name:  "application_name",
									Value: "",
								},
								{
									Name:  "client_encoding",
									Value: "UTF8",
								},
								{
									Name:  "DateStyle",
									Value: "ISO, MDY",
								},
								{
									Name:  "integer_datetimes",
									Value: "on",
								},
								{
									Name:  "IntervalStyle",
									Value: "postgres",
								},
								{
									Name:  "is_superuser",
									Value: "UTF8",
								},
								{
									Name:  "server_version",
									Value: "13.12 (Debian 13.12-1.pgdg120+1)",
								},
								{
									Name:  "session_authorization",
									Value: "keploy-user",
								},
								{
									Name:  "standard_conforming_strings",
									Value: "on",
								},
								{
									Name:  "TimeZone",
									Value: "Etc/UTC",
								},
								{
									Name:  "TimeZone",
									Value: "Etc/UTC",
								},
							}

						}
					}

					if bufStr == "AAAACATSFi8=" {
						ssl := models.Frontend{
							Payload: "Tg==",
						}
						return true, []models.Frontend{ssl}, nil
					}
				}
			}
		}

		logger.Debug("Sorted Mocks: ", zap.Any("Len of sortedTcsMocks", len(sortedTcsMocks)))

		isSorted := false
		var idx int
		if !isMatched {
			//use findBinaryMatch twice one for sorted and another for unsorted
			// give more priority to sorted like if you find more than 0.5 in sorted then return that
			if len(sortedTcsMocks) > 0 {
				isSorted = true
				idx = findBinaryStreamMatch(sortedTcsMocks, requestBuffers, logger, h, isSorted)
				if idx != -1 {
					isMatched = true
					matchedMock = tcsMocks[idx]
				}
			}
		}

		if !isMatched {
			isSorted = false
			idx = findBinaryStreamMatch(tcsMocks, requestBuffers, logger, h, isSorted)
			if idx != -1 {
				isMatched = true
				matchedMock = tcsMocks[idx]
			}
		}

		if isMatched {
			logger.Debug("Matched mock", zap.String("mock", matchedMock.Name))
			if matchedMock.TestModeInfo.IsFiltered {
				originalMatchedMock := *matchedMock
				matchedMock.TestModeInfo.IsFiltered = false
				matchedMock.TestModeInfo.SortOrder = math.MaxInt
				isUpdated := h.UpdateConfigMock(&originalMatchedMock, matchedMock)
				if !isUpdated {
					continue
				}
			}
			return true, matchedMock.Spec.PostgresResponses, nil
		}

		break
	}
	return false, nil, nil
}

func decodePgRequest(buffer []byte, logger *zap.Logger, h *hooks.Hook) *models.Backend {

	pg := NewBackend()

	if !isStartupPacket(buffer) && len(buffer) > 5 {
		bufferCopy := buffer
		for i := 0; i < len(bufferCopy)-5; {
			logger.Debug("Inside the if condition")
			pg.BackendWrapper.MsgType = buffer[i]
			pg.BackendWrapper.BodyLen = int(binary.BigEndian.Uint32(buffer[i+1:])) - 4
			if len(buffer) < (i + pg.BackendWrapper.BodyLen + 5) {
				logger.Error("failed to translate the postgres request message due to shorter network packet buffer")
				break
			}
			msg, err := pg.TranslateToReadableBackend(buffer[i:(i + pg.BackendWrapper.BodyLen + 5)])
			if err != nil && buffer[i] != 112 {
				logger.Error("failed to translate the request message to readable", zap.Error(err))
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

func FuzzyCheck(encoded, reqBuff []byte) float64 {
	k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
	shingles1 := util.CreateShingles(encoded, k)
	shingles2 := util.CreateShingles(reqBuff, k)
	similarity := util.JaccardSimilarity(shingles1, shingles2)
	return similarity
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
