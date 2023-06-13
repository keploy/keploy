package mongoparser

import (
	"encoding/binary"
	"net"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/models/spec"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

// IsOutgoingMongo function determines if the outgoing network call is Mongo by comparing the
// message format with that of a mongo wire message.
func IsOutgoingMongo(buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	messageLength := binary.LittleEndian.Uint32(buffer[0:4])
	return int(messageLength) == len(buffer)
}

func CaptureMongoMessage(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) []*models.Mock {

	// write the request message to the mongo server
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write the request buffer to mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// read reply message from the mongo server
	responseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the reply to mongo client
	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		logger.Error("failed to write the reply message to mongo client", zap.Error(err))
		return nil
	}

	// read the operation request message from the mongo client
	msgRequestbuffer, err := util.ReadBytes(clientConn)
	if err != nil {
		logger.Error("failed to read the message from the mongo client", zap.Error(err))
		return nil
	}

	opr1, _, _, err := Decode(msgRequestbuffer)
	if err != nil {
		// logger.Error("failed to decode t")
		return nil
	}

	// write the request message to mongo server
	_, err = destConn.Write(msgRequestbuffer)
	if err != nil {
		logger.Error("failed to write the request message to mongo server", zap.Error(err), zap.String("mongo server address", destConn.LocalAddr().String()))
		return nil
	}

	// read the response message form the mongo server
	msgResponseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the response message from mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the response message to mongo client
	_, err = clientConn.Write(msgResponseBuffer)
	if err != nil {
		logger.Error("failed to write the response wiremessage to mongo client", zap.Error(err))
		return nil
	}

	// capture if the wiremessage is a mongo operation call
	if !strings.Contains(opr1.String(), "hello") {
		deps := []*models.Mock{}

		// decode the mongo binary request wiremessage
		opr, requestHeader, mongoRequest, err := Decode((requestBuffer))
		if err != nil {
			logger.Error("failed tp decode the mongo wire message from the client", zap.Error(err))
			return nil
		}

		// decode the mongo binary response wiremessage
		op, responseHeader, mongoResp, err := Decode(responseBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
			return nil
		}

		replyDocs := []string{}
		for _, v := range op.(*opReply).documents {
			replyDocs = append(replyDocs, v.String())
		}
		meta1 := map[string]string{
			"operation": opr.String(),
		}
		mongoMock := &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.Mongo,
			Name:    "",
		}
		mongoSpec := &spec.MongoSpec{
			Metadata:       meta1,
			RequestHeader:  requestHeader,
			ResponseHeader: responseHeader,
		}
		err = mongoSpec.Request.Encode(mongoRequest)
		if err != nil {
			logger.Error("failed to encode the request mongo wiremessage into yaml doc", zap.Error(err))
			return nil
		}
		err = mongoSpec.Response.Encode(mongoResp)
		if err != nil {
			logger.Error("failed to encode the response mongo wiremessage into yaml doc", zap.Error(err))
			return nil
		}
		mongoMock.Spec.Encode(mongoSpec)
		deps = append(deps, mongoMock)

		meta := map[string]string{
			"operation": opr1.String(),
		}

		opr, msgRequestHeader, mongoMsgRequest, err := Decode((msgRequestbuffer))
		if err != nil {
			logger.Error("failed tp decode the mongo wire message from the client", zap.Error(err))
			return nil
		}

		op, msgResponseHeader, mongoMsgResponse, err := Decode(msgResponseBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
			return nil
		}
		mongoMock = &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.Mongo,
			Name:    "",
		}
		mongoSpec = &spec.MongoSpec{
			Metadata:       meta,
			RequestHeader:  msgRequestHeader,
			ResponseHeader: msgResponseHeader,
		}
		err = mongoSpec.Request.Encode(mongoMsgRequest)
		if err != nil {
			logger.Error("failed to encode the request mongo wiremessage into yaml doc", zap.Error(err))
			return nil
		}
		err = mongoSpec.Response.Encode(mongoMsgResponse)
		if err != nil {
			logger.Error("failed to encode the response mongo wiremessage into yaml doc", zap.Error(err))
			return nil
		}
		mongoMock.Spec.Encode(mongoSpec)
		deps = append(deps, mongoMock)
		return deps
	}

	return nil
}
