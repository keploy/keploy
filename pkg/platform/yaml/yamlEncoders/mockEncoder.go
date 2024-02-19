package yamlencoders

import (
	"errors"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func EncodeMock(mock *models.Mock, logger *zap.Logger) (*models.NetworkTrafficDoc, error) {
	yamlDoc := models.NetworkTrafficDoc{
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
		mongoSpec := models.MongoSchema{
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

		postgresSpec := models.PostgresSchema{
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
		gRPCSpec := models.GrpcSchema{
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

		sqlSpec := models.MySQLSchema{
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
