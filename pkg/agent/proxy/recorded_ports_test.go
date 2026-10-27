package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestRecordedPortsKnowsOnlyWhatTheRecordingCalled(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Header: map[string]string{"Host": "127.0.0.1:9000"}}}},
		{Kind: models.Postgres, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:5432"}}},
		{Kind: models.DNS},
	})
	for _, p := range []uint32{9000, 5432} {
		if !r.has(p) {
			t.Fatalf("port %d was called in the recording", p)
		}
	}
	if r.has(41234) {
		t.Fatal("a port the recording never called must not count")
	}
}

func TestRecordedPortsTreatsAnUnknownDestinationAsAnyPort(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{{Kind: models.GENERIC}})
	if !r.has(41234) {
		t.Fatal("a mock with no known destination could be for any port")
	}
}

func TestADeadPortIsAChildOnlyWhenEveryRecordingStartedIt(t *testing.T) {
	child := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000", "startedByTests": "true"}}}
	plain := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000"}}}
	var r recordedPorts
	r.add([]*models.Mock{child})
	if !r.child(9000) {
		t.Fatal("a port only test-started dependencies were called on is theirs")
	}
	r.add([]*models.Mock{plain})
	if r.child(9000) {
		t.Fatal("a port another recording called as an outside service must be served from mocks when it is down")
	}
}

func TestAnHTTPHostWithoutAPortUsesTheSchemePort(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{URL: "https://api.example.com/x", Header: map[string]string{"Host": "api.example.com"}}}},
		{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{URL: "http://plain.example.com/x", Header: map[string]string{"Host": "plain.example.com"}}}},
	})
	if !r.has(443) || !r.has(80) || r.has(41234) {
		t.Fatal("a host without a port is called on its scheme's port, and no other")
	}
}

func TestRecordedPortsStartEmptyAfterReset(t *testing.T) {
	var r recordedPorts
	r.add([]*models.Mock{{Kind: models.GENERIC}})
	r.reset()
	if r.has(41234) {
		t.Fatal("a new session must not keep the previous session's ports")
	}
}

// A connection failure says the destination was NOT reached, and it carries no
// metadata.destAddr: counted like any mock without a known port, it would make
// every port "recorded" and switch off the loopback refusal rules for the
// whole test set.
func TestRecordedPortsIgnoreConnectionFailures(t *testing.T) {
	cf := &models.Mock{Kind: models.ConnectionFailure, Spec: models.MockSpec{
		ConnFailure: &models.ConnFailureSpec{Address: "127.0.0.1:5432", Phase: models.ConnFailurePhaseConnect, Outcome: models.ConnFailureRefused},
	}}
	var r recordedPorts
	r.add([]*models.Mock{cf}, []*models.Mock{
		{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "127.0.0.1:9000"}}},
	})
	if !r.has(9000) {
		t.Fatal("port 9000 was called in the recording")
	}
	for _, p := range []uint32{5432, 41234} {
		if r.has(p) {
			t.Fatalf("port %d must not count as recorded: a connection failure is not a call that reached it", p)
		}
	}
}
