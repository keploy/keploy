package yaml

import (
	"errors"

	"fmt"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml/spec"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
	"strings"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version models.Version `json:"version" yaml:"version"`
	Kind    models.Kind    `json:"kind" yaml:"kind"`
	Name    string         `json:"name" yaml:"name"`
	Spec    yamlLib.Node   `json:"spec" yaml:"spec"`
}

// func Encode(tc models.TestCase, logger *zap.Logger) (*NetworkTrafficDoc, []NetworkTrafficDoc, error) {
func EncodeTestcase(tc models.TestCase, logger *zap.Logger) (*NetworkTrafficDoc, error) {
	doc := &NetworkTrafficDoc{
		Version: tc.Version,
		Kind:    tc.Kind,
		Name:    tc.Name,
	}
	// mocks := []NetworkTrafficDoc{}
	// find noisy fields
	m, err := FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		logger.Error(Emoji+msg, zap.Error(err))
	}
	// noise := httpSpec.Assertions["noise"]
	noise := tc.Noise
	fmt.Println("By default noisy fields .. ", noise)
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

	fmt.Println("After Find noisy method .. .", noise)
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
			logger.Error(Emoji+"failed to encode testcase into a yaml doc", zap.Error(err))
			return nil, err
		}
		// mocks, err = encodeMocks(tc.Mocks, logger)
		// if err != nil {
		// 	return nil, err
		// }
	default:
		logger.Error(Emoji + "failed to marshal the testcase into yaml due to invalid kind of testcase")
		return nil, errors.New("type of testcases is invalid")
	}
	return doc, nil
}

// func encodeMocks(mocks []*models.Mock, logger *zap.Logger) ([]NetworkTrafficDoc, error) {
func EncodeMock(mock *models.Mock, logger *zap.Logger) (*NetworkTrafficDoc, error) {
	// yamlMocks := []NetworkTrafficDoc{}
	// for _, m := range mocks {
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
				logger.Error(Emoji+"failed to encode mongo request wiremessage into yaml", zap.Error(err))
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
				logger.Error(Emoji+"failed to encode mongo response wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			responses = append(responses, resp)
		}
		mongoSpec := spec.MongoSpec{
			Metadata:  mock.Spec.Metadata,
			Requests:  requests,
			Response:  responses,
			CreatedAt: mock.Spec.Created,
			// RequestHeader:  *mock.Spec.MongoRequestHeader,
			// ResponseHeader: *mock.Spec.MongoResponseHeader,
		}
		// err := mongoSpec.Request.Encode(mock.Spec.MongoRequest)
		// if err != nil {
		// 	logger.Error(Emoji+"failed to encode mongo request wiremessage into yaml", zap.Error(err))
		// 	return nil, err
		// }

		// err = mongoSpec.Response.Encode(mock.Spec.MongoResponse)
		// if err != nil {
		// 	logger.Error(Emoji+"failed to encode mongo response wiremessage into yaml", zap.Error(err))
		// 	return nil, err
		// }

		err := yamlDoc.Spec.Encode(mongoSpec)
		if err != nil {
			logger.Error(Emoji+"failed to marshal the mongo input-output as yaml", zap.Error(err))
			return nil, err
		}

	case models.HTTP:
		httpSpec := spec.HttpSpec{
			Metadata: mock.Spec.Metadata,
			Request:  *mock.Spec.HttpReq,
			Response: *mock.Spec.HttpResp,
			Created:  mock.Spec.Created,
			Objects:  mock.Spec.OutputBinary,
		}
		err := yamlDoc.Spec.Encode(httpSpec)
		if err != nil {
			logger.Error(Emoji+"failed to marshal the http input-output as yaml", zap.Error(err))
			return nil, err
		}
	case models.GENERIC:
		genericSpec := spec.GenericSpec{
			Metadata: mock.Spec.Metadata,
			Objects:  mock.Spec.OutputBinary,
		}
		err := yamlDoc.Spec.Encode(genericSpec)
		if err != nil {
			logger.Error(Emoji+"failed to marshal binary input-output of external call into yaml", zap.Error(err))
			return nil, err
		}
	default:
		logger.Error(Emoji + "failed to marshal the recorded mock into yaml due to invalid kind of mock")
		return nil, errors.New("type of mock is invalid")
	}

	return &yamlDoc, nil
	// yamlMocks = append(yamlMocks, yamlDoc)
	// }
	// return yamlMocks, nil
}

