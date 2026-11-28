package record

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

type memWorkerMappingDB struct {
	worker []models.MockEntry
}

func (m *memWorkerMappingDB) Insert(_ context.Context, _ *models.Mapping) error {
	return nil
}
func (m *memWorkerMappingDB) Upsert(_ context.Context, _, _ string, _ []models.MockEntry) error {
	return nil
}
func (m *memWorkerMappingDB) UpsertBatch(_ context.Context, _ string, _ map[string][]models.MockEntry) error {
	return nil
}
func (m *memWorkerMappingDB) UpsertWorker(_ context.Context, _ string, _ []models.MockEntry) error {
	return nil
}
func (m *memWorkerMappingDB) Exists(_ context.Context, _ string) (bool, error) { return true, nil }
func (m *memWorkerMappingDB) AddToWorker(_ context.Context, _ string, entries []models.MockEntry) ([]string, error) {
	var added []string
	for _, e := range entries {
		m.worker = append(m.worker, e)
		added = append(added, e.Name)
	}
	return added, nil
}

func TestNewWorkerMarksDisabledUnderProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	db := &memWorkerMappingDB{}
	if got := newWorkerMarks([]models.WorkerMockRule{{
		ID:    "ptw-reconcile-poll",
		Kind:  "Mongo",
		Match: map[string]string{"db": "api-server", "command": "findAndModify", "collection": "ptw_endpoints"},
	}}, db, zap.NewNop()); got != nil {
		t.Fatal("expected nil workerMarks under KEPLOY_PROVEN=1")
	}
}

