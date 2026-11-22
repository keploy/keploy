package http

import (
	"errors"
	"slices"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// listRanger hands a key's walk the mocks of lists, in order, counting what
// it handed (visited), or fails with err.
type listRanger struct {
	lists   map[string][]*models.Mock
	err     error
	visited int
}

func (l *listRanger) walk(key string, fn func(*models.Mock) bool) error {
	if l.err != nil {
		return l.err
	}
	for _, m := range l.lists[key] {
		l.visited++
		if !fn(m) {
			return nil
		}
	}
	return nil
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
