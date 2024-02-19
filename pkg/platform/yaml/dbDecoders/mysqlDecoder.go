package decoders

import (
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func DecodeMySqlMessage(yamlSpec *models.MySQLSchema, logger *zap.Logger) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,
		Created:  yamlSpec.CreatedAt,
	}
	requests := []models.MySQLRequest{}
	for _, v := range yamlSpec.Requests {
		req := models.MySQLRequest{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		switch v.Header.PacketType {
		case "HANDSHAKE_RESPONSE":
			requestMessage := &models.MySQLHandshakeResponse{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLHandshakeResponse ", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "MySQLQuery":
			requestMessage := &models.MySQLQueryPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLQueryPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_PREPARE":
			requestMessage := &models.MySQLComStmtPreparePacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComStmtPreparePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_EXECUTE":
			requestMessage := &models.MySQLComStmtExecute{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComStmtExecute", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_SEND_LONG_DATA":
			requestMessage := &models.MySQLCOM_STMT_SEND_LONG_DATA{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLCOM_STMT_SEND_LONG_DATA", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_RESET":
			requestMessage := &models.MySQLCOM_STMT_RESET{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLCOM_STMT_RESET", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_FETCH":
			requestMessage := &models.MySQLComStmtFetchPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComStmtFetchPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_CLOSE":
			requestMessage := &models.MySQLComStmtClosePacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComStmtClosePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "AUTH_SWITCH_RESPONSE":
			requestMessage := &models.AuthSwitchRequestPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComStmtClosePacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case "COM_CHANGE_USER":
			requestMessage := &models.MySQLComChangeUserPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLComChangeUserPacket", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		}
		requests = append(requests, req)
	}
	mockSpec.MySqlRequests = requests

	responses := []models.MySQLResponse{}
	for _, v := range yamlSpec.Response {
		resp := models.MySQLResponse{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mysql structs
		switch v.Header.PacketType {
		case "HANDSHAKE_RESPONSE_OK":
			responseMessage := &models.MySQLHandshakeResponseOk{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLHandshakeResponseOk ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLHandshakeV10":
			responseMessage := &models.MySQLHandshakeV10Packet{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLHandshakeV10Packet", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLOK":
			responseMessage := &models.MySQLOKPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLOKPacket ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "COM_STMT_PREPARE_OK":
			responseMessage := &models.MySQLStmtPrepareOk{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLStmtPrepareOk ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "RESULT_SET_PACKET":
			responseMessage := &models.MySQLResultSet{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLResultSet ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "AUTH_SWITCH_REQUEST":
			responseMessage := &models.AuthSwitchRequestPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLResultSet ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLErr":
			responseMessage := &models.MySQLERRPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal yml document into MySQLERRPacket ", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		}
		responses = append(responses, resp)
	}
	mockSpec.MySqlResponses = responses
	return &mockSpec, nil

}
