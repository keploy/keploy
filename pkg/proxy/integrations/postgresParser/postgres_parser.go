package postgresparser

import (
	"net"
	"time"

	// "time"

	// "time"
	// "sync"
	// "strings"

	"encoding/binary"
	// "encoding/json"
	"encoding/base64"
	// "fmt"
	"go.keploy.io/server/pkg/proxy/util"
	// "bytes"

	"errors"
	

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

func IsOutgoingPSQL(buffer []byte) bool {
	const ProtocolVersion = 0x00030000 // Protocol version 3.0

	if len(buffer) < 8 {
		// Not enough data for a complete header
		return false
	}

	// The first four bytes are the message length, but we don't need to check those

	// The next four bytes are the protocol version
	version := binary.BigEndian.Uint32(buffer[4:8])

	if version == 80877103 {
		return true
	}
	return version == ProtocolVersion
}

func ProcessOutgoingPSQL(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		// encodeGenericOutgoing(requestBuffer, clientConn, destConn, h, logger)
		// startProxy(requestBuffer, clientConn, destConn, logger, h)
		SaveOutgoingPSQL(requestBuffer, clientConn, destConn, logger, h)
	case models.MODE_TEST:
		decodeOutgoingPSQL(requestBuffer, clientConn, destConn, h, logger)
		// decodeGenericOutgoing(requestBuffer, clientConn, destConn, h, logger)
	default:
		logger.Info(Emoji+"Invalid mode detected while intercepting outgoing http call", zap.Any("mode", models.GetMode()))
	}

}

type PSQLMessage struct {
	// Define fields to store the relevant information from the buffer
	ID      uint32
	Payload []byte
	Field1  string
	Field2  int
	// Add more fields as needed
}

func decodeBuffer(buffer []byte) (*PSQLMessage, error) {
	if len(buffer) < 6 {
		return nil, errors.New("invalid buffer length")
	}

	psqlMessage := &PSQLMessage{
		Field1: "test",
		Field2: 123,
	}

	// Decode the ID (4 bytes)
	psqlMessage.ID = binary.BigEndian.Uint32(buffer[:4])

	// Decode the payload length (2 bytes)
	payloadLength := binary.BigEndian.Uint16(buffer[4:6])

	// Check if the buffer contains the full payload
	if len(buffer[6:]) < int(payloadLength) {
		return nil, errors.New("incomplete payload in buffer")
	}

	// Extract the payload from the buffer
	psqlMessage.Payload = buffer[6 : 6+int(payloadLength)]

	return psqlMessage, nil
}

