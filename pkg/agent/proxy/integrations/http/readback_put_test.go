package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A client test that makes a service account's key record: its request names
// the account and a repository whose name ends in a nanosecond stamp; the
// application PUTs the record to a key store (OpenBao's KV engine) and later
// GETs it back by the same URL, and checks that the record it reads is the
// account's (createdBy). Recorded and in this run the account's id and the
// stamp differ; the key's id, the stamps and the rest are the same.
const (
	kpRecBy, kpLiveBy = "6af515630963371bd83fd48b", "6af515cbe4107bc29b1b1592"
	kpRecNano         = "1794446691151186387"
	kpLiveNano        = "1794446795262129626"
	kpRecKey          = "7c1bb62d-fac5-45a9-a465-ea4856f11804"
)

func kpPath(key string) string {
	return "/v1/secret/data/keploy/personal-access-tokens/keploy.io/" + key
}

// kpRecord is the key record as the PUT sends it and the GET answers it.
func kpRecord(key, by, nano string) string {
	return `{"_id":"` + key + `","cid":"keploy.io","createdAt":1794446691,"createdBy":"` + by + `","email":"sa-` + by +
		`@keploy.io.svc","expiresAt":1825982691,"isServiceAccountKey":true,"isWorkloadIdentity":true,"keyType":"pat","name":"CI identity: github octo-org/repo-` +
		nano + `","scopes":["read","write","admin"],"status":"active","updatedAt":"2026-11-12T01:24:51Z"}`
}

func kpPut(key, by, nano string) string { return `{"data":` + kpRecord(key, by, nano) + `}` }

// kpGetAnswer is the store's answer to the GET: the record inside its
// envelope, as OpenBao's KV engine (version 2) wraps it.
func kpGetAnswer(key, by, nano string) string {
	return `{"request_id":"3813b8a7-fe8d-1e00-2e6d-2ee89c15ba14","data":{"data":` + kpRecord(key, by, nano) +
		`,"metadata":{"created_time":"2026-11-12T01:24:51.154168773Z","version":1}}}`
}

// kpClient is the client test case's request.
func kpClient(by, nano string) string {
	return `{"query":"mutation($sa: String!, $input: CreateWorkloadIdentityInput!) { createServiceAccountWorkloadIdentity(serviceAccountId: $sa, input: $input) { id } }","variables":{"input":{"conditions":[{"claim":"repository","value":"octo-org/repo-` +
		nano + `"}],"provider":"github"},"sa":"` + by + `"}}`
}

var kpVault = map[string]string{"X-Vault-Request": "true"}

// kpRun runs the client test under the proven switch as proven says: the
// recordings mocks, served to calls in turn. It returns the store with what
// each call consumed, and the answers served.
func kpRun(t *testing.T, proven string, mocks []*models.Mock, calls []*req) (*learnDb, []*models.Mock) {
	t.Helper()
	return kpRunNoise(t, proven, nil, mocks, calls)
}

// kpRunNoise is kpRun under keploy.yml's url noise urlNoise.
func kpRunNoise(t *testing.T, proven string, urlNoise []string, mocks []*models.Mock, calls []*req) (*learnDb, []*models.Mock) {
	t.Helper()
	h, _, opts := learnOn(t)
	t.Setenv(provenswitch.Env, proven)
	if provenswitch.On() != (proven == "1") {
		t.Fatalf("the proven switch reads %v under %s=%q", provenswitch.On(), provenswitch.Env, proven)
	}
	n := h.newLearnNoise(opts, nil, urlNoise)
	recBegin := learnAt.Add(time.Second)
	began := learnAt.Add(10 * time.Minute)
	tr := learn.NewTracker()
	tr.SetClientTests(true)
	tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
	tr.RecordedBegin(recBegin)
	tr.BeginAt(began)
	recReq := &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Timestamp: recBegin.Add(100 * time.Millisecond),
		Header: map[string]string{"Content-Type": "application/json"}, Body: kpClient(kpRecBy, kpRecNano)}
	tr.SetCases([]learn.ClientCase{{Name: "post-query-83", Req: recReq}})
	liveReq := *recReq
	liveReq.Body, liveReq.Timestamp = kpClient(kpLiveBy, kpLiveNano), began.Add(100*time.Millisecond)
	if !tr.Incoming(&liveReq, "", liveReq.Timestamp) {
		t.Fatal("the client test's request was not paired with its test case")
	}
	ctx := learn.WithTracker(context.Background(), tr)
	db := &learnDb{perTest: mocks, observe: tr.ServedMock}
	var out []*models.Mock
	for i, c := range calls {
		c.arrived = began.Add(mocks[i].Spec.ReqTimestampMock.Sub(recBegin))
		served, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || served == nil {
			why := ""
			if miss != nil {
				why = miss.res.Why
			}
			t.Fatalf("call %d: served %v miss %s err %v", i, served != nil, why, err)
		}
		out = append(out, served)
	}
	return db, out
}

