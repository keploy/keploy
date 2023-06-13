package yaml

import (
	"errors"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml/spec"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version models.Version   `json:"version" yaml:"version"`
	Kind    models.Kind      `json:"kind" yaml:"kind"`
	Name    string    `json:"name" yaml:"name"`
	Spec    yamlLib.Node `json:"spec" yaml:"spec"`
}

func Encode (tc models.TestCase, logger *zap.Logger) (*NetworkTrafficDoc, []NetworkTrafficDoc, error) {
	doc := &NetworkTrafficDoc{
		Version: tc.Version,
		Kind: tc.Kind,
		Name: tc.Name,
	}
	mocks := []NetworkTrafficDoc{}
	switch tc.Kind {
	case models.HTTP:
		err := doc.Spec.Encode(spec.HttpSpec{
			Request: tc.HttpReq,
			Response: tc.HttpResp,
			Created: tc.Created,
			Assertions: map[string][]string{
				"noise": tc.Noise,
			},
		})
		if err!=nil {
			logger.Error("failed to encode testcase into a yaml doc", zap.Error(err))
			return nil, nil, err
		}
		mocks, err = encodeMocks(tc.Mocks, logger)
		if err!=nil {
			return nil, nil, err
		}
	default:
		logger.Error("failed to marshal the testcase into yaml due to invalid kind of testcase")
		return nil, nil, errors.New("type of testcases is invalid")
	}
	return doc, mocks, nil
}

func encodeMocks (mocks []*models.Mock, logger *zap.Logger) ([]NetworkTrafficDoc, error) {
	yamlMocks := []NetworkTrafficDoc{}
	for _, m := range mocks {
		yamlDoc := NetworkTrafficDoc{
			Version: m.Version,
			Kind: m.Kind,
			Name: m.Name,
		}
		switch m.Kind {
		case models.Mongo:
			mongoSpec := spec.MongoSpec{
				Metadata: m.Spec.Metadata,
				RequestHeader: *m.Spec.MongoRequestHeader,
				ResponseHeader: *m.Spec.MongoResponseHeader,
			} 
			err := mongoSpec.Request.Encode(m.Spec.MongoRequest)
			if err!=nil {
				logger.Error("failed to encode mongo request wiremessage into yaml", zap.Error(err))
				return nil, err
			}
			
			err = mongoSpec.Response.Encode(m.Spec.MongoResponse)
			if err!=nil {
				logger.Error("failed to encode mongo response wiremessage into yaml", zap.Error(err))
				return nil, err
			}

			err = yamlDoc.Spec.Encode(mongoSpec)
			if err!=nil {
				logger.Error("failed to marshal the mongo input-output as yaml", zap.Error(err))
				return nil, err
			}

		case models.HTTP:
			httpSpec := spec.HttpSpec{
				Metadata: m.Spec.Metadata,
				Request: *m.Spec.HttpReq,
				Response: *m.Spec.HttpResp,
				Created: m.Spec.Created,
				Objects: m.Spec.OutputBinary,
			} 
			err := yamlDoc.Spec.Encode(httpSpec)
			if err!=nil {
				logger.Error("failed to marshal the http input-output as yaml", zap.Error(err))
				return nil, err
			}
		case models.GENERIC:
			genericSpec := spec.GenericSpec{
				Metadata: m.Spec.Metadata,
				Objects: m.Spec.OutputBinary,
			}
			err := yamlDoc.Spec.Encode(genericSpec)
			if err!=nil {
				logger.Error("failed to marshal binary input-output of external call into yaml", zap.Error(err))
				return nil, err
			}
		default: 
			logger.Error("failed to marshal the recorded mock into yaml due to invalid kind of mock")
			return nil, errors.New("type of mock is invalid")
		}
		yamlMocks = append(yamlMocks, yamlDoc)
	}
	return yamlMocks, nil
}