// TestWorkerMarksMongoClockPollsFeedEpochTrack verifies that Mongo worker-mock
// rules for ptw_endpoints and loadTestRun mark distinct-second no-op polls into
// worker: so learn.EpochTrack learns their clock offsets, while excluding real
// claimed-endpoint responses and unrelated request-path collections.
func TestWorkerMarksMongoClockPollsFeedEpochTrack(t *testing.T) {
	if provenswitch.On() {
		t.Skip("workerMarks is disabled under KEPLOY_PROVEN=1; windowMappings owns worker scope")
	}

	rules := []models.WorkerMockRule{
		{
			ID:   "ptw-reconcile-poll",
			Kind: "Mongo",
			Match: map[string]string{
				"db":         "api-server",
				"command":    "findAndModify",
				"collection": "ptw_endpoints",
				"response":   `^\{ SectionSingle msg: \{"lastErrorObject":\{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
			},
		},
		{
			ID:   "load-stale-scan",
			Kind: "Mongo",
			Match: map[string]string{
				"db":         "api-server",
				"command":    "update",
				"collection": "loadTestRun",
				"response":   `^\{ SectionSingle msg: \{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
			},
		},
	}
	if probs := models.ValidateWorkerMockRules(rules); len(probs) > 0 {
		t.Fatalf("ValidateWorkerMockRules: %v", probs)
	}

	db := &memWorkerMappingDB{}
	wm := newWorkerMarks(rules, db, zap.NewNop())
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	var ptwReads []learn.EpochRead
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i*2) * time.Second)
		cutoff := at.Unix() - 2
		m := &models.Mock{
			Name: fmt.Sprintf("mock-ptw-%d", i),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{
					Message: &models.MongoOpMessage{Sections: []string{
						fmt.Sprintf(`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"%d"}}},"lsid":{"id":"s-%d"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":%d,"i":1}}},"$db":"api-server"} }`, cutoff, i, at.Unix()),
					}},
				}},
				MongoResponses: []models.MongoResponse{{
					Message: &models.MongoOpMessage{Sections: []string{
						`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"},"updatedExisting":false},"value":null,"ok":{"$numberDouble":"1.0"}} }`,
					}},
				}},
			},
		}
		if wm.add(m) != nil {
			ptwReads = append(ptwReads, learn.EpochRead{
				Value: fmt.Sprintf("%d", cutoff),
				Span:  learn.Span{From: at.Add(-100 * time.Millisecond), To: at},
			})
		}
	}

	// Duplicate static copy of the last ptw poll differing only in lsid/$clusterTime
	// MUST dedup to 1 entry through WorkerMockCallKey normalization.
	dupAt := base.Add(6 * time.Second)
	dupPtw := &models.Mock{
		Name: "mock-ptw-dup-static",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: dupAt,
			ResTimestampMock: dupAt.Add(time.Millisecond),
			MongoRequests: []models.MongoRequest{{
				Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"dirtyAt":{"$lte":{"$numberLong":"%d"}}},"lsid":{"id":"s-other"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":%d,"i":9}}},"$db":"api-server"} }`, dupAt.Unix()-2, dupAt.Unix()+99),
				}},
			}},
			MongoResponses: []models.MongoResponse{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"},"updatedExisting":false},"value":null,"ok":{"$numberDouble":"1.0"}} }`,
				}},
			}},
		},
	}
	if wm.add(dupPtw) != nil {
		t.Fatal("duplicate static ptw poll (differing only in lsid/$clusterTime) was marked twice instead of deduping")
	}

	// Canonical Extended JSON load-stale-scan polls (3 distinct-second no-ops + 1 real mutation).
	var loadReads []learn.EpochRead
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i*10) * time.Second)
		cutoff := at.Unix() - 15
		m := &models.Mock{
			Name: fmt.Sprintf("mock-load-%d", i),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{
					Message: &models.MongoOpMessage{Sections: []string{
						fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"last_active":{"$lt":{"$numberLong":"%d"}}}} ] }`, cutoff),
						`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"$db":"api-server"} }`,
					}},
				}},
				MongoResponses: []models.MongoResponse{{
					Message: &models.MongoOpMessage{Sections: []string{
						`{ SectionSingle msg: {"n":{"$numberInt":"0"},"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`,
					}},
				}},
			},
		}
		if wm.add(m) != nil {
			loadReads = append(loadReads, learn.EpochRead{
				Value: fmt.Sprintf("%d", cutoff),
				Span:  learn.Span{From: at.Add(-100 * time.Millisecond), To: at},
			})
		}
	}
	loadModified := &models.Mock{
		Name: "mock-load-modified",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: base.Add(25 * time.Second),
			ResTimestampMock: base.Add(25*time.Second + time.Millisecond),
			MongoRequests: []models.MongoRequest{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"$db":"api-server"} }`,
				}},
			}},
			MongoResponses: []models.MongoResponse{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"n":{"$numberInt":"1"},"nModified":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`,
				}},
			}},
		},
	}
	if wm.add(loadModified) != nil {
		t.Fatal("loadTestRun update with nModified=1 was marked as worker mock")
	}

	// Real claimed endpoint (non-null value) and unrelated collection MUST NOT be marked.
	claimed := &models.Mock{
		Name: "mock-ptw-real-claim",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: base.Add(10 * time.Second),
			ResTimestampMock: base.Add(10*time.Second + time.Millisecond),
			MongoRequests: []models.MongoRequest{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{},"$db":"api-server"} }`,
				}},
			}},
			MongoResponses: []models.MongoResponse{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"1"},"updatedExisting":true},"value":{"_id":"ep-1","nested":{"value":null}},"ok":{"$numberDouble":"1.0"}} }`,
				}},
			}},
		},
	}
	if wm.add(claimed) != nil {
		t.Fatal("real ptw_endpoints claim with non-null value was marked as worker mock")
	}

	oauth := &models.Mock{
		Name: "mock-oauth-insert",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"destAddr": "mongo:27017"},
			ReqTimestampMock: base.Add(11 * time.Second),
			ResTimestampMock: base.Add(11*time.Second + time.Millisecond),
			MongoRequests: []models.MongoRequest{{
				Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"insert":"oauth_authorization_requests","$db":"api-server"} }`,
				}},
			}},
			MongoResponses: []models.MongoResponse{{
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"n":1,"ok":1} }`}},
			}},
		},
	}
	if wm.add(oauth) != nil {
		t.Fatal("unrelated oauth_authorization_requests insert was marked as worker mock")
	}

	wm.flush(context.Background(), db, "ts-1", true)
	if len(db.worker) != 6 {
		t.Fatalf("worker entries = %d, want 6 (3 ptw_endpoints + 3 loadTestRun, capped at learn.MinEpochReads)", len(db.worker))
	}
	ptwCount, loadCount := 0, 0
	for _, e := range db.worker {
		switch e.Reason {
		case models.WorkerRuleReason("ptw-reconcile-poll"):
			ptwCount++
		case models.WorkerRuleReason("load-stale-scan"):
			loadCount++
		default:
			t.Fatalf("unexpected worker entry reason %q for %s", e.Reason, e.Name)
		}
	}
	if ptwCount != learn.MinEpochReads || loadCount != learn.MinEpochReads {
		t.Fatalf("worker reason counts ptw=%d load=%d, want %d and %d", ptwCount, loadCount, learn.MinEpochReads, learn.MinEpochReads)
	}
	off, ok := learn.EpochTrack(ptwReads)
	if !ok {
		t.Fatalf("learn.EpochTrack failed on %d marked ptw_endpoints polls", len(ptwReads))
	}
	liveT := base.Add(30 * time.Second)
	if !off.HoldsIn(fmt.Sprintf("%d", liveT.Unix()-2), []learn.Span{{From: liveT.Add(-100 * time.Millisecond), To: liveT}}) {
		t.Fatalf("learned EpochOffset %+v did not hold on live replay poll", off)
	}
	loadOff, ok := learn.EpochTrack(loadReads)
	if !ok {
		t.Fatalf("learn.EpochTrack failed on %d marked loadTestRun polls", len(loadReads))
	}
	if !loadOff.HoldsIn(fmt.Sprintf("%d", liveT.Unix()-15), []learn.Span{{From: liveT.Add(-100 * time.Millisecond), To: liveT}}) {
		t.Fatalf("learned load EpochOffset %+v did not hold on live replay poll", loadOff)
	}
}

