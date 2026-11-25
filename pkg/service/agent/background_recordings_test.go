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
// recordings.
type backgroundProxy struct {
	pacingProxy
	bgMu   sync.Mutex
	bgSeen [][]models.BackgroundRecording
}

func (p *backgroundProxy) SeedBackgroundRecordings(recs []models.BackgroundRecording) {
	p.bgMu.Lock()
	defer p.bgMu.Unlock()
	p.bgSeen = append(p.bgSeen, recs)
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
	recs, err := backgroundRecordingsOf([]string{"poll", "none-at"}, nil, [][]*models.Mock{{before, same, poll}, {{Name: "none-at", Kind: models.Mongo}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bgFroms(recs), map[string]int{"poll": 1, "none-at": -1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans' starts %v; want %v", got, want)
	}
	if recs, _ := backgroundRecordingsOf(nil, nil, [][]*models.Mock{{poll}}, nil); recs != nil {
		t.Fatalf("no names resolved %v", bgFroms(recs))
	}
	disk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	_ = disk.Close()
	if recs, err := backgroundRecordingsOf([]string{"poll"}, nil, [][]*models.Mock{{poll}}, disk); err == nil || recs != nil {
		t.Fatalf("a store that cannot be read resolved %v, err %v; want none and its error", bgFroms(recs), err)
	}
}

// A resident list may hold calls sent before another list's: the calls are
// read in time order whatever list holds them, so a recording's previous
// call is the latest sent before it in any list.
func TestABackgroundRecordingsPreviousCallIsReadAcrossTheLists(t *testing.T) {
	poll := bgMock("poll", models.Mongo, 6, 6)
	recs, err := backgroundRecordingsOf([]string{"poll"}, nil, [][]*models.Mock{{bgMock("c3", models.Mongo, 3, 3), poll}, {bgMock("c1", models.Mongo, 1, 1), bgMock("c2", models.Mongo, 2, 2)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bgFroms(recs), map[string]int{"poll": 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans' starts %v; want %v", got, want)
	}
}

// When a non-exact staging explicitly opts in via DeriveBackgroundFromWindows
// alongside RecordedWindows and empty BackgroundMocks (such as round-1
// DaemonSet auto-replay before mappings.yaml exists), mocks recorded outside
// every test window are derived as background recordings in a single pass,
// while mocks inside a test window stay that test's own. Without the opt-in
// flag, with explicit BackgroundMocks, or under ExactStaging, no derivation
// overrides the caller's list; disk errors fall back to seeding none.
func TestStagingWithoutBackgroundMocksDerivesOutsideWindowBackgroundRecordings(t *testing.T) {
	pollBetween := bgMock("poll-between", models.HTTP2, 6, 6)
	pollBetween.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	explicitBg := bgMock("explicit-bg", models.HTTP2, 8, 8)
	explicitBg.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	inTest1 := bgMock("in-test-1", models.HTTP2, 2, 2)
	inTest1.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
	inTest2 := bgMock("in-test-2", models.HTTP2, 11, 11)
	inTest2.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest

	p := &backgroundProxy{}
	a := &Agent{logger: zap.NewNop(), Proxy: p}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{
		filtered: []*models.Mock{inTest1, pollBetween, explicitBg, inTest2},
	})

	baseStaging := models.MockFilterParams{
		AfterTime:              models.BaseTime,
		BeforeTime:             time.Now(),
		FirstRecordedTestStart: bgT0.Add(1 * time.Second),
		RecordedWindows: []models.TestWindow{
			{TestCase: "tc-1", Start: bgT0.Add(1 * time.Second), End: bgT0.Add(4 * time.Second)},
			{TestCase: "tc-2", Start: bgT0.Add(10 * time.Second), End: bgT0.Add(13 * time.Second)},
		},
	}

	// 1. Without DeriveBackgroundFromWindows (e.g. explicit mappings.yaml with
	// empty worker:/startup:), empty BackgroundMocks must seed NONE.
	if err := a.UpdateMockParams(context.Background(), baseStaging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 1 || len(p.bgSeen[0]) != 0 {
		t.Fatalf("without DeriveBackgroundFromWindows must seed none; got %+v", p.bgSeen)
	}

	// 2. With DeriveBackgroundFromWindows=true, outside-window recordings are
	// derived as background recordings while in-window mocks stay per-test.
	derivedStaging := baseStaging
	derivedStaging.DeriveBackgroundFromWindows = true
	if err := a.UpdateMockParams(context.Background(), derivedStaging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 2 {
		t.Fatalf("seeded %d times, want 2", len(p.bgSeen))
	}
	if got, want := bgFroms(p.bgSeen[1]), map[string]int{"poll-between": 2, "explicit-bg": 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("derived background recordings %v; want %v", got, want)
	}

	// 3. Explicit BackgroundMocks together with RecordedWindows + flag: explicit
	// list wins and must not be replaced by derivation.
	explicitStaging := derivedStaging
	explicitStaging.BackgroundMocks = []string{"explicit-bg"}
	if err := a.UpdateMockParams(context.Background(), explicitStaging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 3 {
		t.Fatalf("seeded %d times, want 3", len(p.bgSeen))
	}
	if got, want := bgFroms(p.bgSeen[2]), map[string]int{"explicit-bg": 6}; !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit BackgroundMocks with windows seeded %v; want %v", got, want)
	}

	// 4. Under ExactStaging, mappings.yaml is authoritative; empty BackgroundMocks
	// must NOT derive even if DeriveBackgroundFromWindows is set.
	exactStaging := derivedStaging
	exactStaging.ExactStaging = true
	if err := a.UpdateMockParams(context.Background(), exactStaging); err != nil {
		t.Fatal(err)
	}
	if len(p.bgSeen) != 4 || len(p.bgSeen[3]) != 0 {
		t.Fatalf("ExactStaging with empty BackgroundMocks must seed none; got %+v", p.bgSeen)
	}

	// 5. Disk read error during derivation logs and falls back to seeding none.
	closedDisk, err := proxyPkg.NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	_ = closedDisk.Close()
	if recs, err := derivedBackgroundRecordingsOf(derivedStaging.RecordedWindows, [][]*models.Mock{{inTest1, pollBetween}}, closedDisk); err == nil || recs != nil {
		t.Fatalf("derivedBackgroundRecordingsOf on closed disk = (%v, %v); want (nil, error)", recs, err)
	}
	a.seedBackgroundRecordings(derivedStaging, [][]*models.Mock{{inTest1, pollBetween}}, closedDisk)
	if len(p.bgSeen) != 5 || p.bgSeen[4] != nil {
		t.Fatalf("disk error during derivation must fall back to nil; got %+v", p.bgSeen[4])
	}
}
