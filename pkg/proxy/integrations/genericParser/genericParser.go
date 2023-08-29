package genericparser

import (
	"encoding/base64"

	"errors"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

func ProcessGeneric(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		encodeGenericOutgoing(requestBuffer, clientConn, destConn, h, logger)
	case models.MODE_TEST:
		decodeGenericOutgoing(requestBuffer, clientConn, destConn, h, logger)
	case models.MODE_OFF:
	default:
	}
}

func decodeGenericOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) error {
	genericRequests := [][]byte{requestBuffer}
	for {
		tcsMocks := h.GetTcsMocks()
		err := clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			logger.Error("failed to set the read deadline for the client connection", zap.Error(err))
			return err
		}

		for {
			buffer, err := util.ReadBytes(clientConn)
			if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
				logger.Error("failed to read the request message in proxy for generic dependency", zap.Error(err))
				// errChannel <- err
				return err
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug("the timeout for the client read in generic")
				break
			}
			genericRequests = append(genericRequests, buffer)
		}

		if len(genericRequests) == 0 {
			logger.Debug("the generic request buffer is empty")
			continue
		}
		// bestMatchedIndx := 0
		// fuzzy match gives the index for the best matched generic mock
		matched, genericResponses := fuzzymatch(tcsMocks, genericRequests, h)

		if !matched {
			logger.Error("failed to match the dependency call from user application", zap.Any("request packets", len(genericRequests)))

			return errors.New("failed to match the dependency call from user application")
			// continue
		}
		for _, genericResponse := range genericResponses {
			encoded, _ := PostgresDecoder(genericResponse.Message[0].Data)
			_, err := clientConn.Write([]byte(encoded))
			if err != nil {
				logger.Error("failed to write request message to the client application", zap.Error(err))
				// errChannel <- err
				return err
			}
		}
		// }

		// update for the next dependency call
		genericRequests = [][]byte{}
	}
}

func ReadBuffConn(conn net.Conn, bufferChannel chan []byte, errChannel chan error, logger *zap.Logger) error {
	for {
		buffer, err := util.ReadBytes(conn)
		if err != nil {
			logger.Error("failed to read the packet message in proxy for generic dependency", zap.Error(err))
			errChannel <- err
			return err
		}
		bufferChannel <- buffer
	}
}

func encodeGenericOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) error {
	// destinationWriteChannel := make(chan []byte)
	// clientWriteChannel := make(chan []byte)
	// errChannel := make(chan error)
	// checkInitialRequest := true
	genericRequests := []models.GenericPayload{}
	// isFirstRequest := true
	// bufStr := string(requestBuffer)
	// if !IsAsciiPrintable(bufStr) {
	bufStr := base64.StdEncoding.EncodeToString(requestBuffer)
	// }
	if bufStr != "" {

		genericRequests = append(genericRequests, models.GenericPayload{
			Origin: models.FromClient,
			Message: []models.OutputBinary{
				{
					Type: "binary",
					Data: bufStr,
				},
			},
		})
	}
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write request message to the destination server", zap.Error(err))
		return err
	}
	genericResponses := []models.GenericPayload{}

	clientBufferChannel := make(chan []byte)
	destBufferChannel := make(chan []byte)
	errChannel := make(chan error)
	// read requests from client
	go ReadBuffConn(clientConn, clientBufferChannel, errChannel, logger)
	// read response from destination
	go ReadBuffConn(destConn, destBufferChannel, errChannel, logger)

	isPreviousChunkRequest := false

	// ticker := time.NewTicker(1 * time.Second)
	logger.Debug("the iteration for the generic request starts", zap.Any("genericReqs", len(genericRequests)), zap.Any("genericResps", len(genericResponses)))
	for {

		// start := time.NewTicker(1*time.Second)
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		select {
		// case <-start.C:
		case <-sigChan:
			if !isPreviousChunkRequest && len(genericRequests) > 0 && len(genericResponses) > 0 {
				h.AppendMocks(&models.Mock{
					Version: models.V1Beta2,
					Name:    "mocks",
					Kind:    models.GENERIC,
					Spec: models.MockSpec{
						GenericRequests:  genericRequests,
						GenericResponses: genericResponses,
					},
				})
				genericRequests = []models.GenericPayload{}
				genericResponses = []models.GenericPayload{}
			}
		case buffer := <-clientBufferChannel:
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write request message to the destination server", zap.Error(err))
				return err
			}

			logger.Debug("the iteration for the generic request ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			if !isPreviousChunkRequest && len(genericRequests) > 0 && len(genericResponses) > 0 {
				h.AppendMocks(&models.Mock{
					Version: models.V1Beta2,
					Name:    "mocks",
					Kind:    models.GENERIC,
					Spec: models.MockSpec{
						GenericRequests:  genericRequests,
						GenericResponses: genericResponses,
					},
				})
				genericRequests = []models.GenericPayload{}
				genericResponses = []models.GenericPayload{}
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)
			// }
			if bufStr != "" {

				genericRequests = append(genericRequests, models.GenericPayload{
					Origin: models.FromClient,
					Message: []models.OutputBinary{
						{
							Type: "binary",
							Data: bufStr,
						},
					},
				})
			}

			isPreviousChunkRequest = true
		case buffer := <-destBufferChannel:
			// Write the response message to the client
			_, err := clientConn.Write(buffer)
			if err != nil {
				logger.Error("failed to write response to the client", zap.Error(err))
				return err
			}

			bufStr := base64.StdEncoding.EncodeToString(buffer)
			// }
			if bufStr != "" {

				genericResponses = append(genericResponses, models.GenericPayload{
					Origin: models.FromServer,
					Message: []models.OutputBinary{
						{
							Type: "binary",
							Data: bufStr,
						},
					},
				})
			}

			logger.Debug("the iteration for the generic response ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			isPreviousChunkRequest = false
		case err := <-errChannel:
			return err
			// case <-ticker.C:
			// 	if !isPreviousChunkRequest && len(genericRequests) > 0 && len(genericResponses) > 0 {
			// 		h.AppendMocks(&models.Mock{
			// 			Version: models.V1Beta2,
			// 			Name:    "mocks",
			// 			Kind:    models.GENERIC,
			// 			Spec: models.MockSpec{
			// 				GenericRequests:  genericRequests,
			// 				GenericResponses: genericResponses,
			// 			},
			// 		})
			// 		genericRequests = []models.GenericPayload{}
			// 		genericResponses = []models.GenericPayload{}

			// 	}
		}
		// // fmt.Println("inside connection")

		// // if isFirstRequest {
		// err := clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		// if err != nil {
		// 	logger.Error("failed to set the read deadline for the client connection", zap.Error(err))
		// 	return err
		// }
		// // }

		// // if !checkInitialRequest {
		// // go routine to read from client
		// // go func() {
		// // requestBuffers := [][]byte{}
		// for {
		// 	buffer, err := util.ReadBytes(clientConn)
		// 	if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
		// 		logger.Error("failed to read the request message in proxy for generic dependency", zap.Error(err))
		// 		// errChannel <- err
		// 		return err
		// 	}
		// 	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		// 		logger.Debug( "the timeout for the client read in generic")
		// 		break
		// 	}
		// 	// if len(buffer) == 0 {
		// 	// 	break
		// 	// }
		// 	_, err = destConn.Write(buffer)
		// 	if err != nil {
		// 		logger.Error("failed to write request message to the destination server", zap.Error(err))
		// 		// errChannel <- err
		// 		return err
		// 	}
		// 	// bufStr := string(buffer)
		// 	// if !IsAsciiPrintable(bufStr) {
		// 	bufStr := base64.StdEncoding.EncodeToString(buffer)
		// 	// }

		// 	if bufStr != "" {

		// 		genericRequests = append(genericRequests, models.GenericPayload{
		// 			Origin: models.FromClient,
		// 			Message: []models.OutputBinary{
		// 				{
		// 					Type: "binary",
		// 					Data: bufStr,
		// 				},
		// 			},
		// 		})
		// 	}

		// 	// fmt.Println("buffer from client connection")
		// 	// fmt.Println(buffer)
		// 	// fmt.Println(string(buffer))
		// 	// destinationWriteChannel <- buffer
		// 	// requestBuffers = append(requestBuffers, buffer)
		// }
		// // destinationWriteChannel <- requestBuffers
		// // }()

		// // go routine to read from destination
		// // go func() {
		// // requestBuffers := [][]byte{}
		// err = destConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		// if err != nil {
		// 	logger.Error("failed to set the read deadline for the destination connection", zap.Error(err))
		// 	return err
		// }
		// for {
		// 	buffer, err := util.ReadBytes(destConn)
		// 	if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
		// 		logger.Error("failed to read the request message in proxy for generic dependency", zap.Error(err))
		// 		// errChannel <- err
		// 		return err
		// 	}
		// 	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		// 		logger.Debug("the timeout for the destination read in generic")
		// 		break
		// 	}
		// 	// if len(buffer) == 0 {
		// 	// 	break
		// 	// }
		// 	_, err = clientConn.Write(buffer)
		// 	if err != nil {
		// 		logger.Error("failed to write request message to the client application", zap.Error(err))
		// 		// errChannel <- err
		// 		return err
		// 	}
		// 	// bufStr := string(buffer)
		// 	// if !IsAsciiPrintable(bufStr) {
		// 	bufStr = base64.StdEncoding.EncodeToString(buffer)
		// 	// }

		// 	if bufStr != "" {

		// 		genericResponses = append(genericResponses, models.GenericPayload{
		// 			Origin: models.FromServer,
		// 			Message: []models.OutputBinary{
		// 				{
		// 					Type: "binary",
		// 					Data: bufStr,
		// 				},
		// 			},
		// 		})
		// 	}

		// 	// fmt.Println("buffer from destination connection")
		// 	// fmt.Println(buffer)
		// 	// fmt.Println(string(buffer))
		// 	// clientWriteChannel <- buffer
		// 	// 	requestBuffers = append(requestBuffers, buffer)
		// }
		// // clientWriteChannel <- requestBuffers
		// // }()

		// // select {
		// // case buffer := <-destinationWriteChannel:
		// // 	// Write the request message to the actual destination server
		// // 	// fmt.Println("writing buffer to destination", requestBuffer)
		// // 	_, err := destConn.Write(buffer)
		// // 	if err != nil {
		// // 		logger.Error("failed to write request message to the destination server", zap.Error(err))
		// // 		return err
		// // 	}

		// if len(genericRequests) > 0 && len(genericResponses) > 0 {
		// 	h.AppendMocks(&models.Mock{
		// 		Version: models.V1Beta2,
		// 		Name:    "mocks",
		// 		Kind:    models.GENERIC,
		// 		Spec: models.MockSpec{
		// 			GenericRequests:  genericRequests,
		// 			GenericResponses: genericResponses,
		// 		},
		// 	})

		// 	genericRequests = []models.GenericPayload{}
		// 	genericResponses = []models.GenericPayload{}
		// }
		// logger.Debug("the iteration for the generic request ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))

		// clientConn.SetReadDeadline(time.Time{})
		// buffer, err := util.ReadBytes(clientConn)
		// if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
		// 	logger.Error("failed to read the request message in proxy for generic dependency", zap.Error(err))
		// 	// errChannel <- err
		// 	return err
		// }
		// _, err = destConn.Write(buffer)
		// if err != nil {
		// 	logger.Error("failed to write request message to the destination server", zap.Error(err))
		// 	return err
		// }
		// genericRequests = append(genericRequests,
		// 	models.GenericPayload{
		// 		Origin: models.FromClient,
		// 		Message: []models.OutputBinary{
		// 			{
		// 				Type: "binary",
		// 				Data: base64.StdEncoding.EncodeToString(buffer),
		// 			},
		// 		},
		// 	})

		// // for _, buf := range buffer {
		// // 	bufStr := string(buffer)
		// // 	if !IsAsciiPrintable(bufStr) {
		// // 		bufStr = base64.StdEncoding.EncodeToString(buffer)
		// // 	}
		// // 	if bufStr != "" {

		// // 		genericRequests = append(genericRequests, models.GenericPayload{
		// // 			Origin: models.FromClient,
		// // 			Message: []models.OutputBinary{
		// // 				{
		// // 					Type: "binary",
		// // 					Data: bufStr,
		// // 				},
		// // 			},
		// // 		})
		// // 	}

		// // 	// }
		// // case buffer := <-clientWriteChannel:
		// // 	// Write the response message to the client
		// // 	// fmt.Println("writing buffer to client", responseBuffer)
		// // 	_, err := clientConn.Write(buffer)
		// // 	if err != nil {
		// // 		logger.Error("failed to write response to the client", zap.Error(err))
		// // 		return err
		// // 	}

		// // 	// encodedBuffer := base64.StdEncoding.EncodeToString(buffer)
		// // 	// if encodedBuffer != "" {

		// // 	// 	genericRequests = append(genericRequests, models.GenericPayload{
		// // 	// 		Origin: models.FromServer,
		// // 	// 		Message: []models.OutputBinary{
		// // 	// 			{
		// // 	// 				Type: "binary",
		// // 	// 				Data: encodedBuffer,
		// // 	// 			},
		// // 	// 		},
		// // 	// 	})
		// // 	// }
		// // 	// for _, buf := range buffer {
		// // 	bufStr := string(buffer)
		// // 	if !IsAsciiPrintable(bufStr) {
		// // 		bufStr = base64.StdEncoding.EncodeToString(buffer)
		// // 	}
		// // 	if bufStr != "" {

		// // 		genericResponses = append(genericResponses, models.GenericPayload{
		// // 			Origin: models.FromServer,
		// // 			Message: []models.OutputBinary{
		// // 				{
		// // 					Type: "binary",
		// // 					Data: bufStr,
		// // 				},
		// // 			},
		// // 		})
		// // 	}
		// // 	// }
		// // case err = <-errChannel:
		// // 	return err
		// // }
		// // } else {
		// // 	_, err := destConn.Write(requestBuffer)
		// // 	if err != nil {
		// // 		logger.Error("failed to write request message to the destination server", zap.Error(err))
		// // 		return err
		// // 	}
		// // 	encodedBuffer := base64.StdEncoding.EncodeToString(requestBuffer)
		// // 	if encodedBuffer != "" {
		// // 		genericRequests = append(genericRequests, models.GenericPayload{
		// // 			Origin: models.FromClient,
		// // 			Message: []models.OutputBinary{{
		// // 				Type: "binary",
		// // 				Data: encodedBuffer,
		// // 			}},
		// // 		})
		// // 	}
		// // 	checkInitialRequest = false
		// // }

		// // // RESPONSE
		// // // go routine to read from client
		// // go func() {
		// // 	responseBuffers := [][]byte{}
		// // 	for {
		// // 		buffer, err := util.ReadBytes(clientConn)
		// // 		if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
		// // 			logger.Error("failed to read the response message in proxy for generic dependency", zap.Error(err))
		// // 			errChannel <- err
		// // 			return
		// // 		}
		// // 		if len(buffer) == 0 {
		// // 			break
		// // 		}
		// // 		_, err = destConn.Write(buffer)
		// // 		if err != nil {
		// // 			logger.Error("failed to write response message to the destination server", zap.Error(err))
		// // 			errChannel <- err
		// // 			return
		// // 		}
		// // 		// fmt.Println("buffer from client connection")
		// // 		// fmt.Println(buffer)
		// // 		// fmt.Println(string(buffer))
		// // 		// destinationWriteChannel <- buffer
		// // 		responseBuffers = append(responseBuffers, buffer)
		// // 	}
		// // 	destinationWriteChannel <- responseBuffers
		// // }()

		// // // go routine to read from destination
		// // go func() {
		// // 	responseBuffer := [][]byte{}
		// // 	for {
		// // 		buffer, err := util.ReadBytes(destConn)
		// // 		if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
		// // 			logger.Error("failed to read the response message in proxy for generic dependency", zap.Error(err))

		// // 			errChannel <- err
		// // 			return
		// // 		}
		// // 		// fmt.Println("buffer from destination connection")
		// // 		// fmt.Println(buffer)
		// // 		// fmt.Println(string(buffer))
		// // 		// clientWriteChannel <- buffer
		// // 		if len(buffer) == 0 {
		// // 			break
		// // 		}
		// // 		_, err = clientConn.Write(buffer)
		// // 		if err != nil {
		// // 			logger.Error("failed to write response message to the client server", zap.Error(err))
		// // 			errChannel <- err
		// // 			return
		// // 		}
		// // 		responseBuffer = append(responseBuffer, buffer)
		// // 	}
		// // 	clientWriteChannel <- responseBuffer
		// // }()

		// // select {
		// // case buffer := <-destinationWriteChannel:
		// // 	// Write the request message to the actual destination server
		// // 	// fmt.Println("writing buffer to destination", requestBuffer)
		// // 	// _, err := destConn.Write(buffer)
		// // 	// if err != nil {
		// // 	// 	logger.Error("failed to write request message to the destination server", zap.Error(err))
		// // 	// 	return err
		// // 	// }

		// // 	for _, buf := range buffer {
		// // 		bufStr := string(buf)
		// // 		if !IsAsciiPrintable(bufStr) {
		// // 			bufStr = base64.StdEncoding.EncodeToString(buf)
		// // 		}
		// // 		if bufStr != "" {

		// // 			genericResponses = append(genericResponses, models.GenericPayload{
		// // 				Origin: models.FromClient,
		// // 				Message: []models.OutputBinary{
		// // 					{
		// // 						Type: "binary",
		// // 						Data: bufStr,
		// // 					},
		// // 				},
		// // 			})
		// // 		}
		// // 	}
		// // case buffer := <-clientWriteChannel:
		// // 	// Write the response message to the client
		// // 	// fmt.Println("writing buffer to client", responseBuffer)
		// // 	// _, err := clientConn.Write(buffer)
		// // 	// if err != nil {
		// // 	// 	logger.Error("failed to write response to the client", zap.Error(err))
		// // 	// 	return err
		// // 	// }

		// // 	for _, buf := range buffer {
		// // 		bufStr := string(buf)
		// // 		if !IsAsciiPrintable(bufStr) {
		// // 			bufStr = base64.StdEncoding.EncodeToString(buf)
		// // 		}
		// // 		if bufStr != "" {

		// // 			genericResponses = append(genericResponses, models.GenericPayload{
		// // 				Origin: models.FromServer,
		// // 				Message: []models.OutputBinary{
		// // 					{
		// // 						Type: "binary",
		// // 						Data: bufStr,
		// // 					},
		// // 				},
		// // 			})
		// // 		}
		// // 	}
		// // 	// fmt.Println(Emoji, "Successfully wrote response to the user client ", destConn.RemoteAddr().String())
		// // case err = <-errChannel:
		// // 	return err
		// // }

		// // if len(genericRequests) > 0 && len(genericResponses) > 0 {
		// // 	h.AppendMocks(&models.Mock{
		// // 		Version: models.V1Beta2,
		// // 		Name:    "mocks",
		// // 		Kind:    models.GENERIC,
		// // 		Spec: models.MockSpec{
		// // 			GenericRequests:  genericRequests,
		// // 			GenericResponses: genericResponses,
		// // 		},
		// // 	})
		// // }

	}
}
