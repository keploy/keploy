package models

import "go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"

type MongoOpMessage struct {
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
