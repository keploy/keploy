package mockdb

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func EncodeMock(mock *models.Mock, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {
	yamlDoc := yaml.NetworkTrafficDoc{
		Version: mock.Version,
		Kind:    mock.Kind,
		Name:    mock.Name,
	}
	switch mock.Kind {
	case models.Mongo:
		requests := []models.RequestYaml{}
		for _, v := range mock.Spec.MongoRequests {
			req := models.RequestYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				logger.Error("failed to encode mongo request wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []models.ResponseYaml{}
		for _, v := range mock.Spec.MongoResponses {
			resp := models.ResponseYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				logger.Error("failed to encode mongo response wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			responses = append(responses, resp)
		}
		mongoSpec := models.MongoSpec{
			Metadata:         mock.Spec.Metadata,
			Requests:         requests,
			Response:         responses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(mongoSpec)
		if err != nil {
			logger.Error("failed to marshal the mongo input-output as yaml", zap.Error(err))
			return nil, err
		}

	case models.HTTP:
		httpSpec := models.HttpSchema{
			Metadata:         mock.Spec.Metadata,
			Request:          *mock.Spec.HttpReq,
			Response:         *mock.Spec.HttpResp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(httpSpec)
		if err != nil {
			logger.Error("failed to marshal the http input-output as yaml", zap.Error(err))
			return nil, err
		}
	case models.GENERIC:
		genericSpec := models.GenericSchema{
			Metadata:         mock.Spec.Metadata,
			GenericRequests:  mock.Spec.GenericRequests,
			GenericResponses: mock.Spec.GenericResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(genericSpec)
		if err != nil {
			logger.Error("failed to marshal binary input-output of external call into yaml", zap.Error(err))
			return nil, err
		}
	case models.Postgres:

		postgresSpec := models.PostgresSpec{
			Metadata:          mock.Spec.Metadata,
			PostgresRequests:  mock.Spec.PostgresRequests,
			PostgresResponses: mock.Spec.PostgresResponses,
			ReqTimestampMock:  mock.Spec.ReqTimestampMock,
			ResTimestampMock:  mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(postgresSpec)
		if err != nil {
			logger.Error("failed to marshal postgres of external call into yaml", zap.Error(err))
			return nil, err
		}
	case models.GRPC_EXPORT:
		gRPCSpec := models.GrpcSpec{
			GrpcReq:          *mock.Spec.GRPCReq,
			GrpcResp:         *mock.Spec.GRPCResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(gRPCSpec)
		if err != nil {
			logger.Error(utils.Emoji+"failed to marshal gRPC of external call into yaml", zap.Error(err))
			return nil, err
		}
	case models.SQL:
		requests := []models.MysqlRequestYaml{}
		for _, v := range mock.Spec.MySqlRequests {

			req := models.MysqlRequestYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				logger.Error(utils.Emoji+"failed to encode mongo request wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []models.MysqlResponseYaml{}
		for _, v := range mock.Spec.MySqlResponses {
			resp := models.MysqlResponseYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				logger.Error(utils.Emoji+"failed to encode mongo request wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			responses = append(responses, resp)
		}

		sqlSpec := models.MySQLSpec{
			Metadata:  mock.Spec.Metadata,
			Requests:  requests,
			Response:  responses,
			CreatedAt: mock.Spec.Created,
		}
		err := yamlDoc.Spec.Encode(sqlSpec)
		if err != nil {
			logger.Error(utils.Emoji+"failed to marshal the SQL input-output as yaml", zap.Error(err))
			return nil, err
		}
	default:
		logger.Error("failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
}

func decodeMocks(yamlMocks []*yaml.NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := []*models.Mock{}

	for _, m := range yamlMocks {
		mock := models.Mock{
			Version: m.Version,
			Name:    m.Name,
			Kind:    m.Kind,
		}
		mockCheck := strings.Split(string(m.Kind), "-")
		if len(mockCheck) > 1 {
			logger.Debug("This dependency does not belong to open source version, will be skipped", zap.String("mock kind:", string(m.Kind)))
			continue
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := models.HttpSchema{}
			err := m.Spec.Decode(&httpSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,
				HttpReq:  &httpSpec.Request,
				HttpResp: &httpSpec.Response,

				Created:          httpSpec.Created,
				ReqTimestampMock: httpSpec.ReqTimestampMock,
				ResTimestampMock: httpSpec.ResTimestampMock,
			}
		case models.Mongo:
			mongoSpec := models.MongoSpec{}
			err := m.Spec.Decode(&mongoSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMongoMessage(&mongoSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		case models.GRPC_EXPORT:
			grpcSpec := models.GrpcSpec{}
			err := m.Spec.Decode(&grpcSpec)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				GRPCResp:         &grpcSpec.GrpcResp,
				GRPCReq:          &grpcSpec.GrpcReq,
				ReqTimestampMock: grpcSpec.ReqTimestampMock,
				ResTimestampMock: grpcSpec.ResTimestampMock,
			}
		case models.GENERIC:
			genericSpec := models.GenericSchema{}
			err := m.Spec.Decode(&genericSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into generic mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:         genericSpec.Metadata,
				GenericRequests:  genericSpec.GenericRequests,
				GenericResponses: genericSpec.GenericResponses,
				ReqTimestampMock: genericSpec.ReqTimestampMock,
				ResTimestampMock: genericSpec.ResTimestampMock,
			}

		case models.Postgres:

			PostSpec := models.PostgresSpec{}
			err := m.Spec.Decode(&PostSpec)

			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into generic mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: PostSpec.Metadata,
				// OutputBinary: genericSpec.Objects,
				PostgresRequests:  PostSpec.PostgresRequests,
				PostgresResponses: PostSpec.PostgresResponses,
				ReqTimestampMock:  PostSpec.ReqTimestampMock,
				ResTimestampMock:  PostSpec.ResTimestampMock,
			}
		case models.SQL:
			mysqlSpec := models.MySQLSpec{}
			err := m.Spec.Decode(&mysqlSpec)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMySqlMessage(&mysqlSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		default:
			logger.Error("failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
			continue
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

func decodeMySqlMessage(yamlSpec *models.MySQLSpec, logger *zap.Logger) (*models.MockSpec, error) {
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
func decodeMongoMessage(yamlSpec *models.MongoSpec, logger *zap.Logger) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata:         yamlSpec.Metadata,
		Created:          yamlSpec.CreatedAt,
		ReqTimestampMock: yamlSpec.ReqTimestampMock,
		ResTimestampMock: yamlSpec.ResTimestampMock,
	}

	// mongo request
	requests := []models.MongoRequest{}
	for _, v := range yamlSpec.Requests {
		req := models.MongoRequest{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mongo request wiremessage
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			requestMessage := &models.MongoOpMessage{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpMsg request wiremessage", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpReply:
			requestMessage := &models.MongoOpReply{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpReply wiremessage", zap.Error(err))
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpQuery:
			requestMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpQuery wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
		default:
			// TODO
		}
		requests = append(requests, req)
	}
	mockSpec.MongoRequests = requests

	// mongo response
	responses := []models.MongoResponse{}
	for _, v := range yamlSpec.Response {
		resp := models.MongoResponse{
			Header:    v.Header,
			ReadDelay: v.ReadDelay,
		}
		// decode the yaml document to mongo response wiremessage
		switch v.Header.Opcode {
		case wiremessage.OpMsg:
			responseMessage := &models.MongoOpMessage{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpReply:
			responseMessage := &models.MongoOpReply{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpQuery:
			responseMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error("failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		default:
			// TODO
		}
		responses = append(responses, resp)
	}
	mockSpec.MongoResponses = responses
	return &mockSpec, nil
}

func filterMocks(ctx context.Context, m []*models.Mock, afterTime time.Time, beforeTime time.Time, logger *zap.Logger) []*models.Mock {
	
	filteredMocks := make([]*models.Mock, 0)

	if afterTime == (time.Time{}) {
		logger.Debug("request timestamp is missing  ")
		return m
	}

	if beforeTime == (time.Time{}) {
		logger.Debug("response timestamp is missing  ")
		return m
	}

	for _, mock := range m {
		if mock.Spec.ReqTimestampMock == (time.Time{}) || mock.Spec.ResTimestampMock == (time.Time{}) {
			logger.Debug("request or response timestamp of mock is missing")
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}

		// Checking if the mock's request and response timestamps lie between the test's request and response timestamp
		if mock.Spec.ReqTimestampMock.After(afterTime) && mock.Spec.ResTimestampMock.Before(beforeTime) {
			mock.TestModeInfo.IsFiltered = true
			filteredMocks = append(filteredMocks, mock)
			continue
		}
		mock.TestModeInfo.IsFiltered = false
	}

	return filteredMocks
}
