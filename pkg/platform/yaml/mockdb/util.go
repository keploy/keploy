package mockdb

import (
	"context"
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
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
	case models.REDIS:
		redisSpec := models.RedisSchema{
			Metadata:         mock.Spec.Metadata,
			RedisRequests:    mock.Spec.RedisRequests,
			RedisResponses:   mock.Spec.RedisResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(redisSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the redis input-output as yaml")
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
	case models.MySQL:
		requests := []mysql.RequestYaml{}
		for _, v := range mock.Spec.MySQLRequests {

			req := mysql.RequestYaml{
				Header: v.Header,
				Meta:   v.Meta,
			}
			err := req.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo request wiremessage into yaml")
				return nil, err
			}
			requests = append(requests, req)
		}
		responses := []mysql.ResponseYaml{}
		for _, v := range mock.Spec.MySQLResponses {
			resp := mysql.ResponseYaml{
				Header: v.Header,
				Meta:   v.Meta,
			}
			err := resp.Message.Encode(v.Message)
			if err != nil {
				utils.LogError(logger, err, "failed to encode mongo response wiremessage into yaml")
				return nil, err
			}
			responses = append(responses, resp)
		}

		sqlSpec := mysql.Spec{
			Metadata:         mock.Spec.Metadata,
			Requests:         requests,
			Response:         responses,
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(sqlSpec)
		if err != nil {
			utils.LogError(logger, err, "failed to marshal the MySQL input-output as yaml")
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
		case models.REDIS:
			redisSpec := models.RedisSchema{}
			err := m.Spec.Decode(&redisSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into redis mock", zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:         redisSpec.Metadata,
				RedisRequests:    redisSpec.RedisRequests,
				RedisResponses:   redisSpec.RedisResponses,
				ReqTimestampMock: redisSpec.ReqTimestampMock,
				ResTimestampMock: redisSpec.ResTimestampMock,
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
		case models.MySQL:
			mySQLSpec := mysql.Spec{}
			err := m.Spec.Decode(&mySQLSpec)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal a yaml doc into mysql mock", zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMySQLMessage(context.Background(), logger, &mySQLSpec)
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

func decodeMySQLMessage(_ context.Context, logger *zap.Logger, yamlSpec *mysql.Spec) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata:         yamlSpec.Metadata,
		Created:          yamlSpec.CreatedAt,
		ReqTimestampMock: yamlSpec.ReqTimestampMock,
		ResTimestampMock: yamlSpec.ResTimestampMock,
	}

	// Decode the requests

	requests := []mysql.Request{}
	for _, v := range yamlSpec.Requests {
		req := mysql.Request{
			PacketBundle: mysql.PacketBundle{
				Header: v.Header,
				Meta:   v.Meta,
			},
		}

		switch v.Header.Type {
		// connection phase

		case mysql.SSLRequest:
			msg := &mysql.SSLRequestPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql SSLRequestPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.HandshakeResponse41:
			msg := &mysql.HandshakeResponse41Packet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql HandshakeResponse41Packet")
				return nil, err
			}
			req.Message = msg

		case mysql.CachingSha2PasswordToString(mysql.RequestPublicKey):
			var msg string
			err := v.Message.Decode(&msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql (string) RequestPublicKey")
				return nil, err
			}
			req.Message = msg

		case "encrypted_password":
			var msg string
			err := v.Message.Decode(&msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql (string) encrypted_password")
				return nil, err
			}
			req.Message = msg

		// command phase

		// utility packets
		case mysql.CommandStatusToString(mysql.COM_QUIT):
			msg := &mysql.QuitPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql QuitPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_INIT_DB):
			msg := &mysql.InitDBPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql InitDBPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STATISTICS):
			msg := &mysql.StatisticsPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StatisticsPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_DEBUG):
			msg := &mysql.DebugPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql DebugPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_PING):
			msg := &mysql.PingPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql PingPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_CHANGE_USER):
			msg := &mysql.ChangeUserPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql ChangeUserPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION):
			msg := &mysql.ResetConnectionPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql ResetConnectionPacket")
				return nil, err
			}
			req.Message = msg

		// case mysql.CommandStatusToString(mysql.COM_SET_OPTION):	// not supported yet

		// query packets
		case mysql.CommandStatusToString(mysql.COM_QUERY):
			msg := &mysql.QueryPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql QueryPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_PREPARE):
			msg := &mysql.StmtPreparePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtPreparePacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE):
			msg := &mysql.StmtExecutePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtExecutePacket")
				return nil, err
			}
			req.Message = msg

		// case mysql.CommandStatusToString(mysql.COM_FETCH): // not supported yet

		case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE):
			msg := &mysql.StmtClosePacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtClosePacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_RESET):
			msg := &mysql.StmtResetPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtResetPacket")
				return nil, err
			}
			req.Message = msg

		case mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
			msg := &mysql.StmtSendLongDataPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yaml document into mysql StmtSendLongDataPacket")
				return nil, err
			}
			req.Message = msg
		}
		requests = append(requests, req)
	}

	mockSpec.MySQLRequests = requests

	// Decode the responses

	responses := []mysql.Response{}
	for _, v := range yamlSpec.Response {

		resp := mysql.Response{
			PacketBundle: mysql.PacketBundle{
				Header: v.Header,
				Meta:   v.Meta,
			},
		}

		switch v.Header.Type {
		// generic response
		case mysql.StatusToString(mysql.EOF):
			msg := &mysql.EOFPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql EOFPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.StatusToString(mysql.ERR):
			msg := &mysql.ERRPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql ERRPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.StatusToString(mysql.OK):
			msg := &mysql.OKPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql OKPacket")
				return nil, err
			}
			resp.Message = msg

		// connection phase
		case mysql.AuthStatusToString(mysql.HandshakeV10):
			msg := &mysql.HandshakeV10Packet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql HandshakeV10Packet")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthSwitchRequest):
			msg := &mysql.AuthSwitchRequestPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthSwitchRequestPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthMoreData):
			msg := &mysql.AuthMoreDataPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthMoreDataPacket")
				return nil, err
			}
			resp.Message = msg

		case mysql.AuthStatusToString(mysql.AuthNextFactor): // not supported yet
			msg := &mysql.AuthNextFactorPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql AuthNextFactorPacket")
				return nil, err
			}
			resp.Message = msg

		// command phase
		case mysql.COM_STMT_PREPARE_OK:
			msg := &mysql.StmtPrepareOkPacket{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql StmtPrepareOkPacket")
				return nil, err
			}
			resp.Message = msg

		case string(mysql.Text):
			msg := &mysql.TextResultSet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql TextResultSet")
				return nil, err
			}
			resp.Message = msg

		case string(mysql.Binary):
			msg := &mysql.BinaryProtocolResultSet{}
			err := v.Message.Decode(msg)
			if err != nil {
				utils.LogError(logger, err, "failed to unmarshal yml document into mysql BinaryProtocolResultSet")
				return nil, err
			}
			resp.Message = msg
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
