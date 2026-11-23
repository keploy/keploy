package http

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// keyedLearnDb is learnDb with no exact view that reads by key: each keyed
// walk (integrations.PerTestKeyRanger, SessionKeyReader) is learnDb's read
// of its tier narrowed to the recordings the index files under the key,
// counted with the recordings it visits (keyed, visited), as the reads of a
// whole tier are (whole). Its staging generation is gen. exact and sequence
// are what ExactServing and SequenceView report; failSession fails each
// keyed session walk; dropping leaves the recording of that name out of
// every keyed walk.
type keyedLearnDb struct {
	*learnDb
	gen                   uint64
	keyed, visited, whole int
	keys                  []string
	exact, sequence       bool
	failSession           bool
	dropping              string
}

func newKeyedLearnDb(perTest, session []*models.Mock) *keyedLearnDb {
	return &keyedLearnDb{learnDb: &learnDb{perTest: perTest, mockMemDb: mockMemDb{mocks: session}}, gen: 1}
}

func (d *keyedLearnDb) ExactServing() bool                         { return d.exact }
func (d *keyedLearnDb) StagingGeneration() uint64                  { return d.gen }
func (d *keyedLearnDb) StagingGenerationByKind(models.Kind) uint64 { return d.gen }
func (d *keyedLearnDb) MockSpent(string) bool                      { return false }
func (d *keyedLearnDb) SequenceView() (integrations.SequenceView, bool) {
	return integrations.SequenceView{}, d.sequence
}

func (d *keyedLearnDb) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	d.whole++
	return d.learnDb.GetPerTestMocksInWindow()
}

func (d *keyedLearnDb) GetSessionMocks() ([]*models.Mock, error) {
	d.whole++
	return d.learnDb.GetSessionMocks()
}

func (d *keyedLearnDb) walk(tier []*models.Mock, ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) {
	d.keyed++
	d.keys = append(d.keys, key)
	for _, m := range tier {
		if m.Name == d.dropping || !slices.Contains(ix.Keys(m), key) {
			continue
		}
		d.visited++
		if !fn(m) {
			return
		}
	}
}

func (d *keyedLearnDb) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	d.walk(d.perTest, ix, key, fn)
	return nil
}

func (d *keyedLearnDb) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if d.failSession {
		return errors.New("the keyed session read fails")
	}
	d.walk(d.mocks, ix, key, fn)
	return nil
}

// perTestKeyedOnly reads the per-test tier by key, and the session tier only
// whole.
type perTestKeyedOnly struct {
	*learnDb
	k *keyedLearnDb
}

func (p perTestKeyedOnly) RangePerTestMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	return p.k.RangePerTestMocksWithKey(ix, key, fn)
}

