package models

import (
	"bytes"
	"encoding/gob"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNoMessages_DistinguishesZeroFromOneEmpty is the point of the field.
//
// Both shapes are legal gRPC and were previously indistinguishable: a server
// stream that ends with NO messages (grpc-go's interop DoEmptyStream) and one
// that sends a single message of length 0 (what the gRPC health check records).
// AllMessages() returned one element for both, so an empty stream replayed as
// one empty message and the application saw a count of 1 where it saw 0.
func TestNoMessages_DistinguishesZeroFromOneEmpty(t *testing.T) {
	var zero GrpcResp
	zero.SetMessages(nil)
	if got := len(zero.AllMessages()); got != 0 {
		t.Errorf("a zero-message stream yields %d message(s), want 0", got)
	}

	var oneEmpty GrpcResp
	oneEmpty.SetMessages([]GrpcLengthPrefixedMessage{{MessageLength: 0}})
	if got := len(oneEmpty.AllMessages()); got != 1 {
		t.Errorf("one empty message yields %d, want 1 — the health check depends on this", got)
	}
}

// TestNoMessages_DefaultsToPreExistingBehaviour is the compatibility pin. Every
// mock recorded before this field existed decodes with NoMessages=false and
// must behave exactly as it did.
func TestNoMessages_DefaultsToPreExistingBehaviour(t *testing.T) {
	// A struct built WITHOUT SetMessages, i.e. how an older decode lands.
	old := GrpcResp{Body: GrpcLengthPrefixedMessage{MessageLength: 0}}
	if old.NoMessages {
		t.Fatal("zero value of NoMessages must be false")
	}
	if got := len(old.AllMessages()); got != 1 {
		t.Fatalf("a pre-existing mock yields %d message(s), want 1 — this is the shape "+
			"the health check records and replay sends one empty message for", got)
	}
}

// TestNoMessages_SurvivesYAMLAndGob pins both wire formats mocks travel over:
// yaml to disk, gob agent->CLI. omitempty must keep it off disk for every mock
// that does not need it, so unary yaml stays byte-identical.
func TestNoMessages_SurvivesYAMLAndGob(t *testing.T) {
	var empty GrpcResp
	empty.SetMessages(nil)

	y, err := yaml.Marshal(empty)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if !bytes.Contains(y, []byte("no_messages: true")) {
		t.Errorf("yaml lost no_messages:\n%s", y)
	}
	var backY GrpcResp
	if err := yaml.Unmarshal(y, &backY); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if len(backY.AllMessages()) != 0 {
		t.Error("yaml round-trip turned a zero-message stream back into one message")
	}

	// A normal unary response must NOT gain the key.
	var unary GrpcResp
	unary.SetMessages([]GrpcLengthPrefixedMessage{{MessageLength: 2, DecodedData: "1: 1"}})
	yu, err := yaml.Marshal(unary)
	if err != nil {
		t.Fatalf("yaml marshal unary: %v", err)
	}
	if bytes.Contains(yu, []byte("no_messages")) {
		t.Errorf("omitempty failed: a unary mock gained no_messages:\n%s", yu)
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(empty); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	var backG GrpcResp
	if err := gob.NewDecoder(&buf).Decode(&backG); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	if len(backG.AllMessages()) != 0 {
		t.Error("gob round-trip turned a zero-message stream back into one message")
	}
}

// TestNoMessages_OldGobDecodesUnchanged is the direction that matters for a
// staged rollout: a mock encoded by an OLDER binary (no such field) must decode
// here with the pre-existing meaning, not as an empty stream.
func TestNoMessages_OldGobDecodesUnchanged(t *testing.T) {
	type oldGrpcResp struct {
		Headers            GrpcHeaders
		Body               GrpcLengthPrefixedMessage
		AdditionalMessages []GrpcLengthPrefixedMessage
		Trailers           GrpcHeaders
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(oldGrpcResp{
		Body: GrpcLengthPrefixedMessage{MessageLength: 0},
	}); err != nil {
		t.Fatalf("gob encode old shape: %v", err)
	}
	var got GrpcResp
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("gob decode into new shape: %v", err)
	}
	if got.NoMessages {
		t.Fatal("a mock from an older binary decoded as a zero-message stream")
	}
	if len(got.AllMessages()) != 1 {
		t.Fatalf("old mock yields %d message(s), want 1", len(got.AllMessages()))
	}
}