func Decode (yamlTestcase *NetworkTrafficDoc, yamlMocks []*NetworkTrafficDoc, logger *zap.Logger) (*models.TestCase, error) {
	tc := models.TestCase{
		Version: yamlTestcase.Version,
		Kind: yamlTestcase.Kind,
		Name: yamlTestcase.Name,
	}

	switch tc.Kind {
	case models.HTTP:
		httpSpec := spec.HttpSpec{}
		err := yamlTestcase.Spec.Decode(&httpSpec)
		if err!=nil {
			logger.Error("failed to unmarshal a yaml doc into the http testcase", zap.Error(err))
			return nil, err
		}
		tc.Created = httpSpec.Created
		tc.HttpReq = httpSpec.Request
		tc.HttpResp = httpSpec.Response
		tc.Noise = httpSpec.Assertions["noise"]
		mocks, err := decodeMocks(yamlMocks, logger)
		tc.Mocks = mocks
		// unmarshal its mocks from yaml docs to go struct
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
			Name: m.Name,
			Kind: m.Kind,
		}
		switch m.Kind {
		case models.HTTP:
			httpSpec := spec.HttpSpec{}
			err := m.Spec.Decode(&httpSpec)
			if err!=nil {
				logger.Error("failed to unmarshal a yaml doc into http mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: httpSpec.Metadata,
				HttpReq: &httpSpec.Request,
				HttpResp: &httpSpec.Response,
				OutputBinary: httpSpec.Objects,
				Created: httpSpec.Created,
			}
		case models.Mongo:
			mongoSpec := spec.MongoSpec{}
			err := m.Spec.Decode(&mongoSpec)
			if err!=nil {
				logger.Error("failed to unmarshal a yaml doc into mongo mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}

			mockSpec, err := decodeMongoMessage(&mongoSpec, logger)
			if err!=nil {
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
			if err!=nil {
				logger.Error("failed to unmarshal a yaml doc into generic mock", zap.Error(err), zap.Any("mock name", m.Name))
				return nil, err
			}
			mock.Spec = models.MockSpec{
				Metadata: genericSpec.Metadata,
				OutputBinary: genericSpec.Objects,
			}
		default:
			logger.Error("failed to unmarshal a mock yaml doc of unknown type", zap.Any("type", m.Kind))
			return nil, errors.New("yaml doc of unknown type")
		}	
		mocks = append(mocks, &mock)
	}

	return mocks, nil
}

func decodeMongoMessage (yamlSpec *spec.MongoSpec, logger *zap.Logger)  (*models.MockSpec, error) {
	mockSpec := models.MockSpec{
		Metadata: yamlSpec.Metadata,
		MongoRequestHeader: &yamlSpec.RequestHeader,
		MongoResponseHeader: &yamlSpec.ResponseHeader,
	}

	// mongo request
	switch yamlSpec.RequestHeader.Opcode {
	case wiremessage.OpMsg:
		req := &models.MongoOpMessage{}
		err := yamlSpec.Request.Decode(req)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpMsg request wiremessage", zap.Error(err))
			// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
			return nil, err
		}
		mockSpec.MongoRequest = req
	case wiremessage.OpReply:
		req := &models.MongoOpReply{}
		err := yamlSpec.Request.Decode(req)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpReply wiremessage", zap.Error(err))
			// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
			return nil, err
		}
		mockSpec.MongoRequest = req
		// doc.Spec.MongoRequest = &proto.MongoMessage{
		// 	ResponseFlags: req.ResponseFlags,
		// 	CursorID: req.CursorID,
		// 	StartingFrom: req.StartingFrom,
		// 	NumberReturned: req.NumberReturned,
		// 	Documents: req.Documents,
		// }
	case wiremessage.OpQuery:
		req := &models.MongoOpQuery{}
		err := yamlSpec.Request.Decode(req)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpQuery wiremessage", zap.Error(err))
			// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
			return nil, err
		}
		mockSpec.MongoRequest = req
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

	// mongo response
	switch yamlSpec.ResponseHeader.Opcode {
	case wiremessage.OpMsg:
		resp := &models.MongoOpMessage{}
		err := yamlSpec.Response.Decode(resp)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpMsg response wiremessage", zap.Error(err))
			// return fmt.Errorf("failed to decode the mongo OpMsg of mock with name: %s.  error: %s", doc.Name, err.Error())
			return nil, err
		}
		mockSpec.MongoResponse = resp
		// doc.Spec.MongoResponse = &proto.MongoMessage{
		// 	FlagBits: int64(resp.FlagBits),
		// 	Sections: resp.Sections,
		// 	Checksum: int64(resp.Checksum),
		// }
	case wiremessage.OpReply:
		resp := &models.MongoOpReply{}
		err := yamlSpec.Response.Decode(resp)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpReply wiremessage", zap.Error(err))
			// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
			return nil, err
		}
		mockSpec.MongoResponse = resp
		// doc.Spec.MongoResponse = &proto.MongoMessage{
		// 	ResponseFlags: resp.ResponseFlags,
		// 	CursorID: resp.CursorID,
		// 	StartingFrom: resp.StartingFrom,
		// 	NumberReturned: resp.NumberReturned,
		// 	Documents: resp.Documents,
		// }
	case wiremessage.OpQuery:
		resp := &models.MongoOpQuery{}
		err := yamlSpec.Response.Decode(resp)
		if err != nil {
			logger.Error("failed to unmarshal yml document into mongo OpQuery wiremessage", zap.Error(err))
			return nil, err
			// return fmt.Errorf("failed to decode the mongo OpReply of mock with name: %s.  error: %s", doc.Name, err.Error())
		}
		mockSpec.MongoResponse = resp
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
	return &mockSpec, nil
}