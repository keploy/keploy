package proxy

import (
	"fmt"
	"math/rand"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The lax shapes: calls the exact-body pass does not answer, with the proven
// switch off. A set of n recordings in n/4 groups, group j under its own path
// prefix /shop-<slug j>: two POSTs of one path whose JSON bodies differ in a
// value, a GET with an id segment and a GET with a token segment.
//   - fuzzy: a POST whose body is neither recording's (a value changed), so
//     the body and fuzzy passes choose;
//   - id: a GET whose id segment is not the recording's, answered by the
//     second pass, of auto-detected id segments;
//   - urlnoise: a GET whose token segment differs, under url noise that
//     masks it;
//   - miss: a GET of the group's prefix no recording has;
//   - pass2: a GET whose token segment differs, under the same url noise,
//     and whose id query value differs from its recording's. The first pass
//     refuses that recording (the value), and its id-masked path is not the
//     call's (the token), so the second pass, which relaxes the value, reads
//     the whole pool. In this shape's set each token GET carries an id query.

const laxURLNoise = "/s/tok[a-z]+"

func laxSlug(j int) string {
	b := []byte("aaaa")
	for k := 3; k >= 0; k-- {
		b[k] = byte('a' + j%26)
		j /= 26
	}
	return string(b)
}

func laxRecorded(i int) (method, path, body string) {
	j, slug := i/4, laxSlug(i/4)
	switch i % 4 {
	case 0, 1:
		return "POST", "/shop-" + slug + "/order", fmt.Sprintf(`{"kind":%q,"n":%d,"ts":"rec"}`, string(rune('a'+i%4)), j)
	case 2:
		return "GET", "/shop-" + slug + "/r/" + strconv.Itoa(1000000+j), ""
	}
	return "GET", "/shop-" + slug + "/s/tok" + slug + "rec", ""
}

// laxCall is shape's call of group j and the recording that answers it (-1:
// none does).
func laxCall(shape string, j int) (method, path, body string, answer int) {
	slug := laxSlug(j)
	switch shape {
	case "fuzzy":
		return "POST", "/shop-" + slug + "/order", fmt.Sprintf(`{"kind":"a","n":%d,"ts":"live"}`, j), 4 * j
	case "id":
		return "GET", "/shop-" + slug + "/r/" + strconv.Itoa(5000000+j), "", 4*j + 2
	case "urlnoise":
		return "GET", "/shop-" + slug + "/s/tok" + slug + "live", "", 4*j + 3
	case "pass2":
		return "GET", "/shop-" + slug + "/s/tok" + slug + "live?id=" + strconv.Itoa(1_700_000_500_000+j), "", 4*j + 3
	}
	return "GET", "/shop-" + slug + "/none", "", -1
}

func laxOpts(shape string) models.OutgoingOptions {
	if shape != "urlnoise" && shape != "pass2" {
		return models.OutgoingOptions{}
	}
	return models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"url": {laxURLNoise: nil}}}
}

// laxMock is recording i of shape's set: laxRecorded's, with an id query on
// each token GET of the pass2 set.
func laxMock(shape string, i int, at time.Time) *models.Mock {
	method, path, body := laxRecorded(i)
	var params map[string]string
	if shape == "pass2" && i%4 == 3 {
		id := strconv.Itoa(1_700_000_000_000 + i/4)
		path, params = path+"?id="+id, map[string]string{"id": id}
	}
	hdr := map[string]string{"Host": "upstream"}
	if body != "" {
		hdr["Content-Type"] = "application/json"
		hdr["Content-Length"] = strconv.Itoa(len(body))
	}
	resp := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i)
	m := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mock-" + strconv.Itoa(i),
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": models.HTTPClient},
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method), ProtoMajor: 1, ProtoMinor: 1,
				URL: "http://upstream" + path, URLParams: params, Header: hdr, Body: body,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(resp))},
				Body:       resp,
			},
			ReqTimestampMock: at.Add(time.Duration(i) * time.Millisecond),
			ResTimestampMock: at.Add(time.Duration(i)*time.Millisecond + 500*time.Microsecond),
		},
	}
	m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived = models.LifetimePerTest, true
	return m
}

// stageLax installs a fresh copy of shape's n-recording lax set as the
// serving window.
func stageLax(mm *MockManager, shape string, n int, at time.Time) {
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = laxMock(shape, i, at)
	}
	mm.SetMocksWithWindow(mocks, nil, at.Add(-time.Second), at.Add(time.Duration(n)*time.Millisecond+time.Second))
}

// laxServe makes shape's call of group j and checks its answer.
func laxServe(h integrations.Integrations, mm integrations.MockMemDb, shape string, j int) error {
	method, path, body, answer := laxCall(shape, j)
	got := serveWith(h, mm, laxOpts(shape), "", method, path, body)
	if answer < 0 {
		if strings.HasPrefix(got, "200 ") {
			return fmt.Errorf("%s call %d was answered %q; want a miss", shape, j, got)
		}
		return nil
	}
	if want := fmt.Sprintf(`200 {"id":"item-%d","ok":true}`, answer); got != want {
		return fmt.Errorf("%s call %d was answered %q, want %q", shape, j, got, want)
	}
	return nil
}