func SaveOutgoingPSQL(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger, h *hooks.Hook) []*models.Mock {

	// backend := pgproto3.NewBackend(pgproto3.NewChunkReader(clientConn), clientConn, clientConn)
	// frontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(destConn), destConn, destConn)
	x := 0

	logger.Debug("x count is ", zap.Int("x", x))
	// In case of java note the byte array used for authentication
	logger.Info(Emoji + "Encoding outgoing Postgres call !!")
	// write the request message to the postgres server

	_, err := destConn.Write(requestBuffer)

	if err != nil {
		logger.Error(Emoji+"failed to write the request buffer to postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
		return nil
	}

	// // read reply message from the postgres server
	responseBuffer, _, err := util.ReadBytes1(destConn)
	if err != nil {
		logger.Error(Emoji+"failed to read reply from the postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the reply to postgres client
	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write the reply message to postgres client", zap.Error(err))
		return nil
	}
	logger.Debug("Response buffer " + string(responseBuffer))

	postgresMock := &models.Mock{
		Version: models.V1Beta1,
		Name:    "mocks",
		Kind:    models.Postgres,
		Spec: models.MockSpec{
			PostgresReq: &models.Backend{
				Identfier: "PostgresReq",
				Length:    uint32(len(requestBuffer)),
				Payload:   base64.StdEncoding.EncodeToString(requestBuffer),
			},
			PostgresResp: &models.Frontend{
				Identfier: "PostgresResponse",
				Length:    uint32(len(responseBuffer)),
				Payload:   base64.StdEncoding.EncodeToString(responseBuffer),
			},
		},
	}

	if postgresMock != nil {
		h.AppendMocks(postgresMock)
	}

	// may be bodylen 0 is reason

	// declare postgres mock here

	var msgRequestbuffer []byte

	for {
		// read request message from the postgres client and see if it's authentication buffer
		// if its auth buffer then just save it in global config
		
		for {
			msgRequestbuffer, _, err = util.ReadBytes1(clientConn)
			if err != nil {
				logger.Error(Emoji+"failed to read the message from the postgres client", zap.Error(err))
				// return nil
			}

			if len(msgRequestbuffer) != 0 {
				break
			}

		}

		// check the packet type and add config name accordingly

		// for making readable first identify message type and add the Unmarshaled value for that query object

		logger.Info(Emoji+"The mock is ", zap.String("payload of req ::: :: ::", base64.StdEncoding.EncodeToString(msgRequestbuffer)))

		println(Emoji, "Inside for loop", string(msgRequestbuffer))

		// write the request message to postgres server
		_, err = destConn.Write(msgRequestbuffer)
		if err != nil {
			logger.Error(Emoji+"failed to write the request message to postgres server", zap.Error(err), zap.String("postgres server address", destConn.LocalAddr().String()))
			// return nil
		}

		msgResponseBuffer, _, err := util.ReadBytes1(destConn)
		if msgResponseBuffer == nil {
			println(Emoji, "msgResponseBuffer is nil")
		}

		if err != nil {
			logger.Error(Emoji+"failed to read the response message from postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
			// return nil
		}

		// write the response message to postgres client
		_, err = clientConn.Write(msgResponseBuffer)
		println(Emoji, "After getting response from postgres server")

		// it is failing here
		if err != nil {
			logger.Error(Emoji+"failed to write the response wiremessage to postgres client ", zap.Error(err))
			// return nil
		}
		postgresMock := &models.Mock{
			Version: models.V1Beta1,
			Name:    "mocks",
			Kind:    models.Postgres,
			Spec: models.MockSpec{
				PostgresReq: &models.Backend{
					Identfier: "PostgresReq",
					Length:    uint32(len(msgRequestbuffer)),
					Payload:   base64.StdEncoding.EncodeToString(msgRequestbuffer),
				},
				PostgresResp: &models.Frontend{
					Identfier: "PostgresResponse",
					Length:    uint32(len(msgResponseBuffer)),
					Payload:   base64.StdEncoding.EncodeToString(msgResponseBuffer),
				},
			},
		}

		if postgresMock != nil {
			h.AppendMocks(postgresMock)
		}

	}

}

func decodeOutgoingPSQL(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	// decode the request buffer
	configMocks := h.GetConfigMocks()

	tcsMocks := h.GetTcsMocks()
	println(len(tcsMocks), "len of tcs mocks")
	logger.Debug("tcsMocks is ", zap.Any("tcsMocks", tcsMocks))
	println(len(configMocks), "len of config mocks")

	// backend := pgproto3.NewBackend(pgproto3.NewChunkReader(clientConn), clientConn)
	// frontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(destConn), destConn, destConn)
	logger.Info(Emoji + "Encoding outgoing Postgres call !!")
	// write the request message to the postgres server

	// _, err := destConn.Write(requestBuffer)
	encode, err := PostgresDecoder(tcsMocks[0].Spec.PostgresResp.Payload)
	if err != nil {
		logger.Error(Emoji+"failed to write the request buffer to postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
		return
	}

	// write the reply to postgres client
	_, err = clientConn.Write(encode)
	if err != nil {
		logger.Error(Emoji+"failed to write the reply message to postgres client", zap.Error(err))
		return
	}

	var msgRequestbuffer []byte
	var msgResponseBuffer []byte

	for {

		for {
			msgRequestbuffer, _, err = util.ReadBytes1(clientConn)
			if err != nil {
				logger.Error(Emoji+"failed to read the message from the postgres client", zap.Error(err))
				// return nil
			}

			if len(msgRequestbuffer) != 0 {
				break
			}

		}

		// mock2, err := PostgresDecoder(tcsMocks[y].Spec.PostgresReq.Payload)
		matched, decoded := Fuzzymatch(configMocks, tcsMocks, msgRequestbuffer, h)

		if err != nil {
			logger.Error(Emoji+"failed to write the request buffer to postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
			return
		}

		if err != nil {
			logger.Error(Emoji+"failed to write the request buffer to postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
			return
		}
		if matched {
			logger.Debug("Found a match !!!!")
			msgResponseBuffer, _ = PostgresDecoder(decoded)
		} else {
			logger.Debug("Not found a match !!!!")
			logger.Debug("Actual req is", zap.Any("Actual req", PostgresEncoder(msgRequestbuffer)))
		}
		_, err = clientConn.Write(msgResponseBuffer)
		if err != nil {
			logger.Error(Emoji+"failed to write the response wiremessage to postgres client ", zap.Error(err))
			return
		}

	}
	// return
}

func encodeGenericOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) error {

	genericRequests := []models.GenericPayload{}
	logger.Info("Encoding outgoing generic call from postgres parser !!")
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
		logger.Error(hooks.Emoji+"failed to write request message to the destination server", zap.Error(err))
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
	// logger.Debug("the iteration for the generic request starts", zap.Any("genericReqs", len(genericRequests)), zap.Any("genericResps", len(genericResponses)))
	for {

		select {
		case buffer := <-clientBufferChannel:
			// Write the request message to the destination
			_, err := destConn.Write(buffer)
			if err != nil {
				logger.Error(hooks.Emoji+"failed to write request message to the destination server", zap.Error(err))
				return err
			}

			// logger.Debug("the iteration for the generic request ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
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
				logger.Error(hooks.Emoji+"failed to write response to the client", zap.Error(err))
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
			// logger.Debug("the iteration for the generic response ends with no of genericReqs:" + strconv.Itoa(len(genericRequests)) + " and genericResps: " + strconv.Itoa(len(genericResponses)))
			isPreviousChunkRequest = false
		case err := <-errChannel:
			return err
		}

	}
}

func decodeGenericOutgoing(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) error {
	genericRequests := [][]byte{requestBuffer}
	for {
		tcsMocks := h.GetTcsMocks()
		err := clientConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		if err != nil {
			logger.Error(hooks.Emoji+"failed to set the read deadline for the client connection", zap.Error(err))
			return err
		}

		for {
			buffer, err := util.ReadBytes(clientConn)
			if netErr, ok := err.(net.Error); !(ok && netErr.Timeout()) && err != nil {
				logger.Error(hooks.Emoji+"failed to read the request message in proxy for generic dependency", zap.Error(err))
				// errChannel <- err
				return err
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				logger.Debug(hooks.Emoji + "the timeout for the client read in generic")
				break
			}
			genericRequests = append(genericRequests, buffer)
		}

		if len(genericRequests) == 0 {
			logger.Debug(hooks.Emoji + "the generic request buffer is empty")
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
				logger.Error(hooks.Emoji+"failed to write request message to the client application", zap.Error(err))
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
			logger.Error(hooks.Emoji+"failed to read the packet message in proxy for generic dependency", zap.Error(err))
			errChannel <- err
			return err
		}
		bufferChannel <- buffer
	}

}
