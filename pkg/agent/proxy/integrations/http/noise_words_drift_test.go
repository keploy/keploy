package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// driftPath is the secret store's path the rows below write a note to.
const driftPath = "/v1/secret/data/keploy/notes/K1"

func driftNote(label string) string {
	return `{"data":{"_id":"K1","label":"` + label + `","status":"active"}}`
}

func driftReadBack(label string) string {
	return `{"data":{"data":{"_id":"K1","label":"` + label + `","status":"active"},"metadata":{"version":1}}}`
}

// A PUT's text one word of which drifted between the recording and record's
// full round with no source the call carries (a fresh token, a uuid, any
// other word) is learned as noise of that word alone, never of the whole
// text: "Lead <token> for ci" learned whole passed "Evil <token> for
// attacker" at every replay. It is so for a client's test (a T2 test case,
// its clock read from the recording) and a plain one, with or without a GET
// that reads the note back, under either switch. The proven replay that
// follows lets that word change and compares every other word of the text;
// the replay with the switch off matches a call by its shape, not its
// values (it answers any label of a mock that learned nothing), so there the
// row asks only that the calls the words admit are answered.
func TestLearnMatchADriftedWordOfAPutTextIsTheOnlyWordLeftUnchecked(t *testing.T) {
	for _, w := range []driftWord{
		{"a token", "q7Zk2Lm9Xw", "R4tY8uIo3P", "Zz9xAb3Cd4"},
		{"a uuid", "7c1bb62d-fac5-45a9-a465-ea4856f11804", "9e3f6a1b-2c4d-4e5f-8a9b-0c1d2e3f4a5b", "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
		{"a plain word", "alphabet", "bravoxyz", "charlie"},
	} {
		rec, f := "Lead "+w.rec+" for ci", "Lead "+w.f+" for ci"
		for _, proven := range []string{"", "1"} {
			for _, client := range []bool{true, false} {
				for _, withGet := range []bool{true, false} {
					name := w.name + "/proven=" + proven + "/client=" + map[bool]string{true: "yes", false: "no"}[client] + "/get=" + map[bool]string{true: "yes", false: "no"}[withGet]
					t.Run(name, func(t *testing.T) {
						recs := []*models.Mock{learnRecorded("mock-0", 1130*time.Millisecond, "PUT", driftPath, driftNote(rec), nil, `{"data":{"version":1}}`)}
						calls := []*req{live("PUT", driftPath, driftNote(f), nil)}
						if withGet {
							recs = append(recs, learnRecorded("mock-1", 1300*time.Millisecond, "GET", driftPath, "", nil, driftReadBack(rec)))
							calls = append(calls, live("GET", driftPath, "", nil))
						}
						db := driftLearn(t, proven, client, recs, calls)
						want := map[string][]string{"body.data.label": {learn.NoiseWordsPattern(changedWords(rec, f))}}
						if got := db.consumed[0].Spec.ReqBodyNoise; !reflect.DeepEqual(got, want) {
							t.Fatalf("learned %v on the PUT; want %v: its changed word alone", got, want)
						}
						mocks := learnedOnto(recs, db.consumed)
						replays := []string{"1"}
						if proven != "1" {
							replays = append(replays, proven)
						}
						for _, sw := range replays {
							t.Setenv(provenswitch.Env, sw)
							driftReplay(t, sw == "1", withGet, mocks, w)
						}
					})
				}
			}
		}
	}
}

// driftWord is a word of "Lead <word> for ci" as recorded (rec), at record's
// full round (f), and a third value of it (third).
type driftWord struct{ name, rec, f, third string }

// driftReplay replays the PUT the rows above learned with labels whose
// drifted word is another (each answered, and the GET after it too) and with
// labels another word of which changed (each refused, naming the label, where
// refuse holds).
func driftReplay(t *testing.T, refuse, withGet bool, mocks []*models.Mock, w driftWord) {
	t.Helper()
	rec, f := "Lead "+w.rec+" for ci", "Lead "+w.f+" for ci"
	for _, r := range []struct {
		label string
		want  bool
	}{
		{f, true},
		{rec, true},
		{"Lead " + w.third + " for ci", true},
		{"Lead " + w.f + " for attacker", false},
		{"Evil " + w.f + " for ci", false},
		{"Evil " + w.f + " for attacker", false},
		{"Lead " + w.rec + " for attacker", false},
		{"Evil " + w.third + " for attacker", false},
		{"Lead " + w.f + " for ci now", false},
	} {
		if !r.want && !refuse {
			continue
		}
		vdb := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
		ok, _, diag := matchVar(t, newHTTP(), vdb, live("PUT", driftPath, driftNote(r.label), nil))
		if ok != r.want || !ok && !hasField(differs(diag), "body.data.label") {
			t.Fatalf("replay of label %q answered %v (differs %v); want %v, naming body.data.label", r.label, ok, differs(diag), r.want)
		}
		if ok && withGet {
			if ok, stub, _ := matchVar(t, newHTTP(), vdb, live("GET", driftPath, "", nil)); !ok || stub.Name != "mock-1" {
				t.Fatalf("the GET after the PUT of %q answered %v", r.label, ok)
			}
		}
	}
}

// driftLearn runs record's full round over calls with the switch at proven:
// as a client's test (its clock read from the recording) or a plain one. It
// fails t unless every call is served.
func driftLearn(t *testing.T, proven string, client bool, recs []*models.Mock, calls []*req) *learnDb {
	t.Helper()
	h, _, opts := learnOn(t)
	// learnOn sets the switch; this row runs under both.
	t.Setenv(provenswitch.Env, proven)
	n := h.newLearnNoise(opts, nil, nil)
	tr := learn.NewTracker()
	recBegin, began := learnAt.Add(time.Second), learnAt.Add(10*time.Minute)
	if client {
		tr.SetClientTests(true)
		tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
		tr.RecordedBegin(recBegin)
		tr.BeginAt(began)
	} else {
		tr.Begin()
	}
	ctx := learn.WithTracker(context.Background(), tr)
	per := make([]*models.Mock, len(recs))
	for i, r := range recs {
		cp := *r
		per[i] = &cp
	}
	db := &learnDb{perTest: per, observe: tr.ServedMock}
	for i, c := range calls {
		if client {
			c.arrived = began.Add(recs[i].Spec.ReqTimestampMock.Sub(recBegin))
		}
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || served == nil {
			why := ""
			if miss != nil {
				why = miss.res.Why
			}
			t.Fatalf("record's full round refused call %d (%s %s): %s %v", i, c.method, strings.TrimSpace(c.url.String()), why, err)
		}
	}
	if len(db.consumed) != len(calls) {
		t.Fatalf("record's full round took %d mocks for %d calls", len(db.consumed), len(calls))
	}
	return db
}
