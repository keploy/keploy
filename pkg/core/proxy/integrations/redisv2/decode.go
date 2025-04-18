//go:build linux

package redisv2

import (
	"context"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
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
			for len(redisRequests) == 0 {
				err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
				if err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}

				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				if netErr, ok := err.(net.Error); (!ok || !netErr.Timeout()) && err != nil && err.Error() != "EOF" {
					logger.Debug("failed to read the request message in proxy for redis dependency")
					return
				}

				if len(buffer) > 0 {
					redisRequests = append(redisRequests, buffer)
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
			// fmt.Println("chekc1")
			// spew.Dump(redisResponses)
			// fmt.Println("chekc2")

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
			for _, resp := range redisResponses {
				normBodies := make([]models.RedisBodyType, len(resp.Message))
				for i, b := range resp.Message {
					nb, err := normalizeBody(b)
					if err != nil {
						logger.Error("error normalizing mock data", zap.Error(err))
						return
					}
					normBodies[i] = nb
				}

				encoded, err := SerializeAll(normBodies)
				if err != nil {
					logger.Error("error marshalling mock data", zap.Error(err))
					return
				}
				logger.Debug("serialized RESP3 bytes", zap.ByteString("resp", encoded))

				if _, err := clientConn.Write(encoded); err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(logger, err, "failed to write the response message to the client")
					return
				}
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
		return err
	}
}

// func serializeArray(elems []interface{}) ([]byte, error) {
// 	var buf bytes.Buffer
// 	// array header
// 	buf.WriteString(fmt.Sprintf("*%d\r\n", len(elems)))

// 	for _, el := range elems {
// 		m, ok := el.(map[string]interface{})
// 		if !ok {
// 			fmt.Println("chck here1")
// 			return nil, fmt.Errorf("array element is %T, want map[string]interface{}", el)
// 		}
// 		t, _ := m["type"].(string)
// 		rawData := m["data"]

// 		switch t {
// 		case "string":
// 			s, _ := rawData.(string)
// 			buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(s), s))

// 		case "integer":
// 			// sizeF might be float64 if coming from JSON
// 			sizeF, _ := m["size"].(int)
// 			num := strconv.FormatInt(int64(sizeF), 10)
// 			buf.WriteString(":" + num + "\r\n")

// 		case "array":
// 			nested, _ := rawData.([]interface{})
// 			nestedBytes, err := serializeArray(nested)
// 			if err != nil {
// 				return nil, err
// 			}
// 			buf.Write(nestedBytes)

// 		case "map":
// 			// rawData is []interface{} of map entries
// 			entries, ok := rawData.([]interface{})
// 			if !ok {
// 				return nil, fmt.Errorf("map data is %T, want []interface{}", rawData)
// 			}
// 			// map header
// 			buf.WriteString(fmt.Sprintf("%%%d\r\n", len(entries)))
// 			for _, entry := range entries {
// 				em, ok := entry.(map[string]interface{})
// 				if !ok {
// 					return nil, fmt.Errorf("map element is %T, want map[string]interface{}", entry)
// 				}
// 				// ---- key ----
// 				keyRaw, _ := em["Key"].(map[string]interface{})
// 				keyVal, _ := keyRaw["Value"].(string)
// 				buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(keyVal), keyVal))

// 				// ---- value ----
// 				valRaw, _ := em["Value"].(map[string]interface{})
// 				v := valRaw["Value"]
// 				switch vv := v.(type) {
// 				case string:
// 					buf.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(vv), vv))
// 				case int:
// 					buf.WriteString(fmt.Sprintf(":%d\r\n", int64(vv)))
// 				case []interface{}:
// 					nestedBytes, err := serializeArray(vv)
// 					if err != nil {
// 						return nil, err
// 					}
// 					buf.Write(nestedBytes)
// 				default:
// 					return nil, fmt.Errorf("unsupported map value type %T", vv)
// 				}
// 			}

// 		default:
// 			return nil, fmt.Errorf("unsupported nested type %q", t)
// 		}
// 	}

// 	return buf.Bytes(), nil
// }
