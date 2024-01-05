package postgresparser

import (
	"encoding/base64"

	"errors"
	"fmt"

	"github.com/cloudflare/cfssl/log"
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

func findBinaryStreamMatch(tcsMocks []*models.Mock, requestBuffers [][]byte, h *hooks.Hook) int {

	mxSim := -1.0
	mxIdx := -1
	// sameHeader := -1
	// add condition for header match that if mxIdx = -1 then return just matched header
	for idx, mock := range tcsMocks {

		// println("Inside findBinaryMatch", len(mock.Spec.GenericRequests), len(requestBuffers))
		if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
			for requestIndex, reqBuff := range requestBuffers {
				encoded, err := PostgresDecoderBackend(mock.Spec.PostgresRequests[requestIndex])
				if err != nil {

				}
				if mock.Spec.PostgresRequests[requestIndex].Payload != "" {
					encoded, _ = PostgresDecoder(mock.Spec.PostgresRequests[requestIndex].Payload)
				}
				k := util.AdaptiveK(len(reqBuff), 3, 8, 5)
				shingles1 := util.CreateShingles(encoded, k)
				shingles2 := util.CreateShingles(reqBuff, k)
				similarity := util.JaccardSimilarity(shingles1, shingles2)
				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
					continue
				}
			}
		}

	}
	// println("Max Similarity is ", mxSim)
	// if mxIdx == -1 {
	// 	return sameHeader
	// }
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

func matchingReadablePG(requestBuffers [][]byte, logger *zap.Logger, h *hooks.Hook) (bool, []models.Frontend, error) {

	for {
		var isMatched bool
		var matchedMock *models.Mock

		tcsMocks, err := h.GetTcsMocks()
		if err != nil {
			return false, nil, fmt.Errorf("error while fetching tcs mocks %v", err)
		}

		for _, mock := range tcsMocks {
			if mock == nil {
				continue
			}

			if len(mock.Spec.PostgresRequests) == len(requestBuffers) {
				for requestIndex, reqBuff := range requestBuffers {
					bufStr := base64.StdEncoding.EncodeToString(reqBuff)
					encoded, err := PostgresDecoderBackend(mock.Spec.PostgresRequests[requestIndex])
					if err != nil || encoded == nil {
						logger.Debug("Error while decoding postgres request", zap.Error(err))
						return false, nil, fmt.Errorf("error while decoding postgres request %v", err)
					}
					if mock.Spec.PostgresRequests[requestIndex].Identfier == "StartupRequest" {
						log.Debug("CHANGING TO MD5 for Response")
						mock.Spec.PostgresResponses[requestIndex].AuthType = 5
						continue
					} else {
						if len(encoded) > 0 && encoded[0] == 'p' {
							log.Debug("CHANGING TO MD5 for Request and Response")
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
						// println("Matched SSL")
						return true, []models.Frontend{ssl}, nil
					}

					if string(encoded) == string(reqBuff) || bufStr == mock.Spec.PostgresRequests[requestIndex].Payload {
						matchedMock = mock
						isMatched = true
					}
				}
			}
		}

		idx := findBinaryStreamMatch(tcsMocks, requestBuffers, h)
		if idx != -1 {
			isMatched = true
			matchedMock = tcsMocks[idx]
		}

		if isMatched {
			isDeleted, err := h.DeleteTcsMock(matchedMock)
			if err != nil {
				return false, nil, fmt.Errorf("error while deleting tcs mock: %v", err)
			}
			if !isDeleted {
				continue
			} else {
				return true, matchedMock.Spec.PostgresResponses, nil
			}
		}

		return false, nil, nil
	}
}