// TestWorkerMarksMongoClockPolls2HourSimulationStaysUnderCap simulates 2 full
// hours of production api-server background polling (ptw-reconcile-poll every
// 2 s = 3,600 polls + load-stale-scan every 10 s = 720 polls, each carrying
// advancing time.Now().Unix(), txnNumber, lsid, and $clusterTime) and verifies
// worker marks stay at 6 total entries (far below maxWorkerMarkCalls = 10000),
// both polls still teach learn.EpochTrack, and later rules are never starved.
func TestWorkerMarksMongoClockPolls2HourSimulationStaysUnderCap(t *testing.T) {
	if provenswitch.On() {
		t.Skip("workerMarks is disabled under KEPLOY_PROVEN=1; windowMappings owns worker scope")
	}
	rules := []models.WorkerMockRule{
		{
			ID:   "ptw-reconcile-poll",
			Kind: "Mongo",
			Match: map[string]string{
				"db":         "api-server",
				"command":    "findAndModify",
				"collection": "ptw_endpoints",
				"response":   `^\{ SectionSingle msg: \{"lastErrorObject":\{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
			},
		},
		{
			ID:   "load-stale-scan",
			Kind: "Mongo",
			Match: map[string]string{
				"db":         "api-server",
				"command":    "update",
				"collection": "loadTestRun",
				"response":   `^\{ SectionSingle msg: \{"n":\{"\$number(?:Int|Long)":"0"\}[,}]`,
			},
		},
		{
			ID:   "jwks-fetch",
			Kind: "Http",
			Match: map[string]string{
				"method": "GET",
				"host":   "auth.internal",
				"path":   "/.well-known/jwks.json",
			},
		},
	}
	if probs := models.ValidateWorkerMockRules(rules); len(probs) > 0 {
		t.Fatalf("ValidateWorkerMockRules: %v", probs)
	}

	db := &memWorkerMappingDB{}
	wm := newWorkerMarks(rules, db, zap.NewNop())
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	var ptwReads, loadReads []learn.EpochRead
	// 2 hours = 7,200 s: ptw every 2 s (3,600 polls), load every 10 s (720 polls).
	for sec := 0; sec < 7200; sec += 2 {
		at := base.Add(time.Duration(sec) * time.Second)
		cutoff := at.Unix() - 2
		ptw := &models.Mock{
			Name: fmt.Sprintf("ptw-%d", sec),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo-cluster-rs0-0:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{
					Message: &models.MongoOpMessage{Sections: []string{
						fmt.Sprintf(`{ SectionSingle msg: {"findAndModify":"ptw_endpoints","query":{"$and":[{"dirtyAt":{"$lte":{"$numberLong":"%d"}}},{"status":"pending"}]},"update":{"$set":{"claimedAt":{"$numberLong":"%d"}}},"lsid":{"id":"s-%d"},"txnNumber":{"$numberLong":"%d"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":%d,"i":1}}},"$db":"api-server"} }`, cutoff, at.Unix(), sec, sec/2+1, at.Unix()),
					}},
				}},
				MongoResponses: []models.MongoResponse{{
					Message: &models.MongoOpMessage{Sections: []string{
						`{ SectionSingle msg: {"lastErrorObject":{"n":{"$numberInt":"0"},"updatedExisting":false},"value":null,"ok":{"$numberDouble":"1.0"}} }`,
					}},
				}},
			},
		}
		if wm.add(ptw) != nil {
			ptwReads = append(ptwReads, learn.EpochRead{
				Value: fmt.Sprintf("%d", cutoff),
				Span:  learn.Span{From: at.Add(-100 * time.Millisecond), To: at},
			})
		}
		if sec%10 == 0 {
			loadCutoff := at.Unix() - 15
			load := &models.Mock{
				Name: fmt.Sprintf("load-%d", sec),
				Kind: models.Mongo,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"destAddr": "mongo-cluster-rs0-0:27017"},
					ReqTimestampMock: at,
					ResTimestampMock: at.Add(time.Millisecond),
					MongoRequests: []models.MongoRequest{{
						Message: &models.MongoOpMessage{Sections: []string{
							fmt.Sprintf(`{ SectionSingle identifier: updates , msgs: [ {"q":{"$or":[{"last_active":{"$lt":{"$numberLong":"%d"}}},{"status":"running"}]}} ] }`, loadCutoff),
							fmt.Sprintf(`{ SectionSingle msg: {"update":"loadTestRun","ordered":true,"lsid":{"id":"l-%d"},"txnNumber":{"$numberLong":"%d"},"$clusterTime":{"clusterTime":{"$timestamp":{"t":%d,"i":1}}},"$db":"api-server"} }`, sec, sec/10+1, at.Unix()),
						}},
					}},
					MongoResponses: []models.MongoResponse{{
						Message: &models.MongoOpMessage{Sections: []string{
							`{ SectionSingle msg: {"n":{"$numberInt":"0"},"nModified":{"$numberInt":"0"},"ok":{"$numberDouble":"1.0"}} }`,
						}},
					}},
				},
			}
			if wm.add(load) != nil {
				loadReads = append(loadReads, learn.EpochRead{
					Value: fmt.Sprintf("%d", loadCutoff),
					Span:  learn.Span{From: at.Add(-100 * time.Millisecond), To: at},
				})
			}
		}
	}

	// A later HTTP rule MUST still be marked (cap nowhere near exhausted).
	jwks := &models.Mock{
		Name: "jwks-after-2h",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"destAddr": "auth.internal:443"},
			ReqTimestampMock: base.Add(2 * time.Hour),
			ResTimestampMock: base.Add(2*time.Hour + time.Millisecond),
			HTTPReq:          &models.HTTPReq{Method: models.Method("GET"), URL: "https://auth.internal/.well-known/jwks.json"},
		},
	}
	if wm.add(jwks) == nil {
		t.Fatal("HTTP rule after 2-hour Mongo poll simulation was starved")
	}

	wm.flush(context.Background(), db, "ts-2h", true)
	if wm.overflowed {
		t.Fatalf("workerMarks overflowed during 2-hour poll simulation")
	}
	wantTotal := 2*learn.MinEpochReads + 1
	if len(db.worker) != wantTotal {
		t.Fatalf("2-hour poll simulation produced %d worker entries (of %d cap), want %d", len(db.worker), maxWorkerMarkCalls, wantTotal)
	}
	if _, ok := learn.EpochTrack(ptwReads); !ok {
		t.Fatalf("learn.EpochTrack failed for ptw_endpoints after 2h simulation (%d reads)", len(ptwReads))
	}
	if _, ok := learn.EpochTrack(loadReads); !ok {
		t.Fatalf("learn.EpochTrack failed for loadTestRun after 2h simulation (%d reads)", len(loadReads))
	}
}

