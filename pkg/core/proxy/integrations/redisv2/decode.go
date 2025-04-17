package redisv2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/davecgh/go-spew/spew"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func decodeRedis(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	redisRequests := [][]byte{reqBuf}
	logger.Debug("Into the redis parser in test mode")
	errCh := make(chan error, 1)

	go func(errCh chan error, redisRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)
		for {
			for {
				if len(redisRequests) > 0 {
					break
				}
				err := clientConn.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil && err.Error() != "EOF" {
					logger.Debug("failed to read the request message in proxy for redis dependency")
					return
				}
				if len(buffer) > 0 {
					redisRequests = append(redisRequests, buffer)
					break
				}
			}

			if len(redisRequests) == 0 {
				logger.Debug("redis request buffer is empty")
				continue
			}

			// Fuzzy match to get the best matched redis mock
			matched, redisResponses, err := fuzzyMatch(ctx, redisRequests, mockDb)
			if err != nil {
				utils.LogError(logger, err, "error while matching redis mocks")
			}
			fmt.Println("chekc1")
			spew.Dump(redisResponses)
			fmt.Println("chekc2")

			if !matched {
				err := clientConn.SetReadDeadline(time.Time{})
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}

				logger.Debug("redisRequests before pass through:", zap.Any("length", len(redisRequests)))
				for _, redReq := range redisRequests {
					logger.Debug("redisRequests:", zap.Any("h", string(redReq)))
				}

				reqBuffer, err := pUtil.PassThrough(ctx, logger, clientConn, dstCfg, redisRequests)
				if err != nil {
					utils.LogError(logger, err, "failed to passthrough the redis request")
					return
				}

				redisRequests = [][]byte{}
				logger.Debug("request buffer after pass through in redis:", zap.Any("buffer", string(reqBuffer)))
				if len(reqBuffer) > 0 {
					redisRequests = [][]byte{reqBuffer}
				}
				logger.Debug("length of redisRequests after passThrough:", zap.Any("length", len(redisRequests)))
				continue
			}
			for _, redisResponse := range redisResponses {
				var (
					encoded []byte
					err     error
				)

				// Data comes back as interface{} (always a string for Redis payloads)
				switch raw := redisResponse.Message[0].Data.(type) {
				case string:
					if redisResponse.Message[0].Type != models.String {
						// binary path: decode base64 into bytes
						encoded, err = util.DecodeBase64(raw)
						if err != nil {
							utils.LogError(logger, err, "failed to decode the base64 response")
							return
						}
					} else {
						// simple string path
						encoded = []byte(raw)
					}
				case []interface{}:
					encoded, err = serializeArray(raw)
				default:
					utils.LogError(logger, fmt.Errorf("unexpected data type %T", raw),
						"cannot cast RedisBodyType.Data to []byte or string")
					return
				}
				fmt.Println("sofkdkjf")

				if _, err := clientConn.Write(encoded); err != nil {
					if ctx.Err() != nil {
						fmt.Println("dsknfsnkjgnf")
						return
					}
					fmt.Println("nkfnkjgnjkjkhkn gkmhg")
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return
				}
				fmt.Println("after client conn write")
				// reqBuf, err = pUtil.ReadBytes(ctx, logger, clientConn)
				// if err != nil {
				// 	logger.Debug("failed to read the request buffer from the client", zap.Error(err))
				// 	logger.Debug("This was the last response from the server:\n" + string(encoded))
				// 	errCh <- nil
				// 	return
				// }
				// fmt.Println(reqBuf)
				fmt.Println(redisRequests)
				logger.Info("redisRequests after the iteration check ", zap.Any("length", len(redisRequests)))
			}

			// Clear the redisRequests buffer for the next dependency call
			redisRequests = [][]byte{}
			logger.Debug("redisRequests after the iteration:", zap.Any("length", len(redisRequests)))
		}
	}(errCh, redisRequests)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		fmt.Println("inside error select")
		return err
	}
}

func serializeArray(elems []interface{}) ([]byte, error) {
	var buf bytes.Buffer
	// array header
	buf.WriteString(fmt.Sprintf("*%d\r\n", len(elems)))

	for _, el := range elems {
		// each el should be a map[string]interface{} describing one RedisBodyType
		m, ok := el.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("array element is %T, want map[string]interface{}", el)
		}
		t, _ := m["type"].(string)
		sizeF, _ := m["size"].(int)
		rawData := m["data"]

		switch t {
		case "string":
			s, _ := rawData.(string)
			buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))
		case "integer":
			// JSON numeric fields come in as float64
			num := strconv.FormatInt(int64(sizeF), 10)
			buf.WriteString(":" + num + "\r\n")
		case "array":
			// nested array: rawData is again []interface{}
			nested, _ := rawData.([]interface{})
			nestedBytes, err := serializeArray(nested)
			if err != nil {
				return nil, err
			}
			buf.Write(nestedBytes)
		default:
			// TODO: handle other types as needed...
			return nil, fmt.Errorf("unsupported nested type %q", t)
		}
	}

	return buf.Bytes(), nil
}
