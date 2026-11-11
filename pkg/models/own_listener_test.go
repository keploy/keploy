package models

import (
	"strings"
	"testing"
)

func TestAnOwnListenerCallIsOutsideTestScopesAndKeptByACopy(t *testing.T) {
	m := &Mock{Name: "m", Kind: HTTP}
	if m.OwnListenerCall() || m.OutsideTestScopes() {
		t.Fatal("an unmarked HTTP mock reads as a self-call or outside test scopes")
	}
	m.MarkOwnListenerCall()
	if !m.OwnListenerCall() || !m.OutsideTestScopes() {
		t.Fatalf("marked: OwnListenerCall %v, OutsideTestScopes %v; want both true", m.OwnListenerCall(), m.OutsideTestScopes())
	}
	if !m.DeepCopy().OwnListenerCall() {
		t.Fatal("a copy lost the mark")
	}
	var none *Mock
	none.MarkOwnListenerCall()
	if none.OwnListenerCall() {
		t.Fatal("a nil mock reads as a self-call")
	}
}

func TestAPayloadLeftOnDiskKeepsTheHostItWasRecordedWith(t *testing.T) {
	m := &Mock{Name: "m", Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Header: map[string]string{"Host": "localhost:6100"}}}}
	m.LeavePayloadOnDisk(nil, PayloadAt{Name: "m"})
	if got, ok := m.RecordedDestination(); !ok || got != "localhost:6100" {
		t.Fatalf("RecordedDestination = %q, %v; want localhost:6100, true", got, ok)
	}
	// One that names none, on disk, names none.
	bare := &Mock{Name: "b", Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{}}}
	bare.LeavePayloadOnDisk(nil, PayloadAt{Name: "b"})
	if got, ok := bare.RecordedDestination(); ok || got != "" {
		t.Fatalf("RecordedDestination = %q, %v; want none", got, ok)
	}
	// A Host too long to keep is not kept.
	long := &Mock{Name: "l", Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Header: map[string]string{"Host": strings.Repeat("h", maxKeptDisplay+1)}}}}
	long.LeavePayloadOnDisk(nil, PayloadAt{Name: "l"})
	if _, ok := long.RecordedDestination(); ok {
		t.Fatal("a Host longer than maxKeptDisplay was kept")
	}
}
