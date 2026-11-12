package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// OutsideRequests reaches the recorder, which takes mocks from the agent as gob.
func TestOutsideRequestsTravelsToTheRecorder(t *testing.T) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&Mock{Name: "m", OutsideRequests: true}); err != nil {
		t.Fatal(err)
	}
	var got Mock
	if err := gob.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OutsideRequests {
		t.Fatal("OutsideRequests was lost on the way to the recorder")
	}
}

// It is never stored: a recording's file reads as it did before.
func TestOutsideRequestsIsNeverStored(t *testing.T) {
	m := &Mock{Name: "m", OutsideRequests: true}
	asJSON, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	asYAML, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	asBSON, err := bson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for format, out := range map[string]string{"json": string(asJSON), "yaml": string(asYAML), "bson": string(asBSON)} {
		if strings.Contains(strings.ToLower(out), "outside") {
			t.Errorf("%s holds the mark: %s", format, out)
		}
	}
}

func TestOutsideRequestsSurvivesACopy(t *testing.T) {
	m := &Mock{OutsideRequests: true}
	if !m.DeepCopy().OutsideRequests {
		t.Error("DeepCopy lost it")
	}
	if !m.ShallowCopy().OutsideRequests {
		t.Error("ShallowCopy lost it")
	}
}
