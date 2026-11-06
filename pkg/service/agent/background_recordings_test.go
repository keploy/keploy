package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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

// bgClockProxy is a wireProxy that takes the background check's clock
// (coreAgent.BackgroundClockSetter).
type bgClockProxy struct {
	*wireProxy
	set   int
	clock func(time.Time) (time.Time, bool)
}

func (p *bgClockProxy) SetBackgroundClock(f func(time.Time) (time.Time, bool)) {
	p.set++
	p.clock = f
}

// bgClockHook is the no-op hooks that tell the background check the app's
// clock, offset past the real one (BackgroundClockReader).
type bgClockHook struct {
	AgentHook
	offset time.Duration
}

func (h *bgClockHook) BackgroundAppClock(real time.Time) (time.Time, bool) {
	return real.Add(h.offset), true
}

// Once the mocks are stored with the proven switch off, the background check
// alone is handed the app's clock: the hooks' where they tell it, the real
// one from this build's own hooks, which move none, and none from hooks that
// tell neither, replacing the earlier. The learn step is handed none, as
// before. Under the switch the background check is handed nothing: the
// learn tracker tells a call's span there.
func TestStoredMocksHandTheBackgroundCheckTheAppsClockWithTheSwitchOff(t *testing.T) {
	prev := ActiveHooks
	t.Cleanup(func() { ActiveHooks = prev })
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	ActiveHooks = &AgentHook{}
	a, wp := wireAgent(t, models.MODE_TEST)
	t.Setenv(provenswitch.Env, "")
	p := &bgClockProxy{wireProxy: wp}
	a.Proxy = p
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 1 || p.clock == nil {
		t.Fatalf("this build's own hooks: handed %d times, clock %v; want the real clock once", p.set, p.clock != nil)
	}
	if got, ok := p.clock(at); !ok || !got.Equal(at) {
		t.Fatalf("this build's own hooks: the clock read %v %v; want the real instant", got, ok)
	}
	if wp.appClock != nil {
		t.Fatal("the switch off: the learn step is handed no clock, as before")
	}

	ActiveHooks = &bgClockHook{offset: 7 * time.Second}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 2 || p.clock == nil {
		t.Fatalf("hooks that tell it: handed %d times, clock %v; want theirs", p.set, p.clock != nil)
	}
	if got, ok := p.clock(at); !ok || !got.Equal(at.Add(7*time.Second)) {
		t.Fatalf("hooks that tell it: the clock read %v %v; want theirs", got, ok)
	}

	ActiveHooks = &otherHooks{}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 3 || p.clock != nil {
		t.Fatalf("hooks that tell neither: handed %d times, clock %v; want none, replacing the earlier", p.set, p.clock != nil)
	}

	t.Setenv(provenswitch.Env, "1")
	ActiveHooks = &AgentHook{}
	a.finalizeClientMocks(context.Background(), &ClientMockStorage{})
	if p.set != 3 {
		t.Fatalf("under the switch the background check was handed a clock (%d times): the learn tracker tells the span there", p.set)
	}
	if wp.appClock == nil {
		t.Fatal("under the switch the learn step reads the real clock, as before")
	}
}
