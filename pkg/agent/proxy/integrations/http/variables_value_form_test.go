package http

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Rows for the learned value a word bind is a word of
// (models.MockVariable.Of), as the learn step writes it: the whole UUID, as
// recorded, on each of its five word variables. Where the mock also sends
// the UUID whole at another place, or sends it once as a whole token of its
// field (alone, or after "APP:"), the five words are one value at replay,
// so two fresh UUIDs that share a group by chance bind; with no Of, or an Of
// that is not that stretch, each word is its own value and the shared group
// is refused, as main refuses it. A copy (one UUID where the recording had
// two) is refused either way.

// ofWords is vars with variables prefix0..prefix<n-1> each named a word of
// of, the learned value.
func ofWords(vars models.MockVariables, prefix, of string, n int) models.MockVariables {
	for i := range n {
		k := prefix + strconv.Itoa(i)
		v := vars[k]
		v.Of = of
		vars[k] = v
	}
	return vars
}

// eventAndSuiteOf is eventAndSuite with each UUID's word variables naming
// evOf and suOf their learned values ("": none).
func eventAndSuiteOf(prefixed, sentWhole bool, evOf, suOf string) []*models.Mock {
	ms := eventAndSuite(prefixed, sentWhole)
	if evOf != "" {
		ofWords(ms[0].Spec.Variables, "ev", evOf, 5)
	}
	if suOf != "" {
		ofWords(ms[1].Spec.Variables, "su", suOf, 5)
	}
	return ms
}

// Two fresh UUIDs whose version groups are equal by chance bind where
// learning named each UUID's words one value, bare or after "APP:", whether
// the mock sends each UUID whole in a field of its own or only once, as the
// whole token of its id field.
func TestVariables_TwoFreshUUIDsWithOneEqualGroupBindAsTheValuesLearningNamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, sentWhole := range []bool{true, false} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuiteOf(prefixed, sentWhole, recEvent, recSuite)}
			if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, sentWhole))); !ok || stub.Name != "mock-1" {
				t.Fatalf("sent whole %v, prefixed %v: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", sentWhole, prefixed, ok, stub, diag)
			}
			if ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveSuite, prefixed, sentWhole))); !ok || stub.Name != "mock-2" {
				t.Fatalf("sent whole %v, prefixed %v: the suite event should be answered by mock-2 (the equal group is chance inside two values): ok=%v stub=%v diag=%+v", sentWhole, prefixed, ok, stub, diag)
			}
			if b, bound := db.table.Lookup("su2"); !bound || b.Value != "41a3" {
				t.Fatalf("sent whole %v, prefixed %v: su2 bound %v to %q, want 41a3", sentWhole, prefixed, bound, b.Value)
			}
		}
	}
}

// The same calls stay refused at the equal group, as main refuses them,
// where learning named no value, named only one of the two UUIDs, or named
// a text that is not the UUID, whether or not the mock sends each UUID
// whole elsewhere.
func TestVariables_TwoFreshUUIDsWithOneEqualGroupAreRefusedWithoutTheirLearnedValues(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		name        string
		sentWhole   bool
		evOf, suOf  string
		wantRefused string
	}{
		{"no learned value", true, "", "", "su2"},
		{"only the suite's UUID named", true, "", recSuite, "su2"},
		{"only the event's UUID named", true, recEvent, "", "su2"},
		{"each named the other's UUID", true, recSuite, recEvent, "su2"},
		{"each named a part of its UUID", true, recEvent[:13], recSuite[:13], "su2"},
		{"sent once, no learned value", false, "", "", "su2"},
		{"sent once, only the suite's UUID named", false, "", recSuite, "su2"},
		{"sent once, each named the other's UUID", false, recSuite, recEvent, "su2"},
		{"sent once, each named a part of its UUID", false, recEvent[:13], recSuite[:13], "su2"},
		{"sent once, each named its UUID with a text after it", false, recEvent + "/1", recSuite + "/1", "su2"},
	} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuiteOf(prefixed, c.sentWhole, c.evOf, c.suOf)}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, c.sentWhole))); !ok {
				t.Fatalf("%s, prefixed %v: the event was not answered: %+v", c.name, prefixed, diag)
			}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveSuite, prefixed, c.sentWhole)))
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != c.wantRefused {
				t.Fatalf("%s, prefixed %v: refused at %s, want %s (the equal group, as main): %q", c.name, prefixed, p.name, c.wantRefused, p.why)
			}
		}
	}
}