func TestWorkerMarksDistinctSnowflakeLookupsAreEachMarked(t *testing.T) {
	if provenswitch.On() {
		t.Skip("workerMarks is disabled under KEPLOY_PROVEN=1; windowMappings owns worker scope")
	}
	rules := []models.WorkerMockRule{{
		ID:    "users-read",
		Kind:  "Mongo",
		Match: map[string]string{"db": "app", "command": "find", "collection": "users"},
	}}
	db := &memWorkerMappingDB{}
	wm := newWorkerMarks(rules, db, zap.NewNop())
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		snowflake := int64(1864123456789012345) + int64(i)
		m := &models.Mock{
			Name: fmt.Sprintf("user-lookup-%d", i),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"find":"users","filter":{"_id":{"$numberLong":"%d"}},"$db":"app"} }`, snowflake),
				}}}},
				MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":{"$numberLong":"%d"}}]},"ok":1} }`, snowflake),
				}}}},
			},
		}
		if wm.add(m) == nil {
			t.Fatalf("distinct snowflake user lookup %d (%d) was silently collapsed instead of marked", i, snowflake)
		}
	}
	wm.flush(context.Background(), db, "ts-snow", true)
	if len(db.worker) != 10 {
		t.Fatalf("snowflake user lookups marked = %d, want 10", len(db.worker))
	}
}

