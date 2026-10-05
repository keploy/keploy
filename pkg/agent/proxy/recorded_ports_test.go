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
