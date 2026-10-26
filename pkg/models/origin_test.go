package models

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Where keploy would not name a process (it runs in a PID namespace of its
// own), an origin says why and gives the kernel pid. That travels to keploy
// record with the mock (gob) and to the replay with the call (json), and is
// never part of what a file holds: Persisted drops it, and YAML never writes
// it. An origin left with nothing a file holds is none.
func TestAnUnnamedOriginTravelsAndIsNeverPersisted(t *testing.T) {
	o := &Origin{Clock: "proxy:run-1", Unnamed: UnnamedPIDNamespace, PID: 2912345}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&Mock{Name: "mock-6", Origin: o}); err != nil {
		t.Fatal(err)
	}
	var back Mock
	if err := gob.NewDecoder(&buf).Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back.Origin == nil || *back.Origin != *o {
		t.Fatalf("gob carried origin %+v; want %+v", back.Origin, o)
	}
	b, err := json.Marshal(UnmatchedCall{Source: &Origin{Unnamed: UnnamedPIDNamespace, PID: 2912345}})
	if err != nil {
		t.Fatal(err)
	}
	var call UnmatchedCall
	if err := json.Unmarshal(b, &call); err != nil {
		t.Fatal(err)
	}
	if call.Source == nil || call.Source.Unnamed != UnnamedPIDNamespace || call.Source.PID != 2912345 {
		t.Fatalf("json carried source %+v", call.Source)
	}

	if p := o.Persisted(); p == nil || *p != (Origin{Clock: "proxy:run-1"}) {
		t.Fatalf("Persisted = %+v; want the clock alone", p)
	}
	if p := (&Origin{Unnamed: UnnamedPIDNamespace, PID: 7}).Persisted(); p != nil {
		t.Fatalf("Persisted of an origin with only why = %+v; want none", p)
	}
	named := &Origin{Clock: "c", Comm: "sqs", Exe: "./sqs"}
	if named.Persisted() != named {
		t.Fatal("Persisted copied an origin with nothing to drop")
	}
	if (*Origin)(nil).Persisted() != nil {
		t.Fatal("Persisted of none is some")
	}
	y, err := yaml.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "unnamed") || strings.Contains(string(y), "2912345") {
		t.Fatalf("YAML wrote what only travels:\n%s", y)
	}
}

// A reason line names a process by its comm and argv[0], and one keploy
// would not name by why, with its kernel pid and the remedy, never by a name.
func TestProcessText(t *testing.T) {
	for _, tc := range []struct {
		o    *Origin
		want string
	}{
		{nil, ""},
		{&Origin{}, ""},
		{&Origin{Clock: "proxy:run-1"}, ""},
		{&Origin{Comm: "sqs", Exe: "./sqs"}, "`sqs` (`./sqs`)"},
		{&Origin{Comm: "sqs", Exe: "sqs"}, "`sqs`"},
		{&Origin{Exe: "./sqs"}, "`./sqs`"},
		{&Origin{Unnamed: UnnamedPIDNamespace, PID: 42}, "a process keploy could not name (" + PIDNamespaceReason(42) + ")"},
	} {
		if got := tc.o.ProcessText(); got != tc.want {
			t.Errorf("%+v.ProcessText() = %q; want %q", tc.o, got, tc.want)
		}
	}
	for _, want := range []string{"kernel pid 42", "keploy runs in a PID namespace of its own", "--pid=host", "hostPID in Kubernetes", "docker and compose modes"} {
		if !strings.Contains(PIDNamespaceReason(42), want) {
			t.Errorf("PIDNamespaceReason does not say %q", want)
		}
	}
	// A committed line names the process as a log line does, but for one
	// keploy would not name gives why without the kernel pid.
	unnamed := &Origin{Unnamed: UnnamedPIDNamespace, PID: 42}
	if got, want := unnamed.FileProcessText(), "a process keploy could not name ("+strings.TrimPrefix(PIDNamespaceReason(42), "kernel pid 42: ")+")"; got != want || strings.Contains(got, "42") {
		t.Errorf("FileProcessText = %q; want %q", got, want)
	}
	for _, o := range []*Origin{nil, {}, {Comm: "sqs", Exe: "./sqs"}, {Exe: "./sqs"}} {
		if o.FileProcessText() != o.ProcessText() {
			t.Errorf("%+v: FileProcessText %q, ProcessText %q; want one", o, o.FileProcessText(), o.ProcessText())
		}
	}
}
