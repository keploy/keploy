package models

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

func mongoSummaryMock(message interface{}, metadata map[string]string) *Mock {
	return &Mock{
		Kind: Mongo,
		Spec: MockSpec{
			Metadata: metadata,
			MongoRequests: []MongoRequest{{
				Header:  &MongoHeader{Opcode: wiremessage.OpMsg},
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
// instead of raw BSON; the command name is still the first field.
func TestMockSummaryDerivesMongoOperationFromJSONSection(t *testing.T) {
	mock := mongoSummaryMock(&MongoOpMessage{Sections: []string{`{"find":"products","filter":{}}`}}, nil)
	if got := MockSummaryFromSpec(mock); got != "MongoDB find" {
		t.Fatalf("summary = %q, want %q", got, "MongoDB find")
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

// Legacy OP_QUERY messages name their command as the first field of the query.
func TestMockSummaryDerivesMongoOperationFromLegacyQuery(t *testing.T) {
	mock := mongoSummaryMock(&MongoOpQuery{Query: `{"ping":1}`}, nil)
	if got := MockSummaryFromSpec(mock); got != "MongoDB ping" {
		t.Fatalf("summary = %q, want %q", got, "MongoDB ping")
	}
}

// Nothing recognisable to derive from leaves the bare kind, as before.
func TestMockSummaryFallsBackToBareMongoKind(t *testing.T) {
	for name, mock := range map[string]*Mock{
		"no section":      mongoSummaryMock(&MongoOpMessage{}, nil),
		"garbage section": mongoSummaryMock(&MongoOpMessage{Sections: []string{"not a document"}}, nil),
		"no message":      mongoSummaryMock(nil, nil),
	} {
		if got := MockSummaryFromSpec(mock); got != "MongoDB" {
			t.Fatalf("%s: summary = %q, want %q", name, got, "MongoDB")
		}
	}
}
