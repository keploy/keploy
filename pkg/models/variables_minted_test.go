package models

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// A minted mark goes with the bind it was declared at: a's bind keeps a's
// (a variable both bind keeps a's, as a binds it), a bind taken from b
// keeps b's, and a use marked is no minted bind. Neither a clone nor a
// merge shares a mark with its input.
func TestMergeVariablesKeepsAMintedMarkWithItsBind(t *testing.T) {
	hex := &MintedShape{Rule: "audit-id", Alphabet: "hex"}
	digits := &MintedShape{Rule: "audit-id", Alphabet: "digits"}
	for _, c := range []struct {
		name string
		a, b MockVariable
		want *MintedShape
	}{
		{"a's bind, marked", MockVariable{Recorded: "r", Bind: "body._id", Minted: hex}, MockVariable{Recorded: "r", Use: []string{"filter._id"}}, hex},
		{"a bind taken from b, marked", MockVariable{Recorded: "r", Use: []string{"filter._id"}}, MockVariable{Recorded: "r", Bind: "body._id", Minted: hex}, hex},
		{"both bind, a's mark", MockVariable{Recorded: "r", Bind: "body._id", Minted: hex}, MockVariable{Recorded: "r", Bind: "body._id", Minted: digits}, hex},
		{"both bind, only b marked", MockVariable{Recorded: "r", Bind: "body._id"}, MockVariable{Recorded: "r", Bind: "body._id", Minted: hex}, nil},
		{"a use marked is no minted bind", MockVariable{Recorded: "r", Bind: "body._id"}, MockVariable{Recorded: "r", Use: []string{"filter._id"}, Minted: hex}, nil},
	} {
		in, other := MockVariables{"id": c.a}, MockVariables{"id": c.b}
		got := MergeVariables(in, other)["id"]
		if !reflect.DeepEqual(got.Minted, c.want) {
			t.Errorf("%s: merged Minted %+v, want %+v", c.name, got.Minted, c.want)
		}
		if got.Minted != nil {
			got.Minted.Alphabet = "base64"
			if in["id"].Minted != nil && in["id"].Minted.Alphabet == "base64" || other["id"].Minted != nil && other["id"].Minted.Alphabet == "base64" {
				t.Errorf("%s: the merged block shares its mark with an input", c.name)
			}
		}
	}
	in := MockVariables{"id": {Recorded: "r", Bind: "body._id", Minted: &MintedShape{Rule: "audit-id", Alphabet: "hex"}}}
	c := in.Clone()
	c["id"].Minted.Rule = "other"
	if in["id"].Minted.Rule != "audit-id" {
		t.Error("a clone shares its mark with the block it copies")
	}
}

// A minted mark is written in a mock, and read back, in each of its
// encodings; a variable with none writes no mark.
func TestAMintedMarkSurvivesItsEncodings(t *testing.T) {
	v := MockVariables{"auditId": {Recorded: "0f8a", Bind: "documents[0]._id", Minted: &MintedShape{Rule: "audit-id", Alphabet: "hex"}}, "appId": {Recorded: "7d3b", Use: []string{"filter._id"}}}
	y, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML MockVariables
	if err := yaml.Unmarshal(y, &fromYAML); err != nil || !reflect.DeepEqual(fromYAML, v) {
		t.Fatalf("YAML round trip: %v, got %+v from\n%s", err, fromYAML, y)
	}
	j, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON MockVariables
	if err := json.Unmarshal(j, &fromJSON); err != nil || !reflect.DeepEqual(fromJSON, v) {
		t.Fatalf("JSON round trip: %v, got %+v from %s", err, fromJSON, j)
	}
	b, err := bson.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fromBSON MockVariables
	if err := bson.Unmarshal(b, &fromBSON); err != nil || !reflect.DeepEqual(fromBSON["auditId"].Minted, v["auditId"].Minted) || fromBSON["appId"].Minted != nil {
		t.Fatalf("BSON round trip: %v, got %+v", err, fromBSON)
	}
	var plain map[string]map[string]any
	if err := json.Unmarshal(j, &plain); err != nil {
		t.Fatal(err)
	}
	if _, has := plain["appId"]["minted"]; has {
		t.Errorf("a variable with no mark writes one: %s", j)
	}
	bj, _ := json.Marshal(VariableBinding{Name: "auditId", Value: "5c1e", Minted: v["auditId"].Minted})
	var back VariableBinding
	if err := json.Unmarshal(bj, &back); err != nil || !reflect.DeepEqual(back.Minted, v["auditId"].Minted) {
		t.Fatalf("a binding's mark: %v, got %+v from %s", err, back.Minted, bj)
	}
}
