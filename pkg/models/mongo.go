package models

import (
	"bytes"
	"encoding/binary"  
	"encoding/gob"
	"encoding/json"
	"fmt" 
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"gopkg.in/yaml.v3"
)

// MongoDB Wire Protocol OpCodes
// Reference: https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/
// Legacy opcodes (deprecated in MongoDB 5.0, removed in 5.1): https://www.mongodb.com/docs/manual/legacy-opcodes/
const (
	// Current opcodes
	OpCodeCompressed wiremessage.OpCode = 2012 // OP_COMPRESSED - Wraps other messages using compression
	OpCodeMsg        wiremessage.OpCode = 2013 // OP_MSG - Extensible message format (MongoDB 3.6+)

	// Legacy opcodes (for backward compatibility with older MongoDB versions)
	OpCodeReply       wiremessage.OpCode = 1    // OP_REPLY - Reply to a client request
	OpCodeUpdate      wiremessage.OpCode = 2001 // OP_UPDATE - Update document
	OpCodeInsert      wiremessage.OpCode = 2002 // OP_INSERT - Insert document
	OpCodeGetByOID    wiremessage.OpCode = 2003 // OP_GET_BY_OID - Reserved, not used
	OpCodeQuery       wiremessage.OpCode = 2004 // OP_QUERY - Query a collection
	OpCodeGetMore     wiremessage.OpCode = 2005 // OP_GET_MORE - Get more data from a query
	OpCodeDelete      wiremessage.OpCode = 2006 // OP_DELETE - Delete documents
	OpCodeKillCursors wiremessage.OpCode = 2007 // OP_KILL_CURSORS - Close cursors
)

type MongoSpec struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []RequestYaml     `json:"requests" yaml:"requests"`
	Response         []ResponseYaml    `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"reqTimestampMock" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock" yaml:"resTimestampMock,omitempty"`
}

