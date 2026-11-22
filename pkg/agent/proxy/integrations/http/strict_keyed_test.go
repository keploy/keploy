package http

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// listRanger hands a key's walk the mocks of lists, in order, counting what
// it handed (visited), or fails with err (before walking) or errAfter (after
// walking completes or fn stops early).
type listRanger struct {
	lists    map[string][]*models.Mock
	err      error
	errAfter error
	visited  int
}

func (l *listRanger) walk(key string, fn func(*models.Mock) bool) error {
	if l.err != nil {
		return l.err
	}
	for _, m := range l.lists[key] {
		l.visited++
		if !fn(m) {
			return l.errAfter
		}
	}
	return l.errAfter
}

func (l *listRanger) RangePerTestMocksWithKey(_ *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	return l.walk(key, fn)
}

func (l *listRanger) RangeSessionMocksWithKey(_ *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	return l.walk(key, fn)
}

// Fits found in two lists of a tier are put in the order of the tier's list
// of the call's method, read from the tier's own walk and only until each is
// found, each once; a list that cannot be read, or no longer holds every
// fit, gives none.
func TestFitsOfTwoListsAreInTheOrderOfTheirMethodsList(t *testing.T) {
	ms := make([]*models.Mock, 6)
	for i := range ms {
		ms[i] = &models.Mock{Name: "m" + strconv.Itoa(i)}
	}
	key := strictMethodKey("GET")
	perTest := &listRanger{lists: map[string][]*models.Mock{key: {ms[0], ms[1], ms[1], ms[2], ms[3], ms[4]}}}
	session := &listRanger{lists: map[string][]*models.Mock{key: {ms[5], ms[3], ms[0]}}}
	of := [][]*models.Mock{{ms[3]}, {ms[1], ms[0]}}

	got, ok := strictFitsInOrder(perTest, session, nil, 0, "GET", of)
	if want := []*models.Mock{ms[0], ms[1], ms[3]}; !ok || !slices.Equal(got, want) {
		t.Fatalf("the per-test tier's fits are %v (%v); want %v", strictNames(got), ok, strictNames(want))
	}
	if perTest.visited != 5 || session.visited != 0 {
		t.Fatalf("the walk visited %d per-test and %d session entries; want the per-test tier's up to its last fit, 5", perTest.visited, session.visited)
	}
	got, ok = strictFitsInOrder(perTest, session, nil, 1, "GET", of)
	if want := []*models.Mock{ms[3], ms[0]}; ok || !slices.Equal(got, want) {
		t.Fatalf("the session tier, which no longer holds m1, gave %v (%v); want %v and not ok", strictNames(got), ok, strictNames(want))
	}
	session.lists[key] = []*models.Mock{ms[1], ms[3], ms[0]}
	if got, ok = strictFitsInOrder(perTest, session, nil, 1, "GET", of); !ok || !slices.Equal(got, []*models.Mock{ms[1], ms[3], ms[0]}) {
		t.Fatalf("the session tier's fits are %v (%v); want m1 m3 m0", strictNames(got), ok)
	}
	session.err = errors.New("refused")
	if _, ok = strictFitsInOrder(perTest, session, nil, 1, "GET", of); ok {
		t.Fatal("a method list that cannot be read gave the fits an order")
	}
	session.err = nil
	session.errAfter = errors.New("walk ended with error")
	if _, ok = strictFitsInOrder(perTest, session, nil, 1, "GET", of); ok {
		t.Fatal("a method list whose walk returned an error after finding all fits gave them an order")
	}
}