func TestWorkerMarksTimePollsSameSecondAndAdoptRollover(t *testing.T) {
	if provenswitch.On() {
		t.Skip("workerMarks is disabled under KEPLOY_PROVEN=1; windowMappings owns worker scope")
	}
	rules := []models.WorkerMockRule{{
		ID:         "retention-sweep",
		Kind:       "Mongo",
		Match:      map[string]string{"db": "app", "command": "find", "collection": "jobs"},
		TimeFields: []string{"cutoff"},
	}}
	db := &memWorkerMappingDB{}
	wm := newWorkerMarks(rules, db, zap.NewNop())
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	mkPoll := func(name string, at time.Time, cutoff int64) *models.Mock {
		return &models.Mock{
			Name: name,
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"cutoff":{"$lt":{"$numberLong":"%d"}}},"$db":"app"} }`, cutoff),
				}}}},
				MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"cursor":{"firstBatch":[]},"ok":1} }`,
				}}}},
			},
		}
	}

	// Same-second burst: 3 polls within the same second count as 1 sample; the
	// 4th poll in the next second MUST still be marked.
	cutoffOld := base.AddDate(-2, 0, 0).Unix()
	c1 := wm.add(mkPoll("s0-a", base, cutoffOld))
	if c1 == nil {
		t.Fatal("first poll in second 0 was not marked")
	}
	if wm.add(mkPoll("s0-b", base.Add(100*time.Millisecond), cutoffOld+1)) != nil {
		t.Fatal("second poll in the same second should not consume a second sample")
	}
	if wm.add(mkPoll("s0-c", base.Add(200*time.Millisecond), cutoffOld+2)) != nil {
		t.Fatal("third poll in the same second should not consume a third sample")
	}
	c2 := wm.add(mkPoll("s1", base.Add(time.Second), cutoffOld+3))
	if c2 == nil {
		t.Fatal("poll in second 1 was dropped because same-second burst exhausted samples")
	}
	c3 := wm.add(mkPoll("s2", base.Add(2*time.Second), cutoffOld+4))
	if c3 == nil {
		t.Fatal("poll in second 2 was not marked (want learn.MinEpochReads distinct-second samples)")
	}
	if wm.add(mkPoll("s3", base.Add(3*time.Second), cutoffOld+5)) != nil {
		t.Fatal("poll in second 3 exceeded learn.MinEpochReads cap")
	}

	// Rollover adopt: carrying time-tagged calls preserves hasTime and sec so
	// the new set retains all 3 distinct-second samples (and does not truncate
	// to 1) while skipping duplicates on already-carried seconds.
	dbRoll := &memWorkerMappingDB{}
	wmRoll := newWorkerMarks(rules, dbRoll, zap.NewNop())
	wmRoll.adopt([]markedCall{*c1, *c2})
	if wmRoll.add(mkPoll("roll-dup-s1", base.Add(time.Second+50*time.Millisecond), cutoffOld+10)) != nil {
		t.Fatal("adopted set re-marked an already-carried second")
	}
	if wmRoll.add(mkPoll("roll-s2", base.Add(2*time.Second), cutoffOld+11)) == nil {
		t.Fatal("adopted set failed to mark 3rd distinct-second sample after adopting 2")
	}
	if wmRoll.add(mkPoll("roll-s3", base.Add(3*time.Second), cutoffOld+12)) != nil {
		t.Fatal("adopted set marked a 4th sample past learn.MinEpochReads")
	}
	wmRoll.flush(context.Background(), dbRoll, "ts-roll", true)
	if len(dbRoll.worker) != learn.MinEpochReads {
		t.Fatalf("adopted set worker count = %d, want %d", len(dbRoll.worker), learn.MinEpochReads)
	}
}