type RequestYaml struct {
	Header    *MongoHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node    `json:"message,omitempty" yaml:"message"`
	ReadDelay int64        `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type ResponseYaml struct {
	Header    *MongoHeader `json:"header,omitempty" yaml:"header"`
	Message   yaml.Node    `json:"message,omitempty" yaml:"message"`
	ReadDelay int64        `json:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type MongoOpMessage struct {
	FlagBits int            `json:"flagBits" yaml:"flagBits" bson:"flagBits"`
	Sections []OpMsgSection `json:"sections" yaml:"sections" bson:"sections"`
	Checksum int            `json:"checksum" yaml:"checksum" bson:"checksum"`
}

type OpMsgSection struct {
	Kind       int           `json:"kind" yaml:"kind" bson:"kind"` // 0 = Body, 1 = Document Sequence
	Identifier string        `json:"identifier,omitempty" yaml:"identifier,omitempty" bson:"identifier,omitempty"`
	Documents  []interface{} `json:"documents" yaml:"documents" bson:"documents"`
}

type MongoOpQuery struct {
	Flags                int32  `json:"flags" yaml:"flags" bson:"flags"`
	FullCollectionName   string `json:"collection_name" yaml:"collection_name" bson:"collection_name"`
	NumberToSkip         int32  `json:"number_to_skip" yaml:"number_to_skip" bson:"number_to_skip"`
	NumberToReturn       int32  `json:"number_to_return" yaml:"number_to_return" bson:"number_to_return"`
	Query                string `json:"query" yaml:"query" bson:"query"`
	ReturnFieldsSelector string `json:"return_fields_selector" yaml:"return_fields_selector" bson:"return_fields_selector"`
}

type MongoOpReply struct {
	ResponseFlags  int32    `json:"response_flags" yaml:"response_flags" bson:"response_flags"`
	CursorID       int64    `json:"cursor_id" yaml:"cursor_id" bson:"cursor_id"`
	StartingFrom   int32    `json:"starting_from" yaml:"starting_from" bson:"starting_from"`
	NumberReturned int32    `json:"number_returned" yaml:"number_returned" bson:"number_returned"`
	Documents      []string `json:"documents" yaml:"documents" bson:"documents"`
}

// MongoOpUpdate represents the OP_UPDATE (2001) wire protocol message
// Deprecated in MongoDB 5.0, removed in MongoDB 5.1
// Format: Header | ZERO | fullCollectionName | flags | selector | update
// Reference: https://www.mongodb.com/docs/manual/legacy-opcodes/#op_update
type MongoOpUpdate struct {
	FullCollectionName string `json:"fullCollectionName" yaml:"fullCollectionName" bson:"fullCollectionName"`
	Flags              int32  `json:"flags" yaml:"flags" bson:"flags"` // bit 0: Upsert, bit 1: MultiUpdate
	Selector           string `json:"selector" yaml:"selector" bson:"selector"`
	Update             string `json:"update" yaml:"update" bson:"update"`
}

// MongoOpInsert represents the OP_INSERT (2002) wire protocol message
// Deprecated in MongoDB 5.0, removed in MongoDB 5.1
// Format: Header | flags | fullCollectionName | documents
// Reference: https://www.mongodb.com/docs/manual/legacy-opcodes/#op_insert
type MongoOpInsert struct {
	Flags              int32    `json:"flags" yaml:"flags" bson:"flags"` // bit 0: ContinueOnError
	FullCollectionName string   `json:"fullCollectionName" yaml:"fullCollectionName" bson:"fullCollectionName"`
	Documents          []string `json:"documents" yaml:"documents" bson:"documents"`
}

// MongoOpDelete represents the OP_DELETE (2006) wire protocol message
// Deprecated in MongoDB 5.0, removed in MongoDB 5.1
// Format: Header | ZERO | fullCollectionName | flags | selector
// Reference: https://www.mongodb.com/docs/manual/legacy-opcodes/#op_delete
type MongoOpDelete struct {
	FullCollectionName string `json:"fullCollectionName" yaml:"fullCollectionName" bson:"fullCollectionName"`
	Flags              int32  `json:"flags" yaml:"flags" bson:"flags"` // bit 0: SingleRemove
	Selector           string `json:"selector" yaml:"selector" bson:"selector"`
}

// MongoOpGetMore represents the OP_GET_MORE (2005) wire protocol message
// Deprecated in MongoDB 5.0, removed in MongoDB 5.1
// Format: Header | ZERO | fullCollectionName | numberToReturn | cursorID
// Reference: https://www.mongodb.com/docs/manual/legacy-opcodes/#op_get_more
type MongoOpGetMore struct {
	FullCollectionName string `json:"fullCollectionName" yaml:"fullCollectionName" bson:"fullCollectionName"`
	NumberToReturn     int32  `json:"numberToReturn" yaml:"numberToReturn" bson:"numberToReturn"`
	CursorID           int64  `json:"cursorID" yaml:"cursorID" bson:"cursorID"`
}

// MongoOpKillCursors represents the OP_KILL_CURSORS (2007) wire protocol message
// Deprecated in MongoDB 5.0, removed in MongoDB 5.1
// Format: Header | ZERO | numberOfCursorIDs | cursorIDs
// Reference: https://www.mongodb.com/docs/manual/legacy-opcodes/#op_kill_cursors
type MongoOpKillCursors struct {
	NumberOfCursorIDs int32   `json:"numberOfCursorIDs" yaml:"numberOfCursorIDs" bson:"numberOfCursorIDs"`
	CursorIDs         []int64 `json:"cursorIDs" yaml:"cursorIDs" bson:"cursorIDs"`
}

// MongoOpCompressed represents the OP_COMPRESSED (2012) wire protocol message
// Used to wrap other opcodes with compression
// Format: Header | originalOpcode | uncompressedSize | compressorId | compressedMessage
// compressorId: 0=noop, 1=snappy, 2=zlib, 3=zstd
// Reference: https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/#op_compressed
type MongoOpCompressed struct {
	OriginalOpcode   int32  `json:"originalOpcode" yaml:"originalOpcode" bson:"originalOpcode"`
	UncompressedSize int32  `json:"uncompressedSize" yaml:"uncompressedSize" bson:"uncompressedSize"`
	CompressorID     int8   `json:"compressorId" yaml:"compressorId" bson:"compressorId"` // 0=noop, 1=snappy, 2=zlib, 3=zstd
	CompressedData   []byte `json:"compressedData,omitempty" yaml:"compressedData,omitempty" bson:"compressedData,omitempty"`
}

// MongoOpUnknown represents an unknown or unsupported wire protocol message
// This is used as a fallback when the opcode is not recognized
type MongoOpUnknown struct {
	Opcode int32  `json:"opcode" yaml:"opcode" bson:"opcode"`
	Data   []byte `json:"data,omitempty" yaml:"data,omitempty" bson:"data,omitempty"`
}

type MongoHeader struct {
	Length     int32              `json:"length" yaml:"length" bson:"length"`
	RequestID  int32              `json:"requestId" yaml:"requestId" bson:"request_id"`
	ResponseTo int32              `json:"responseTo" yaml:"responseTo" bson:"response_to"`
	Opcode     wiremessage.OpCode `json:"Opcode" yaml:"Opcode" bson:"opcode"`
}

type MongoRequest struct {
	Header    *MongoHeader `json:"header,omitempty" yaml:"header,omitempty" bson:"header,omitempty"`
	Message   interface{}  `json:"message,omitempty" yaml:"message,omitempty" bson:"message,omitempty"`
	ReadDelay int64        `json:"read_delay,omitempty" yaml:"read_delay,omitempty" bson:"read_delay,omitempty"`
}

type MongoResponse struct {
	Header    *MongoHeader `json:"header,omitempty" yaml:"header,omitempty" bson:"header,omitempty"`
	Message   interface{}  `json:"message,omitempty" yaml:"message,omitempty" bson:"message,omitempty"`
	ReadDelay int64        `json:"read_delay,omitempty" yaml:"read_delay,omitempty" bson:"read_delay,omitempty"`
}

func init() {
	gob.Register(&MongoOpMessage{})
	gob.Register(&MongoOpQuery{})
	gob.Register(&MongoOpReply{})
}

// UnmarshalBSON implements bson.Unmarshaler for mongoRequests because of interface typeof field
func (mr *MongoRequest) UnmarshalBSON(data []byte) error {

	// duplicate struct to avoid infinite recursion
	type MongoRequestAlias struct {
		Header    *MongoHeader `bson:"header,omitempty"`
		Message   bson.Raw     `bson:"message,omitempty"`
		ReadDelay int64        `bson:"read_delay,omitempty"`
	}
	var aux MongoRequestAlias

	if err := bson.Unmarshal(data, &aux); err != nil {
		return err
	}

	// assign the unmarshalled data to the original data
	mr.Header = aux.Header
	mr.ReadDelay = aux.ReadDelay

	// unmarshal the message into the correct type
	switch mr.Header.Opcode {
	case wiremessage.OpMsg:
                // Parse the raw OpMsg binary body
                msg, err := parseOpMsg(aux.Message)
                if err != nil {
                        return err
                }
                mr.Message = msg
	case wiremessage.OpQuery:
		var msg MongoOpQuery
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeUpdate:
		var msg MongoOpUpdate
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeInsert:
		var msg MongoOpInsert
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeDelete:
		var msg MongoOpDelete
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeGetMore:
		var msg MongoOpGetMore
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeKillCursors:
		var msg MongoOpKillCursors
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeCompressed:
		var msg MongoOpCompressed
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	default:
		// Handle unknown opcodes gracefully - store as raw message
		var msg MongoOpUnknown
		msg.Opcode = int32(mr.Header.Opcode)
		msg.Data = aux.Message
		mr.Message = &msg
	}
	return nil
}

// UnmarshalJSON implements json.Unmarshaler for mongoRequests because of interface typeof field
func (mr *MongoRequest) UnmarshalJSON(data []byte) error {
	// duplicate struct to avoid infinite recursion
	type MongoRequestAlias struct {
		Header    *MongoHeader    `json:"header"`
		Message   json.RawMessage `json:"message"`
		ReadDelay int64           `json:"read_delay"`
	}
	var aux MongoRequestAlias

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// assign the unmarshalled data to the original data
	mr.Header = aux.Header
	mr.ReadDelay = aux.ReadDelay

	// unmarshal the message into the correct type
	switch mr.Header.Opcode {
	case wiremessage.OpMsg:
		var msg MongoOpMessage
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case wiremessage.OpQuery:
		var msg MongoOpQuery
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeUpdate:
		var msg MongoOpUpdate
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeInsert:
		var msg MongoOpInsert
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeDelete:
		var msg MongoOpDelete
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeGetMore:
		var msg MongoOpGetMore
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeKillCursors:
		var msg MongoOpKillCursors
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeCompressed:
		var msg MongoOpCompressed
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	default:
		// Handle unknown opcodes gracefully
		var msg MongoOpUnknown
		msg.Opcode = int32(mr.Header.Opcode)
		msg.Data = aux.Message
		mr.Message = &msg
	}

	return nil
}

// MarshalJSON implements json.Marshaler for mongoRequests because of interface typeof field
func (mr *MongoRequest) MarshalJSON() ([]byte, error) {
	// duplicate struct to avoid infinite recursion
	type MongoRequestAlias struct {
		Header    *MongoHeader    `json:"header"`
		Message   json.RawMessage `json:"message"`
		ReadDelay int64           `json:"read_delay"`
	}

	aux := MongoRequestAlias{
		Header:    mr.Header,
		Message:   json.RawMessage(nil),
		ReadDelay: mr.ReadDelay,
	}

	if mr.Message != nil {
		// Marshal the message interface{} into JSON
		msgJSON, err := json.Marshal(mr.Message)
		if err != nil {
			return nil, err
		}
		aux.Message = msgJSON
	}

	return json.Marshal(aux)
}

// UnmarshalBSON implements bson.Unmarshaler for mongoResponses because of interface typeof field
func (mr *MongoResponse) UnmarshalBSON(data []byte) error {
	// duplicate struct to avoid infinite recursion
	type MongoResponseAlias struct {
		Header    *MongoHeader `bson:"header,omitempty"`
		Message   bson.Raw     `bson:"message,omitempty"`
		ReadDelay int64        `bson:"read_delay,omitempty"`
	}
	var aux MongoResponseAlias

	if err := bson.Unmarshal(data, &aux); err != nil {
		return err
	}

	// assign the unmarshalled data to the original data
	mr.Header = aux.Header
	mr.ReadDelay = aux.ReadDelay

	// unmarshal the message into the correct type
	switch mr.Header.Opcode {
	case wiremessage.OpMsg:
		var msg MongoOpMessage
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case wiremessage.OpReply:
		var msg MongoOpReply
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeCompressed:
		var msg MongoOpCompressed
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	default:
		// Handle unknown opcodes gracefully
		var msg MongoOpUnknown
		msg.Opcode = int32(mr.Header.Opcode)
		msg.Data = aux.Message
		mr.Message = &msg
	}

	return nil
}

// UnmarshalJSON implements json.Unmarshaler for mongoResponses because of interface typeof field
func (mr *MongoResponse) UnmarshalJSON(data []byte) error {
	// duplicate struct to avoid infinite recursion
	type MongoResponseAlias struct {
		Header    *MongoHeader    `json:"header"`
		Message   json.RawMessage `json:"message"`
		ReadDelay int64           `json:"read_delay"`
	}
	var aux MongoResponseAlias

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// assign the unmarshalled data to the original data
	mr.Header = aux.Header
	mr.ReadDelay = aux.ReadDelay

	// unmarshal the message into the correct type
	switch mr.Header.Opcode {
	case wiremessage.OpMsg:
		var msg MongoOpMessage
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case wiremessage.OpReply:
		var msg MongoOpReply
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case OpCodeCompressed:
		var msg MongoOpCompressed
		if err := json.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	default:
		// Handle unknown opcodes gracefully
		var msg MongoOpUnknown
		msg.Opcode = int32(mr.Header.Opcode)
		msg.Data = aux.Message
		mr.Message = &msg
	}

	return nil

}

// MarshalJSON implements json.Marshaler for mongoResponses because of interface typeof field
func (mr *MongoResponse) MarshalJSON() ([]byte, error) {
	// duplicate struct to avoid infinite recursion
	type MongoResponseAlias struct {
		Header    *MongoHeader    `json:"header"`
		Message   json.RawMessage `json:"message"`
		ReadDelay int64           `json:"read_delay"`
	}

	aux := MongoResponseAlias{
		Header:    mr.Header,
		Message:   json.RawMessage(nil),
		ReadDelay: mr.ReadDelay,
	}

	if mr.Message != nil {
		// Marshal the message interface{} into JSON
		msgJSON, err := json.Marshal(mr.Message)
		if err != nil {
			return nil, err
		}
		aux.Message = msgJSON
	}

	return json.Marshal(aux)
}

// parseOpMsg decodes the raw binary body of a MongoDB OP_MSG packet.
// See: https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/#op_msg
func parseOpMsg(data []byte) (*MongoOpMessage, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("invalid OpMsg: data too short for flags")
	}

	// 1. Read Flags (First 4 bytes)
	flags := int(binary.LittleEndian.Uint32(data[0:4]))
	data = data[4:]

	msg := &MongoOpMessage{
		FlagBits: flags,
		Sections: []OpMsgSection{},
	}

	// 2. Read Sections
	// Loop until we hit the Checksum (last 4 bytes) or end of data.
	for len(data) > 4 { 
		kind := int(data[0])
		data = data[1:]

		section := OpMsgSection{Kind: kind}

		if kind == 0 {
			// Kind 0: Single BSON Document (Body)
			if len(data) < 4 {
				return nil, fmt.Errorf("invalid OpMsg: data too short for section 0 size")
			}
			bsonSize := int(binary.LittleEndian.Uint32(data[0:4]))
			if len(data) < bsonSize {
				return nil, fmt.Errorf("invalid OpMsg: section 0 data incomplete")
			}

			rawBson := data[:bsonSize]
			var doc interface{}
			if err := bson.Unmarshal(rawBson, &doc); err != nil {
				return nil, fmt.Errorf("failed to unmarshal section 0 bson: %w", err)
			}
			
			// Store as a single document slice
			section.Documents = []interface{}{doc}
			msg.Sections = append(msg.Sections, section)
			
			data = data[bsonSize:]

		} else if kind == 1 {
			// Kind 1: Document Sequence (Bulk)
			if len(data) < 4 {
				return nil, fmt.Errorf("invalid OpMsg: data too short for section 1 size")
			}
			sectionSize := int(binary.LittleEndian.Uint32(data[0:4]))
			if len(data) < sectionSize {
				return nil, fmt.Errorf("invalid OpMsg: section 1 data incomplete")
			}
			
			// Isolate the section data
			sectionData := data[:sectionSize]
			data = data[sectionSize:]
			
			// Skip the 4 size bytes we just read
			sectionData = sectionData[4:]
			
			// Read Identifier (CString)
			idx := bytes.IndexByte(sectionData, 0)
			if idx == -1 {
				return nil, fmt.Errorf("invalid OpMsg: section 1 identifier missing null terminator")
			}
			section.Identifier = string(sectionData[:idx])
			sectionData = sectionData[idx+1:] 

			// Read Sequence of BSONs
			for len(sectionData) > 0 {
				// Each BSON doc starts with its size (int32)
				if len(sectionData) < 4 {
					break 
				}
				docSize := int(binary.LittleEndian.Uint32(sectionData[0:4]))
				if len(sectionData) < docSize {
					return nil, fmt.Errorf("invalid OpMsg: section 1 document incomplete")
				}
				
				rawDoc := sectionData[:docSize]
				var doc interface{}
				if err := bson.Unmarshal(rawDoc, &doc); err != nil {
					return nil, fmt.Errorf("failed to unmarshal section 1 bson: %w", err)
				}
				section.Documents = append(section.Documents, doc)
				sectionData = sectionData[docSize:]
			}
			msg.Sections = append(msg.Sections, section)

		} else {
			return nil, fmt.Errorf("unknown OpMsg section kind: %d", kind)
		}
	}

	// 3. Handle Checksum (Remaining 4 bytes)
	if len(data) == 4 {
		msg.Checksum = int(binary.LittleEndian.Uint32(data))
	}

	return msg, nil
}
