package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// failedAfterEmpty is an answer whose server sent one empty message and then
// ended the stream with an error, as a recorder that writes NoMessages marks
// it.
func failedAfterEmpty() GrpcResp {
	var r GrpcResp
	r.SetMessages([]GrpcLengthPrefixedMessage{{}})
	r.EmptyMessageSent = true
	r.Trailers = GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "13", "grpc-message": "boom"}}
	return r
}

// Every way a mock travels keeps the mark: YAML on disk, JSON, gob from the
// agent to the CLI, BSON in the cloud.
func TestEmptyMessageSentSurvivesEveryEncoding(t *testing.T) {
	in := failedAfterEmpty()
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if !bytes.Contains(y, []byte("empty_message_sent: true")) {
		t.Fatalf("yaml has no empty_message_sent key:\n%s", y)
	}
	var fromYAML GrpcResp
	if err := yaml.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var fromJSON GrpcResp
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(in); err != nil {
		t.Fatalf("gob encode: %v", err)
	}
	var fromGob GrpcResp
	if err := gob.NewDecoder(&buf).Decode(&fromGob); err != nil {
		t.Fatalf("gob decode: %v", err)
	}
	b, err := bson.Marshal(in)
	if err != nil {
		t.Fatalf("bson marshal: %v", err)
	}
	var fromBSON GrpcResp
	if err := bson.Unmarshal(b, &fromBSON); err != nil {
		t.Fatalf("bson unmarshal: %v", err)
	}
	for via, got := range map[string]GrpcResp{"yaml": fromYAML, "json": fromJSON, "gob": fromGob, "bson": fromBSON} {
		if !got.EmptyMessageSent {
			t.Errorf("through %s the mark is lost", via)
		}
		if n := len(got.AllMessages()); n != 1 {
			t.Errorf("through %s the answer has %d message(s), want 1", via, n)
		}
	}
	m := &Mock{Kind: GRPC_EXPORT, Spec: MockSpec{GRPCResp: &in}}
	if c := m.DeepCopy(); !c.Spec.GRPCResp.EmptyMessageSent {
		t.Error("a deep copy lost the mark")
	}
}

// An answer without the mark gains no key on disk or on the wire: it is
// byte-identical to one written before the field existed.
func TestEmptyMessageSentIsOffDiskWhenUnset(t *testing.T) {
	in := failedAfterEmpty()
	in.EmptyMessageSent = false
	y, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if bytes.Contains(y, []byte("empty_message_sent")) {
		t.Errorf("an unmarked answer gained empty_message_sent:\n%s", y)
	}
	j, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	if bytes.Contains(j, []byte("empty_message_sent")) {
		t.Errorf("an unmarked answer gained empty_message_sent in JSON: %s", j)
	}
}

// A mock encoded by a binary from before the field decodes unmarked: replay
// serves its one empty message under a failed status with that status alone,
// as before.
func TestEmptyMessageSentOldGobDecodesUnmarked(t *testing.T) {
	type oldGrpcResp struct {
		Headers    GrpcHeaders
		Body       GrpcLengthPrefixedMessage
		NoMessages bool
		SentAfter  []uint32
		Trailers   GrpcHeaders
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(oldGrpcResp{
		Trailers: GrpcHeaders{OrdinaryHeaders: map[string]string{"grpc-status": "13"}},
	}); err != nil {
		t.Fatalf("gob encode old shape: %v", err)
	}
	var got GrpcResp
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("gob decode into new shape: %v", err)
	}
	if got.EmptyMessageSent {
		t.Fatal("an older binary's mock decoded marked")
	}
}