// func Decode(yamlTestcase *NetworkTrafficDoc, yamlMocks []*NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
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
			logger.Error(Emoji+"failed to unmarshal a yaml doc into the http testcase", zap.Error(err))
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HttpReq = httpSpec.Request
		tc.HttpResp = httpSpec.Response
		tc.Noise = httpSpec.Assertions["noise"]
		// mocks, err := decodeMocks(yamlMocks, logger)
		// tc.Mocks = mocks
		// unmarshal its mocks from yaml docs to go struct
	default:
		logger.Error(Emoji+"failed to unmarshal yaml doc of unknown type", zap.Any("type of yaml doc", tc.Kind))
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
				logger.Error(Emoji+"failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:     httpSpec.Metadata,
				HttpReq:      &httpSpec.Request,
				HttpResp:     &httpSpec.Response,
				OutputBinary: httpSpec.Objects,
				Created:      httpSpec.Created,
			}
		case models.Mongo:
			mongoSpec := spec.MongoSpec{}
			err := m.Spec.Decode(&mongoSpec)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMongoMessage(&mongoSpec, logger)
			if err != nil {
				return nil, err
			}
			mock.Spec = *mockSpec
			// mock.Spec = models.MockSpec{
			// 	Metadata: mongoSpec.Metadata,
			// 	MongoRequestHeader: &mongoSpec.RequestHeader,
			// 	MongoResponseHeader: &mongoSpec.ResponseHeader,
			// 	// MongoRequest: ,
			// }
		case models.GENERIC:
			genericSpec := spec.GenericSpec{}
			err := m.Spec.Decode(&genericSpec)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal a yaml doc into generic mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata:     genericSpec.Metadata,
				OutputBinary: genericSpec.Objects,
			}
		default:
			logger.Error(Emoji+"failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
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
		// MongoRequestHeader:  &yamlSpec.RequestHeader,
		// MongoResponseHeader: &yamlSpec.ResponseHeader,
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
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpMsg request wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
		case wiremessage.OpReply:
			requestMessage := &models.MongoOpReply{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpReply wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
			// doc.Spec.MongoRequest = &proto.MongoMessage{
			// 	ResponseFlags: req.ResponseFlags,
			// 	CursorID: req.CursorID,
			// 	StartingFrom: req.StartingFrom,
			// 	NumberReturned: req.NumberReturned,
			// 	Documents: req.Documents,
			// }
		case wiremessage.OpQuery:
			requestMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(requestMessage)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpQuery wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			req.Message = requestMessage
			// doc.Spec.MongoRequest = &proto.MongoMessage{
			// 	Flags: req.Flags,
			// 	FullCollectionName: req.FullCollectionName,
			// 	NumberToSkip: req.NumberToSkip,
			// 	NumberToReturn: req.NumberToReturn,
			// 	Query: req.Query,
			// 	ReturnFieldsSelector: req.ReturnFieldsSelector,
			// }
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
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
			// doc.Spec.MongoResponse = &proto.MongoMessage{
			// 	FlagBits: int64(resp.FlagBits),
			// 	Sections: resp.Sections,
			// 	Checksum: int64(resp.Checksum),
			// }
		case wiremessage.OpReply:
			responseMessage := &models.MongoOpReply{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
			// doc.Spec.MongoResponse = &proto.MongoMessage{
			// 	ResponseFlags: resp.ResponseFlags,
			// 	CursorID: resp.CursorID,
			// 	StartingFrom: resp.StartingFrom,
			// 	NumberReturned: resp.NumberReturned,
			// 	Documents: resp.Documents,
			// }
		case wiremessage.OpQuery:
			responseMessage := &models.MongoOpQuery{}
			err := v.Message.Decode(responseMessage)
			if err != nil {
				logger.Error(Emoji+"failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
				// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
				return nil, err
			}
			resp.Message = responseMessage
			// doc.Spec.MongoResponse = &proto.MongoMessage{
			// 	Flags: resp.Flags,
			// 	FullCollectionName: resp.FullCollectionName,
			// 	NumberToSkip: resp.NumberToSkip,
			// 	NumberToReturn: resp.NumberToReturn,
			// 	Query: resp.Query,
			// 	ReturnFieldsSelector: resp.ReturnFieldsSelector,
			// }
		default:
			// TODO
		}
		responses = append(responses, resp)
	}
	mockSpec.MongoResponses = responses
	return &mockSpec, nil
}