// With the proven switch off a call the exact-body pass does not answer is
// compared with the recordings of its own method and path alone: a changed
// body, an id segment the second pass relates, a token under url noise. Each
// call is served its recording without a walk of the per-test tier and
// examines a few keyed entries, in a set of 4000.
func TestAProvenOffLaxCallReadsOnlyItsPathsRecordings(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	const n = 4000
	at := time.Unix(1_700_000_000, 0)
	for _, shape := range []string{"fuzzy", "id", "urlnoise"} {
		t.Run(shape, func(t *testing.T) {
			h := httpint.New(zap.NewNop())
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			stageLax(mm, shape, n, at)
			walks := watchWalks(t, mm.filtered)
			for k := 0; k < 48; k++ {
				j := (k * 7919) % (n / 4)
				before, walked := keyedExamined.Load(), walks.Load()
				if err := laxServe(h, mm, shape, j); err != nil {
					t.Fatal(err)
				}
				// The first call of a shape builds the indexes it reads.
				if k == 0 {
					continue
				}
				if w := walks.Load() - walked; w != 0 {
					t.Fatalf("%s call %d walked the per-test tier whole %d times; want none", shape, j, w)
				}
				if e := keyedExamined.Load() - before; e > 8 {
					t.Fatalf("%s call %d examined %d keyed entries; want its path's few", shape, j, e)
				}
			}
		})
	}
}

// wideReq is a call of a corpus wider than corpusReq's: paths an id segment
// relates (/u/<digits>, /u/<hex>), token paths a url noise relates
// (/s/tok<digits>), id-shaped query values the second pass relaxes, and JSON
// bodies that differ in a value or in their bytes
// alone, so calls are answered by every pass of the cascade, or missed.
func wideReq(r *rand.Rand) (method, path, body string) {
	paths := []string{"/a", "/a/1", "/u/123", "/u/456", "/u/0123456789abcdef", "/u/abc", "/s/tok1", "/s/tok22", "/b/5/c"}
	bodies := []string{"", `{"x":1}`, `{"x":2}`, `{"x":1,"y":[1,2]}`, `{"y":[1,2],"x":1}`, `{"x":3,"y":[2]}`, "k=v", "k=w"}
	queries := []string{"", "", "?id=1700000000123", "?id=1700000000456"}
	method = []string{"GET", "POST"}[r.Intn(2)]
	path = paths[r.Intn(len(paths))] + queries[r.Intn(len(queries))]
	if method == "POST" {
		body = bodies[r.Intn(len(bodies))]
	}
	return method, path, body
}

// sessionKeyRefused is the manager with its keyed session read refused: a
// call reads the whole pool, as when a store's keyed read fails.
type sessionKeyRefused struct{ *MockManager }

func (sessionKeyRefused) RangeSessionMocksWithKey(*integrations.MockIndex, string, func(*models.Mock) bool) error {
	return errWholeReads
}