// A recording is filed under the keys the learn step reads it by with no
// index of its own: its method and masked path's, its body's print or the
// unprinted key (a form body, learned noise of its body), and, where a
// segment of its path may be excused (a learned place of its URL, an
// obfuscation noise), its method and segment count's too; one that declares
// variables, of no request, or of another kind, under none. A path is
// masked with the index's url noise, and one url noise has one index, at
// most noiseIndexesCap kept.
func TestTheLearnKeysOfARecording(t *testing.T) {
	h, n, _ := learnOn(t)
	plain := learnRecorded("plain", 0, "POST", "/v1/item", `{"x":1}`, nil, `{}`)
	sum, ok := learnPlainPrint(plain, recordedParts(plain), n)
	if !ok {
		t.Fatal("a plain JSON body has no print")
	}
	k := "POST v1/item"
	form := learnRecorded("form", 0, "POST", "/v1/item", "k=v", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `{}`)
	bodyNoise := learnRecorded("body-noise", 0, "POST", "/v1/item", `{"x":1}`, nil, `{}`)
	bodyNoise.Spec.ReqBodyNoise = map[string][]string{"body.x": {}}
	urlLearned := learnRecorded("url-learned", 0, "GET", "/v1/item/7", "", nil, `{}`)
	urlLearned.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}}
	obfuscated := learnRecorded("obfuscated", 0, "POST", "/v1/item", `{"x":1}`, nil, `{}`)
	obfuscated.Noise = []string{`^[0-9]$`}
	vars := learnRecorded("vars", 0, "GET", "/v1/item/7", "", nil, `{}`)
	vars.Spec.Variables = models.MockVariables{"id": {Recorded: "7", Bind: "url.path[2]"}}
	other := learnRecorded("other", 0, "POST", "/v1/item", `{"x":1}`, nil, `{}`)
	other.Kind = models.Kind("Redis")
	for _, c := range []struct {
		name string
		m    *models.Mock
		want []string
	}{
		{"plain", plain, []string{"p" + k, learnPrintKey(k, sum)}},
		{"form", form, []string{"p" + k, "u" + k}},
		{"learned body noise", bodyNoise, []string{"p" + k, "u" + k}},
		{"learned url", urlLearned, []string{"pGET v1/item/7", "uGET v1/item/7", "wGET 3"}},
		{"obfuscated", obfuscated, []string{"p" + k, learnPrintKey(k, sum), "wPOST 2"}},
		{"variables", vars, nil},
		{"no request", &models.Mock{Kind: models.HTTP}, nil},
		{"another kind", other, nil},
		{"nil", nil, nil},
	} {
		if got := learnKeys(c.m, n); !slices.Equal(got, c.want) {
			t.Errorf("%s: keys %q; want %q", c.name, got, c.want)
		}
	}
	if learnPrintKey(k, sum) == learnPrintKey(k, sum+1) || learnPrintKey(k, sum) == learnPrintKey("POST v1/other", sum) {
		t.Fatal("two prints, or two paths, share a print key")
	}
	masked := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{`v[0-9]+`})
	if got, want := learnKeys(plain, masked)[0], "p"+"POST "+masked.maskedPath("/v1/item"); got != want || got == "p"+k {
		t.Fatalf("under url noise the path key is %q; want %q, the masked path's", got, want)
	}
	a := learnKeyIndexOf(masked)
	if learnKeyIndexOf(h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{`v[0-9]+`})) != a || learnKeyIndexOf(n) == a {
		t.Fatal("one url noise has several indexes, or two share one")
	}
	if got := a.Keys(plain); !slices.Equal(got, learnKeys(plain, masked)) {
		t.Fatalf("the index files a recording under %q; want its keys under its url noise", got)
	}
	for i := 0; i < 3*noiseIndexesCap; i++ {
		learnKeyIndexOf(h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{"y" + strconv.Itoa(i)}))
		learnKeyIndexes.mu.Lock()
		size := len(learnKeyIndexes.m)
		learnKeyIndexes.mu.Unlock()
		if size > noiseIndexesCap {
			t.Fatalf("%d indexes kept; at most %d", size, noiseIndexesCap)
		}
	}
}