// TestMongoWorkerRuleRecordToMappingsFileRoundTrip exercises the full OSS
// persistence path for Mongo worker-mock rules: Recorder.consumeMappingsFor
// writes mappings.yaml through mapdb, flushes both a static Mongo find rule
// and a dotted-key timeFields rule into worker: with worker-rule:<id> reasons,
// and reloads the file via Mapping.WorkerRuleMockNames (the exact list replay
// serves to every cold worker).
func TestMongoWorkerRuleRecordToMappingsFileRoundTrip(t *testing.T) {
	if provenswitch.On() {
		t.Skip("workerMarks is disabled under KEPLOY_PROVEN=1; windowMappings owns worker scope")
	}
	rules := []models.WorkerMockRule{
		{
			ID:    "config-read",
			Kind:  "Mongo",
			Match: map[string]string{"db": "app", "command": "find", "collection": "config"},
		},
		{
			ID:         "retention-sweep",
			Kind:       "Mongo",
			Match:      map[string]string{"db": "app", "command": "find", "collection": "jobs"},
			TimeFields: []string{"cutoff"},
		},
	}
	if probs := models.ValidateWorkerMockRules(rules); len(probs) > 0 {
		t.Fatalf("ValidateWorkerMockRules: %v", probs)
	}

	dir := t.TempDir()
	db := mapdb.New(zap.NewNop(), dir, "mappings")
	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}
	marks := newWorkerMarks(rules, db, zap.NewNop())
	base := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	// Two workers make the same config read; only the first is marked.
	for i := 0; i < 2; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		marks.add(&models.Mock{
			Name: fmt.Sprintf("mock-cfg-%d", i),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"find":"config","filter":{"_id":"app_settings"},"lsid":{"id":"w-%d"},"$db":"app"} }`, i),
				}}}},
				MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"cursor":{"firstBatch":[{"_id":"app_settings","value":"v1"}]},"ok":1} }`,
				}}}},
			},
		})
	}
	// Dotted-key distant-cutoff poll (> 400 days old) across 4 distinct seconds:
	// marks learn.MinEpochReads (3) samples and drops the 4th.
	old := base.AddDate(-2, 0, 0).Unix()
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i*2) * time.Second)
		marks.add(&models.Mock{
			Name: fmt.Sprintf("mock-sweep-%d", i),
			Kind: models.Mongo,
			Spec: models.MockSpec{
				Metadata:         map[string]string{"destAddr": "mongo:27017"},
				ReqTimestampMock: at,
				ResTimestampMock: at.Add(time.Millisecond),
				MongoRequests: []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{
					fmt.Sprintf(`{ SectionSingle msg: {"find":"jobs","filter":{"run.cutoff":{"$lt":{"$numberLong":"%d"}}},"$db":"app"} }`, old+int64(i)),
				}}}},
				MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{
					`{ SectionSingle msg: {"cursor":{"firstBatch":[]},"ok":1} }`,
				}}}},
			},
		})
	}

	var correlationMap, asyncMockIDs, shortPool sync.Map
	var dropped droppedMockSet
	correlationMap.Store("m-cfg-0", models.MockEntry{Name: "mock-cfg-0", Kind: "Mongo"})
	correlationMap.Store("m-user-1", models.MockEntry{Name: "mock-user-1", Kind: "Mongo"})
	mappingsCh := make(chan models.TestMockMapping, 1)
	mappingsCh <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m-cfg-0", "m-user-1"}}
	close(mappingsCh)

	if err := r.consumeMappingsFor(context.Background(), "set-0", mappingsCh, &correlationMap, &asyncMockIDs, &dropped, nil, nil, &shortPool, nil, marks, nil); err != nil {
		t.Fatalf("consumeMappingsFor: %v", err)
	}

	// Re-open mapdb from disk to verify persisted mappings.yaml round-trips cleanly.
	reloaded, err := mapdb.New(zap.NewNop(), dir, "mappings").GetMapping(context.Background(), "set-0")
	if err != nil || reloaded == nil {
		t.Fatalf("GetMapping after consumeMappingsFor: (%v, %v)", reloaded, err)
	}
	gotNames := reloaded.WorkerRuleMockNames()
	wantNames := []string{"mock-cfg-0", "mock-sweep-0", "mock-sweep-1", "mock-sweep-2"}
	if len(gotNames) != len(wantNames) {
		t.Fatalf("WorkerRuleMockNames = %v, want %v", gotNames, wantNames)
	}
	for i := range wantNames {
		if gotNames[i] != wantNames[i] {
			t.Fatalf("WorkerRuleMockNames[%d] = %q, want %q (all=%v)", i, gotNames[i], wantNames[i], gotNames)
		}
	}
	if len(reloaded.Worker) != len(wantNames) {
		t.Fatalf("reloaded.Worker len = %d, want %d", len(reloaded.Worker), len(wantNames))
	}
	if reloaded.Worker[0].Reason != models.WorkerRuleReason("config-read") {
		t.Fatalf("config entry reason = %q, want %q", reloaded.Worker[0].Reason, models.WorkerRuleReason("config-read"))
	}
	for i := 1; i < len(reloaded.Worker); i++ {
		if reloaded.Worker[i].Reason != models.WorkerRuleReason("retention-sweep") {
			t.Fatalf("sweep entry %d reason = %q, want %q", i, reloaded.Worker[i].Reason, models.WorkerRuleReason("retention-sweep"))
		}
	}
}
