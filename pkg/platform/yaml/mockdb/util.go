package mockdb

import (
	"errors"
	"strings"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
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
	case models.Mongo, models.GRPC_EXPORT, models.Postgres, models.MySQL:
		// These kinds are now handled directly in their respective integration packages
		// and should not reach this central encoding function
		utils.LogError(logger, nil, "mock kind should be encoded in integration package", zap.String("kind", string(mock.Kind)))
		return nil, errors.New("mock kind should be encoded in integration package")
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
		// Try to find a registered decoder for this mock kind
		var integrationType integrations.IntegrationType
		switch m.Kind {
		case models.HTTP:
			integrationType = integrations.HTTP
		case models.Mongo:
			integrationType = integrations.MONGO
		case models.GRPC_EXPORT:
			integrationType = integrations.GRPC
		case models.GENERIC:
			integrationType = integrations.GENERIC
		case models.REDIS:
			integrationType = integrations.REDIS
		case models.Postgres:
			integrationType = integrations.POSTGRES_V1
		case models.MySQL:
			integrationType = integrations.MYSQL
		default:
			utils.LogError(logger, nil, "failed to unmarshal a mock yaml doc of unknown type", zap.String("type", string(m.Kind)))
			continue
		}

		decoder, ok := integrations.GetDecoder(integrationType)
		if !ok {
			utils.LogError(logger, nil, "no decoder found for mock kind", zap.String("kind", string(m.Kind)))
			return nil, errors.New("no decoder found for mock kind: " + string(m.Kind))
		}

		mockPtr, err := decoder(m, logger)
		if err != nil {
			return nil, err
		}
		mock = *mockPtr
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

// decodeRedisMock handles Redis mock decoding (kept locally as no Redis integration package)
func decodeRedisMock(networkDoc *yaml.NetworkTrafficDoc, logger *zap.Logger) (*models.Mock, error) {
	if networkDoc.Kind != models.REDIS {
		return nil, errors.New("expected REDIS mock kind")
	}

	mock := models.Mock{
		Version:      networkDoc.Version,
		Name:         networkDoc.Name,
		Kind:         networkDoc.Kind,
		ConnectionID: networkDoc.ConnectionID,
	}

	redisSpec := models.RedisSchema{}
	err := networkDoc.Spec.Decode(&redisSpec)
	if err != nil {
		utils.LogError(logger, err, "failed to unmarshal a yaml doc into redis mock", zap.String("mock name", networkDoc.Name))
		return nil, err
	}

	mock.Spec = models.MockSpec{
		Metadata:         redisSpec.Metadata,
		RedisRequests:    redisSpec.RedisRequests,
		RedisResponses:   redisSpec.RedisResponses,
		ReqTimestampMock: redisSpec.ReqTimestampMock,
		ResTimestampMock: redisSpec.ResTimestampMock,
	}

	return &mock, nil
}
