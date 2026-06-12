package models

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// TestMongoMessageGobRoundTrip guards the gob.Register() calls in this
// package's init(). Mock persistence is gob-based and the Message field of
// MongoRequest/MongoResponse is an interface{}; gob refuses to encode a
// concrete type stored in an interface unless it is registered. If a future
// edit drops one of these Register calls, recording that opcode would fail gob
// encoding and the mock would be skipped — exactly the OP_COMPRESSED
// regression this PR fixes. Encoding through the interface field (not the
// concrete type directly) is what exercises the registration.
func TestMongoMessageGobRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		msg  interface{}
	}{
		{"OP_COMPRESSED", &MongoOpCompressed{
			OriginalOpcode:   2013,
			UncompressedSize: 42,
			CompressorID:     2, // zlib
			CompressedData:   []byte{0x78, 0x9c, 0x01, 0x02, 0x03},
		}},
		{"OP_MSG", &MongoOpMessage{
			FlagBits: 0,
			Sections: []string{`{ "find": "products" }`},
			Checksum: 12345,
		}},
		{"OP_REPLY", &MongoOpReply{
			ResponseFlags:  8,
			CursorID:       99,
			NumberReturned: 1,
			Documents:      []string{`{ "_id": 1 }`},
		}},
		{"OP_QUERY", &MongoOpQuery{
			FullCollectionName: "protodb.products",
			Flags:              0,
			Query:              `{ "x": 1 }`,
		}},
		{"OP_GET_MORE", &MongoOpGetMore{
			FullCollectionName: "protodb.products",
			NumberToReturn:     2,
			CursorID:           12345,
		}},
		{"OP_KILL_CURSORS", &MongoOpKillCursors{
			NumberOfCursorIDs: 1,
			CursorIDs:         []int64{12345},
		}},
		{"OP_INSERT", &MongoOpInsert{
			Flags:              0,
			FullCollectionName: "protodb.products",
			Documents:          []string{`{ "_id": 1 }`},
		}},
		{"OP_UPDATE", &MongoOpUpdate{
			FullCollectionName: "protodb.products",
			Flags:              0,
			Selector:           `{ "_id": 1 }`,
			Update:             `{ "$set": { "x": 2 } }`,
		}},
		{"OP_DELETE", &MongoOpDelete{
			FullCollectionName: "protodb.products",
			Flags:              0,
			Selector:           `{ "_id": 1 }`,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Encode through the interface field, the way mock persistence does.
			in := MongoRequest{
				Header:  &MongoHeader{Opcode: wiremessage.OpCode(2012)},
				Message: tc.msg,
			}
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(&in); err != nil {
				t.Fatalf("gob encode failed (type not registered?): %v", err)
			}
			var out MongoRequest
			if err := gob.NewDecoder(&buf).Decode(&out); err != nil {
				t.Fatalf("gob decode failed: %v", err)
			}
			if !reflect.DeepEqual(in.Message, out.Message) {
				t.Fatalf("round trip mismatch:\n in: %#v\nout: %#v", in.Message, out.Message)
			}
		})
	}
}