// One UUID where the recording had two is a copy, refused at its first word
// even where learning named each UUID one value, sent whole elsewhere or
// sent once.
func TestVariables_OneUUIDWhereTwoWereRecordedIsRefusedWhereLearningNamedThem(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, sentWhole := range []bool{true, false} {
		for _, prefixed := range []bool{false, true} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuiteOf(prefixed, sentWhole, recEvent, recSuite)}
			if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(liveEvent, sentWhole))); !ok {
				t.Fatalf("sent whole %v, prefixed %v: the event was not answered: %+v", sentWhole, prefixed, diag)
			}
			ok, _, diag := matchVar(t, h, db, liveReq("POST", "/suites/events", suiteBody(liveEvent, prefixed, sentWhole)))
			if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "su0" {
				t.Fatalf("sent whole %v, prefixed %v: refused at %s, want su0: %q", sentWhole, prefixed, p.name, p.why)
			}
		}
	}
}

// Two groups of one fresh UUID equal by chance bind where learning named
// its words one value, sent whole elsewhere or sent once; refused at the
// second, as main, where it named none.
func TestVariables_TwoEqualGroupsOfOneUUIDBindAsTheValueLearningNamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const live = "7d444840-41a3-41a3-a15b-5b4c7a3f9e21"
	for _, sentWhole := range []bool{true, false} {
		for _, of := range []string{recEvent, ""} {
			h := newHTTP()
			db := &varDb{perTest: eventAndSuiteOf(false, sentWhole, of, "")[:1]}
			ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/events", eventBody(live, sentWhole)))
			if of == "" {
				if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "ev2" {
					t.Fatalf("sent whole %v, no learned value: refused at %s, want ev2: %q", sentWhole, p.name, p.why)
				}
				continue
			}
			if !ok || stub.Name != "mock-1" {
				t.Fatalf("sent whole %v, named one value: the event should be answered by mock-1: ok=%v stub=%v diag=%+v", sentWhole, ok, stub, diag)
			}
		}
	}
}

// Named a learned value each, the words of "<cid>/<appId>/<n>" and
// "<cid>/<n>" still refuse the one 7 where 3 and 5 were recorded: the count
// is no word of a learned value, so it is its own, as main checks it.
func TestVariables_OneWordWhereTwoWereRecordedBesideNamedValuesIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const (
		recA  = "6f9619ff-8b86-4011-b42d-00c04fc964ff"
		recB  = "c9bf9e57-1685-4c89-bafb-ff5af830be8a"
		recC  = "1e2d3c4b-5a69-4788-9766-554433221100"
		recD  = "aabbccdd-eeff-4a11-9b22-334455667788"
		liveX = "7d444840-9dc0-41a3-a15b-5b4c7a3f9e21"
		liveY = "e3b0c442-98fc-4b11-8d2e-1c5a4e9b7f30"
		liveP = "11112222-3333-4444-8555-666677778888"
		liveQ = "9999aaaa-bbbb-4ccc-9ddd-eeeeffff0000"
	)
	body := func(cid, app, n string) string {
		return `{"cid":"` + cid + `","app":"` + app + `","id":"` + cid + `/` + app + `/` + n + `"}`
	}
	mock := func(name, p, cid, app, n string) *models.Mock {
		v := ofWords(uuidWordBinds(p+"c", cid, "body.id", 0), p+"c", cid, 5)
		for k, b := range ofWords(uuidWordBinds(p+"a", app, "body.id", 5), p+"a", app, 5) {
			v[k] = b
		}
		v[p+"cid"], v[p+"app"] = bindV(cid, "body.cid"), bindV(app, "body.app")
		v[p+"n"] = bindV(n, "body.id#10")
		return varMock(name, "POST", "http://store/markers", body(cid, app, n), 201, `{}`, v)
	}
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{at(mock("mock-1", "o", recB, recA, "3"), 1), at(mock("mock-2", "t", recD, recC, "5"), 2)}}
	if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", body(liveP, liveX, "7"))); !ok {
		t.Fatalf("the first marker was not answered: %+v", diag)
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/markers", body(liveQ, liveY, "7")))
	if p := wantProblem(t, ok, diag, "they were two values when the test was recorded"); p.name != "tn" {
		t.Fatalf("refused at %s, want tn (the one 7 where 3 and 5 were recorded): %q", p.name, p.why)
	}
}