// A learned call with no index of its own reads, of each tier, the
// recordings of its key: where its body has a print, those of that print and
// those whose body is compared otherwise, else every one of its path, and
// those of its method and segment count a segment of whose path may be
// excused; each once, each tier in recorded order, the per-test tier first,
// each entry with its tier and its masked path. A store under exact serving
// or a stateful sequence view, one that reads no key, and one whose keyed
// read fails give none, and are read whole.
func TestAKeyedLearnReadReadsOnlyTheCallsKeys(t *testing.T) {
	h, _, _ := learnOn(t)
	n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, []string{`tok[0-9]+`})
	call := `{"id":1}`
	mine := learnRecorded("mine", 5, "POST", "/item", call, nil, `{}`)
	early := learnRecorded("early", 1, "POST", "/item", call, nil, `{}`)
	otherBody := learnRecorded("other-body", 2, "POST", "/item", `{"id":2}`, nil, `{}`)
	form := learnRecorded("form", 3, "POST", "/item", "id=1", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `{}`)
	// Learned a place of its URL: read as its path's unprinted recording
	// and as its method's wide one, once.
	wideHere := learnRecorded("wide-here", 4, "POST", "/item", call, nil, `{}`)
	wideHere.Spec.ReqBodyNoise = map[string][]string{"url.path.0": {}}
	wideThere := learnRecorded("wide-there", 0, "POST", "/other", `{"id":9}`, nil, `{}`)
	wideThere.Spec.ReqBodyNoise = map[string][]string{"url.path.0": {}}
	wideLonger := learnRecorded("wide-longer", 0, "POST", "/item/x", call, nil, `{}`)
	wideLonger.Spec.ReqBodyNoise = map[string][]string{"url.path.1": {}}
	get := learnRecorded("get", 0, "GET", "/item", "", nil, `{}`)
	tokened := learnRecorded("tokened", 6, "POST", "/s/tok7", call, nil, `{}`)
	sessionMine := learnRecorded("session-mine", 0, "POST", "/item", call, nil, `{}`)
	sessionMine.TestModeInfo.Lifetime = models.LifetimeSession
	perTest := []*models.Mock{mine, otherBody, wideThere, form, early, wideHere, wideLonger, get, tokened}
	session := []*models.Mock{sessionMine, otherBody}

	names := func(es []learnEntry) string {
		var out []string
		for _, e := range es {
			tier := "s"
			if e.perTest {
				tier = "t"
			}
			out = append(out, tier+":"+e.m.Name)
			if want := n.maskedPath(recordedParts(e.m).path); e.masked != want {
				t.Fatalf("%s's entry is masked %q; want %q", e.m.Name, e.masked, want)
			}
		}
		return strings.Join(out, " ")
	}
	sum, printed := n.learnLivePrint(liveParts(context.Background(), live("POST", "/item", call, nil)))
	if !printed {
		t.Fatal("a JSON call has no print")
	}
	db := newKeyedLearnDb(perTest, session)
	es, ok := keyedLearnEntries(db, n, "POST", "item", sum, true)
	if want := "t:wide-there t:early t:form t:wide-here t:mine s:session-mine"; !ok || names(es) != want {
		t.Fatalf("the printed call's entries are %q (%v); want %q", names(es), ok, want)
	}
	if want := []string{learnPrintKey("POST item", sum), "uPOST item", "wPOST 1"}; db.keyed != 6 || db.whole != 0 || !slices.Equal(slices.Compact(slices.Clone(db.keys)), want) {
		t.Fatalf("the store walked %d keys %q and was read whole %d times; want each of %q in each tier, never whole", db.keyed, db.keys, db.whole, want)
	}
	db = newKeyedLearnDb(perTest, session)
	es, ok = keyedLearnEntries(db, n, "POST", "item", 0, false)
	if want := "t:wide-there t:early t:other-body t:form t:wide-here t:mine s:session-mine s:other-body"; !ok || names(es) != want {
		t.Fatalf("the unprinted call's entries are %q (%v); want %q", names(es), ok, want)
	}
	if want := []string{"pPOST item", "wPOST 1"}; !slices.Equal(slices.Compact(slices.Clone(db.keys)), want) {
		t.Fatalf("the unprinted call walked %q; want %q", db.keys, want)
	}
	es, ok = keyedLearnEntries(newKeyedLearnDb(perTest, session), n, "POST", n.maskedPath("/s/tok9"), sum, true)
	if want := "t:wide-longer t:tokened"; !ok || names(es) != want {
		t.Fatalf("a call under url noise read %q (%v); want %q, its masked path's and its segment count's wide one", names(es), ok, want)
	}

	for _, c := range []struct {
		name string
		db   func() (integrations.MockMemDb, *keyedLearnDb)
	}{
		{"exact serving", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb(perTest, session)
			d.exact = true
			return d, d
		}},
		{"a sequence view", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb(perTest, session)
			d.sequence = true
			return d, d
		}},
		{"no keyed read", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb(perTest, session)
			return d.learnDb, d
		}},
		{"no keyed session read", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb(perTest, session)
			return perTestKeyedOnly{learnDb: d.learnDb, k: d}, d
		}},
	} {
		db, d := c.db()
		if es, ok := keyedLearnEntries(db, n, "POST", "item", sum, true); ok || es != nil || d.keyed != 0 {
			t.Fatalf("%s: read %q (%v) in %d keyed walks; want none", c.name, names(es), ok, d.keyed)
		}
	}
	d := newKeyedLearnDb(perTest, session)
	d.failSession = true
	if es, ok := keyedLearnEntries(d, n, "POST", "item", sum, true); ok || es != nil {
		t.Fatalf("a failed keyed session read gave %q (%v); want none", names(es), ok)
	}
}

