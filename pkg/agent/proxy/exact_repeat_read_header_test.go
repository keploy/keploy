package proxy

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// headerRun serves mapping as test "t"'s own recordings, its matcher handed
// opts and its proof the noise of proof (nil: never set, which excuses no
// header), sends sends through the store view(mm) gives (the manager itself
// when view is nil) and returns the sends that missed and the recordings its
// end charged as never made.
func headerRun(t *testing.T, mapping []*models.Mock, opts models.OutgoingOptions, proof *models.OutgoingOptions, view func(*MockManager) integrations.MockMemDb, sends []string) (missed []int, unmade []string) {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	if proof != nil {
		mm.SetRepeatReadNoise(*proof)
	}
	mm.SetMocksWithWindow(nil, mapping, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("t", mapping)
	var db integrations.MockMemDb = mm
	if view != nil {
		db = view(mm)
	}
	for i, req := range sends {
		if r := sendForMissLogged(t, context.Background(), zap.NewNop(), db, opts, nil, req); r != nil {
			missed = append(missed, i)
		}
	}
	test, _, left, ok := mm.ExactTestOutcome()
	if !ok || test != "t" {
		t.Fatalf("not serving t exactly at its end: %q %v", test, ok)
	}
	return missed, left
}

// usersReads are two recordings of GET /api/notifications with one answer,
// one per user (Cookie), both named one read.
func usersReads(prefix string) []*models.Mock {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var mapping []*models.Mock
	for i, sid := range []string{"a", "b"} {
		mk := readMock(fmt.Sprintf("%s-%d", prefix, i), t0.Add(time.Duration(i)*time.Second), "GET", "/api/notifications", "", `{"data":[]}`)
		mk.Spec.HTTPReq.Header["Cookie"] = "sid=" + sid
		mk.TestModeInfo.RepeatRead = models.RepeatReadMark{Name: "GET /api/notifications", At: 1}
		mapping = append(mapping, mk)
	}
	return mapping
}

// asUser is GET /api/notifications as user sid.
func asUser(sid string) string {
	return "GET /api/notifications HTTP/1.1\r\nHost: localhost:8083\r\nCookie: sid=" + sid + "\r\n\r\n"
}

// Two recordings the strict matcher tells apart by a header value it
// compares (Cookie: two users), with one URL, no body and one answer, are two
// calls, each recorded once. Neither is a repeated read, even named one: a
// test that never makes the second is charged for it.
func TestTwoCallsAHeaderValueTellsApartAreNotOneRead(t *testing.T) {
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			for _, named := range []bool{false, true} {
				mapping := usersReads(fmt.Sprintf("n%v", named))
				if !named {
					for _, mk := range mapping {
						mk.TestModeInfo.RepeatRead = models.RepeatReadMark{}
					}
				}
				missed, unmade := headerRun(t, mapping, models.OutgoingOptions{}, &models.OutgoingOptions{}, nil, []string{asUser("a")})
				if len(missed) != 0 {
					t.Fatalf("named=%v: user a's recorded call missed: %v", named, missed)
				}
				if want := []string{mapping[1].Name}; !reflect.DeepEqual(unmade, want) {
					t.Errorf("named=%v: user b's call, recorded once and never made, was charged %v; want %v", named, unmade, want)
				}
			}
		})
	}
}

// User a's call recorded once and user b's once, one answer, named one read:
// a second call as user a is one more than recorded, and misses.
func TestACallRecordedOnceIsNotAnsweredOnceMoreForASiblingsHeader(t *testing.T) {
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			missed, _ := headerRun(t, usersReads("u"), models.OutgoingOptions{}, &models.OutgoingOptions{}, nil, []string{asUser("a"), asUser("b"), asUser("a")})
			if !reflect.DeepEqual(missed, []int{2}) {
				t.Errorf("user a's second call, recorded once: missed %v; want [2]", missed)
			}
		})
	}
}

// pageReads are three recordings of one read asked from three pages
// (Referer), every one with one answer.
func pageReads() []*models.Mock {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var mapping []*models.Mock
	for i := range 3 {
		mk := readMock(fmt.Sprintf("page-%d", i), t0.Add(time.Duration(i)*time.Second), "GET", "/api/onboarding", "", `{"done":false}`)
		mk.Spec.HTTPReq.Header["Referer"] = fmt.Sprintf("http://localhost:8083/crm/leads/%d", i)
		mk.TestModeInfo.RepeatRead = models.RepeatReadMark{Name: "GET /api/onboarding", At: 1}
		mapping = append(mapping, mk)
	}
	return mapping
}

