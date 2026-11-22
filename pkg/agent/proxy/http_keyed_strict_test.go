package proxy

import (
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Under the proven switch with no exact view installed (an agent whose
// filters carry no exact mapping, as the integration-test runner's do) the
// matcher compares a call with the recordings that may be it alone: a GET of
// its own path and a POST of a path every POST shares, by its body. Each is
// served its recording through the stores' key indexes, examining a few keyed
// entries, in a set of 4000.
func TestAProvenCallWithNoExactViewReadsOnlyItsRecordings(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	const n = 4000
	at := time.Unix(1_700_000_000, 0)
	h := httpint.New(zap.NewNop())
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	stageScale(mm, n, at, "none")
	for k := 0; k < 64; k++ {
		i := (k * 7919) % n
		before := keyedExamined.Load()
		body, err := serveScale(h, mm, i)
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf(`{"id":"item-%d","ok":true}`, i); body != want {
			t.Fatalf("call %d served %q, want %q", i, body, want)
		}
		// The first call builds the index the calls read.
		if k == 0 {
			continue
		}
		if e := keyedExamined.Load() - before; e == 0 || e > 8 {
			t.Fatalf("call %d examined %d keyed entries; want its own few, read by key", i, e)
		}
	}
}

// watchTreeWalks counts the whole walks (TreeDb.rangeValues) of each of
// trees, by its place.
func watchTreeWalks(t *testing.T, trees ...*TreeDb) []*atomic.Int64 {
	t.Helper()
	n := make([]*atomic.Int64, len(trees))
	for i := range n {
		n[i] = new(atomic.Int64)
	}
	w := func(db *TreeDb) {
		for i, tree := range trees {
			if db == tree {
				n[i].Add(1)
			}
		}
	}
	rangeValuesWatch.Store(&w)
	t.Cleanup(func() { rangeValuesWatch.Store(nil) })
	return n
}

// strictCorpusMock is recording i of a corpus drawn by r for the strict
// pass: corpusMock's tiers, windows and cursors, of wideReq's requests, or of
// an earlier recording's (reqs), so requests repeat. Some learned noise of
// their body (a member it holds, or one it does not, which leaves the
// recording compared as a plain one would be but filed apart), or a place of
// their URL (one their path has, or one past it), and some declare a
// variable at their path's second segment.
func strictCorpusMock(r *rand.Rand, i int, at time.Time, reqs *[][3]string) (*models.Mock, bool) {
	m, s := corpusMock(r, i, at)
	method, path, body := wideReq(r)
	if len(*reqs) > 0 && r.Intn(3) == 0 {
		q := (*reqs)[r.Intn(len(*reqs))]
		method, path, body = q[0], q[1], q[2]
	}
	*reqs = append(*reqs, [3]string{method, path, body})
	m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body = models.Method(method), "http://upstream"+path, body
	p, q, _ := strings.Cut(path, "?")
	if q != "" {
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
	switch r.Intn(9) {
	case 0:
		m.Spec.ReqBodyNoise = map[string][]string{"body.x": {}}
	case 1:
		m.Spec.ReqBodyNoise = map[string][]string{"body.z": {}}
	case 2:
		m.Spec.ReqBodyNoise = map[string][]string{"url.path.1": {}}
	case 3:
		m.Spec.ReqBodyNoise = map[string][]string{"url.path.9": {}}
	case 4:
		if segs := strings.Split(strings.TrimPrefix(p, "/"), "/"); len(segs) > 1 {
			m.Spec.Variables = models.MockVariables{"id": {Recorded: segs[1], Bind: "url.path[1]"}}
		}
	}
	return m, s
}

// The keyed strict read answers every call under the proven switch with no
// exact view as the whole read answers it: over random corpora of repeated
// requests, stateful recordings, both tiers, mocks in and out of the window,
// recordings that learned noise of their body or a place of their URL, and
// ones that declare a variable, each call (a recording's request or another)
// is served the same recording, or missed with the same report, by a manager
// that reads by key, by one that reads the whole pool, and by one whose keyed
// session read fails: with no url noise, under url noise that relates paths
// (tok1 and tok22), and under a bare pattern that masks every digit run.
func TestAKeyedStrictReadAnswersAsTheWholeRead(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	noises := map[string][]string{"none": nil, "token": {"/s/tok[0-9]+"}, "digits": {"[0-9]+"}}
	for _, noise := range []string{"none", "token", "digits"} {
		t.Run("noise="+noise, func(t *testing.T) {
			var opts models.OutgoingOptions
			if pats := noises[noise]; pats != nil {
				opts.NoiseConfig = map[string]map[string][]string{"url": {}}
				for _, p := range pats {
					opts.NoiseConfig["url"][p] = nil
				}
			}
			at := time.Unix(1_700_000_000, 0)
			// served counts the calls answered by a recording; matched,
			// those the matcher answered (the whole manager walked its
			// per-test tier more than the learn step's once); fewer, those
			// of them the keyed manager answered walking its tier fewer
			// times.
			served, matched, fewer, missed := 0, 0, 0, 0
			for seed := int64(1); seed <= 16; seed++ {
				r := rand.New(rand.NewSource(seed))
				var perTest, session []*models.Mock
				var perTest2, session2, perTest3, session3 []*models.Mock
				var reqs [][3]string
				for i := 0; i < 40; i++ {
					m, s := strictCorpusMock(r, i, at, &reqs)
					if s {
						session, session2, session3 = append(session, m), append(session2, m.DeepCopy()), append(session3, m.DeepCopy())
					} else {
						perTest, perTest2, perTest3 = append(perTest, m), append(perTest2, m.DeepCopy()), append(perTest3, m.DeepCopy())
					}
				}
				keyed := NewMockManager(nil, nil, zap.NewNop())
				whole := NewMockManager(nil, nil, zap.NewNop())
				refused := NewMockManager(nil, nil, zap.NewNop())
				start, end := at.Add(-time.Second), at.Add(time.Minute)
				keyed.SetMocksWithWindow(perTest, session, start, end)
				whole.SetMocksWithWindow(perTest2, session2, start, end)
				refused.SetMocksWithWindow(perTest3, session3, start, end)
				h1, h2, h3 := httpint.New(zap.NewNop()), httpint.New(zap.NewNop()), httpint.New(zap.NewNop())
				walks := watchTreeWalks(t, keyed.filtered, whole.filtered)
				for c := 0; c < 60; c++ {
					method, path, body := wideReq(r)
					if r.Intn(2) == 0 {
						q := reqs[r.Intn(len(reqs))]
						method, path, body = q[0], q[1], q[2]
					}
					w0 := walks[0].Load()
					a := serveWith(h1, keyed, opts, "", method, path, body)
					w1 := walks[0].Load() - w0
					w0 = walks[1].Load()
					b := serveWith(h2, wholeReads{whole}, opts, "", method, path, body)
					w2 := walks[1].Load() - w0
					if a != b {
						t.Fatalf("seed %d call %d (%s %s %q): the keyed read answered %q, the whole read %q", seed, c, method, path, body, a, b)
					}
					if d := serveWith(h3, sessionKeyRefused{refused}, opts, "", method, path, body); d != b {
						t.Fatalf("seed %d call %d (%s %s %q): with its keyed session read refused the store answered %q, the whole read %q", seed, c, method, path, body, d, b)
					}
					if !strings.HasPrefix(a, "200 resp-") {
						missed++
						continue
					}
					served++
					if w2 > 1 {
						matched++
						if w1 < w2 {
							fewer++
						}
					}
				}
				keyed.Close()
				whole.Close()
				refused.Close()
			}
			t.Logf("served %d calls, the matcher %d of them, %d without its whole read; %d missed", served, matched, fewer, missed)
			if served < 200 || matched < 150 || missed < 60 || fewer < matched*9/10 {
				t.Fatalf("the corpora served %d calls, the matcher %d of them, %d without its whole read, and missed %d; the row needs many of each, nearly all the matcher's by key", served, matched, fewer, missed)
			}
		})
	}
}
