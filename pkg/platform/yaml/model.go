package yaml

import (
	"errors"

	"strings"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml/spec"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version models.Version `json:"version" yaml:"version"`
	Kind    models.Kind    `json:"kind" yaml:"kind"`
	Name    string         `json:"name" yaml:"name"`
	Spec    yamlLib.Node   `json:"spec" yaml:"spec"`
	Curl    string         `json:"curl" yaml:"curl,omitempty"`
}

func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*NetworkTrafficDoc, error) {

	header := pkg.ToHttpHeader(tc.HttpReq.Header)
	curl := pkg.MakeCurlCommand(string(tc.HttpReq.Method), tc.HttpReq.URL, pkg.ToYamlHttpHeader(header), tc.HttpReq.Body)
	doc := &NetworkTrafficDoc{
		Version: tc.Version,
		Kind:    tc.Kind,
		Name:    tc.Name,
		Curl:    curl,
	}
	// find noisy fields
	m, err := FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		logger.Error(msg, zap.Error(err))
	}
	noise := tc.Noise

	noise = append(noise, FindNoisyFields(m, func(k string, vals []string) bool {
		// check if k is date
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}

		// maybe we need to concatenate the values
		return pkg.IsTime(strings.Join(vals, ", "))
	})...)

	switch tc.Kind {
	case models.HTTP:
		err := doc.Spec.Encode(spec.HttpSpec{
			Request:  tc.HttpReq,
			Response: tc.HttpResp,
			Created:  tc.Created,
			Assertions: map[string][]string{
				"noise": noise,
			},
		})
		if err != nil {
			logger.Error("failed to encode testcase into a yaml doc", zap.Error(err))
			return nil, err
		}
	default:
		logger.Error("failed to marshal the testcase into yaml due to invalid kind of testcase")
		return nil, errors.New("type of testcases is invalid")
	}
	return doc, nil
}

func EncodeMock(mock *models.Mock, logger *zap.Logger) (*NetworkTrafficDoc, error) {
	yamlDoc := NetworkTrafficDoc{
		Version: mock.Version,
		Kind:    mock.Kind,
		Name:    mock.Name,
	}
	switch mock.Kind {
	case models.Mongo:
		requests := []spec.RequestYaml{}
		for _, v := range mock.Spec.MongoRequests {
			req := spec.RequestYaml{
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
		responses := []spec.ResponseYaml{}
		for _, v := range mock.Spec.MongoResponses {
			resp := spec.ResponseYaml{
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
		mongoSpec := spec.MongoSpec{
			Metadata:  mock.Spec.Metadata,
			Requests:  requests,
			Response:  responses,
			CreatedAt: mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(mongoSpec)
		if err != nil {
			logger.Error("failed to marshal the mongo input-output as yaml", zap.Error(err))
			return nil, err
		}

	case models.HTTP:
		httpSpec := spec.HttpSpec{
			Metadata: mock.Spec.Metadata,
			Request:  *mock.Spec.HttpReq,
			Response: *mock.Spec.HttpResp,
			Created:  mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(httpSpec)
		if err != nil {
			logger.Error("failed to marshal the http input-output as yaml", zap.Error(err))
			return nil, err
		}
	case models.GENERIC:
		genericSpec := spec.GenericSpec{
			Metadata: mock.Spec.Metadata,
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

		postgresSpec := spec.PostgresSpec{
			Metadata: mock.Spec.Metadata,
			PostgresRequests:  mock.Spec.PostgresRequests,
			PostgresResponses: mock.Spec.PostgresResponses,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}

		err := yamlDoc.Spec.Encode(postgresSpec)
		if err != nil {
			logger.Error("failed to marshal postgres of external call into yaml", zap.Error(err))
			return nil, err
		}
	case models.GRPC_EXPORT:
		gRPCSpec := spec.GrpcSpec{
			GrpcReq:  *mock.Spec.GRPCReq,
			GrpcResp: *mock.Spec.GRPCResp,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		err := yamlDoc.Spec.Encode(gRPCSpec)
		if err != nil {
			logger.Error(Emoji+"failed to marshal gRPC of external call into yaml", zap.Error(err))
			return nil, err
		}
	default:
		logger.Error("failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
}

func Decode(yamlTestcase *NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := models.TestCase{
		Version: yamlTestcase.Version,
		Kind:    yamlTestcase.Kind,
		Name:    yamlTestcase.Name,
	}

	switch tc.Kind {
	case models.HTTP:
		httpSpec := spec.HttpSpec{}
		err := yamlTestcase.Spec.Decode(&httpSpec)
		if err != nil {
			logger.Error("failed to unmarshal a yaml doc into the http testcase", zap.Error(err))
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HttpReq = httpSpec.Request
		tc.HttpResp = httpSpec.Response
		tc.Noise = httpSpec.Assertions["noise"]
	// unmarshal its mocks from yaml docs to go struct
	case models.GRPC_EXPORT:
		grpcSpec := spec.GrpcSpec{}
		err := yamlTestcase.Spec.Decode(&grpcSpec)
		if err != nil {
			logger.Error(Emoji+"failed to unmarshal a yaml doc into the gRPC testcase", zap.Error(err))
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

func decodeMocks(yamlMocks []*NetworkTrafficDoc, logger *zap.Logger) ([]*models.Mock, error) {
	mocks := []*models.Mock{}

	for _, m := range yamlMocks {
		mock := models.Mock{
			Version: m.Version,
			Name:    m.Name,
			Kind:    m.Kind,
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := spec.HttpSpec{}
			err := m.Spec.Decode(&httpSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,
				HttpReq:  &httpSpec.Request,
				HttpResp: &httpSpec.Response,
				Created: httpSpec.Created,
				ReqTimestampMock: httpSpec.ReqTimestampMock,
				ResTimestampMock: httpSpec.ResTimestampMock,
			}
		case models.Mongo:
			mongoSpec := spec.MongoSpec{}
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
			grpcSpec := spec.GrpcSpec{}
			err := m.Spec.Decode(&grpcSpec)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				GRPCResp: &grpcSpec.GrpcResp,
				GRPCReq: &grpcSpec.GrpcReq,
				ReqTimestampMock: grpcSpec.ReqTimestampMock,
				ResTimestampMock: grpcSpec.ResTimestampMock,
			}
		case models.GENERIC:
			genericSpec := spec.GenericSpec{}
			err := m.Spec.Decode(&genericSpec)
			if err != nil {
				logger.Error("failed to unmarshal a yaml doc into generic mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: genericSpec.Metadata,
				GenericRequests:  genericSpec.GenericRequests,
				GenericResponses: genericSpec.GenericResponses,
				ReqTimestampMock: genericSpec.ReqTimestampMock,
				ResTimestampMock: genericSpec.ResTimestampMock,
			}

		case models.Postgres:

			PostSpec := spec.PostgresSpec{}
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
				ReqTimestampMock: PostSpec.ReqTimestampMock,
				ResTimestampMock: PostSpec.ResTimestampMock,

			}
		default:
			logger.Error("failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
			return nil, errors.New("yaml doc of unknown type")
		}
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

func decodeMongoMessage(yamlSpec *spec.MongoSpec, logger *zap.Logger) (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,
		Created:  yamlSpec.CreatedAt,
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
