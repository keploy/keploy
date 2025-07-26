package v1

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jackc/pgproto3/v2"
	"go.keploy.io/server/v2/pkg/models"
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

// PG Response Packet Transcoder
func (b *BackendWrapper) translateToReadableBackend(msgBody []byte) (pgproto3.FrontendMessage, error) {
	// Safety check for message body length
	if len(msgBody) < 5 {
		return nil, fmt.Errorf("message body too short: %d bytes, minimum required: 5", len(msgBody))
	}

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
		return nil, fmt.Errorf("unknown message type: %c (0x%02X)", b.BackendWrapper.MsgType, b.BackendWrapper.MsgType)
	}

	// Safely decode the message
	err := msg.Decode(msgBody[5:])
	if err != nil {
		return nil, fmt.Errorf("failed to decode message type %c: %w", b.BackendWrapper.MsgType, err)
	}

	if b.BackendWrapper.MsgType == 'P' {
		*msg.(*pgproto3.Parse) = b.BackendWrapper.Parse
	}

	return msg, nil
}

func (f *FrontendWrapper) translateToReadableResponse(logger *zap.Logger, msgBody []byte) (pgproto3.BackendMessage, error) {
	// Safety check for message body length
	if len(msgBody) < 5 {
		return nil, fmt.Errorf("response message body too short: %d bytes, minimum required: 5", len(msgBody))
	}

	f.FrontendWrapper.BodyLen = int(binary.BigEndian.Uint32(msgBody[1:])) - 4
	f.FrontendWrapper.MsgType = msgBody[0]

	// Validate body length against actual message size
	if len(msgBody) < f.FrontendWrapper.BodyLen+5 {
		return nil, fmt.Errorf("insufficient message body: got %d bytes, expected %d",
			len(msgBody), f.FrontendWrapper.BodyLen+5)
	}

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
		msg, err = f.findAuthMsgType(msgBody)
		if err != nil {
			return nil, fmt.Errorf("failed to find auth message type: %w", err)
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
		return nil, fmt.Errorf("unknown message type: %c (0x%02X)", f.FrontendWrapper.MsgType, f.FrontendWrapper.MsgType)
	}

	logger.Debug("msgFrontend", zap.String("msgFrontend", string(msgBody)))

	err := msg.Decode(msgBody[5:])
	if err != nil {
		logger.Error("Error from decoding response message",
			zap.Error(err),
			zap.Uint8("message_type", f.FrontendWrapper.MsgType),
			zap.Int("body_length", f.FrontendWrapper.BodyLen))
		return nil, fmt.Errorf("failed to decode response message type %c: %w", f.FrontendWrapper.MsgType, err)
	}

	// Validate encoding consistency
	bits := msg.Encode([]byte{})
	if len(bits) != len(msgBody) {
		logger.Debug("Encoded data doesn't match the original data",
			zap.Int("encoded_length", len(bits)),
			zap.Int("original_length", len(msgBody)))
	}

	return msg, nil
}

const (
	minStartupPacketLen = 4     // minStartupPacketLen is a single 32-bit int version or code.
	maxStartupPacketLen = 10000 // maxStartupPacketLen is MAX_STARTUP_PACKET_LENGTH from PG source.
	sslRequestNumber    = 80877103
	cancelRequestCode   = 80877102
	gssEncReqNumber     = 80877104
)

// ProtocolVersionNumber Replace with actual version number if different
const ProtocolVersionNumber uint32 = 196608

func (b *BackendWrapper) decodeStartupMessage(buf []byte) (pgproto3.FrontendMessage, error) {
	// Add safety check for buffer length
	if len(buf) < 8 {
		return nil, fmt.Errorf("startup message too short: %d bytes, minimum required: 8", len(buf))
	}

	reader := pgproto3.NewByteReader(buf)
	lenBuf, err := reader.Next(4)
	if err != nil {
		return nil, fmt.Errorf("failed to read message length: %w", err)
	}

	msgSize := int(binary.BigEndian.Uint32(lenBuf) - 4)

	if msgSize < minStartupPacketLen || msgSize > maxStartupPacketLen {
		return nil, fmt.Errorf("invalid length of startup packet: %d (min: %d, max: %d)",
			msgSize, minStartupPacketLen, maxStartupPacketLen)
	}

	// Check if we have enough bytes for the message
	if len(buf) < msgSize+4 {
		return nil, fmt.Errorf("insufficient buffer for startup message: got %d bytes, need %d",
			len(buf), msgSize+4)
	}

	msgBuf, err := reader.Next(msgSize)
	if err != nil {
		return nil, fmt.Errorf("failed to read startup message body: %w", err)
	}

	code := binary.BigEndian.Uint32(msgBuf)

	switch code {
	case ProtocolVersionNumber:
		err := b.BackendWrapper.StartupMessage.Decode(msgBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to decode startup message: %w", err)
		}
		return &b.BackendWrapper.StartupMessage, nil
	case sslRequestNumber:
		err := b.BackendWrapper.SSlRequest.Decode(msgBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to decode SSL request: %w", err)
		}
		return &b.BackendWrapper.SSlRequest, nil
	case cancelRequestCode:
		err := b.BackendWrapper.CancelRequest.Decode(msgBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to decode cancel request: %w", err)
		}
		return &b.BackendWrapper.CancelRequest, nil
	case gssEncReqNumber:
		err := b.BackendWrapper.GssEncRequest.Decode(msgBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to decode GSS encryption request: %w", err)
		}
		return &b.BackendWrapper.GssEncRequest, nil
	default:
		return nil, fmt.Errorf("unknown startup message code: %d (0x%08X)", code, code)
	}
}

// constants for the authentication message types
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

func (f *FrontendWrapper) findAuthMsgType(src []byte) (pgproto3.BackendMessage, error) {
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
	_, err := reader.Seek(1, 0)
	if err != nil {
		return 0, err
	}

	// Read the length of the message (4 bytes)
	var length int32
	err = binary.Read(reader, binary.BigEndian, &length)
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

func isStartupPacket(packet []byte) bool {
	if len(packet) < 8 {
		return false
	}
	protocolVersion := binary.BigEndian.Uint32(packet[4:8])
	// printStartupPacketDetails(packet)
	return protocolVersion == 196608 // 3.0 in PostgreSQL
}
