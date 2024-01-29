package postgresparser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	// "strings"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

type BackendWrapper struct {
	BackendWrapper models.Backend
}

type FrontendWrapper struct {
	FrontendWrapper models.Frontend
}

func NewBackend() *BackendWrapper {
	return &BackendWrapper{}
}

func NewFrontend() *FrontendWrapper {
	return &FrontendWrapper{}
}

// func checkScram(encoded []byte, log *zap.Logger) bool {
// 	// encoded, err := PostgresDecoder(packet)

// 	// check if payload contains SCRAM-SHA-256
// 	messageType := encoded[0]
// 	log.Debug("Message Type: %c\n", zap.String("messageType", string(messageType)))
// 	if messageType == 'N' {
// 		return false
// 	}
// 	// Print the message payload (for simplicity, the payload is printed as a string)
// 	payload := string(encoded[5:])
// 	if messageType == 'R' {
// 		if strings.Contains(payload, "SCRAM-SHA") {
// 			log.Debug("scram packet")
// 			return true
// 		}
// 	}

// 	return false
// }

func isStartupPacket(packet []byte) bool {
	protocolVersion := binary.BigEndian.Uint32(packet[4:8])
	// printStartupPacketDetails(packet)
	return protocolVersion == 196608 // 3.0 in PostgreSQL
}

// func isRegularPacket(packet []byte) bool {
// 	messageType := packet[0]
// 	return messageType == 'Q' || messageType == 'P' || messageType == 'D' || messageType == 'C' || messageType == 'E'
// }

const (
	AuthTypeOk                = 0
	AuthTypeCleartextPassword = 3
	AuthTypeMD5Password       = 5
	AuthTypeSCMCreds          = 6
	AuthTypeGSS               = 7
	AuthTypeGSSCont           = 8
	AuthTypeSSPI              = 9
	AuthTypeSASL              = 10
	AuthTypeSASLContinue      = 11
	AuthTypeSASLFinal         = 12
)

const ProtocolVersionNumber uint32 = 196608 // Replace with actual version number if different

// PG Response Packet Transcoder
func (b *BackendWrapper) TranslateToReadableBackend(msgBody []byte) (pgproto3.FrontendMessage, error) {

	// fmt.Println("msgType", b.BackendWrapper.MsgType)
	var msg pgproto3.FrontendMessage
	switch b.BackendWrapper.MsgType {
	case 'B':
		msg = &b.BackendWrapper.Bind
	case 'C':
		msg = &b.BackendWrapper.Close
	case 'D':
		msg = &b.BackendWrapper.Describe
	case 'E':
		msg = &b.BackendWrapper.Execute
	case 'F':
		msg = &b.BackendWrapper.FunctionCall
	case 'f':
		msg = &b.BackendWrapper.CopyFail
	case 'd':
		msg = &b.BackendWrapper.CopyData
	case 'c':
		msg = &b.BackendWrapper.CopyDone
	case 'H':
		msg = &b.BackendWrapper.Flush
	case 'P':
		msg = &b.BackendWrapper.Parse
	case 'p':
		switch b.BackendWrapper.AuthType {
		case pgproto3.AuthTypeSASL:
			msg = &pgproto3.SASLInitialResponse{}
		case pgproto3.AuthTypeSASLContinue:
			msg = &pgproto3.SASLResponse{}
		case pgproto3.AuthTypeSASLFinal:
			msg = &pgproto3.SASLResponse{}
		case pgproto3.AuthTypeGSS, pgproto3.AuthTypeGSSCont:
			msg = &pgproto3.GSSResponse{}
		case pgproto3.AuthTypeCleartextPassword, pgproto3.AuthTypeMD5Password:
			fallthrough
		default:
			// to maintain backwards compatability
			msg = &pgproto3.PasswordMessage{}
		}
	case 'Q':
		msg = &b.BackendWrapper.Query
	case 'S':
		msg = &b.BackendWrapper.Sync
	case 'X':
		msg = &b.BackendWrapper.Terminate
	default:
		return nil, fmt.Errorf("unknown message type: %c", b.BackendWrapper.MsgType)
	}
	err := msg.Decode(msgBody[5:])
	if b.BackendWrapper.MsgType == 'P' {
		*msg.(*pgproto3.Parse) = b.BackendWrapper.Parse
	}

	return msg, err
}