// fromPage is GET /api/onboarding asked from page.
func fromPage(page string) string {
	return "GET /api/onboarding HTTP/1.1\r\nHost: localhost:8083\r\nReferer: http://localhost:8083/crm/" + page + "\r\n\r\n"
}

// A header keploy.yml names noise (the Frappe app names Referer) is excused
// by the matcher, so recordings that differ only there are one read: the
// session's noise (SetRepeatReadNoise) proves it, and one more is answered,
// one fewer excused. Without that noise the proof excuses no header, and
// the read keeps its exact count; with the noise, a fourth call more misses.
func TestARepeatedReadIsProvedUnderTheSessionsHeaderNoise(t *testing.T) {
	referer := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"Referer": {}}}}
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			four := []string{fromPage("a"), fromPage("b"), fromPage("c"), fromPage("d")}
			if missed, unmade := headerRun(t, pageReads(), referer, &referer, nil, four); len(missed) != 0 || len(unmade) != 0 {
				t.Errorf("under keploy.yml's Referer noise: missed %v, unmade %v; want none", missed, unmade)
			}
			if missed, _ := headerRun(t, pageReads(), referer, &referer, nil, append(four, fromPage("e"))); !reflect.DeepEqual(missed, []int{4}) {
				t.Errorf("under keploy.yml's Referer noise, two more than recorded: missed %v; want [4]", missed)
			}
			if _, unmade := headerRun(t, pageReads(), referer, &referer, nil, four[:2]); len(unmade) != 0 {
				t.Errorf("under keploy.yml's Referer noise, one fewer: unmade %v; want none", unmade)
			}
			if missed, _ := headerRun(t, pageReads(), referer, nil, nil, four); !reflect.DeepEqual(missed, []int{3}) {
				t.Errorf("with no noise set for the proof: missed %v; want [3]", missed)
			}
			if missed, _ := headerRun(t, pageReads(), referer, &models.OutgoingOptions{}, nil, four); !reflect.DeepEqual(missed, []int{3}) {
				t.Errorf("with a session that names no Referer noise: missed %v; want [3]", missed)
			}
		})
	}
}

// A connection served through a scoped view of the store (scopedMockDb, as
// one opened before exact serving began is) gets a repeated read's answer
// once more, as one served by the store itself does.
func TestARepeatedReadIsAnsweredOnceMoreThroughAScopedView(t *testing.T) {
	referer := models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"Referer": {}}}}
	for _, on := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			scoped := func(mm *MockManager) integrations.MockMemDb { return &scopedMockDb{MockMemDb: mm} }
			missed, unmade := headerRun(t, pageReads(), referer, &referer, scoped, []string{fromPage("a"), fromPage("b"), fromPage("c"), fromPage("d")})
			if len(missed) != 0 || len(unmade) != 0 {
				t.Errorf("through a scoped view: missed %v, unmade %v; want none", missed, unmade)
			}
		})
	}
}

// Each replay session hands the manager the header noise its matcher
// compares the set's calls with (Proxy.Mock -> SetRepeatReadNoise): a test's
// repeated reads are proved under the set's keploy.yml noise, and a later
// session's noise replaces it.
func TestAReplaySessionHandsTheProofItsHeaderNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), nil, &config.Config{})
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	defer mm.Close()
	p.setMockManager(mm)
	excusesReferer := func() bool {
		a, b := pageReads()[0], pageReads()[1]
		ca, _, _ := httpint.RepeatReadOf(a, mm.provingNoise())
		cb, _, _ := httpint.RepeatReadOf(b, mm.provingNoise())
		return ca == cb
	}
	_ = p.Mock(context.Background(), models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{"header": {"Referer": {}}}})
	if !excusesReferer() {
		t.Fatal("a session whose keploy.yml names Referer noise: the proof compares Referer")
	}
	_ = p.Mock(context.Background(), models.OutgoingOptions{})
	if excusesReferer() {
		t.Fatal("a later session that names no Referer noise: the proof still excuses it")
	}
}
