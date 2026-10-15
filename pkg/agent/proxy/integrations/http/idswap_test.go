package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/models"
)

const (
	otherKey    = "f9009dbe-feff-45ed-91df-8c97431e5a9f"
	recordedKey = "11f4a513-bee3-45e7-bd44-62be219cdcfd"
	liveKey     = "5edbe3af-8cda-452a-8780-868f15a19ced"
)

func keyMock(name, key string, at time.Time) *models.Mock {
	m := httpMock(name, "GET", "http://bao/v1/secret/data/keys/"+key)
	m.Spec.ReqTimestampMock = at
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"id":"` + key + `"}`, Header: map[string]string{"Location": "/keys/" + key}}
	return m
}

// A key the app made on this run is read under its new id. The run's ids say
// which recorded id it stands for, so the read is answered by the recording of
// that key, not by another key's recording that only looks alike, and the
// answer carries the run's id.
func TestARunsIDIsAnsweredByTheRecordingOfTheIDItStandsFor(t *testing.T) {
	h := newHTTP()
	base := time.Unix(1_700_000_000, 0)
	db := func() *mockMemDb {
		return &mockMemDb{mocks: []*models.Mock{keyMock("other", otherKey, base), keyMock("this", recordedKey, base.Add(time.Second))}, updateUnFilteredReturn: true}
	}
	read := putGet("/v1/secret/data/keys/" + liveKey)

	ok, stub, _, err := h.match(context.Background(), read, db(), nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub.Name != "other" {
		t.Fatalf("without the run's ids the read is answered by whichever recording looks alike first: ok=%v stub=%v err=%v", ok, stub, err)
	}

	ids.Default.Add(recordedKey, liveKey)
	t.Cleanup(ids.Default.Reset)
	ok, stub, _, err = h.match(context.Background(), restoreIDs(read), db(), nil, nil, nil, true, false, false, true, true)
	if err != nil || !ok || stub.Name != "this" {
		t.Fatalf("the read must be answered by the recording of the key it stands for: ok=%v stub=%v err=%v", ok, stub, err)
	}
	served := swapIDs(stub)
	if served.Spec.HTTPResp.Body != `{"id":"`+liveKey+`"}` || served.Spec.HTTPResp.Header["Location"] != "/keys/"+liveKey {
		t.Fatalf("the answer must carry the run's id: %+v", served.Spec.HTTPResp)
	}
	if stub.Spec.HTTPResp.Body != `{"id":"`+recordedKey+`"}` {
		t.Fatalf("the recording itself is left as recorded: %s", stub.Spec.HTTPResp.Body)
	}
}