func (f *FrontendWrapper) TranslateToReadableResponse(msgBody []byte, logger *zap.Logger) (pgproto3.BackendMessage, error) {
	f.FrontendWrapper.BodyLen = int(binary.BigEndian.Uint32(msgBody[1:])) - 4
	f.FrontendWrapper.MsgType = msgBody[0]
	var msg pgproto3.BackendMessage
	switch f.FrontendWrapper.MsgType {
	case '1':
		msg = &f.FrontendWrapper.ParseComplete
	case '2':
		msg = &f.FrontendWrapper.BindComplete
	case '3':
		msg = &f.FrontendWrapper.CloseComplete
	case 'A':
		msg = &f.FrontendWrapper.NotificationResponse
	case 'c':
		msg = &f.FrontendWrapper.CopyDone
	case 'C':
		msg = &f.FrontendWrapper.CommandComplete
	case 'd':
		msg = &f.FrontendWrapper.CopyData
	case 'D':
		msg = &f.FrontendWrapper.DataRow
		logger.Debug("Data Row", zap.String("data", string(msgBody)))
	case 'E':
		msg = &f.FrontendWrapper.ErrorResponse
	case 'G':
		msg = &f.FrontendWrapper.CopyInResponse
	case 'H':
		msg = &f.FrontendWrapper.CopyOutResponse
	case 'I':
		msg = &f.FrontendWrapper.EmptyQueryResponse
	case 'K':
		msg = &f.FrontendWrapper.BackendKeyData
	case 'n':
		msg = &f.FrontendWrapper.NoData
	case 'N':
		msg = &f.FrontendWrapper.NoticeResponse
	case 'R':
		var err error
		msg, err = f.findAuthenticationMessageType(msgBody)
		if err != nil {
			return nil, err
		}
	case 's':
		msg = &f.FrontendWrapper.PortalSuspended
	case 'S':
		msg = &f.FrontendWrapper.ParameterStatus
	case 't':
		msg = &f.FrontendWrapper.ParameterDescription
	case 'T':
		msg = &f.FrontendWrapper.RowDescription
	case 'V':
		msg = &f.FrontendWrapper.FunctionCallResponse
	case 'W':
		msg = &f.FrontendWrapper.CopyBothResponse
	case 'Z':
		msg = &f.FrontendWrapper.ReadyForQuery
	default:
		return nil, fmt.Errorf("unknown message type: %c", f.FrontendWrapper.MsgType)
	}

	logger.Debug("msgFrontend", zap.String("msgFrontend", string(msgBody)))

	err := msg.Decode(msgBody[5:])
	if err != nil {
		logger.Error("Error from decoding request message ..", zap.Error(err))
	}

	bits := msg.Encode([]byte{})
	// println("Length of bits", len(bits), "Length of msgBody", len(msgBody))
	if len(bits) != len(msgBody) {
		logger.Debug("Encoded Data doesn't match the original data ..")
	}

	return msg, err
}

