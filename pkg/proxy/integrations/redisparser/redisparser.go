package redisparser

import (
	"net"
	"strings"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/models/spec"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

// IsOutgoingRedis function determines if the outgoing network call is a Redis command.
func IsOutgoingRedis(buffer []byte) bool {
	command := string(buffer[:])
	return strings.HasPrefix(command, "*") // All Redis commands start with '*'
}

// CaptureRedisMessage function parses the Redis commands and responses to capture outgoing network calls as mocks.
func CaptureRedisMessage(clientConn, destConn net.Conn, logger *zap.Logger) *models.Mock {

	handshakeRequest, err := util.ReadBytes(clientConn)
	//_, err = reader.ReadBytes('\n')
	if err != nil {
		logger.Error("failed to read the command from client", zap.Error(err))
		return nil
	}
	// write the command to the actual Redis server
	_, err = destConn.Write(handshakeRequest)
	if err != nil {
		logger.Error("failed to write command to the Redis server", zap.Error(err))
		return nil
	}

	// read the response from the Redis server
	handshakeResponse, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the response message from the Redis server", zap.Error(err))
		return nil
	}

	// write the response message to the client
	_, err = clientConn.Write(handshakeResponse)
	if err != nil {
		logger.Error("failed to write response message to the client", zap.Error(err))
		return nil
	}
	requestBuffer, err := util.ReadBytes(clientConn)
	if err != nil {
		logger.Error("failed to read the command from client", zap.Error(err))
		return nil
	}

	_, err = destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to forward the command to Redis server", zap.Error(err))
		return nil
	}
	respBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the command from client", zap.Error(err))
		return nil
	}
	_, err = clientConn.Write(respBuffer)
	if err != nil {
		logger.Error("failed to read the response from Redis server", zap.Error(err))
		return nil
	}

	command, err := DecodeRedisRequest(string(requestBuffer))
	if err != nil {
		logger.Error("failed to decode the Redis request", zap.Error(err))
		return nil
	}
	response, err := DecodeRedisResponse(string(respBuffer))
	if err != nil {
		logger.Error("failed to decode the Redis response", zap.Error(err))
		return nil
	}

	interactions := []spec.RedisInteraction{}

	// Now we are assigning slices of strings to Request and Response
	interactions = append(interactions, spec.RedisInteraction{Request: command, Response: response})

	// store the command and response as mocks
	meta := map[string]string{
		"name": "Redis",
		"type": models.RedisClient,
	}

	redisMock := &models.Mock{
		Version: models.V1Beta2,
		Name:    "",
		Kind:    models.Redis,
	}

	err = redisMock.Spec.Encode(&spec.RedisSpec{
		Metadata:     meta,
		Interactions: interactions,
	})
	if err != nil {
		logger.Error("failed to encode the Redis message into the yaml")
		return nil
	}

	return redisMock

}
