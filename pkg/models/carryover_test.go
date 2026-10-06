package models

import (
	"encoding/json"
	"testing"
)

func isSend(m *Mock) bool { return m.Spec.Metadata["commandType"] == "SEND" }

func TestRegisterCarryOver(t *testing.T) {
	if CarryOverRegistered() {
		t.Fatal("precondition: another test left a carry-over registration behind")
	}
	unregister := RegisterCarryOver(brokerKind, isSend)
	t.Cleanup(unregister)
	RegisterCarryOver(brokerKind, nil)() // ignored; its remover is a no-op

	send := brokerMock("mocks", "SEND")
	send.DeriveLifetime()
	flow := brokerMock("mocks", "FLOW")
	flow.DeriveLifetime()
	cfgSend := brokerMock("config", "SEND")
	cfgSend.DeriveLifetime()
	other := &Mock{Kind: "OtherBroker", Spec: MockSpec{Metadata: map[string]string{"type": "mocks", "commandType": "SEND"}}}
	other.DeriveLifetime()

	if !IsCarryOver(send) {
		t.Fatal("a per-test SEND of the registered kind is carry-over")
	}
	if IsCarryOver(flow) {
		t.Fatal("the predicate decides: FLOW is not carry-over")
	}
	if IsCarryOver(cfgSend) {
		t.Fatal("only per-test mocks carry over; a session SEND is reusable already")
	}
	if IsCarryOver(other) || CarryOverKind("OtherBroker") {
		t.Fatal("an unregistered kind is not carry-over")
	}
	if !CarryOverKind(brokerKind) || !CarryOverRegistered() {
		t.Fatal("the registration is not visible")
	}
	if IsCarryOver(nil) {
		t.Fatal("nil is not carry-over")
	}

	unregister()
	if CarryOverRegistered() || CarryOverKind(brokerKind) || IsCarryOver(send) {
		t.Fatal("unregister did not remove the registration")
	}
}

// Two registrations for one kind are OR-ed, and removing one keeps the other.
func TestRegisterCarryOverComposes(t *testing.T) {
	a := RegisterCarryOver(brokerKind, isSend)
	b := RegisterCarryOver(brokerKind, func(m *Mock) bool { return m.Spec.Metadata["commandType"] == "ACK" })
	defer b()
	ack := brokerMock("mocks", "ACK")
	ack.DeriveLifetime()
	send := brokerMock("mocks", "SEND")
	send.DeriveLifetime()
	if !IsCarryOver(ack) || !IsCarryOver(send) {
		t.Fatal("registrations for one kind must be OR-ed")
	}
	a()
	if IsCarryOver(send) || !IsCarryOver(ack) {
		t.Fatal("removing one registration must keep the other")
	}
}

// The flag crosses the agent → CLI boundary as JSON (GetConsumedMocks).
func TestMockStateCarryOverRoundTrips(t *testing.T) {
	b, err := json.Marshal(MockState{Name: "mock-7552", CarryOver: true})
	if err != nil {
		t.Fatal(err)
	}
	var back MockState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.CarryOver {
		t.Fatalf("CarryOver lost in %s", b)
	}
	b, _ = json.Marshal(MockState{Name: "mock-1"})
	if string(b) != `{"name":"mock-1","kind":"","usage":"","isFiltered":false,"sortOrder":0,"type":"","timestamp":0}` {
		t.Fatalf("an ordinary state's wire form changed: %s", b)
	}
}
