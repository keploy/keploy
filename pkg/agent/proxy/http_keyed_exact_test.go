package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// windowRowMock is a per-test mock named name, asked at req and answered at
// res (either zero for none), filed by byName under name's last digit.
func windowRowMock(name string, req, res time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP}
	m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "http://upstream/" + name}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = req, res
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

var byLastDigit = &integrations.MockIndex{Keys: func(m *models.Mock) []string { return []string{m.Name[len(m.Name)-1:]} }}

// Without exact serving a keyed per-test read is the windowed read narrowed to
// the key, in its order: a mock missing either time kept, one answered before
// it was asked and one outside the window left out, and with no window every mock
// kept. It examines only the entries filed under the key.
func TestAKeyedPerTestReadWithoutAViewIsTheWindowedReadNarrowed(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	var mocks []*models.Mock
	for i := 0; i < 60; i++ {
		req := at.Add(time.Duration(i) * time.Second)
		res := req.Add(time.Millisecond)
		switch i % 5 {
		case 1:
			req, res = time.Time{}, time.Time{} // undated
		case 2:
			res = req.Add(-time.Millisecond) // answered before asked
		case 3:
			res = time.Time{} // asked, with no answer's time: kept as undated
		}
		mocks = append(mocks, windowRowMock(fmt.Sprintf("m%02d", i), req, res))
	}
	for _, window := range []struct {
		name       string
		start, end time.Time
	}{
		{"window", at.Add(10 * time.Second), at.Add(40 * time.Second)},
		{"none", time.Time{}, time.Time{}},
	} {
		t.Run(window.name, func(t *testing.T) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			// Staged with no window, so the tier holds every mock, then the
			// window set on its own, as a test's window is moved.
			mm.SetMocksWithWindow(mocks, nil, time.Time{}, time.Time{})
			if !window.start.IsZero() {
				mm.SetCurrentTestWindow(window.start, window.end)
			}
			all, err := mm.GetPerTestMocksInWindow()
			if err != nil {
				t.Fatal(err)
			}
			if window.start.IsZero() && len(all) != len(mocks) {
				t.Fatalf("with no window the read kept %d of %d", len(all), len(mocks))
			}
			if !window.start.IsZero() && (len(all) == 0 || len(all) == len(mocks)) {
				t.Fatalf("the window kept %d of %d; the row needs some kept and some not", len(all), len(mocks))
			}
			for d := 0; d < 10; d++ {
				key := strconv.Itoa(d)
				var want, got []string
				for _, m := range all {
					if m.Name[len(m.Name)-1:] == key {
						want = append(want, m.Name)
					}
				}
				before := keyedExamined.Load()
				if err := mm.RangePerTestMocksWithKey(byLastDigit, key, func(m *models.Mock) bool {
					got = append(got, m.Name)
					return true
				}); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("key %s: the keyed read gave %v; the windowed read narrowed is %v", key, got, want)
				}
				if n := keyedExamined.Load() - before; n != uint64(len(mocks)/10) {
					t.Fatalf("key %s: the keyed read examined %d entries; %d are filed under it", key, n, len(mocks)/10)
				}
			}
		})
	}
}

// Without the proven switch a call that is a recording's very request reads
// the recordings of that request alone: no call walks the per-test tier whole,
// and each examines a few entries of its key, in a set of 2000.
func TestAProvenOffExactCallReadsOnlyItsRequestsRecordings(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	const n = 2000
	at := time.Unix(1_700_000_000, 0)
	h := httpint.New(zap.NewNop())
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	stageScale(mm, n, at, "none")
	walks := watchWalks(t, mm.filtered)
	for k := 0; k < 64; k++ {
		i := (k * 7919) % n
		before, walked := keyedExamined.Load(), walks.Load()
		body, err := serveScale(h, mm, i)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i); body != want {
			t.Fatalf("call %d served %q, want %q", i, body, want)
		}
		if w := walks.Load() - walked; w != 0 {
			t.Fatalf("call %d walked the per-test tier whole %d times; want none", i, w)
		}
		if e := keyedExamined.Load() - before; e > 4 {
			t.Fatalf("call %d examined %d keyed entries; want its request's few", i, e)
		}
	}
}

// wholeReads is the manager with its keyed per-test read refused, so every
// call of the HTTP integration reads the whole pool, as before the keyed
// exact-body pass: the row's reference.
type wholeReads struct{ *MockManager }

var errWholeReads = errors.New("the row's reference reads the whole pool")

func (wholeReads) RangePerTestMocksWithKey(*integrations.MockIndex, string, func(*models.Mock) bool) error {
	return errWholeReads
}

