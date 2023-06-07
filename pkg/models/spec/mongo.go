package spec

import (
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"gopkg.in/yaml.v3"
)

type MongoSpec struct {
	Metadata map[string]string `json:"metadata" yaml:"metadata"`
	RequestHeader   MongoHeader       `json:"request_mongo_header" yaml:"request_mongo_header"`
	ResponseHeader   MongoHeader       `json:"response_mongo_header" yaml:"response_mongo_header"`
	Request  yaml.Node         `json:"mongo_request" yaml:"mongo_request"`
	Response yaml.Node         `json:"mongo_response" yaml:"mongo_response"`
	// RequestMessage  MongoOpMessage `json:"request_mongo_message" yaml:"request_mongo_message,omitempty"`
	// ResponseMessage MongoOpMessage `json:"response_mongo_message" yaml:"response_mongo_message,omitempty"`
}

type MongoOpMessage struct {
	// Header   MongoHeader `json:"mongo_header" yaml:"mongo_header"`
	FlagBits int    `json:"flagBits" yaml:"flagBits"`
	Sections []string `json:"sections" yaml:"sections"`
	Checksum int    `json:"checksum" yaml:"checksum"`
}

type MongoOpQuery struct {
	Flags                int32    `json:"flags" yaml:"flags"`                       // bit values of query options
	FullCollectionName   string   `json:"collection_name" yaml:"collection_name"`   // "dbname.collectionname"
	NumberToSkip         int32    `json:"number_to_skip" yaml:"number_to_skip"`     // number of documents to skip
	NumberToReturn       int32    `json:"number_to_return" yaml:"number_to_return"` // number of documents to return in the first OP_REPLY batch
	Query                string   `json:"query" yaml:"query"`                       // query object.  See below for details.
	ReturnFieldsSelector string `json:"return_fields_selector" yaml:"return_fields_selector"`
}

type MongoOpReply struct {
	ResponseFlags  int32  `json:"response_flags" yaml:"response_flags"`   // bit values - see details below
	CursorID       int64  `json:"cursor_id" yaml:"cursor_id"`             // cursor ID if client needs to do get more's
	StartingFrom   int32  `json:"starting_from" yaml:"starting_from"`     // where in the cursor this reply is starting
	NumberReturned int32  `json:"number_returned" yaml:"number_returned"` // number of documents in the reply
	Documents      []string `json:"documents" yaml:"documents"`             // documents
}

type MongoHeader struct {
	Length     int32              `json:"length" yaml:"length"`
	RequestID  int32              `json:"requestId" yaml:"requestId"`
	ResponseTo int32              `json:"responseTo" yaml:"responseTo"`
	Opcode     wiremessage.OpCode `json:"Opcode" yaml:"Opcode"`
}
