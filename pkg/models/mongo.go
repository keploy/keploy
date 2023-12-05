package models

import (
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
)

type MongoOpMessage struct {
	FlagBits int      `json:"flagBits" bson:"flagBits" yaml:"flagBits"`
	Sections []string `json:"sections" bson:"sections" yaml:"sections"`
	Checksum int      `json:"checksum" bson:"checksum" yaml:"checksum"`
}

type MongoOpQuery struct {
	Flags                int32  `json:"flags" yaml:"flags" bson:"flags"`                                  // bit values of query options
	FullCollectionName   string `json:"collection_name" yaml:"collection_name" bson:"collection_name"`    // "dbname.collectionname"
	NumberToSkip         int32  `json:"number_to_skip" yaml:"number_to_skip" bson:"number_to_skip"`       // number of documents to skip
	NumberToReturn       int32  `json:"number_to_return" yaml:"number_to_return" bson:"number_to_return"` // number of documents to return in the first OP_REPLY batch
	Query                string `json:"query" yaml:"query" bson:"query"`                                  // query object.  See below for details.
	ReturnFieldsSelector string `json:"return_fields_selector" yaml:"return_fields_selector" bson:"return_fields_selector" `
}
type MongoOpReply struct {
	ResponseFlags  int32    `json:"response_flags" bson:"response_flags" yaml:"response_flags"`
	CursorID       int64    `json:"cursor_id" bson:"cursor_id" yaml:"cursor_id"`
	StartingFrom   int32    `json:"starting_from" bson:"starting_from" yaml:"starting_from"`
	NumberReturned int32    `json:"number_returned" bson:"number_returned" yaml:"number_returned"`
	Documents      []string `json:"documents" bson:"documents" yaml:"documents"`
}

type MongoHeader struct {
	Length     int32              `json:"length" bson:"length" yaml:"length"`
	RequestID  int32              `json:"requestId" bson:"requestId" yaml:"requestId"`
	ResponseTo int32              `json:"responseTo" bson:"responseTo" yaml:"responseTo"`
	Opcode     wiremessage.OpCode `json:"Opcode" bson:"Opcode" yaml:"Opcode"`
}

type MongoRequest struct {
	Header    *MongoHeader `json:"header,omitempty" bson:"header,omitempty" yaml:"header,omitempty"`
	Message   interface{}  `json:"message,omitempty" bson:"message,omitempty" yaml:"message,omitempty"`
	ReadDelay int64        `json:"read_delay,omitempty" bson:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}

type MongoResponse struct {
	Header    *MongoHeader `json:"header,omitempty" bson:"header,omitempty" yaml:"header,omitempty"`
	Message   interface{}  `json:"message,omitempty" bson:"message,omitempty" yaml:"message,omitempty"`
	ReadDelay int64        `json:"read_delay,omitempty" bson:"read_delay,omitempty" yaml:"read_delay,omitempty"`
}
