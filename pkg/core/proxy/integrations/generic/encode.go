//go:build linux

package generic

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	pUtil "go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func encodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn, destConn net.Conn, mocks chan<- *models.Mock, _ models.OutgoingOptions) error {

	var genericRequests []models.Payload
	var genericResponses []models.Payload
	var currRespChunks []models.OutputBinary
	var respAccumBuf []byte
	var reqAccumBuf []byte

	bufStr := string(reqBuf)
	// dataType := models.String
	if !util.IsASCII(string(reqBuf)) {
		bufStr = util.EncodeBase64(reqBuf)
		// dataType = "binary"
	}

	if bufStr != "" {
		reqAccumBuf = append(reqAccumBuf, reqBuf...)
	}

	_, err := destConn.Write(reqBuf)
	if err != nil {
		utils.LogError(logger, err, "failed to write request message to the destination server")
		return err
	}

	clientBuffChan := make(chan []byte)
	destBuffChan := make(chan []byte)
	errChan := make(chan error)

	err = pUtil.ReadFromPeer(ctx, logger, clientConn, clientBuffChan, errChan, pUtil.Client)
	if err != nil {
		return fmt.Errorf("error reading from client:%v", err)
	}

	err = pUtil.ReadFromPeer(ctx, logger, destConn, destBuffChan, errChan, pUtil.Destination)
	if err != nil {
		return fmt.Errorf("error reading from destination:%v", err)
	}

	prevChunkWasReq := false
	reqTimestampMock := time.Now()
	var resTimestampMock time.Time

	for {
		select {
		case <-ctx.Done():
			if len(respAccumBuf) > 0 {
				dataStr := string(respAccumBuf)
				typeVal := models.String
				if !util.IsASCII(string(respAccumBuf)) {
					dataStr = util.EncodeBase64(respAccumBuf)
					typeVal = "binary"
				}
				currRespChunks = append(currRespChunks, models.OutputBinary{Type: typeVal, Data: dataStr})
				genericResponses = append(genericResponses, models.Payload{Origin: models.FromServer, Message: currRespChunks})
			}
			if len(reqAccumBuf) > 0 {
				dataStr := string(reqAccumBuf)
				typeVal := models.String
				if !util.IsASCII(string(reqAccumBuf)) {
					dataStr = util.EncodeBase64(reqAccumBuf)
					typeVal = "binary"
				}
				genericRequests = append(genericRequests, models.Payload{Origin: models.FromClient, Message: []models.OutputBinary{{Type: typeVal, Data: dataStr}}})
			}
			if len(genericRequests) > 0 && len(genericResponses) > 0 {
				metadata := map[string]string{"type": "config"}
				mocks <- &models.Mock{
					Version: models.GetVersion(),
					Name:    "mocks",
					Kind:    models.GENERIC,
					Spec: models.MockSpec{
						GenericRequests:  genericRequests,
						GenericResponses: genericResponses,
						ReqTimestampMock: reqTimestampMock,
						ResTimestampMock: resTimestampMock,
						Metadata:         metadata,
					},
				}
			}
			return ctx.Err()

		case buffer := <-clientBuffChan:
			if len(respAccumBuf) > 0 {
				dataStr := string(respAccumBuf)
				typeVal := models.String
				if !util.IsASCII(string(respAccumBuf)) {
					dataStr = util.EncodeBase64(respAccumBuf)
					typeVal = "binary"
				}
				currRespChunks = append(currRespChunks, models.OutputBinary{Type: typeVal, Data: dataStr})
				genericResponses = append(genericResponses, models.Payload{Origin: models.FromServer, Message: currRespChunks})
				currRespChunks = nil
				respAccumBuf = nil
			}

			_, err := destConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write request message to the destination server")
				return err
			}

			if !prevChunkWasReq && len(genericRequests) > 0 && len(genericResponses) > 0 {
				reqs := append([]models.Payload(nil), genericRequests...)
				resps := append([]models.Payload(nil), genericResponses...)
				go func(reqs, resps []models.Payload) {
					metadata := map[string]string{"type": "config"}
					mocks <- &models.Mock{
						Version: models.GetVersion(),
						Name:    "mocks",
						Kind:    models.GENERIC,
						Spec: models.MockSpec{
							GenericRequests:  reqs,
							GenericResponses: resps,
							ReqTimestampMock: reqTimestampMock,
							ResTimestampMock: resTimestampMock,
							Metadata:         metadata,
						},
					}
				}(reqs, resps)
				genericRequests = nil
				genericResponses = nil
			}

			reqAccumBuf = append(reqAccumBuf, buffer...)
			prevChunkWasReq = true

		case buffer := <-destBuffChan:
			if prevChunkWasReq {
				reqTimestampMock = time.Now()
				respAccumBuf = nil
				if len(reqAccumBuf) > 0 {
					dataStr := string(reqAccumBuf)
					typeVal := models.String
					if !util.IsASCII(string(reqAccumBuf)) {
						dataStr = util.EncodeBase64(reqAccumBuf)
						typeVal = "binary"
					}
					genericRequests = append(genericRequests, models.Payload{Origin: models.FromClient, Message: []models.OutputBinary{{Type: typeVal, Data: dataStr}}})
					reqAccumBuf = nil
				}
			}

			respAccumBuf = append(respAccumBuf, buffer...)

			_, err := clientConn.Write(buffer)
			if err != nil {
				utils.LogError(logger, err, "failed to write response message to the client")
				return err
			}
			resTimestampMock = time.Now()
			prevChunkWasReq = false

		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
