package yamldecoders

import (
	"strings"

	"go.keploy.io/server/pkg/models"
	decoders "go.keploy.io/server/pkg/platform/yaml/dbDecoders"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func DecodeMocks(yamlMocks []*models.NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
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
			mongoSpec := models.MongoSchema{}
			err := m.Spec.Decode(&mongoSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decoders.DecodeMongoMessage(&mongoSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
		case models.GRPC_EXPORT:
			grpcSpec := models.GrpcSchema{}
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

			PostSpec := models.PostgresSchema{}
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
			mysqlSpec := models.MySQLSchema{}
			err := m.Spec.Decode(&mysqlSpec)
			if err != nil {
				logger.Error(utils.Emoji+"failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decoders.DecodeMySqlMessage(&mysqlSpec, logger)
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
