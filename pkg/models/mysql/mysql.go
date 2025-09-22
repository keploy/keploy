package mysql

import (
	"encoding/json"
	"errors"
	"time"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []RequestYaml     `json:"requests" yaml:"requests"`
	Response         []ResponseYaml    `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"ReqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"ResTimestampMock,omitempty"`
}

type RequestYaml struct {
	Header  *PacketInfo       `json:"header,omitempty" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message,omitempty" yaml:"message"`
}

type ResponseYaml struct {
	Header  *PacketInfo       `json:"header,omitempty" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message,omitempty" yaml:"message"`
}

type PacketInfo struct {
	Header *Header `json:"header" yaml:"header"`
	Type   string  `json:"packet_type" yaml:"packet_type"`
}

type Request struct {
	PacketBundle `json:"packet_bundle" yaml:"packet_bundle"`
}

type Response struct {
	PacketBundle `json:"packet_bundle" yaml:"packet_bundle"`
	Payload      string `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type PacketBundle struct {
	Header  *PacketInfo       `json:"header" yaml:"header"`
	Message interface{}       `json:"message" yaml:"message"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
}

// MySql Packet
//refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_packets.html

type Packet struct {
	Header  Header `json:"header" yaml:"header"`
	Payload []byte `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type Header struct {
	PayloadLength uint32 `json:"payload_length" yaml:"payload_length"`
	SequenceID    uint8  `json:"sequence_id" yaml:"sequence_id"`
}

// custom marshal and unmarshal methods for Request and Response structs

// MarshalJSON implements json.Marshaler for Request because of interface type of field 'Message'
func (r *Request) MarshalJSON() ([]byte, error) {
	// create an alias struct to avoid infinite recursion
	type RequestAlias struct {
		Header  *PacketInfo       `json:"header"`
		Message json.RawMessage   `json:"message"`
		Meta    map[string]string `json:"meta,omitempty"`
	}

	aux := RequestAlias{
		Header:  r.Header,
		Message: json.RawMessage(nil),
		Meta:    r.Meta,
	}

	if r.Message != nil {
		// Marshal the message interface{} into JSON
		msgJSON, err := json.Marshal(r.Message)
		if err != nil {
			return nil, err
		}
		aux.Message = msgJSON
	}

	// Marshal the alias struct into JSON
	return json.Marshal(aux)
}

// UnmarshalJSON implements json.Unmarshaler for Request because of interface type of field 'Message'
func (r *Request) UnmarshalJSON(data []byte) error {
	// Alias struct to prevent recursion during unmarshalling
	type RequestAlias struct {
		Header  *PacketInfo       `json:"header"`
		Message json.RawMessage   `json:"message"`
		Meta    map[string]string `json:"meta,omitempty"`
	}
	var aux RequestAlias

	// Unmarshal the data into the alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Assign the unmarshalled data to the original struct
	r.Header = aux.Header
	r.Meta = aux.Meta

	// Unmarshal the message field based on the type in the header
	switch r.Header.Type {
	case HandshakeResponse41:
		var msg HandshakeResponse41Packet
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CachingSha2PasswordToString(RequestPublicKey):
		var msg string
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = msg

	case "encrypted_password":
		var msg string
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = msg

	case CommandStatusToString(COM_QUIT):
		var msg QuitPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_INIT_DB):
		var msg InitDBPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STATISTICS):
		var msg StatisticsPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_DEBUG):
		var msg DebugPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_PING):
		var msg PingPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_CHANGE_USER):
		var msg ChangeUserPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_RESET_CONNECTION):
		var msg ResetConnectionPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_QUERY):
		var msg QueryPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STMT_PREPARE):
		var msg StmtPreparePacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STMT_EXECUTE):
		var msg StmtExecutePacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STMT_CLOSE):
		var msg StmtClosePacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STMT_RESET):
		var msg StmtResetPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case CommandStatusToString(COM_STMT_SEND_LONG_DATA):
		var msg StmtSendLongDataPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	default:
		return errors.New("failed to unmarshal unknown request packet type")
	}
	return nil
}

// MarshalJSON implements json.Marshaler for Response because of interface type of field 'Message'
func (r *Response) MarshalJSON() ([]byte, error) {
	// Alias to avoid recursion
	type ResponseAlias struct {
		PacketBundle `json:"packet_bundle"`
		Payload      string          `json:"payload,omitempty"`
		Message      json.RawMessage `json:"message"`
	}

	aux := ResponseAlias{
		PacketBundle: r.PacketBundle,
		Payload:      r.Payload,
	}

	if r.Message != nil {
		// Marshal the message interface{} into JSON
		msgJSON, err := json.Marshal(r.Message)
		if err != nil {
			return nil, err
		}
		aux.Message = msgJSON
	}

	return json.Marshal(aux)
}

// UnmarshalJSON implements json.Unmarshaler for Response because of interface type of field 'Message'
func (r *Response) UnmarshalJSON(data []byte) error {
	// Alias struct to prevent recursion
	type ResponseAlias struct {
		PacketBundle `json:"packet_bundle"`
		Payload      string          `json:"payload,omitempty"`
		Message      json.RawMessage `json:"message"`
	}
	var aux ResponseAlias

	// Unmarshal the data into the alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Assign the unmarshalled data to the original struct
	r.PacketBundle = aux.PacketBundle
	r.Payload = aux.Payload

	// Unmarshal the message field based on the type in the header
	switch r.PacketBundle.Header.Type {
	// Generic response
	case StatusToString(EOF):
		var msg EOFPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case StatusToString(ERR):
		var msg ERRPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case StatusToString(OK):
		var msg OKPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	// Connection phase
	case AuthStatusToString(HandshakeV10):
		var msg HandshakeV10Packet
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case AuthStatusToString(AuthSwitchRequest):
		var msg AuthSwitchRequestPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case AuthStatusToString(AuthMoreData):
		var msg AuthMoreDataPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case AuthStatusToString(AuthNextFactor): // not supported yet
		var msg AuthNextFactorPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	// Command phase
	case COM_STMT_PREPARE_OK:
		var msg StmtPrepareOkPacket
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case string(Text):
		var msg TextResultSet
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	case string(Binary):
		var msg BinaryProtocolResultSet
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		r.Message = &msg

	default:
		return errors.New("failed to unmarshal unknown response packet type")
	}

	return nil
}
