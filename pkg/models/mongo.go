package models

import (
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
)

type MongoOpMessage struct {
	FlagBits int      `json:"flagBits" yaml:"flagBits" bson:"flag_bits"`
	Sections []string `json:"sections" yaml:"sections" bson:"sections"`
	Checksum int      `json:"checksum" yaml:"checksum" bson:"checksum"`
}

type MongoOpQuery struct {
	Flags                int32  `json:"flags" yaml:"flags" bson:"flags"`
	FullCollectionName   string `json:"collection_name" yaml:"collection_name" bson:"full_collection_name"`
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
