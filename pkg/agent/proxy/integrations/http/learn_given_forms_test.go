package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// learnGiven runs record's full round on one POST /v1/apps recorded with
// recBody while the open test gave name (rec as recorded, full now), and
// sent liveBody: the mock it served, or the miss.
func learnGiven(t *testing.T, recBody, liveBody, name, rec, full string) (*models.Mock, *learnMiss) {
	t.Helper()
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-0", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", recBody, nil, `{"id":7}`)
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{m}}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: name, Value: full, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}); !ok {
		t.Fatal("the test's given did not bind")
	}
	tr := learn.NewTracker()
	tr.Begin()
	tr.GiveValue(learn.TestGiven("test o::TestApps", name, rec, full))
	served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/apps", liveBody, nil), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served == nil {
		return nil, miss
	}
	return &db.consumed[0], nil
}

// A short given the call carries whole beside the application's createdAt,
// whose text holds it as a fraction of its time ("00" of "09:00:00"): the
// given is its variable at qty only, and createdAt is learned as the clock
// read (MockSpec.Clocks, never noise), as with no given.
func TestLearnMatchAShortGivenIsNoPartOfCreatedAt(t *testing.T) {
	recAt := learnAt.Add(-time.Hour) // 09:00:00
	body := func(q string, at time.Time) string {
		return `{"qty":"` + q + `","createdAt":"` + at.UTC().Format(time.RFC3339) + `"}`
	}
	m, miss := learnGiven(t, body("00", recAt), body("01", recAt.Add(learn.FullRoundLead)), "given:slot", "00", "01")
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	if v := m.Spec.Learned()["given:slot"]; !reflect.DeepEqual(v.Use, []string{"body.qty"}) || len(m.Spec.Learned()) != 1 {
		t.Fatalf("variables %+v, want given:slot used at body.qty only", m.Spec.Learned())
	}
	if _, ok := m.Spec.Clocks["body.createdAt"]; !ok || len(m.Spec.ReqBodyNoise) != 0 {
		t.Fatalf("clocks %v noise %v, want body.createdAt learned as the clock read", m.Spec.Clocks, m.Spec.ReqBodyNoise)
	}
}

// A value made from a UnixNano nonce: its exact text inside the value,
// between words or glued to letters, is refused naming the given; a hash or
// a cut of it learns as it would with no given, here noise (a stated limit:
// give the value the application sends).
func TestLearnMatchAValueMadeFromAGivenByItsForm(t *testing.T) {
	const rec, full = "1761131656960000000", "1761225360000000001"
	hash := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	for form, c := range map[string]struct {
		of      func(string) string
		refused bool
	}{
		"app-<nonce>":             {func(s string) string { return "app-" + s }, true},
		"app<nonce>":              {func(s string) string { return "app" + s }, true},
		"sha256":                  {hash, false},
		"app-<its last 6 digits>": {func(s string) string { return "app-" + s[len(s)-6:] }, false},
	} {
		body := func(n string) string { return `{"name":"` + c.of(n) + `"}` }
		m, miss := learnGiven(t, body(rec), body(full), "given:nonce", rec, full)
		switch {
		case c.refused && (miss == nil || !strings.Contains(miss.res.Why, learn.GivenDerivedWhy("body.name", "given:nonce", "apps_test.go:20"))):
			t.Errorf("%s: served %v miss %+v; want it refused naming given:nonce", form, m, miss)
		case !c.refused && miss != nil:
			t.Errorf("%s: refused (%s); want it learned as with no given", form, miss.res.Why)
		case !c.refused:
			if _, ok := m.Spec.ReqBodyNoise["body.name"]; !ok || len(m.Spec.Learned()) != 0 {
				t.Errorf("%s: noise %v variables %+v; want body.name noise, as with no given", form, m.Spec.ReqBodyNoise, m.Spec.Learned())
			}
		}
	}
}

