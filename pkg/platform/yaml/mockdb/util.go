package mockdb

import (
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func EncodeMock(mock *models.Mock, logger *zap.Logger) (*yaml.NetworkTrafficDoc, error) {
	yamlDoc := yaml.NetworkTrafficDoc{
		Version:      mock.Version,
		Kind:         mock.Kind,
		Name:         mock.Name,
		ConnectionID: mock.ConnectionID,
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
				utils.LogError(logger, err, "failed to encode mongo request wiremessage into yaml")
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
				utils.LogError(logger, err, "failed to encode mongo response wiremessage into yaml")
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
			utils.LogError(logger, err, "failed to marshal the mongo input-output as yaml")
			return nil, err
		}

	case models.HTTP:
		httpSpec := models.HTTPSchema{
			Metadata:         mock.Spec.Metadata,
			Request:          *mock.Spec.HTTPReq,
			Response:         *mock.Spec.HTTPResp,
			Created:          mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(httpSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the http input-output as yaml")
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
			utils.LogError(logger, err, "failed to marshal the generic input-output as yaml")
			return nil, err
		}
	case models.Postgres:
		// case models.PostgresV2:

		postgresSpec := models.PostgresSpec{
			Metadata:          mock.Spec.Metadata,
			PostgresRequests:  mock.Spec.PostgresRequests,
			PostgresResponses: mock.Spec.PostgresResponses,
			ReqTimestampMock:  mock.Spec.ReqTimestampMock,
			ResTimestampMock:  mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(postgresSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the postgres input-output as yaml")
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
			utils.LogError(logger, err, "failed to marshal gRPC of external call into yaml")
			return nil, err
		}
	case models.SQL:
		requests := []models.MysqlRequestYaml{}
		for _, v := range mock.Spec.MySQLRequests {

			req := models.MysqlRequestYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo request wiremessage into yaml")
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []models.MysqlResponseYaml{}
		for _, v := range mock.Spec.MySQLResponses {
			resp := models.MysqlResponseYaml{
				Header:    v.Header,
				ReadDelay: v.ReadDelay,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo response wiremessage into yaml")
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
			utils.LogError(logger, err, "failed to marshal the SQL input-output as yaml")
			return nil, err
		}
	default:
		utils.LogError(logger, nil, "failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
}

func decodeMocks(yamlMocks []*yaml.NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := []*models.Mock{}

	for _, m := range yamlMocks {
		mock := models.Mock{
			Version:      m.Version,
			Name:         m.Name,
			Kind:         m.Kind,
			ConnectionID: m.ConnectionID,
		}
		mockCheck := strings.Split(string(m.Kind), "-")
		if len(mockCheck) > 1 {
			logger.Debug("This dependency does not belong to open source version, will be skipped", zap.String("mock kind:", string(m.Kind)))
			continue
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := models.HTTPSchema{}
			err := m.Spec.Decode(&httpSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http mock", zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,
				HTTPReq:  &httpSpec.Request,
				HTTPResp: &httpSpec.Response,

				Created:          httpSpec.Created,
				ReqTimestampMock: httpSpec.ReqTimestampMock,
				ResTimestampMock: httpSpec.ResTimestampMock,
			}
		case models.Mongo:
			mongoSpec := models.MongoSpec{}
			err := m.Spec.Decode(&mongoSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into mongo mock", zap.Any("mock name", m.Name))
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
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into http mock", zap.Any("mock name", m.Name))
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
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into generic mock", zap.Any("mock name", m.Name))
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
			// case models.PostgresV2:

			PostSpec := models.PostgresSpec{}
			err := m.Spec.Decode(&PostSpec)

			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into generic mock", zap.Any("mock name", m.Name))
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
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into mysql mock", zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMySQLMessage(&mysqlSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		default:
			utils.LogError(logger, nil, "failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
			continue
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

func decodeMySQLMessage(yamlSpec *models.MySQLSpec, logger *zap.Logger) (*models.MockSpec, error) {
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
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLHandshakeResponse")
				return nil, err
			}
			req.Message = requestMessage
		case "MySQLQuery":
			requestMessage := &models.MySQLQueryPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLQueryPacket")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_PREPARE":
			requestMessage := &models.MySQLComStmtPreparePacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComStmtPreparePacket")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_EXECUTE":
			requestMessage := &models.MySQLComStmtExecute{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComStmtExecute")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_SEND_LONG_DATA":
			requestMessage := &models.MySQLCOMSTMTSENDLONGDATA{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLCOM_STMT_SEND_LONG_DATA")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_RESET":
			requestMessage := &models.MySQLCOMSTMTRESET{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLCOM_STMT_RESET")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_FETCH":
			requestMessage := &models.MySQLComStmtFetchPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComStmtFetchPacket")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_STMT_CLOSE":
			requestMessage := &models.MySQLComStmtClosePacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComStmtClosePacket")
				return nil, err
			}
			req.Message = requestMessage
		case "AUTH_SWITCH_RESPONSE":
			requestMessage := &models.AuthSwitchRequestPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComStmtClosePacket")
				return nil, err
			}
			req.Message = requestMessage
		case "COM_CHANGE_USER":
			requestMessage := &models.MySQLComChangeUserPacket{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLComChangeUserPacket")
				return nil, err
			}
			req.Message = requestMessage
		}
		requests = append(requests, req)
	}
	mockSpec.MySQLRequests = requests

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
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLHandshakeResponseOk")
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLHandshakeV10":
			responseMessage := &models.MySQLHandshakeV10Packet{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLHandshakeV10Packet")
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLOK":
			responseMessage := &models.MySQLOKPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLOKPacket")
				return nil, err
			}
			resp.Message = responseMessage
		case "COM_STMT_PREPARE_OK":
			responseMessage := &models.MySQLStmtPrepareOk{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLStmtPrepareOk")
				return nil, err
			}
			resp.Message = responseMessage
		case "RESULT_SET_PACKET":
			responseMessage := &models.MySQLResultSet{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLResultSet")
				return nil, err
			}
			resp.Message = responseMessage
		case "AUTH_SWITCH_REQUEST":
			responseMessage := &models.AuthSwitchRequestPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into AuthSwitchRequestPacket")
				return nil, err
			}
			resp.Message = responseMessage
		case "MySQLErr":
			responseMessage := &models.MySQLERRPacket{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into MySQLERRPacket")
				return nil, err
			}
			resp.Message = responseMessage
		}
		responses = append(responses, resp)
	}
	mockSpec.MySQLResponses = responses
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
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg request wiremessage")
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpReply:
			requestMessage := &models.MongoOpReply{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpReply request wiremessage")
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpQuery:
			requestMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpQuery request wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
		default:
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
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpReply:
			responseMessage := &models.MongoOpReply{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				return nil, err
			}
			resp.Message = responseMessage
		case wiremessage.OpQuery:
			responseMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mongo OpMsg response wiremessage")
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
		default:
		}
		responses = append(responses, resp)
	}
	mockSpec.MongoResponses = responses
	return &mockSpec, nil
}
