package redisv2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func encodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {
	var redisProtocolVersion int
	var redisRequests []models.RedisRequests
	var redisResponses []models.RedisResponses

	bufStr := string(reqBuf)

	dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr = util.EncodeBase64(reqBuf)
		dataType = "binary"
	}

	// bufStr = removeCRLF(bufStr)
	size,err := strconv.Atoi(bufStr[1:2])
	if err !=nil{
		return fmt.Errorf("Error finding size of the data")
	}

	if bufStr != "" {
		redisRequests = append(redisRequests, models.RedisRequests{
			Origin: models.FromClient,
			Message: []models.RedisBodyType{
				{
					Type: "array",
					Size: size,
					Data: bufStr[4:],
				},
			},
		})
	}

	logger.Debug("Encoding redis request", zap.String("dataType", dataType), zap.String("bufStr", bufStr))

	// read initial buffer, if it ping, then version 2
	// if we get hello, then see the next argument to find out the version
	if len(bufStr) > 0 {
		// Check if 'ping' is in the buffer, version 2
		if strings.Contains(bufStr, "ping") {
			redisProtocolVersion = 2
		} else if strings.Contains(bufStr, "hello") {
			// If "hello" is found, extract the next value after it to determine the version
			// Find the start of the next value (after "hello" and CRLF characters)
			startIndex := strings.Index(bufStr, "hello") + len("hello") + 6 // Skip "hello" and CRLF
			if startIndex < len(bufStr) {
				fmt.Println(bufStr,"heelo che k")
				// Extract the value after "hello" to determine the protocol version
				fmt.Println(bufStr[startIndex : startIndex+1])
				version, err := strconv.Atoi(bufStr[startIndex : startIndex+1]) // Adjust depending on the expected protocol version format
				fmt.Println(version,"version here")
				redisProtocolVersion = version
				if err != nil {
					// Handle error if conversion fails
					logger.Error("Failed to convert protocol version to int", zap.Error(err))
					return fmt.Errorf("failed to convert protocol version to int: %w", err)
				}
			} else {
				// Handle case where version after "hello" is not found
				logger.Error("No protocol version found after hello")
				return fmt.Errorf("no protocol version found after hello")
			}
		}
	}

	logger.Debug("here is redisprotocolversion",zap.Any("here",redisProtocolVersion))

	_, err = destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}
	errCh := make(chan error, 1)

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	reqTimestampMock := time.Now()

	// Read and process responses from the destination server
	g.Go(func() error {
		defer pUtil.Recover(logger, clientConn, destConn)
		defer close(errCh)

		for {
			// Read the response from the destination server
			resp, err := pUtil.ReadBytes(ctx, logger, destConn)
			logger.Info("here is response", zap.Any("here", resp))
			if err != nil {
				if err == io.EOF {
					logger.Debug("Response complete, exiting the loop.")
					// if there is any buffer left before EOF, we must send it to the client and save this as mock
					if len(resp) != 0 {
						resTimestampMock := time.Now()
						_, err = clientConn.Write(resp)
						if err != nil {
							utils.LogError(logger, err, "failed to write response message to the client")
							errCh <- err
							return nil
						}
						processBufferResponses(resp, models.FromServer, &redisResponses)
						saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
					}
					break
				}
				utils.LogError(logger, err, "failed to read the response message from the destination server")
				errCh <- err
				return nil
			}

			// Write the response message to the client
			_, err = clientConn.Write(resp)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				errCh <- err
				return nil
			}

			resTimestampMock := time.Now()
			processBufferResponses(resp, models.FromServer, &redisResponses)

			// Save the mock with both request and response
			if len(redisRequests) > 0 && len(redisResponses) > 0 {
				saveMock(redisRequests, redisResponses, reqTimestampMock, resTimestampMock, mocks)
				redisRequests = []models.RedisRequests{}
				redisResponses = []models.RedisResponses{}
			}

			// Read the next request from the client
			reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read the request message from the client")
					errCh <- err
					return nil
				}
				errCh <- err
				return nil
			}

			processBufferRequests(reqBuf, models.FromClient, &redisRequests)
			_, err = destConn.Write(reqBuf)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				errCh <- err
				return nil
			}
			reqTimestampMock = time.Now()
		}
		return nil
	})

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}

}