// kpMocks are the recorded PUT (mock-0) and GET (mock-1) of the key record,
// with the GET's answer as given.
func kpMocks(getAnswer string) []*models.Mock {
	return []*models.Mock{
		learnRecorded("mock-0", 1130*time.Millisecond, "PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpRecBy, kpRecNano), kpVault, `{"data":{"version":1}}`),
		learnRecorded("mock-1", 1300*time.Millisecond, "GET", kpPath(kpRecKey), "", kpVault, getAnswer),
	}
}

// The token exchange's read of the key record (gl7-p2's mock-3278): the GET
// is the recorded one, and its recorded answer holds the recorded account as
// createdBy, while the application checks it against this run's account.
// Read back from the PUT, the GET answers the account (a variable of the
// client test, used at the PUT's createdBy), the account's word of the email
// (used at the PUT's email#1) and the name's word (the client test's word,
// used at the PUT's name#6) as this run wrote them.
func TestReadBackAnswersAGetWithWhatTheTestPut(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven, func(t *testing.T) {
			db, served := kpRun(t, proven, kpMocks(kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)), []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("GET", kpPath(kpRecKey), "", kpVault),
			})
			if got, want := served[1].Spec.HTTPResp.Body, kpGetAnswer(kpRecKey, kpLiveBy, kpLiveNano); got != want {
				t.Fatalf("the GET answered\n%s\nwant what this run's PUT stored\n%s", got, want)
			}
			vars := db.consumed[1].Spec.Learned()
			answers := map[string]string{}
			for name, v := range vars {
				for _, a := range v.Answer {
					answers[a] = name
				}
				if len(v.Use) != 0 {
					t.Fatalf("the GET uses %s at %v, want answers only", name, v.Use)
				}
			}
			put := db.consumed[0].Spec.Learned()
			for place, at := range map[string]string{
				"body.data.data.createdBy": "body.data.createdBy",
				"body.data.data.email#1":   "body.data.email#1",
				"body.data.data.name#6":    "body.data.name#6",
			} {
				name, ok := answers[place]
				if !ok {
					t.Fatalf("the GET answers %v, want %s answered", answers, place)
				}
				if !containsString(put[name].Use, at) {
					t.Fatalf("the GET answers %s with %s, which the PUT uses at %v, want at %s", place, name, put[name].Use, at)
				}
			}
			if len(answers) != 3 {
				t.Fatalf("the GET answers %v, want the three fields this run changed", answers)
			}
			if b := db.consumed[1].Spec.LearnedBinds(); len(b) != 0 {
				t.Fatalf("the GET binds %+v, want none: each field is a variable the PUT uses", b)
			}
		})
	}
}

// Read back only where the two calls name one resource, and the GET's
// recorded answer is what the PUT stored: a GET of another key, a GET whose
// recorded answer was not 200, and a write after the PUT that keploy does
// not follow (a DELETE) each leave the GET's answer as recorded. A call to
// the URL that is no GET is no read: a POST answered 200 with the record's
// envelope, whose body sent the repository the client test sent (so its
// learn step pairs it, as a call that drifted), is answered as recorded.
func TestReadBackOfAPutFailsClosed(t *testing.T) {
	const other = "8d2cc73e-0b5d-46b0-b576-fb5967f22915"
	recordedAnswer := kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven+"/another-key", func(t *testing.T) {
			mocks := kpMocks(kpGetAnswer(other, kpRecBy, kpRecNano))
			mocks[1].Spec.HTTPReq.URL = kpPath(other)
			_, served := kpRun(t, proven, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("GET", kpPath(other), "", kpVault),
			})
			if got := served[1].Spec.HTTPResp.Body; got != kpGetAnswer(other, kpRecBy, kpRecNano) {
				t.Fatalf("a GET of another key answered %s, want its recording", got)
			}
		})
		t.Run("proven="+proven+"/not-200", func(t *testing.T) {
			mocks := kpMocks(recordedAnswer)
			mocks[1].Spec.HTTPResp.StatusCode = 203
			_, served := kpRun(t, proven, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("GET", kpPath(kpRecKey), "", kpVault),
			})
			if got := served[1].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a GET answered 203 answered %s, want its recording", got)
			}
		})
		t.Run("proven="+proven+"/post-is-no-read", func(t *testing.T) {
			mocks := kpMocks(recordedAnswer)
			repo := func(nano string) string { return `{"repo":"octo-org/repo-` + nano + `"}` }
			mocks[1] = learnRecorded("mock-1", 1300*time.Millisecond, "POST", kpPath(kpRecKey), repo(kpRecNano), kpVault, recordedAnswer)
			_, served := kpRun(t, proven, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("POST", kpPath(kpRecKey), repo(kpLiveNano), kpVault),
			})
			if got := served[1].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a POST after the PUT answered %s, want its recording", got)
			}
		})
		t.Run("proven="+proven+"/deleted-after", func(t *testing.T) {
			mocks := kpMocks(recordedAnswer)
			del := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", kpPath(kpRecKey), "", kpVault, `{}`)
			mocks = []*models.Mock{mocks[0], del, mocks[1]}
			_, served := kpRun(t, proven, mocks, []*req{
				live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("DELETE", kpPath(kpRecKey), "", kpVault),
				live("GET", kpPath(kpRecKey), "", kpVault),
			})
			if got := served[2].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a GET after a DELETE answered %s, want its recording", got)
			}
		})
	}
}