// The keyed lax read answers every call as the whole read answers it: over
// random corpora of shared requests, stateful recordings, both tiers and mocks
// in and out of the window, calls that differ from every recording in their
// body, in an id segment, in a token segment, or that no recording answers
// are each served the same recording, or missed, by a manager that reads by
// key and by one that reads the whole pool: with no url noise, under url
// noise that relates paths no id mask does (tok1 and tok22), and under a bare
// pattern that masks every digit run, with the proven switch off and on. A
// store whose keyed session read fails answers as the whole read too.
func TestAKeyedLaxReadAnswersAsTheWholeRead(t *testing.T) {
	noises := map[string][]string{"none": nil, "token": {"/s/tok[0-9]+"}, "digits": {"[0-9]+"}}
	for _, proven := range []string{"", "1"} {
		for _, noise := range []string{"none", "token", "digits"} {
			t.Run("proven="+proven+"/noise="+noise, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				var opts models.OutgoingOptions
				if pats := noises[noise]; pats != nil {
					opts.NoiseConfig = map[string]map[string][]string{"url": {}}
					for _, p := range pats {
						opts.NoiseConfig["url"][p] = nil
					}
				}
				at := time.Unix(1_700_000_000, 0)
				// served counts the calls answered by a recording that the
				// exact-body pass does not answer; unwalked, those of them the
				// keyed manager answered without walking its per-test tier.
				served, unwalked, missed := 0, 0, 0
				for seed := int64(1); seed <= 12; seed++ {
					r := rand.New(rand.NewSource(seed))
					var perTest, session []*models.Mock
					var perTest2, session2, perTest3, session3 []*models.Mock
					recorded := map[string]bool{}
					for i := 0; i < 40; i++ {
						m, s := corpusMock(r, i, at)
						method, path, body := wideReq(r)
						m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body = models.Method(method), "http://upstream"+path, body
						if _, q, ok := strings.Cut(path, "?"); ok {
							vals, _ := url.ParseQuery(q)
							m.Spec.HTTPReq.URLParams = map[string]string{}
							for k := range vals {
								m.Spec.HTTPReq.URLParams[k] = vals.Get(k)
							}
						}
						delete(m.Spec.HTTPReq.Header, "Content-Type")
						if body != "" {
							m.Spec.HTTPReq.Header["Content-Type"] = corpusType(body)
						}
						recorded[method+" "+path+" "+body] = true
						if s {
							session, session2, session3 = append(session, m), append(session2, m.DeepCopy()), append(session3, m.DeepCopy())
						} else {
							perTest, perTest2, perTest3 = append(perTest, m), append(perTest2, m.DeepCopy()), append(perTest3, m.DeepCopy())
						}
					}
					keyed := NewMockManager(nil, nil, zap.NewNop())
					whole := NewMockManager(nil, nil, zap.NewNop())
					start, end := at.Add(-time.Second), at.Add(time.Minute)
					keyed.SetMocksWithWindow(perTest, session, start, end)
					whole.SetMocksWithWindow(perTest2, session2, start, end)
					refused := NewMockManager(nil, nil, zap.NewNop())
					refused.SetMocksWithWindow(perTest3, session3, start, end)
					h1, h2, h3 := httpint.New(zap.NewNop()), httpint.New(zap.NewNop()), httpint.New(zap.NewNop())
					walks := watchWalks(t, keyed.filtered)
					for c := 0; c < 60; c++ {
						method, path, body := wideReq(r)
						walked := walks.Load()
						a := serveWith(h1, keyed, opts, "", method, path, body)
						fast := walks.Load() == walked
						b := serveWith(h2, wholeReads{whole}, opts, "", method, path, body)
						if a != b {
							t.Fatalf("seed %d call %d (%s %s %q): the keyed read answered %q, the whole read %q", seed, c, method, path, body, a, b)
						}
						if d := serveWith(h3, sessionKeyRefused{refused}, opts, "", method, path, body); d != b {
							t.Fatalf("seed %d call %d (%s %s %q): with its keyed session read refused the store answered %q, the whole read %q", seed, c, method, path, body, d, b)
						}
						switch {
						case !strings.HasPrefix(a, "200 resp-"):
							missed++
						case !recorded[method+" "+path+" "+body]:
							served++
							if fast {
								unwalked++
							}
						}
					}
					keyed.Close()
					whole.Close()
					refused.Close()
				}
				t.Logf("served %d calls no recording's request is byte for byte, %d of them without a whole read; %d missed", served, unwalked, missed)
				if proven == "" && (served < 60 || missed < 60 || unwalked < served/2) {
					t.Fatalf("the corpora served %d calls by a pass after the exact one, %d without a whole read, and missed %d; the row needs many of each, most served by key", served, unwalked, missed)
				}
			})
		}
	}
}

// BenchmarkHTTPReplayLaxPerCall is one replayed call of a lax shape (see
// laxRecorded) against a set of n recordings, with the proven switch off. The
// set is restaged (untimed) every 200 calls, fewer than its groups, so each
// call is answered by a recording no call before it took, and calls are taken
// in a stride through the groups. The first call after a staging builds the
// indexes the shape reads and is timed apart, as first-ns.
func BenchmarkHTTPReplayLaxPerCall(b *testing.B) {
	for _, shape := range []string{"fuzzy", "id", "urlnoise", "miss", "pass2"} {
		for _, n := range []int{1000, 10000} {
			b.Run(shape+"/n="+strconv.Itoa(n), func(b *testing.B) {
				b.Setenv("KEPLOY_PROVEN", "0")
				const per = 200
				at := time.Unix(1_700_000_000, 0)
				h := httpint.New(zap.NewNop())
				mm := NewMockManager(nil, nil, zap.NewNop())
				defer mm.Close()
				var took, firstTook time.Duration
				var calls, firsts int
				b.ReportAllocs()
				for k := 0; k < b.N; k++ {
					first := k%per == 0
					if first {
						b.StopTimer()
						stageLax(mm, shape, n, at)
					}
					j := ((k % per) * 7919) % (n / 4)
					t0 := time.Now()
					err := laxServe(h, mm, shape, j)
					if d := time.Since(t0); first {
						firstTook += d
						firsts++
						b.StartTimer()
					} else {
						took += d
						calls++
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				if calls > 0 {
					b.ReportMetric(float64(took.Nanoseconds())/float64(calls), "call-ns")
				}
				if firsts > 0 {
					b.ReportMetric(float64(firstTook.Nanoseconds())/float64(firsts), "first-ns")
				}
			})
		}
	}
}
