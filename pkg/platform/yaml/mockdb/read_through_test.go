package mockdb

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestPruneKeeps_RetainsReadThroughMergesNoiseAndDropsOrdinaryUnused(t *testing.T) {
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	rt := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-rt-config",
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			MongoRequests: []models.MongoRequest{
				{
					Header: &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{
						Sections: []string{`{ SectionSingle msg: {"find":"service_configs","sort":{"version":1},"projection":{"owner.id":1},"$db":"appdb"} }`},
					},
				},
			},
			MongoResponses: []models.MongoResponse{
				{
					Header:  &models.MongoHeader{Opcode: 2013},
					Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":1} }`}},
				},
			},
			ReqTimestampMock: t0,
			ResTimestampMock: t0.Add(time.Millisecond),
		},
	}
	if !models.MarkReadThrough(rt) {
		t.Fatal("MarkReadThrough failed for Mongo find")
	}

	unusedOrdinary := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-unused-1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "http://example.com/a"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200},
			ReqTimestampMock: t0,
			ResTimestampMock: t0.Add(time.Millisecond),
		},
	}

	fakeRTWrite := &models.Mock{
		Version: models.Version("api.keploy.io/v1beta2"),
		Name:    "mock-rt-write",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"type":                        "mocks",
				models.MetadataReadThroughKey: models.MetadataReadThroughValue,
			},
			HTTPReq:          &models.HTTPReq{Method: models.Method("POST"), URL: "http://example.com/mutate"},
			HTTPResp:         &models.HTTPResp{StatusCode: 200},
			ReqTimestampMock: t0,
			ResTimestampMock: t0.Add(time.Millisecond),
		},
	}

	mockNames := map[string]models.MockState{}
	pruneBefore := t0.Add(time.Minute)
	startupCutoff := t0.Add(-time.Minute)

	if !pruneKeeps(rt, mockNames, pruneBefore, startupCutoff) {
		t.Fatal("pruneKeeps must retain valid read-through mocks even when unused")
	}
	if pruneKeeps(unusedOrdinary, mockNames, pruneBefore, startupCutoff) {
		t.Fatal("pruneKeeps must drop unused ordinary per-test mocks")
	}
	if pruneKeeps(fakeRTWrite, mockNames, pruneBefore, startupCutoff) {
		t.Fatal("pruneKeeps must NOT treat a hand-tagged POST write mock as read-through")
	}

	// Consumed read-through mock must STILL merge learned noise before returning true.
	rtConsumed := rt.DeepCopy()
	mockNames[rtConsumed.Name] = models.MockState{
		Name:         rtConsumed.Name,
		Kind:         models.Mongo,
		Usage:        models.Updated,
		ReadThrough:  true,
		ReqBodyNoise: map[string][]string{"comment": {"volatile"}},
	}
	if !pruneKeeps(rtConsumed, mockNames, pruneBefore, startupCutoff) {
		t.Fatal("pruneKeeps must keep consumed read-through mock")
	}
	if len(rtConsumed.Spec.ReqBodyNoise["comment"]) == 0 {
		t.Fatal("pruneKeeps must merge learned ReqBodyNoise on consumed read-through mocks")
	}
}