// corpusMock is recording i of a corpus drawn by r: a GET or a POST of a few
// paths and bodies, so recordings share requests, some stateful (a cursor),
// some of the session tier, some outside the window or undated.
func corpusMock(r *rand.Rand, i int, at time.Time) (m *models.Mock, session bool) {
	method, path, body := corpusReq(r)
	hdr := map[string]string{"Host": "upstream"}
	if body != "" {
		hdr["Content-Type"] = corpusType(body)
	}
	resp := fmt.Sprintf("resp-%d", i)
	m = &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-" + strconv.Itoa(i),
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq:  &models.HTTPReq{Method: models.Method(method), ProtoMajor: 1, ProtoMinor: 1, URL: "http://upstream" + path, Header: hdr, Body: body},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": strconv.Itoa(len(resp))}, Body: resp},
		},
	}
	switch r.Intn(6) {
	case 0: // undated
	case 1: // before the window
		m.Spec.ReqTimestampMock = at.Add(-time.Hour)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
	default:
		m.Spec.ReqTimestampMock = at.Add(time.Duration(i) * time.Millisecond)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(500 * time.Microsecond)
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	if session = r.Intn(4) == 0; session {
		m.TestModeInfo.Lifetime = models.LifetimeSession
	}
	if r.Intn(3) == 0 {
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	}
	return m, session
}

func corpusReq(r *rand.Rand) (method, path, body string) {
	paths := []string{"/a", "/b", "/a/1"}
	bodies := []string{"", `{"x":1}`, `{"x":2}`, `{"x":1,"y":[1,2]}`, "k=v"}
	method = []string{"GET", "POST"}[r.Intn(2)]
	path = paths[r.Intn(len(paths))]
	if method == "POST" {
		body = bodies[r.Intn(len(bodies))]
	}
	return method, path, body
}

func corpusType(body string) string {
	if strings.HasPrefix(body, "{") {
		return "application/json"
	}
	return "application/x-www-form-urlencoded"
}

// corpusServe sends one call through the HTTP integration's replay path and
// tells what it was answered: the status and body, or that it was not.
func corpusServe(h integrations.Integrations, mm integrations.MockMemDb, method, path, body string) string {
	return serveWith(h, mm, models.OutgoingOptions{}, "", method, path, body)
}

// serveWith is corpusServe under opts, the call carrying the header lines
// extra too. A call that is not answered tells its miss too (missOf), so two
// managers that miss a call alike must also report it alike.
func serveWith(h integrations.Integrations, mm integrations.MockMemDb, opts models.OutgoingOptions, extra, method, path, body string) string {
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, opts)
		_ = agent.Close()
	}()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: upstream\r\n%s", method, path, extra)
	if body != "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\nContent-Type: %s\r\n", len(body), corpusType(body))
	}
	b.WriteString("\r\n" + body)
	go func() { _, _ = app.Write([]byte(b.String())) }()
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		_ = app.Close()
		return "no answer" + missOf(<-done)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	return fmt.Sprintf("%d %s", resp.StatusCode, got) + missOf(<-done)
}

// replayGap is the part of a repeat's next steps that tells how long after
// the earlier call the call came, at this replay.
var replayGap = regexp.MustCompile(`this call came \S+ after`)

// missOf is what the integration returned for a call, after " | ": its
// error and the miss report it carries, less what tells when the call was
// made (the report's time, a repeat's answer time and gap), so the misses of
// two managers serving one corpus compare. Nothing for no error.
func missOf(err error) string {
	if err == nil {
		return ""
	}
	s := " | " + err.Error()
	var rep interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &rep) || rep.MismatchReport() == nil {
		return s
	}
	r := *rep.MismatchReport()
	r.At = time.Time{}
	if r.Repeat != nil {
		note := *r.Repeat
		note.AnsweredAt, note.ReplayGapSeconds = time.Time{}, 0
		r.Repeat = &note
	}
	r.NextSteps = replayGap.ReplaceAllString(r.NextSteps, "this call came later")
	js, err := json.Marshal(r)
	if err != nil {
		return s + " | the report does not encode: " + err.Error()
	}
	return s + " | " + string(js)
}

