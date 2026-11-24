package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// backgroundProxy is pacingProxy that also takes the set's background
// recordings and declared background rules.
type backgroundProxy struct {
	pacingProxy
	bgMu      sync.Mutex
	bgSeen    [][]models.BackgroundRecording
	mongoSeen [][]models.BackgroundMongoRule
	httpSeen  [][]models.BackgroundHTTPRule
}

func (p *backgroundProxy) SeedBackgroundRecordings(recs []models.BackgroundRecording) {
	p.bgMu.Lock()
	defer p.bgMu.Unlock()
	p.bgSeen = append(p.bgSeen, recs)
}

func (p *backgroundProxy) SeedBackgroundMongoRules(rules []models.BackgroundMongoRule) {
	p.bgMu.Lock()
	defer p.bgMu.Unlock()
	p.mongoSeen = append(p.mongoSeen, rules)
}

func (p *backgroundProxy) SeedBackgroundHTTPRules(rules []models.BackgroundHTTPRule) {
	p.bgMu.Lock()
	defer p.bgMu.Unlock()
	p.httpSeen = append(p.httpSeen, rules)
}

var bgT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// bgMock is a Mongo recording named name, sent at sec and answered at res
// seconds (res < 0: not answered).
func bgMock(name string, kind models.Kind, sec, res int) *models.Mock {
	m := &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{ReqTimestampMock: bgT0.Add(time.Duration(sec) * time.Second), Metadata: map[string]string{}}}
	if res >= 0 {
		m.Spec.ResTimestampMock = bgT0.Add(time.Duration(res) * time.Second)
	}
	return m
}

// bgFroms is each recording's name and recorded span start, in seconds from
// bgT0 (-1 for none).
func bgFroms(recs []models.BackgroundRecording) map[string]int {
	out := map[string]int{}
	for _, r := range recs {
		out[r.Mock.Name] = -1
		if !r.From.IsZero() {
			out[r.Mock.Name] = int(r.From.Sub(bgT0) / time.Second)
		}
	}
	return out
}

