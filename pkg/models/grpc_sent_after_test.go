package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// pingPongResp is a recorded bidi answer whose server ended the stream before
// the client half-closed: three replies, each after its ping, then the end
// after the third ping; and one whose server wrote first, before any of the
// client's messages: an end after none, which a zero entry says.
func pingPongResp() (GrpcResp, GrpcResp) {
	var pp GrpcResp
	pp.SetMessages([]GrpcLengthPrefixedMessage{
		{MessageLength: 8, DecodedData: `1: "pong-1"`},
		{MessageLength: 8, DecodedData: `1: "pong-2"`},
		{MessageLength: 8, DecodedData: `1: "pong-3"`},
	})
	pp.SentAfter = []uint32{1, 2, 3, 3}
	var first GrpcResp
	first.SetMessages(nil)
	first.SentAfter = []uint32{0}
	return pp, first
}

// Every way a mock travels keeps SentAfter whole, a zero entry included: YAML
// on disk, JSON, gob from the agent to the CLI, BSON in the cloud.
func TestSentAfterSurvivesEveryEncoding(t *testing.T) {
	pp, first := pingPongResp()
	for name, in := range map[string]GrpcResp{"ping-pong": pp, "server-first": first} {
		t.Run(name, func(t *testing.T) {
			y, err := yaml.Marshal(in)
			if err != nil {
				t.Fatalf("yaml marshal: %v", err)
			}
			if !bytes.Contains(y, []byte("sent_after:")) {
				t.Fatalf("yaml has no sent_after key:\n%s", y)
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
				if !reflect.DeepEqual(got.SentAfter, in.SentAfter) {
					t.Errorf("through %s SentAfter is %v, want %v", via, got.SentAfter, in.SentAfter)
				}
			}
		})
	}
}

// A call with no write points, every unary call among them, gains no key on
// disk or on the wire: its mock is byte-identical to one written before the
// field existed.
func TestSentAfterIsOffDiskWhenEmpty(t *testing.T) {
	var unary GrpcResp
	unary.SetMessages([]GrpcLengthPrefixedMessage{{MessageLength: 2, DecodedData: "1: 1"}})
	y, err := yaml.Marshal(unary)
	if err != nil {
		t.Fatalf("yaml marshal: %v", err)
	}
	if bytes.Contains(y, []byte("sent_after")) {
		t.Errorf("a unary answer gained sent_after:\n%s", y)
	}
	j, err := json.Marshal(unary)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	if bytes.Contains(j, []byte("sent_after")) {
		t.Errorf("a unary answer gained sent_after in JSON: %s", j)
	}
}

// A mock encoded by a binary from before the field decodes with no write
// points: replay reads the client's whole side before it answers, as before.
func TestSentAfterOldGobDecodesEmpty(t *testing.T) {
	type oldGrpcResp struct {
		Headers            GrpcHeaders
		Body               GrpcLengthPrefixedMessage
		AdditionalMessages []GrpcLengthPrefixedMessage
		NoMessages         bool
		Trailers           GrpcHeaders
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(oldGrpcResp{
		Body:               GrpcLengthPrefixedMessage{MessageLength: 8, DecodedData: `1: "pong-1"`},
		AdditionalMessages: []GrpcLengthPrefixedMessage{{MessageLength: 8, DecodedData: `1: "pong-2"`}},
	}); err != nil {
		t.Fatalf("gob encode old shape: %v", err)
	}
	var got GrpcResp
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("gob decode into new shape: %v", err)
	}
	if got.SentAfter != nil {
		t.Fatalf("an older binary's mock decoded with write points %v", got.SentAfter)
	}
	if len(got.AllMessages()) != 2 {
		t.Fatalf("old mock yields %d message(s), want 2", len(got.AllMessages()))
	}
}

// A deep copy's write points are its own.
func TestSentAfterDeepCopyIsItsOwn(t *testing.T) {
	pp, _ := pingPongResp()
	m := &Mock{Kind: GRPC_EXPORT, Spec: MockSpec{GRPCResp: &pp}}
	c := m.DeepCopy()
	c.Spec.GRPCResp.SentAfter[0] = 9
	if m.Spec.GRPCResp.SentAfter[0] != 1 {
		t.Fatalf("editing the copy's write points changed the mock's: %v", m.Spec.GRPCResp.SentAfter)
	}
}