// A recording is filed under the keys the strict pass reads it by: one that
// declares variables under the variables' key alone, one that learned a
// place of its URL under its method's and the wild key, any other under its
// method's, its path's, and its body's print or the unprinted key; a mock of
// another kind, or of no request, under none. One url noise has one index.
func TestTheStrictKeysOfARecording(t *testing.T) {
	rec := func(method, url, body, ctype string) *models.Mock {
		return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: models.Method(method), URL: url, Body: body, Header: map[string]string{"Content-Type": ctype}}}}
	}
	n := strictNoise{}
	k := "POST a/b"
	plain := rec("POST", "http://h/a/b", `{"x":1}`, "application/json")
	sum, ok := plainPrint(plain)
	if !ok {
		t.Fatal("a plain JSON body has no print")
	}
	vars := rec("GET", "http://h/a/b", "", "")
	vars.Spec.Variables = models.MockVariables{"id": {Recorded: "b", Bind: "url.path[1]"}}
	noReq := &models.Mock{Kind: models.HTTP}
	noReq.Spec.Variables = vars.Spec.Variables
	wild := rec("GET", "http://h/a/b", "", "")
	wild.Spec.ReqBodyNoise = map[string][]string{"url.path.1": {}}
	for _, c := range []struct {
		name string
		m    *models.Mock
		want []string
	}{
		{"plain", plain, []string{"mPOST", "s" + k, strictPrintKey(k, sum)}},
		{"form", rec("POST", "http://h/a/b", "k=v", "application/x-www-form-urlencoded"), []string{"mPOST", "s" + k, "u" + k}},
		{"variables", vars, []string{strictVarsKey}},
		{"variables of no request", noReq, []string{strictVarsKey}},
		{"learned url", wild, []string{"mGET", "wGET"}},
		{"no request", &models.Mock{Kind: models.HTTP}, nil},
		{"another kind", &models.Mock{Kind: models.Kind("Redis"), Spec: plain.Spec}, nil},
		{"nil", nil, nil},
	} {
		if got := strictKeys(c.m, n); !slices.Equal(got, c.want) {
			t.Errorf("%s: keys %q; want %q", c.name, got, c.want)
		}
	}
	if strictPrintKey(k, sum) == strictPrintKey(k, sum+1) || strictPrintKey(k, sum) == strictPrintKey("POST a/c", sum) {
		t.Fatal("two prints, or two paths, share a print key")
	}
	h := &HTTP{Logger: zap.NewNop()}
	a := strictKeyIndexOf(h.newStrictNoise(nil, nil, []string{"x[0-9]+"}))
	if strictKeyIndexOf(h.newStrictNoise(nil, nil, []string{"x[0-9]+"})) != a || strictKeyIndexOf(h.newStrictNoise(nil, nil, nil)) == a {
		t.Fatal("one url noise has several indexes, or two share one")
	}
	for i := 0; i < 3*noiseIndexesCap; i++ {
		strictKeyIndexOf(h.newStrictNoise(nil, nil, []string{"y" + strconv.Itoa(i)}))
		strictKeyIndexes.mu.Lock()
		size := len(strictKeyIndexes.m)
		strictKeyIndexes.mu.Unlock()
		if size > noiseIndexesCap {
			t.Fatalf("%d indexes kept; at most %d", size, noiseIndexesCap)
		}
	}
}

// A strict call under the switch reads by key only from a store with no
// exact view and no stateful sequence view that reads both tiers by key: it
// is served its recording by key from such a store, and from one under
// exact serving or a sequence view, one with no keyed session read, and one
// that reads no key, it reads no key and leaves the call to the whole read.
func TestAKeyedStrictReadReadsByKeyOnlyWhereNoViewServes(t *testing.T) {
	h, _, _ := learnOn(t)
	n := h.newStrictNoise(nil, nil, nil)
	m := learnRecorded("mock-0", 0, "POST", "/item", `{"id":1}`, nil, `{}`)
	in := live("POST", "/item", `{"id":1}`, nil)
	db := newKeyedLearnDb([]*models.Mock{m}, nil)
	served, retry, err := h.keyedStrictRead(context.Background(), in, db, varScope{}, n, nil, nil, nil, true)
	if err != nil || retry || served == nil || served.Name != "mock-0" || db.keyed == 0 || db.whole != 0 {
		t.Fatalf("served %v (retry %v, err %v) in %d keyed walks and %d whole reads; want mock-0, by key alone", served, retry, err, db.keyed, db.whole)
	}
	for _, c := range []struct {
		name string
		db   func() (integrations.MockMemDb, *keyedLearnDb)
	}{
		{"exact serving", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb([]*models.Mock{m}, nil)
			d.exact = true
			return d, d
		}},
		{"a sequence view", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb([]*models.Mock{m}, nil)
			d.sequence = true
			return d, d
		}},
		{"no keyed session read", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb([]*models.Mock{m}, nil)
			return perTestKeyedOnly{learnDb: d.learnDb, k: d}, d
		}},
		{"no keyed read", func() (integrations.MockMemDb, *keyedLearnDb) {
			d := newKeyedLearnDb([]*models.Mock{m}, nil)
			return d.learnDb, d
		}},
	} {
		db, d := c.db()
		served, retry, err := h.keyedStrictRead(context.Background(), in, db, varScope{}, n, nil, nil, nil, true)
		if served != nil || retry || err != nil || d.keyed != 0 {
			t.Fatalf("%s: served %v (retry %v, err %v) in %d keyed walks; want no answer and no key read", c.name, served, retry, err, d.keyed)
		}
	}
}