// The staging call hands the proxy the set's background recordings it names,
// resident or on disk, served or not, each with its recorded span's start:
// the set's data-plane recording sent before it, not a DNS lookup, not one
// answered after it was sent. A per-test call hands none, and the next
// staging call that names none clears them.
func TestTheStagingCallHandsTheProxyTheSetsBackgroundRecordings(t *testing.T) {
	disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	for _, m := range []*models.Mock{
		bgMock("own-1", models.Mongo, 3, 4),
		bgMock("poll-2", models.Mongo, 6, 6),
		bgMock("long-1", models.HTTP, 7, 20),
	} {
		if err := disk.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	disk.Finalize()
	resident := []*models.Mock{
		bgMock("poll-1", models.Mongo, 2, 2),
		bgMock("dns-1", models.DNS, 5, 5),
		bgMock("poll-3", models.Mongo, 8, 8),
	}
	p := &backgroundProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{unfiltered: resident, diskMocks: disk})

	staging := models.MockFilterParams{AfterTime: models.BaseTime, BeforeTime: time.Now(), FirstRecordedTestStart: bgT0.Add(3 * time.Second),
		BackgroundMocks: []string{"poll-1", "poll-2", "poll-3", "gone"}}
	if err := a.UpdateMockParams(context.Background(), staging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 1 {
		t.Fatalf("the staging call seeded %d times; want once", len(p.bgSeen))
	}
	// poll-1 is the set's first call; poll-2's previous call is own-1 (the
	// DNS lookup at 5 is no data-plane call); poll-3's is poll-2, as long-1,
	// sent at 7, was answered after poll-3 was sent.
	if got, want := bgFroms(p.bgSeen[0]), map[string]int{"poll-1": -1, "poll-2": 3, "poll-3": 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the background recordings and their spans' starts are %v; want %v", got, want)
	}

	perTest := models.MockFilterParams{AfterTime: bgT0.Add(3 * time.Second), BeforeTime: bgT0.Add(4 * time.Second), BackgroundMocks: []string{"poll-1"}}
	if err := a.UpdateMockParams(context.Background(), perTest); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 1 {
		t.Fatalf("a per-test call seeded background recordings: %d seeds", len(p.bgSeen))
	}

	staging.BackgroundMocks = nil
	if err := a.UpdateMockParams(context.Background(), staging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 2 || p.bgSeen[1] != nil {
		t.Fatalf("a staging call that names none must clear the previous set's; seeds %v", p.bgSeen)
	}
}

// A recording's previous call is never itself, nor one sent at its own
// instant, and a recording with no request time has no span start.
func TestABackgroundRecordingsSpanStartsAtTheCallBeforeIt(t *testing.T) {
	poll := bgMock("poll", models.Mongo, 4, 4)
	same := bgMock("same-instant", models.Mongo, 4, 4)
	before := bgMock("before", models.Mongo, 1, 1)
	recs, err := backgroundRecordingsOf([]string{"poll", "none-at"}, [][]*models.Mock{{before, same, poll}, {{Name: "none-at", Kind: models.Mongo}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bgFroms(recs), map[string]int{"poll": 1, "none-at": -1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans' starts %v; want %v", got, want)
	}
	if recs, _ := backgroundRecordingsOf(nil, [][]*models.Mock{{poll}}, nil); recs != nil {
		t.Fatalf("no names resolved %v", bgFroms(recs))
	}
	disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	_ = disk.Close()
	if recs, err := backgroundRecordingsOf([]string{"poll"}, [][]*models.Mock{{poll}}, disk); err == nil || recs != nil {
		t.Fatalf("a store that cannot be read resolved %v, err %v; want none and its error", bgFroms(recs), err)
	}
}

// A resident list may hold calls sent before another list's: the calls are
// read in time order whatever list holds them, so a recording's previous
// call is the latest sent before it in any list.
func TestABackgroundRecordingsPreviousCallIsReadAcrossTheLists(t *testing.T) {
	poll := bgMock("poll", models.Mongo, 6, 6)
	recs, err := backgroundRecordingsOf([]string{"poll"}, [][]*models.Mock{{bgMock("c3", models.Mongo, 3, 3), poll}, {bgMock("c1", models.Mongo, 1, 1), bgMock("c2", models.Mongo, 2, 2)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bgFroms(recs), map[string]int{"poll": 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans' starts %v; want %v", got, want)
	}
}

// Fail-closed: whenever reading the set's background recordings errors (e.g.
// unreadable DiskMocks) OR any named background mock is missing from the
// staged set, seedBackgroundRecordings must seed NO declared Mongo or HTTP
// background rules so strict per-recording checks are never bypassed and every
// unmatched call stays its test's miss.
func TestSeedBackgroundRecordingsFailsClosedOnDiskErrorOrMissingMock(t *testing.T) {
	mongoRules := []models.BackgroundMongoRule{{
		ID:          "e2e-pack-sweep-unread",
		DB:          "api-server",
		Command:     "find",
		Collections: []string{"e2eRecs"},
		Where:       []models.BackgroundMongoWhere{{Field: "body.cmd.filter.kind", Equals: "startup"}},
	}}
	httpRules := []models.BackgroundHTTPRule{{
		ID:     "openbao-liveness-check",
		Method: "GET",
		Host:   "localhost:8200",
		Path:   "/v1/sys/health",
	}}
	poll := bgMock("poll-1", models.Mongo, 2, 2)

	t.Run("all named background mocks found seeds rules", func(t *testing.T) {
		p := &backgroundProxy{}
		a := &Agent{logger: zap.NewNop(), Proxy: p}
		a.seedBackgroundRecordings(models.MockFilterParams{
			BackgroundMocks:      []string{"poll-1"},
			BackgroundMongoRules: mongoRules,
			BackgroundHTTPRules:  httpRules,
		}, [][]*models.Mock{{poll}}, nil)
		if len(p.mongoSeen) != 1 || !reflect.DeepEqual(p.mongoSeen[0], mongoRules) {
			t.Fatalf("mongo rules seeded = %+v; want %+v", p.mongoSeen, mongoRules)
		}
		if len(p.httpSeen) != 1 || !reflect.DeepEqual(p.httpSeen[0], httpRules) {
			t.Fatalf("http rules seeded = %+v; want %+v", p.httpSeen, httpRules)
		}
		if len(p.bgSeen) != 1 || len(p.bgSeen[0]) != 1 {
			t.Fatalf("bg recs seeded = %+v; want 1", p.bgSeen)
		}
	})

	t.Run("disk read error beside recorded rule disables declared rule fallback", func(t *testing.T) {
		disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		_ = disk.Close()
		p := &backgroundProxy{}
		a := &Agent{logger: zap.NewNop(), Proxy: p}
		a.seedBackgroundRecordings(models.MockFilterParams{
			BackgroundMocks:      []string{"poll-1"},
			BackgroundMongoRules: mongoRules,
			BackgroundHTTPRules:  httpRules,
		}, [][]*models.Mock{{poll}}, disk)
		if len(p.mongoSeen) != 1 || p.mongoSeen[0] != nil {
			t.Fatalf("on disk error, mongo rules seeded = %+v; want nil", p.mongoSeen)
		}
		if len(p.httpSeen) != 1 || p.httpSeen[0] != nil {
			t.Fatalf("on disk error, http rules seeded = %+v; want nil", p.httpSeen)
		}
		if len(p.bgSeen) != 1 || p.bgSeen[0] != nil {
			t.Fatalf("on disk error, bg recs seeded = %+v; want nil", p.bgSeen)
		}
	})

	t.Run("missing named background mock disables declared rule fallback", func(t *testing.T) {
		p := &backgroundProxy{}
		a := &Agent{logger: zap.NewNop(), Proxy: p}
		a.seedBackgroundRecordings(models.MockFilterParams{
			BackgroundMocks:      []string{"poll-1", "poll-missing"},
			BackgroundMongoRules: mongoRules,
			BackgroundHTTPRules:  httpRules,
		}, [][]*models.Mock{{poll}}, nil)
		if len(p.mongoSeen) != 1 || p.mongoSeen[0] != nil {
			t.Fatalf("on missing background mock, mongo rules seeded = %+v; want nil", p.mongoSeen)
		}
		if len(p.httpSeen) != 1 || p.httpSeen[0] != nil {
			t.Fatalf("on missing background mock, http rules seeded = %+v; want nil", p.httpSeen)
		}
		// The found recording itself is still seeded for exact recording matching.
		if len(p.bgSeen) != 1 || len(p.bgSeen[0]) != 1 || p.bgSeen[0][0].Mock.Name != "poll-1" {
			t.Fatalf("on missing background mock, bg recs seeded = %+v; want [poll-1]", p.bgSeen)
		}
	})
}
