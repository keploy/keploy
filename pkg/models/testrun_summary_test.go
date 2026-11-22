package models

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

func mongoSummaryMock(message interface{}, metadata map[string]string) *Mock {
	return mongoSummaryMockWithOpcode(wiremessage.OpMsg, message, metadata)
}

func mongoSummaryMockWithOpcode(opcode wiremessage.OpCode, message interface{}, metadata map[string]string) *Mock {
	return &Mock{
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: metadata,
			MongoRequests: []MongoRequest{{
				Header:  &MongoHeader{Opcode: opcode},
				Message: message,
			}},
		},
	}
}

// A mock recorded without Spec.Metadata["operation"] still gets a named
// summary: the operation is the command name carried by the message itself.
func TestMockSummaryDerivesMongoOperationFromBSONSection(t *testing.T) {
	section, err := bson.Marshal(bson.D{{Key: "update", Value: "entity_versions"}})
	if err != nil {
		t.Fatalf("marshal section: %v", err)
	}
	mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{string(section)}}, nil)
	if got := MockSummaryFromSpec(mock); got != "MongoDB update" {
		t.Fatalf("summary = %q, want %q", got, "MongoDB update")
	}
}

// Sections recorded by older parsers hold the JSON rendering of the document
// instead of raw BSON; the command name is still the first non-$ field.
func TestMockSummaryDerivesMongoOperationFromJSONSection(t *testing.T) {
	for name, section := range map[string]string{
		"plain":           `{"find":"products","filter":{}}`,
		"leading spaces":  `   {"find":"products"}  `,
		"skips $db first": `{"$db":"shop","$readPreference":{"mode":"primary"},"aggregate":"orders"}`,
	} {
		t.Run(name, func(t *testing.T) {
			want := "MongoDB find"
			if name == "skips $db first" {
				want = "MongoDB aggregate"
			}
			mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{section}}, nil)
			if got := MockSummaryFromSpec(mock); got != want {
				t.Fatalf("summary = %q, want %q", got, want)
			}
		})
	}
}

// A stored operation keeps winning over the derived one, so existing mocks
// render exactly as before.
func TestMockSummaryPrefersStoredMongoOperation(t *testing.T) {
	section, err := bson.Marshal(bson.D{{Key: "find", Value: "products"}})
	if err != nil {
		t.Fatalf("marshal section: %v", err)
	}
	mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{string(section)}}, map[string]string{"operation": "update"})
	if got := MockSummaryFromSpec(mock); got != "MongoDB update" {
		t.Fatalf("summary = %q, want %q", got, "MongoDB update")
	}
}

// Legacy OP_QUERY messages name their command only when targeting .$cmd (or
// when FullCollectionName is unset). Ordinary collection queries carry filter
// documents whose keys must not be reported as MongoDB operations.
func TestMockSummaryDerivesMongoOperationFromLegacyQuery(t *testing.T) {
	bsonPing, err := bson.Marshal(bson.D{{Key: "ping", Value: 1}})
	if err != nil {
		t.Fatalf("marshal ping: %v", err)
	}
	for _, tc := range []struct {
		name string
		msg  *MongoOpQuery
		want string
	}{
		{
			name: "json admin.$cmd",
			msg:  &MongoOpQuery{FullCollectionName: "admin.$cmd", Query: `{"ping":1}`},
			want: "MongoDB ping",
		},
		{
			name: "bson admin.$cmd",
			msg:  &MongoOpQuery{FullCollectionName: "admin.$cmd", Query: string(bsonPing)},
			want: "MongoDB ping",
		},
		{
			name: "unset collection name",
			msg:  &MongoOpQuery{Query: `{"isMaster":1}`},
			want: "MongoDB isMaster",
		},
		{
			name: "ordinary collection filter is not a command",
			msg:  &MongoOpQuery{FullCollectionName: "mydb.users", Query: `{"email":"a@example.com"}`},
			want: "MongoDB",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := mongoSummaryMockWithOpcode(wiremessage.OpQuery, tc.msg, nil)
			if got := MockSummaryFromSpec(mock); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// Multi-section OP_MSG must only read Sections[0]; an empty or unparseable
// command section must not fall through to user payload documents in Sections[1:].
func TestMockSummaryDoesNotFallThroughToPayloadSection(t *testing.T) {
	emptyBSON, err := bson.Marshal(bson.D{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	payloadBSON, err := bson.Marshal(bson.D{{Key: "_id", Value: "123"}, {Key: "email", Value: "u@e.com"}})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{string(emptyBSON), string(payloadBSON)}}, nil)
	if got := MockSummaryFromSpec(mock); got != "MongoDB" {
		t.Fatalf("summary = %q, want %q", got, "MongoDB")
	}
}

// Typed-nil messages, empty/corrupt BSON, malformed JSON, and invalid keys
// must never panic or produce bogus operation names.
func TestMockSummaryFallsBackToBareMongoKind(t *testing.T) {
	emptyBSON, err := bson.Marshal(bson.D{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	emptyKeyBSON, err := bson.Marshal(bson.D{{Key: "", Value: 1}})
	if err != nil {
		t.Fatalf("marshal empty key: %v", err)
	}
	controlKeyBSON, err := bson.Marshal(bson.D{{Key: "find\nrm", Value: 1}})
	if err != nil {
		t.Fatalf("marshal control key: %v", err)
	}

	var nilOpMsg *MongoOpMessage
	var nilOpQuery *MongoOpQuery

	for name, mock := range map[string]*Mock{
		"no section":           mongoSummaryMock(&MongoOpMessage{}, nil),
		"garbage section":      mongoSummaryMock(&MongoOpMessage{Sections: []string{"not a document"}}, nil),
		"no message":           mongoSummaryMock(nil, nil),
		"typed nil op_msg":     mongoSummaryMock(nilOpMsg, nil),
		"typed nil op_query":   mongoSummaryMockWithOpcode(wiremessage.OpQuery, nilOpQuery, nil),
		"empty bson":           mongoSummaryMock(&MongoOpMessage{Sections: []string{string(emptyBSON)}}, nil),
		"empty bson key":       mongoSummaryMock(&MongoOpMessage{Sections: []string{string(emptyKeyBSON)}}, nil),
		"control char key":     mongoSummaryMock(&MongoOpMessage{Sections: []string{string(controlKeyBSON)}}, nil),
		"corrupt bson zero":    mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x08\x00\x00\x00\x00ab\x00"}}, nil),
		"corrupt bson 7 bytes": mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x07\x00\x00\x00\x02a\x00"}}, nil),
		"corrupt bson no nul":  mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x08\x00\x00\x00\x02a\x00X"}}, nil),
		"empty json":           mongoSummaryMock(&MongoOpMessage{Sections: []string{"{}"}}, nil),
		"padded empty json":    mongoSummaryMock(&MongoOpMessage{Sections: []string{"   {}   "}}, nil),
		"truncated json":       mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"garbage`}}, nil),
		"whitespace json key":  mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"   ":1}`}}, nil),
		"json array":           mongoSummaryMock(&MongoOpMessage{Sections: []string{`["find"]`}}, nil),
		"only $ metadata":      mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"$db":"test"}`}}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			if got := MockSummaryFromSpec(mock); got != "MongoDB" {
				t.Fatalf("%s: summary = %q, want %q", name, got, "MongoDB")
			}
		})
	}
}
