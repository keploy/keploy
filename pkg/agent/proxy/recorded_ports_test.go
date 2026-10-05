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
