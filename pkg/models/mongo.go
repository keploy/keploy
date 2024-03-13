package models

import (
	"encoding/json"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
)

type MongoOpMessage struct {
	FlagBits int      `json:"flagBits" yaml:"flagBits" bson:"flagBits"`
	Sections []string `json:"sections" yaml:"sections" bson:"sections"`
	Checksum int      `json:"checksum" yaml:"checksum" bson:"checksum"`
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
		var msg MongoOpMessage
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	case wiremessage.OpQuery:
		var msg MongoOpQuery
		if err := bson.Unmarshal(aux.Message, &msg); err != nil {
			return err
		}
		mr.Message = &msg
	default:
		return errors.New("failed to unmarshal unknown opcode")
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
	default:
		return errors.New("failed to unmarshal unknown opcode")
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

type MongoResponse struct {
	Header    *MongoHeader `json:"header,omitempty" yaml:"header,omitempty" bson:"header,omitempty"`
	Message   interface{}  `json:"message,omitempty" yaml:"message,omitempty" bson:"message,omitempty"`
	ReadDelay int64        `json:"read_delay,omitempty" yaml:"read_delay,omitempty" bson:"read_delay,omitempty"`
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
	default:
		return errors.New("failed to unmarshal unknown opcode")
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
	default:
		return errors.New("failed to unmarshal unknown opcode")
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
