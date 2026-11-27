package agent

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Parity check: standalone agent derivedBackgroundRecordingsWith uses the exact
// same two-tier split (256 startup + 256 worker, 8 per shape, 64 startup head
// reserve, sparse-first eviction) as the replayer's windowBgTracker.
func TestDerivedBackgroundRecordings_MatchesTwoTierSplitAndProtectsPoller(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	windows := []models.TestWindow{
		{TestCase: "test-1", Start: at(100000), End: at(101000)},
		{TestCase: "test-2", Start: at(200000), End: at(201000)},
	}
	var mocks []*models.Mock
	for i := 0; i < 100; i++ {
		ts := at(1000 + i*10)
		mocks = append(mocks, &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    fmt.Sprintf("boot-%03d", i),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: fmt.Sprintf("http://upstream.internal:8080/boot/%d", i)},
			},
		})
	}
	for i := 0; i < 300; i++ {
		ts := at(110000 + i*100)
		mocks = append(mocks, &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    fmt.Sprintf("gap-%03d", i),
			Kind:    models.HTTP,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"type": "mocks", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
				HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: fmt.Sprintf("http://upstream.internal:8080/gap/%d", i)},
			},
		})
	}
	for i := 0; i < 8; i++ {
		ts := at(150000 + i*1000)
		mocks = append(mocks, &models.Mock{
			Version: "api.keploy.io/v1beta1",
			Name:    fmt.Sprintf("poll-%d", i),
			Kind:    models.Mongo,
			Spec: models.MockSpec{
				Metadata: map[string]string{
					"type":                     "mocks",
					models.MetadataLifetimeKey: models.MetadataLifetimePerTest,
					"destAddr":                 "mongo-cluster-rs1-0:27017",
					"operation":                "findAndModify",
				},
				ReqTimestampMock: ts,
				ResTimestampMock: ts.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{
					Sections: []string{`msg:{"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":1795784500}}}`},
				}}},
			},
		})
	}
	recs, err := derivedBackgroundRecordingsWith(windows, time.Time{}, zap.NewNop(), [][]*models.Mock{mocks})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 356 {
		t.Fatalf("len(recs) = %d, want 356 (100 startup + 256 worker matching replayer)", len(recs))
	}
	names := make([]string, 0, len(recs))
	polls := 0
	for _, r := range recs {
		names = append(names, r.Mock.Name)
		if len(r.Mock.Name) >= 5 && r.Mock.Name[:5] == "poll-" {
			polls++
		}
	}
	sort.Strings(names)
	if polls != 8 {
		t.Fatalf("kept %d of 8 poller samples, want all 8", polls)
	}
}