func processBufferRequests(buffer []byte, origin models.OriginType, payloads *[]models.RedisRequests) {
	bufStr := string(buffer)
	var buffDataType string

	// bufStr = removeCRLF(bufStr)

	// Check the first character of the buffer to determine its type
	if len(bufStr) > 0 {
		switch {
		case bufStr[0] == '*':
			// If it starts with '*', it's an array
			buffDataType = "array"
		case bufStr[0] == '+':
			// If it starts with '+', it's a string
			buffDataType = "SimpleString"
		case bufStr[0] == '%':
			// If it starts with '%', it's a map
			buffDataType = "map"
		case bufStr[0] == '-':
			buffDataType = "error"
		case bufStr[0] == '#':
			buffDataType = "boolean"
		case bufStr[0] == ':':
			buffDataType = "integer"
		case bufStr[0] == '~':
			buffDataType = "set"
		case bufStr[0] == '$':
			buffDataType = "BulkString"
		default:
			// If it starts with anything else, default to string type
			buffDataType = "string"
		}
	}

	// If buffer is not empty, append the Redis response
	firstCRLFRes := getBeforeFirstCRLF(bufStr)
	size,err := strconv.Atoi(firstCRLFRes[1:])
	if err !=nil{
		fmt.Errorf("error finding first crlf")
	}

	dataFromBuf := bufStr
	// if the type is simple string we do not get it size, so we pass in 1
	if buffDataType !="SimpleString"{
		// dataFromBuf = dataFromBuf[4:]
		dataFromBuf = removeBeforeFirstCRLF(dataFromBuf)
	}
	if buffDataType == "SimpleString"{
		size = 1
		dataFromBuf = removeCRLF(dataFromBuf)
	}
	if buffDataType == "BulkString"{
		dataFromBuf = removeCRLF(dataFromBuf)
	}

	if bufStr != "" {

		*payloads = append(*payloads, models.RedisRequests{
			Origin: origin,
			Message: []models.RedisBodyType{
				{
					Type: buffDataType,
					Size: size,
					Data: handleDataByType(buffDataType,dataFromBuf),
				},
			},
		})
	}
}

func processBufferResponses(buffer []byte, origin models.OriginType, payloads *[]models.RedisResponses) {
	bufStr := string(buffer)
	var buffDataType string

	// bufStr = removeCRLF(bufStr)

	// Check the first character of the buffer to determine its type
	if len(bufStr) > 0 {
		switch {
		case bufStr[0] == '*':
			// If it starts with '*', it's an array
			buffDataType = "array"
		case bufStr[0] == '+':
			// If it starts with '+', it's a string
			buffDataType = "SimpleString"
		case bufStr[0] == '%':
			// If it starts with '%', it's a map
			buffDataType = "map"
		case bufStr[0] == '-':
			buffDataType = "error"
		case bufStr[0] == '#':
			buffDataType = "boolean"
		case bufStr[0] == ':':
			buffDataType = "integer"
		case bufStr[0] == '~':
			buffDataType = "set"
		case bufStr[0] == '$':
			buffDataType = "BulkString"

		default:
			// If it starts with anything else, default to string type
			buffDataType = "string"
		}
	}

	// If buffer is not empty, append the Redis response
	firstCRLFRes := getBeforeFirstCRLF(bufStr)
	size,err := strconv.Atoi(firstCRLFRes[1:])
	if err !=nil{
		fmt.Errorf("error finding first crlf")
	}

	// if the type is simple string we do not get it size, so we pass in 1
	dataFromBuf := bufStr
	if buffDataType !="SimpleString"{
		// dataFromBuf = dataFromBuf[4:]
		dataFromBuf = removeBeforeFirstCRLF(dataFromBuf)
	}
	if buffDataType == "SimpleString"{
		size = 1
		dataFromBuf = removeCRLF(dataFromBuf)
	}
	if buffDataType == "BulkString"{
		dataFromBuf = removeCRLF(dataFromBuf)
	}

	if bufStr != "" {

		*payloads = append(*payloads, models.RedisResponses{
			Origin: origin,
			Message: []models.RedisBodyType{
				{
					Type: buffDataType,
					Size: size,
					Data: handleDataByType(buffDataType,dataFromBuf),
				},
			},
		})
	}
}

func saveMock(requests []models.RedisRequests, responses []models.RedisResponses, reqTimestampMock, resTimestampMock time.Time, mocks chan<- *models.Mock) {
	redisRequestsCopy := make([]models.RedisRequests, len(requests))
	redisResponsesCopy := make([]models.RedisResponses, len(responses))
	copy(redisResponsesCopy, responses)
	copy(redisRequestsCopy, requests)

	metadata := make(map[string]string)
	metadata["type"] = "config"

	mocks <- &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.REDIS,
		Spec: models.MockSpec{
			RedisRequests:    redisRequestsCopy,
			RedisResponses:   redisResponsesCopy,
			ReqTimestampMock: reqTimestampMock,
			ResTimestampMock: resTimestampMock,
			Metadata:         metadata,
		},
	}
}