type keyedVarDb struct {
	*keyedLearnDb
	table variables.Table
}

func (d *keyedVarDb) Variables() *variables.Table { return &d.table }

// A keyed strict read advances the stateful cursor for variable mocks only when
// serveVariables succeeds: repeated calls advance through two stateful variable
// mocks in record order, and a render problem returns no answer without
// advancing the cursor.
func TestAKeyedStrictReadStatefulVariableCursor(t *testing.T) {
	h, _, _ := learnOn(t)
	n := h.newStrictNoise(nil, nil, nil)
	mk := func(name string, offset time.Duration, respBody string) *models.Mock {
		m := varMock(name, "GET", "http://api.test/item/rec", "", 200, respBody, models.MockVariables{
			"id": {Recorded: "rec", Bind: "url.path[1]", Echo: true, Answer: []string{"body.id"}},
		})
		m.Spec.HTTPReq.Header["Host"] = "api.test"
		m.Spec.ReqTimestampMock = recordedAt.Add(offset)
		m.TestModeInfo.Lifetime = models.LifetimeSession
		m.TestModeInfo.Consume = models.ConsumeCursorSaturate
		return m
	}

	// Two stateful variable mocks advance 1 -> 2 on repeated calls.
	m1 := mk("v-1", 0, `{"id":"rec","seq":1}`)
	m2 := mk("v-2", time.Second, `{"id":"rec","seq":2}`)
	db := &keyedVarDb{keyedLearnDb: newKeyedLearnDb(nil, []*models.Mock{m1, m2})}
	db.updateUnFilteredReturn = true
	call := live("GET", "/item/live1", "", map[string]string{"Content-Type": "application/json"})
	scope := readVarScope(db)
	got1, retry, err := h.keyedStrictRead(context.Background(), call, db, scope, n, nil, nil, nil, true)
	if err != nil || retry || got1 == nil || !strings.Contains(got1.Spec.HTTPResp.Body, `"seq":1`) {
		t.Fatalf("first call: served %v (retry %v, err %v); want seq:1", got1, retry, err)
	}
	got2, retry, err := h.keyedStrictRead(context.Background(), call, db, scope, n, nil, nil, nil, true)
	if err != nil || retry || got2 == nil || !strings.Contains(got2.Spec.HTTPResp.Body, `"seq":2`) {
		t.Fatalf("second call: served %v (retry %v, err %v); want seq:2", got2, retry, err)
	}

	// A render problem leaves the cursor untouched and falls back to the whole read.
	bad := mk("v-bad", 0, `{"other":1}`)
	good := mk("v-good", time.Second, `{"id":"rec","seq":2}`)
	db2 := &keyedVarDb{keyedLearnDb: newKeyedLearnDb(nil, []*models.Mock{bad, good})}
	db2.updateUnFilteredReturn = true
	served, retry, err := h.keyedStrictRead(context.Background(), call, db2, readVarScope(db2), n, nil, nil, nil, true)
	if served != nil || retry || err != nil {
		t.Fatalf("render problem: served %v (retry %v, err %v); want (nil, false, nil)", served, retry, err)
	}
	if idx := db2.cursors[servedKey(bad)]; idx != 0 {
		t.Fatalf("render problem advanced stateful cursor to %d; want 0", idx)
	}
}
