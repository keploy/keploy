package models

import (
	"encoding/binary"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestParseOpMsg(t *testing.T) {
	// 1. Construct a fake BSON document: { "ping": 1 }
	doc := bson.D{{Key: "ping", Value: 1}}
	bsonData, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("Failed to create mock BSON: %v", err)
	}

	// 2. Construct the OP_MSG payload
	// Structure: [Flags (4)] + [SectionKind (1)] + [BSON Size (4)] + [BSON Data] + [Checksum (4)]
	
	buf := []byte{}

	// Flags (0)
	flags := make([]byte, 4)
	binary.LittleEndian.PutUint32(flags, 0)
	buf = append(buf, flags...)

	// Section Kind (0 = Body)
	buf = append(buf, 0)

	// Note: The BSON data already includes its own size header, so we append it directly
	buf = append(buf, bsonData...)

	// Checksum (Fake CRC32)
	checksum := make([]byte, 4)
	binary.LittleEndian.PutUint32(checksum, 123456)
	buf = append(buf, checksum...)

	// 3. Run the Parser (The function you wrote)
	msg, err := parseOpMsg(buf)
	if err != nil {
		t.Fatalf("parseOpMsg failed: %v", err)
	}

	// 4. Validate Results
	if msg.FlagBits != 0 {
		t.Errorf("Expected flags 0, got %d", msg.FlagBits)
	}
	if len(msg.Sections) != 1 {
		t.Fatalf("Expected 1 section, got %d", len(msg.Sections))
	}
	if msg.Sections[0].Kind != 0 {
		t.Errorf("Expected Section Kind 0, got %d", msg.Sections[0].Kind)
	}
	if msg.Checksum != 123456 {
		t.Errorf("Expected Checksum 123456, got %d", msg.Checksum)
	}

	t.Log("✅ Success! OpMsg parsed correctly.")
}