// The keyed exact-body pass answers every call as the whole read answers it:
// over random corpora of shared requests, stateful recordings, both tiers and
// mocks in and out of the window, each call is served the same recording, or
// missed, by a manager that reads by key and by one that reads the whole
// pool, under the proven switch off and on.
func TestAKeyedExactBodyPassAnswersAsTheWholeRead(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("proven="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			at := time.Unix(1_700_000_000, 0)
			// served counts the calls answered by a recording; unwalked, those
			// the keyed manager answered without walking its per-test tier.
			served, unwalked := 0, 0
			for seed := int64(1); seed <= 12; seed++ {
				r := rand.New(rand.NewSource(seed))
				var perTest, session []*models.Mock
				var perTest2, session2 []*models.Mock
				for i := 0; i < 40; i++ {
					m, s := corpusMock(r, i, at)
					if s {
						session, session2 = append(session, m), append(session2, m.DeepCopy())
					} else {
						perTest, perTest2 = append(perTest, m), append(perTest2, m.DeepCopy())
					}
				}
				keyed := NewMockManager(nil, nil, zap.NewNop())
				whole := NewMockManager(nil, nil, zap.NewNop())
				start, end := at.Add(-time.Second), at.Add(time.Minute)
				keyed.SetMocksWithWindow(perTest, session, start, end)
				whole.SetMocksWithWindow(perTest2, session2, start, end)
				h1, h2 := httpint.New(zap.NewNop()), httpint.New(zap.NewNop())
				walks := watchWalks(t, keyed.filtered)
				for c := 0; c < 60; c++ {
					method, path, body := corpusReq(r)
					walked := walks.Load()
					a := corpusServe(h1, keyed, method, path, body)
					fast := walks.Load() == walked
					b := corpusServe(h2, wholeReads{whole}, method, path, body)
					if a != b {
						t.Fatalf("seed %d call %d (%s %s %q): the keyed read answered %q, the whole read %q", seed, c, method, path, body, a, b)
					}
					if strings.HasPrefix(a, "200 resp-") {
						served++
						if fast {
							unwalked++
						}
					}
				}
				keyed.Close()
				whole.Close()
			}
			if proven == "" && (served < 200 || unwalked < served/2) {
				t.Fatalf("the corpora served %d calls, %d without a whole read; the row needs most served, most of them by key", served, unwalked)
			}
		})
	}
}

// keyedRowManager is a manager of the per-test recordings mocks, each dated
// in the window in the given order.
func keyedRowManager(t *testing.T, mocks ...*models.Mock) *MockManager {
	t.Helper()
	at := time.Unix(1_700_000_000, 0)
	for i, m := range mocks {
		m.Spec.ReqTimestampMock = at.Add(time.Duration(i+1) * time.Millisecond)
		m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(500 * time.Microsecond)
		m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(mocks, nil, at, at.Add(time.Minute))
	return mm
}

// keyedRowMock is recording i of the request method path body, with the
// recorded headers extra besides Host and Content-Type, answered resp-i.
func keyedRowMock(i int, method, path, body string, extra map[string]string) *models.Mock {
	m, _ := corpusMock(rand.New(rand.NewSource(1)), i, time.Time{})
	m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body = models.Method(method), "http://upstream"+path, body
	m.Spec.HTTPReq.Header = map[string]string{"Host": "upstream", "Content-Type": corpusType(body)}
	for k, v := range extra {
		m.Spec.HTTPReq.Header[k] = v
	}
	m.TestModeInfo.Consume = models.ConsumeReuse
	return m
}

// Under the proven switch no call is answered by the keyed exact-body pass,
// which compares no header value: a recording of the call's very method,
// path and body whose header holds another value is not served it (the
// strict comparison refuses the value), while with the switch off the call
// is served it, and under the switch a call of the recorded value is too.
func TestAProvenCallIsNotAnsweredByTheKeyedExactBodyPass(t *testing.T) {
	for _, c := range []struct {
		proven, tenant string
		served         bool
	}{{"1", "t2", false}, {"1", "t1", true}, {"", "t2", true}} {
		t.Run(fmt.Sprintf("proven=%s/tenant=%s", c.proven, c.tenant), func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", c.proven)
			mm := keyedRowManager(t, keyedRowMock(0, "POST", "/a", `{"x":1}`, map[string]string{"X-Tenant": "t1"}))
			got := serveWith(httpint.New(zap.NewNop()), mm, models.OutgoingOptions{}, "X-Tenant: "+c.tenant+"\r\n", "POST", "/a", `{"x":1}`)
			if served := got == "200 resp-0"; served != c.served {
				t.Fatalf("the call of X-Tenant %s was answered %q; want served %v", c.tenant, got, c.served)
			}
		})
	}
}

// Under keploy.yml's url noise a call is answered by the whole read, which
// reads recordings of other paths the noise lets match, in the pool's order:
// of a recording of /a/2 and a later one of /a/1, both of the call's body, a
// call of /a/1 under noise of the segment is served the first, as before the
// keyed pass, and with no noise the one of its own path.
func TestAnExactCallUnderURLNoiseIsAnsweredByTheWholeRead(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	for _, c := range []struct {
		noise map[string]map[string][]string
		want  string
	}{{map[string]map[string][]string{"url": {"/a/[0-9]+": nil}}, "200 resp-0"}, {nil, "200 resp-1"}} {
		t.Run(fmt.Sprintf("noise=%v", c.noise != nil), func(t *testing.T) {
			mm := keyedRowManager(t, keyedRowMock(0, "POST", "/a/2", `{"x":1}`, nil), keyedRowMock(1, "POST", "/a/1", `{"x":1}`, nil))
			got := serveWith(httpint.New(zap.NewNop()), mm, models.OutgoingOptions{NoiseConfig: c.noise}, "", "POST", "/a/1", `{"x":1}`)
			if got != c.want {
				t.Fatalf("the call of /a/1 was answered %q; want %q", got, c.want)
			}
		})
	}
}
