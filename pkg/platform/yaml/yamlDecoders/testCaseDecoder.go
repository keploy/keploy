package yamldecoders

import (
	"errors"
	"reflect"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

func DecodeTestCase(yamlTestcase *models.NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := models.TestCase{
		Version: yamlTestcase.Version,
		Kind:    yamlTestcase.Kind,
		Name:    yamlTestcase.Name,
		Curl:    yamlTestcase.Curl,
	}
	switch tc.Kind {
	case models.HTTP:
		httpSpec := models.HttpSchema{}
		err := yamlTestcase.Spec.Decode(&httpSpec)
		if err != nil {
			logger.Error("failed to unmarshal a yaml doc into the http testcase", zap.Error(err))
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HttpReq = httpSpec.Request
		tc.HttpResp = httpSpec.Response
		tc.Noise = map[string][]string{}
		switch reflect.ValueOf(httpSpec.Assertions["noise"]).Kind() {
		case reflect.Map:
			for k, v := range httpSpec.Assertions["noise"].(map[string]interface{}) {
				tc.Noise[k] = []string{}
				for _, val := range v.([]interface{}) {
					tc.Noise[k] = append(tc.Noise[k], val.(string))
				}
			}
		case reflect.Slice:
			for _, v := range httpSpec.Assertions["noise"].([]interface{}) {
				tc.Noise[v.(string)] = []string{}
			}
		}
	// unmarshal its mocks from yaml docs to go struct
	case models.GRPC_EXPORT:
		grpcSpec := models.GrpcSchema{}
		err := yamlTestcase.Spec.Decode(&grpcSpec)
		if err != nil {
			logger.Error(utils.Emoji+"failed to unmarshal a yaml doc into the gRPC testcase", zap.Error(err))
			return nil, err
		}
		tc.GrpcReq = grpcSpec.GrpcReq
		tc.GrpcResp = grpcSpec.GrpcResp
	default:
		logger.Error("failed to unmarshal yaml doc of unknown type", zap.Any("type of yaml doc", tc.Kind))
		return nil, errors.New("yaml doc of unknown type")
	}
	return &tc, nil
}
