package decoders

import (
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func DecodeMongoMessage(yamlSpec *models.MongoSchema, logger *zap.Logger) (*models.MockSpec, error) {
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