// A learned call with no index of its own is grouped as a read of the whole
// store groups it, reading the store by key alone: over random stores of
// both tiers, bodies, body noise, learned places of the URL, obfuscation
// noise, recordings that declare variables, url noise, an open test or none,
// and per-test recordings consumed between calls, the group and the owner of
// each recording of it are the read's, and no call reads a tier whole.
func TestAKeyedLearnedGroupIsTheOneAReadGives(t *testing.T) {
	h, _, _ := learnOn(t)
	verifyOff(t)
	rng := rand.New(rand.NewSource(13))
	paths := []string{"/v1/item/1", "/v2/item/1", "/v1/item/2", "/v1/item", "/v1/user/1", "/v3/item/7"}
	compared, grouped, narrowed, narrowedGrouped := 0, 0, 0, 0
	for round := 0; round < 400; round++ {
		urlNoise := []string(nil)
		if rng.Intn(2) == 0 {
			urlNoise = []string{`v[0-9]+`}
		}
		n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, learnBodyNoiseOf(rng), urlNoise)
		var perTest, session []*models.Mock
		for i := 0; i < 2+rng.Intn(14); i++ {
			method := []string{"GET", "POST"}[rng.Intn(2)]
			body, header := learnBodyOf(rng)
			m := learnRecorded(fmt.Sprintf("r%d-%d", round, i), time.Duration(rng.Intn(4)), method, paths[rng.Intn(len(paths))], body, header, `{}`)
			switch rng.Intn(7) {
			case 3:
				m.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
			case 0:
				m.Spec.ReqBodyNoise = map[string][]string{"url.path.2": {}}
			case 1:
				m.Noise = []string{`^[0-9]$`}
			case 2:
				m.Spec.Variables = models.MockVariables{"x": {}}
			}
			if rng.Intn(2) == 0 {
				perTest = append(perTest, m)
			} else {
				m.TestModeInfo.Lifetime = models.LifetimeSession
				session = append(session, m)
			}
		}
		db := newKeyedLearnDb(perTest, session)
		ctx := context.Background()
		if rng.Intn(3) > 0 {
			tr := learn.NewTracker()
			tr.Begin()
			ctx = learn.WithTracker(ctx, tr)
		}
		for call := 0; call < 6; call++ {
			if len(db.perTest) > 0 && rng.Intn(3) == 0 {
				_ = db.DeleteFilteredMock(*db.perTest[rng.Intn(len(db.perTest))])
			}
			in := learnCallOf(rng, paths, append(slices.Clone(perTest), session...))
			lp := liveParts(ctx, in)
			narrows := learnNarrows(n, lp, append(slices.Clone(perTest), session...))
			ls := n.shape(lp, sameDest)
			r, err := h.learnReads(db, learn.TrackerFrom(ctx))
			if err != nil {
				t.Fatal(err)
			}
			sc := r.scope(lp, ls, n, true)
			want, wantOwner := exactGroup(ls, sc, false), sc.owner()
			whole, keyed := db.whole, db.keyed
			got, gotOwner, err := h.learnedGroupOf(ctx, db, db, learn.TrackerFrom(ctx), lp, ls, n, call%2 == 1)
			if err != nil {
				t.Fatal(err)
			}
			if db.whole != whole || db.keyed == keyed {
				t.Fatalf("round %d: the call read the store whole %d times and by key %d times; want by key alone", round, db.whole-whole, db.keyed-keyed)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("round %d call %s %s (url noise %v): keyed %v, read %v", round, in.method, in.url.Path, urlNoise, strictNames(got), strictNames(want))
			}
			for _, m := range got {
				if gotOwner(m) != wantOwner(m) {
					t.Fatalf("round %d: %s own %v by key, %v from a read", round, m.Name, gotOwner(m), wantOwner(m))
				}
			}
			compared++
			if len(want) > 0 {
				grouped++
			}
			if narrows {
				narrowed++
				if len(want) > 0 {
					narrowedGrouped++
				}
			}
		}
	}
	if grouped < compared/5 || narrowed < compared/20 || narrowedGrouped < 30 {
		t.Fatalf("%d of %d calls had a group, %d were read through a bucket their body's print narrowed, %d of those had a group: the row compares too little", grouped, compared, narrowed, narrowedGrouped)
	}
}

// In the exact view's verify mode a keyed learn read that lost a recording
// a read finds the call exactly panics, while the store's staging generation
// holds; one that lost none passes.
func TestAKeyedLearnReadThatLostAMatchPanicsInVerifyMode(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	verifyOf(h, true)
	m := learnRecorded("mock-0", 0, "GET", "/item/1", "", nil, `{}`)
	db := newKeyedLearnDb([]*models.Mock{m}, nil)
	db.gen = 7
	in := live("GET", "/item/1", "", nil)
	lp := liveParts(context.Background(), in)
	ls := n.shape(lp, sameDest)
	if g, _, err := h.learnedGroupOf(context.Background(), db, db, nil, lp, ls, n, false); err != nil || len(g) != 1 || db.keyed == 0 {
		t.Fatalf("group %v err %v in %d keyed walks; want mock-0, by key", strictNames(g), err, db.keyed)
	}
	db.dropping = "mock-0"
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "indexed learned group") {
			t.Fatalf("recovered %v; want the verify mode's panic", r)
		}
	}()
	_, _, _ = h.learnedGroupOf(context.Background(), db, db, nil, lp, ls, n, false)
}