func (f *FrontendWrapper) findAuthenticationMessageType(src []byte) (pgproto3.BackendMessage, error) {
	if len(src) < 4 {
		return nil, errors.New("authentication message too short")
	}

	authType, err := parseAuthType(src)
	if err != nil {
		return nil, err

	}

	f.FrontendWrapper.AuthType = authType
	switch f.FrontendWrapper.AuthType {
	case pgproto3.AuthTypeOk:
		return &f.FrontendWrapper.AuthenticationOk, nil
	case pgproto3.AuthTypeCleartextPassword:
		return &f.FrontendWrapper.AuthenticationCleartextPassword, nil
	case pgproto3.AuthTypeMD5Password:
		return &f.FrontendWrapper.AuthenticationMD5Password, nil
	case pgproto3.AuthTypeSCMCreds:
		return nil, errors.New("AuthTypeSCMCreds is unimplemented")
	case pgproto3.AuthTypeGSS:
		return &f.FrontendWrapper.AuthenticationGSS, nil
	case pgproto3.AuthTypeGSSCont:
		return &f.FrontendWrapper.AuthenticationGSSContinue, nil
	case pgproto3.AuthTypeSSPI:
		return nil, errors.New("AuthTypeSSPI is unimplemented")
	case pgproto3.AuthTypeSASL:
		return &f.FrontendWrapper.AuthenticationSASL, nil
	case pgproto3.AuthTypeSASLContinue:
		return &f.FrontendWrapper.AuthenticationSASLContinue, nil
	case pgproto3.AuthTypeSASLFinal:
		return &f.FrontendWrapper.AuthenticationSASLFinal, nil
	default:
		return nil, fmt.Errorf("unknown authentication type: %d", f.FrontendWrapper.AuthType)
	}

}

// GetAuthType returns the authType used in the current state of the frontend.
// See SetAuthType for more information.
func parseAuthType(buffer []byte) (int32, error) {
	// Create a bytes reader from the buffer
	reader := bytes.NewReader(buffer)

	// Skip the message type (1 byte) as you know it's 'R'
	reader.Seek(1, 0)

	// Read the length of the message (4 bytes)
	var length int32
	err := binary.Read(reader, binary.BigEndian, &length)
	if err != nil {
		return 0, err
	}

	// Read the auth type code (4 bytes)
	var authType int32
	err = binary.Read(reader, binary.BigEndian, &authType)
	if err != nil {
		return 0, err
	}

	return authType, nil
}

const (
	minStartupPacketLen = 4     // minStartupPacketLen is a single 32-bit int version or code.
	maxStartupPacketLen = 10000 // maxStartupPacketLen is MAX_STARTUP_PACKET_LENGTH from PG source.
	sslRequestNumber    = 80877103
	cancelRequestCode   = 80877102
	gssEncReqNumber     = 80877104
)

func (b *BackendWrapper) DecodeStartupMessage(buf []byte) (pgproto3.FrontendMessage, error) {

	reader := pgproto3.NewByteReader(buf)
	buf, err := reader.Next(4)

	if err != nil {
		return nil, err
	}
	msgSize := int(binary.BigEndian.Uint32(buf) - 4)

	if msgSize < minStartupPacketLen || msgSize > maxStartupPacketLen {
		return nil, fmt.Errorf("invalid length of startup packet: %d", msgSize)
	}

	buf, err = reader.Next(msgSize)
	if err != nil {
		return nil, fmt.Errorf("invalid length of startup packet: %d", msgSize)
	}

	code := binary.BigEndian.Uint32(buf)

	switch code {
	case ProtocolVersionNumber:
		err := b.BackendWrapper.StartupMessage.Decode(buf)
		if err != nil {
			return nil, err
		}
		return &b.BackendWrapper.StartupMessage, nil
	case sslRequestNumber:
		err := b.BackendWrapper.SSlRequest.Decode(buf)
		if err != nil {
			return nil, err
		}
		return &b.BackendWrapper.SSlRequest, nil
	case cancelRequestCode:
		err := b.BackendWrapper.CancelRequest.Decode(buf)
		if err != nil {
			return nil, err
		}
		return &b.BackendWrapper.CancelRequest, nil
	case gssEncReqNumber:
		err := b.BackendWrapper.GssEncRequest.Decode(buf)
		if err != nil {
			return nil, err
		}
		return &b.BackendWrapper.GssEncRequest, nil
	default:
		return nil, fmt.Errorf("unknown startup message code: %d", code)
	}
}
