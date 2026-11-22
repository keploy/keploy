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
// summary: the operation is the first non-$ command name carried by the BSON document.
func TestMockSummaryDerivesMongoOperationFromBSONSection(t *testing.T) {
	plain, err := bson.Marshal(bson.D{{Key: "update", Value: "entity_versions"}})
	if err != nil {
		t.Fatalf("marshal plain: %v", err)
	}
	withDollarFirst, err := bson.Marshal(bson.D{
		{Key: "$db", Value: "shop"},
		{Key: "find", Value: "products"},
	})
	if err != nil {
		t.Fatalf("marshal withDollarFirst: %v", err)
	}

	for _, tc := range []struct {
		name    string
		section string
		want    string
	}{
		{name: "plain", section: string(plain), want: "MongoDB update"},
		{name: "skips $db first", section: string(withDollarFirst), want: "MongoDB find"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{tc.section}}, nil)
			if got := MockSummaryFromSpec(mock); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

// Sections recorded by older parsers hold the JSON rendering of the document
// instead of raw BSON; the command name is still the first non-$ field.
func TestMockSummaryDerivesMongoOperationFromJSONSection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		section string
		want    string
	}{
		{name: "plain", section: `{"find":"products","filter":{}}`, want: "MongoDB find"},
		{name: "leading spaces", section: `   {"find":"products"}  `, want: "MongoDB find"},
		{name: "skips $db first", section: `{"$db":"shop","$readPreference":{"mode":"primary"},"aggregate":"orders"}`, want: "MongoDB aggregate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{tc.section}}, nil)
			if got := MockSummaryFromSpec(mock); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
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
	mustBSON := func(d bson.D) string {
		b, err := bson.Marshal(d)
		if err != nil {
			t.Fatalf("marshal bson: %v", err)
		}
		return string(b)
	}

	emptyBSON := mustBSON(bson.D{})
	emptyKeyBSON := mustBSON(bson.D{{Key: "", Value: 1}})
	emptyKeyBeforeValidBSON := mustBSON(bson.D{{Key: "", Value: 1}, {Key: "find", Value: "products"}})
	controlKeyBSON := mustBSON(bson.D{{Key: "find\nrm", Value: 1}})
	onlyDollarBSON := mustBSON(bson.D{{Key: "$db", Value: "shop"}})
	invalidUTF8KeyBSON := mustBSON(bson.D{{Key: "find\xff", Value: 1}})

	var nilOpMsg *MongoOpMessage
	var nilOpQuery *MongoOpQuery

	for _, tc := range []struct {
		name string
		mock *Mock
	}{
		{name: "no section", mock: mongoSummaryMock(&MongoOpMessage{}, nil)},
		{name: "garbage section", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"not a document"}}, nil)},
		{name: "no message", mock: mongoSummaryMock(nil, nil)},
		{name: "typed nil op_msg", mock: mongoSummaryMock(nilOpMsg, nil)},
		{name: "typed nil op_query", mock: mongoSummaryMockWithOpcode(wiremessage.OpQuery, nilOpQuery, nil)},
		{name: "empty bson", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{emptyBSON}}, nil)},
		{name: "empty bson key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{emptyKeyBSON}}, nil)},
		{name: "invalid first bson key stops before valid key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{emptyKeyBeforeValidBSON}}, nil)},
		{name: "control char bson key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{controlKeyBSON}}, nil)},
		{name: "only $ bson metadata", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{onlyDollarBSON}}, nil)},
		{name: "invalid utf8 bson key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{invalidUTF8KeyBSON}}, nil)},
		{name: "corrupt bson zero", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x08\x00\x00\x00\x00ab\x00"}}, nil)},
		{name: "corrupt bson 7 bytes", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x07\x00\x00\x00\x02a\x00"}}, nil)},
		{name: "corrupt bson no nul", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x08\x00\x00\x00\x02a\x00X"}}, nil)},
		{name: "corrupt bson negative length", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"\xff\xff\xff\xff\x02a\x00\x00"}}, nil)},
		{name: "corrupt bson mismatched length", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"\x10\x00\x00\x00\x02a\x00\x00"}}, nil)},
		{name: "empty json", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"{}"}}, nil)},
		{name: "padded empty json", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"   {}   "}}, nil)},
		{name: "truncated json", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"garbage`}}, nil)},
		{name: "whitespace json key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"   ":1}`}}, nil)},
		{name: "invalid first json key stops before valid key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"bad key":1,"find":"products"}`}}, nil)},
		{name: "invalid utf8 json key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"{\"find\xff\":1}"}}, nil)},
		{name: "replacement char json key", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{"{\"find\ufffd\":1}"}}, nil)},
		{name: "json array", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{`["find"]`}}, nil)},
		{name: "only $ json metadata", mock: mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"$db":"test"}`}}, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MockSummaryFromSpec(tc.mock); got != "MongoDB" {
				t.Fatalf("%s: summary = %q, want %q", tc.name, got, "MongoDB")
			}
		})
	}
}