// A nonce sent as a JSON number is the given's whole.
func TestLearnMatchANumberNonceIsTheGivensWhole(t *testing.T) {
	const rec, full = "1761131656960000000", "1761225360000000001"
	m, miss := learnGiven(t, `{"nonce":`+rec+`}`, `{"nonce":`+full+`}`, "given:nonce", rec, full)
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	if v := m.Spec.Learned()["given:nonce"]; !reflect.DeepEqual(v.Use, []string{"body.nonce"}) {
		t.Fatalf("variables %+v, want given:nonce used at body.nonce", m.Spec.Learned())
	}
}

// "user-<Unix s>" or "user-<Unix ms>", the common test name, beside the
// application's createdAt of the same second in four forms: the name is the
// given's, createdAt is not, in either test-clock regime.
func TestLearnMatchAPrefixedUnixNonceBesideCreatedAt(t *testing.T) {
	recAt := learnAt.Add(-time.Hour)
	for gn, g := range map[string]func(time.Time) string{
		"user-<unix s>":  func(c time.Time) string { return "user-" + strconv.FormatInt(c.Unix(), 10) },
		"user-<unix ms>": func(c time.Time) string { return "user-" + strconv.FormatInt(c.UnixMilli(), 10) },
	} {
		for fn, f := range map[string]func(time.Time) string{
			"unix s string": func(c time.Time) string { return `"` + strconv.FormatInt(c.Unix(), 10) + `"` },
			"unix s number": func(c time.Time) string { return strconv.FormatInt(c.Unix(), 10) },
			"unix ms":       func(c time.Time) string { return strconv.FormatInt(c.Add(3*time.Millisecond).UnixMilli(), 10) },
			"RFC 3339":      func(c time.Time) string { return `"` + c.UTC().Format(time.RFC3339) + `"` },
		} {
			for rn, full := range map[string]time.Time{"moved": recAt.Add(learn.FullRoundLead + 5*time.Millisecond), "real": recAt.Add(26 * time.Hour)} {
				body := func(n string, at time.Time) string { return `{"name":"` + n + `","createdAt":` + f(at) + `}` }
				m, miss := learnGiven(t, body(g(recAt), recAt), body(g(full), recAt.Add(learn.FullRoundLead)), "given:user", g(recAt), g(full))
				if miss != nil {
					t.Errorf("%s, createdAt %s, %s: refused: %s", gn, fn, rn, miss.res.Why)
					continue
				}
				if v := m.Spec.Learned()["given:user"]; !reflect.DeepEqual(v.Use, []string{"body.name"}) {
					t.Errorf("%s, createdAt %s, %s: given uses %v, want body.name only; noise %v", gn, fn, rn, v.Use, m.Spec.ReqBodyNoise)
				}
			}
		}
	}
}

// Stated limit: a bare Unix-seconds nonce the application's createdAt equals
// in the same second cannot be told from that clock read by its text. With
// the test's clock moved with the application's, createdAt is taken as a use
// of the given (a replay then needs the two reads in one second); on real
// time it is refused, naming the given. Give a distinctive value.
func TestLearnMatchAUnixSecondsNonceEqualToCreatedAtIsTheGivens(t *testing.T) {
	recAt := learnAt.Add(-time.Hour)
	s := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	body := func(n string, at time.Time) string { return `{"nonce":"` + n + `","createdAt":"` + s(at) + `"}` }
	moved := recAt.Add(learn.FullRoundLead + 5*time.Millisecond)
	m, miss := learnGiven(t, body(s(recAt), recAt), body(s(moved), recAt.Add(learn.FullRoundLead)), "given:nonce", s(recAt), s(moved))
	if miss != nil || !reflect.DeepEqual(m.Spec.Learned()["given:nonce"].Use, []string{"body.createdAt", "body.nonce"}) {
		t.Fatalf("moved with the app's: served %v miss %+v; the stated limit takes createdAt as a use of given:nonce", m, miss)
	}
	onReal := recAt.Add(26 * time.Hour)
	if _, miss = learnGiven(t, body(s(recAt), recAt), body(s(onReal), recAt.Add(learn.FullRoundLead)), "given:nonce", s(recAt), s(onReal)); miss == nil || !strings.Contains(miss.res.Why, "given:nonce") {
		t.Fatalf("on real time: miss %+v; the stated limit refuses createdAt naming given:nonce", miss)
	}
}
