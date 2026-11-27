package http

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// TestLearnMatchDeclaredClockDropsClientBeforeDBAnswer verifies HTTP classify
// parity with DB Decide:
//   - When keploy.yml declares kind: clock on an HTTP body field and its range
//     holds both runs, incoming client test requests (e.g. test-4 body.replayId)
//     are dropped BEFORE classifying so an in-scope DB/earlier answer (mock-7)
//     holding the same second stays eligible instead of being relabeled as a
//     clock read.
//   - When no earlier answer holds the timestamp, the declared clock settles it
//     and names BOTH the keploy.yml rule ID and the set-aside client place.
//   - Out-of-range client timestamps stay bound to test-4, and non-declared
//     fields on the same HTTP call still bind to test-4.
func TestLearnMatchDeclaredClockDropsClientBeforeDBAnswer(t *testing.T) {
	const (
		ruleName = "pat-created-at"
		path     = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/pat-1"
	)
	clockRule := []models.DeclaredSource{{
		Rule:  ruleName,
		Kind:  models.DeclaredSourceClock,
		Field: "body.data.createdAt",
		Clock: &models.ClockOffset{Min: -time.Second, Max: 0, Res: time.Second},
	}}

	// Recording: test-4 arrives at .873Z carrying Unix second in replayId#7;
	// mock-7 (when present) answers 50ms before the PUT at .953Z holding the
	// same second; PUT arrives at next second .003Z (window [.953Z/.002Z, .003Z]).
	// Round F: 90s later with the exact same geometry.
	rBegin := time.Date(2026, 11, 10, 10, 1, 31, 788_000_000, time.UTC)
	rTest4At := time.Date(2026, 11, 10, 10, 1, 31, 873_000_000, time.UTC)
	rDBAt := time.Date(2026, 11, 10, 10, 1, 31, 953_000_000, time.UTC)
	rPrevAt := time.Date(2026, 11, 10, 10, 1, 32, 2_000_000, time.UTC)
	rCallAt := time.Date(2026, 11, 10, 10, 1, 32, 3_000_000, time.UTC)

	fBegin := rBegin.Add(90 * time.Second)
	fTest4At := rTest4At.Add(90 * time.Second)
	fDBAt := rDBAt.Add(90 * time.Second)
	fPrevAt := rPrevAt.Add(90 * time.Second)
	fCallAt := rCallAt.Add(90 * time.Second)

	rSec := strconv.FormatInt(rTest4At.Unix(), 10)
	fSec := strconv.FormatInt(fTest4At.Unix(), 10)

	identity := func(r time.Time) (time.Time, bool) { return r, true }
	jsonHdr := map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"}

	clientReq := func(at time.Time, sec, tag string, multiWord bool) *models.HTTPReq {
		roundPart := "999"
		if multiWord {
			roundPart = strconv.FormatInt(at.UnixNano(), 10)
		}
		return &models.HTTPReq{
			Method:    "POST",
			URL:       "http://localhost:8083/k8s-proxy/autoreplay-rounds",
			Timestamp: at,
			Header:    jsonHdr,
			Body:      `{"replayId":"e2e-ns-e2e-round-` + roundPart + `-auto-replay-` + sec + `-1","tag":"` + tag + `"}`,
		}
	}
	putBody := func(sec, tag string) string {
		return `{"data":{"createdAt":` + sec + `,"tag":"` + tag + `"}}`
	}
	dbMockOf := func(sec string) *models.Mock {
		return &models.Mock{
			Name: "mock-7",
			Kind: models.GENERIC,
			Spec: models.MockSpec{
				ReqTimestampMock: rDBAt,
				ResTimestampMock: rDBAt.Add(time.Millisecond),
				GenericRequests: []models.Payload{{
					Origin:  models.FromClient,
					Message: []models.OutputBinary{{Type: "utf-8", Data: `{"find":"pat_records","filter":{"_id":"pat-1"}}`}},
				}},
				GenericResponses: []models.Payload{{
					Origin:  models.FromServer,
					Message: []models.OutputBinary{{Type: "utf-8", Data: `{"cursor":{"firstBatch":[{"_id":"pat-1","createdAt":` + sec + `}]}}`}},
				}},
			},
		}
	}

	type optsCfg struct {
		recSec, liveSec string
		recTag, liveTag string
		multiWord       bool
		noClient        bool
		noAppClock      bool
		echoAnswer      bool
		withBegin       bool
		timeForm        bool
		dbRaw           bool
		earlierResp     bool
		bindEarlierResp bool
		withClockRule   bool
		customClockRule []models.DeclaredSource
	}
	run := func(cfg optsCfg) (*models.Mock, *learnMiss, *learnDb) {
		h, n, opts := learnOn(t)
		if cfg.withClockRule || len(cfg.customClockRule) > 0 {
			rule := clockRule
			if len(cfg.customClockRule) > 0 {
				rule = cfg.customClockRule
			}
			n.sources = func(mock string) []models.DeclaredSource {
				if mock == "mock-0" {
					return rule
				}
				return nil
			}
		}
		tr := learn.NewTracker()
		tr.SetClientTests(true)
		if !cfg.noAppClock {
			tr.SetAppClock(identity)
		}
		if cfg.withBegin {
			tr.RecordedBegin(rBegin)
			tr.BeginAt(fBegin)
		} else {
			tr.Begin()
		}
		if !cfg.noClient {
			recC := clientReq(rTest4At, cfg.recSec, cfg.recTag, cfg.multiWord)
			liveC := clientReq(fTest4At, cfg.liveSec, cfg.liveTag, cfg.multiWord)
			tr.SetCases([]learn.ClientCase{{Name: "test-4", Req: recC}})
			if !tr.Incoming(liveC, "", fTest4At) {
				t.Fatal("incoming client request test-4 did not pair")
			}
		}

		// Anchor the call's own window to [rPrevAt, rCallAt] / [fPrevAt, fCallAt]
		// so the call window starts in the second AFTER rSec/fSec.
		prevMock := learnRecorded("mock-prev", 0, "GET", "/ping", "", nil, `{"ok":true}`)
		prevMock.Spec.ReqTimestampMock = rPrevAt
		prevMock.Spec.ResTimestampMock = rPrevAt.Add(time.Millisecond / 2)
		var staged []*models.Mock
		if cfg.dbRaw {
			dbRec := dbMockOf(cfg.recSec)
			dbLive := dbMockOf(cfg.liveSec)
			tr.ServedMockAt(dbRec, fDBAt)
			tr.ServedMockLive(dbRec, "v1", func() (*models.Mock, error) { return dbLive, nil })
			staged = append(staged, dbRec)
		}
		if cfg.earlierResp || cfg.bindEarlierResp {
			method, reqBody := "GET", ""
			side, place := learn.Response, ""
			if cfg.bindEarlierResp {
				method, reqBody = "POST", `{"createdAt":`+cfg.recSec+`}`
				side, place = learn.Request, "body.createdAt"
			}
			eRec := learnRecorded("mock-7", 0, method, "/pat/pat-1", reqBody, nil, `{"createdAt":`+cfg.recSec+`}`)
			eRec.Spec.ReqTimestampMock = rDBAt
			eRec.Spec.ResTimestampMock = rDBAt.Add(time.Millisecond)
			tr.ServedMockAt(eRec, fDBAt)
			testNum, _, _, _ := tr.Test()
			tr.Served(testNum, learn.Earlier{
				Mock: "mock-7",
				At:   rDBAt,
				Own:  cfg.bindEarlierResp,
				Seen: []learn.Seen{{
					Side:  side,
					Place: place,
					Leaf:  learn.Leaf{Path: "body.createdAt", Noise: "body.createdAt", Type: learn.Number, Value: cfg.recSec},
					Live:  cfg.liveSec,
				}},
			})
			staged = append(staged, eRec)
		}
		tr.ServedMockAt(prevMock, fPrevAt)
		tr.ServedMockAsRecorded(prevMock)
		staged = append(staged, prevMock)

		respBody := `{"data":{"version":1}}`
		if cfg.echoAnswer {
			respBody = `{"data":{"version":1,"createdAt":` + cfg.recSec + `}}`
		}
		put := learnRecorded("mock-0", 0, "PUT", path, putBody(cfg.recSec, cfg.recTag), nil, respBody)
		put.Spec.ReqTimestampMock = rCallAt
		put.Spec.ResTimestampMock = rCallAt.Add(time.Millisecond)
		db := &learnDb{perTest: []*models.Mock{put}, staged: staged}
		in := live("PUT", path, putBody(cfg.liveSec, cfg.liveTag), nil)
		in.arrived = fCallAt
		var store integrations.MockMemDb = db
		if cfg.timeForm {
			store = timeFormDb{db}
		}
		served, miss, _ := h.learnMatch(learn.WithTracker(context.Background(), tr), in, store, n, opts)
		return served, miss, db
	}

	t.Run("in-scope DB answer mock-7 stays eligible over client test-4", func(t *testing.T) {
		for _, mode := range []struct {
			name        string
			dbRaw       bool
			earlierResp bool
		}{
			{"raw DB answer", true, false},
			{"leaf-by-leaf earlier answer", false, true},
		} {
			for _, timeForm := range []bool{false, true} {
				// Control row with no client request: HTTP's normal behavior for an
				// earlier DB/answer-sourced timestamp is to refuse naming mock-7
				// (HTTP does not bind from DB/answer payloads), never ClockRead.
				ctrlServed, ctrlMiss, ctrlDB := run(optsCfg{
					recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
					noClient: true, withBegin: true, timeForm: timeForm, dbRaw: mode.dbRaw, earlierResp: mode.earlierResp, withClockRule: true,
				})
				if ctrlServed != nil || ctrlMiss == nil || len(ctrlDB.consumed) != 0 || !strings.Contains(ctrlMiss.res.Why, "mock-7") {
					t.Fatalf("control no-client %s (timeForm=%v): served=%v miss=%+v; want normal HTTP refusal naming mock-7", mode.name, timeForm, ctrlServed != nil, ctrlMiss)
				}
				// With client test-4 present: test-4 is dropped before classifying so
				// mock-7 stays eligible (same refusal naming mock-7, never relabeled
				// ClockRead) AND the miss reason names the rule and set-aside client place.
				served, miss, db := run(optsCfg{
					recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
					withBegin: true, timeForm: timeForm, dbRaw: mode.dbRaw, earlierResp: mode.earlierResp, withClockRule: true,
				})
				if served != nil || miss == nil || len(db.consumed) != 0 {
					var clocks map[string]models.ClockOffset
					if len(db.consumed) > 0 {
						clocks = db.consumed[0].Spec.Clocks
					}
					t.Fatalf("%s (timeForm=%v): served=%v clocks=%+v miss=%+v; want mock-7 kept (refused naming mock-7, NOT relabeled ClockRead)", mode.name, timeForm, served != nil, clocks, miss)
				}
				for _, want := range []string{"mock-7", ruleName, "test case test-4 body.replayId#7"} {
					if !strings.Contains(miss.res.Why, want) {
						t.Fatalf("%s (timeForm=%v): miss.Why = %q; want it to name %q", mode.name, timeForm, miss.res.Why, want)
					}
				}
			}
		}
	})

	t.Run("without DB answer, declared clock overrides test-4 and names rule + set-aside client place", func(t *testing.T) {
		for _, shape := range []struct {
			name      string
			multiWord bool
			wantPlace string
		}{
			{"single-word client replayId", false, "test case test-4 body.replayId#7"},
			{"multi-word client replayId", true, "test case test-4 body.replayId#7"},
		} {
			for _, withBegin := range []bool{true, false} {
				for _, timeForm := range []bool{false, true} {
					served, miss, db := run(optsCfg{
						recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
						multiWord: shape.multiWord, withBegin: withBegin, timeForm: timeForm, withClockRule: true,
					})
					if served == nil || miss != nil || len(db.consumed) != 1 {
						t.Fatalf("%s withBegin=%v timeForm=%v: served=%v miss=%+v; want PUT learned as ClockRead", shape.name, withBegin, timeForm, served != nil, miss)
					}
					o, ok := db.consumed[0].Spec.Clocks["body.data.createdAt"]
					if !ok || o.Res != time.Second || o.Layout != "epoch-s" {
						t.Fatalf("%s withBegin=%v timeForm=%v: Clocks = %+v; want body.data.createdAt Res=1s Layout=epoch-s", shape.name, withBegin, timeForm, db.consumed[0].Spec.Clocks)
					}
					if timeForm && o.Form != "number 9999999999" {
						t.Fatalf("%s withBegin=%v timeForm=true: clock %+v; want Form='number 9999999999'", shape.name, withBegin, o)
					}
					if !timeForm {
						if o.Form != "" {
							t.Fatalf("%s withBegin=%v timeForm=false: clock %+v; want empty Form under strict times", shape.name, withBegin, o)
						}
						if o.Since != "" {
							t.Fatalf("%s withBegin=%v timeForm=false: clock %+v; want empty Since (call span, not SinceTestBegin)", shape.name, withBegin, o)
						}
						if !strings.Contains(o.Why, "declares it a read of the clock") {
							t.Errorf("%s withBegin=%v timeForm=false: Clock.Why %q must state that the rule declares it a read of the clock", shape.name, withBegin, o.Why)
						}
					}
					for _, want := range []string{ruleName, shape.wantPlace} {
						if !strings.Contains(o.Why, want) {
							t.Errorf("%s withBegin=%v timeForm=%v: Clock.Why %q must name %q", shape.name, withBegin, timeForm, o.Why, want)
						}
					}
				}
			}
		}
		// Without any client request (no coincidence), strict times with no Began
		// still settles the same plain declared clock rule identically.
		servedNoC, missNoC, dbNoC := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
			noClient: true, withBegin: false, timeForm: false, withClockRule: true,
		})
		if servedNoC == nil || missNoC != nil || len(dbNoC.consumed) != 1 {
			t.Fatalf("no-client strict (withBegin=false): served=%v miss=%+v; want ClockRead", servedNoC != nil, missNoC)
		}
		if o, ok := dbNoC.consumed[0].Spec.Clocks["body.data.createdAt"]; !ok || o.Res != time.Second || o.Layout != "epoch-s" || o.Form != "" || !strings.Contains(o.Why, ruleName) {
			t.Fatalf("no-client strict (withBegin=false): Clocks = %+v; want ClockRead naming %s", dbNoC.consumed[0].Spec.Clocks, ruleName)
		}
	})

	t.Run("strict times with echoed recorded answer keeps in-window KindEcho even beside coincidental client and never serves recorded moment", func(t *testing.T) {
		assertAnsweredEcho := func(t *testing.T, label string, served *models.Mock, miss *learnMiss, db *learnDb, wantWhy ...string) {
			t.Helper()
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("%s: served=%v miss=%+v; want served as answered echo", label, served != nil, miss)
			}
			if len(db.consumed[0].Spec.Clocks) != 0 {
				t.Fatalf("%s: Clocks=%+v; want answered echo (no ClockRead)", label, db.consumed[0].Spec.Clocks)
			}
			if len(db.consumed[0].Spec.LearnedBinds()["test case test-4"]) != 0 {
				t.Fatalf("%s: LearnedBinds=%+v; must not bind coincidental client test-4", label, db.consumed[0].Spec.LearnedBinds())
			}
			var gotEcho bool
			for _, v := range db.consumed[0].Spec.Learned() {
				if v.Bind == "body.data.createdAt" && v.Echo && len(v.Answer) > 0 {
					gotEcho = true
					for _, want := range wantWhy {
						if !strings.Contains(v.Why, want) {
							t.Errorf("%s: Learned.Why = %q; want %q", label, v.Why, want)
						}
					}
				}
			}
			if !gotEcho {
				t.Fatalf("%s: Learned=%+v; want answered echo on body.data.createdAt", label, db.consumed[0].Spec.Learned())
			}
		}

		// Regression guard: when an HTTP PUT writes createdAt = now inside its
		// own call window and the dependency's recorded answer echoes it,
		// adding a kind: clock rule under mock.times: strict must NEVER turn a
		// passing answered echo (KindEcho, where replay rewrites the answer
		// with this run's value) red — both rule off and rule on must serve as
		// an answered echo.
		rInWin := strconv.FormatInt(rCallAt.Unix(), 10)
		fInWin := strconv.FormatInt(fCallAt.Unix(), 10)
		for _, withRule := range []bool{false, true} {
			servedE, missE, dbE := run(optsCfg{
				recSec: rInWin, liveSec: fInWin, recTag: "same", liveTag: "same",
				noClient: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: withRule,
			})
			assertAnsweredEcho(t, "no-client in-window strict echoed answer withRule="+strconv.FormatBool(withRule), servedE, missE, dbE)
		}

		// Coincidental 1-word and multi-word client requests holding the same
		// in-window second beside an echoing answer under strict times:
		// - Without the rule: 1-word client captures the bind to test-4 and
		//   multi-word client refuses naming test-4.
		// - With the rule: the client is set aside (including from offsetPlaces)
		//   and the echoed field stays KindEcho (never bound to test-4 and never
		//   refused as a client offset), carrying the set-aside reason in Why.
		for _, shape := range []struct {
			name      string
			multiWord bool
		}{
			{"1-word client", false},
			{"multi-word client", true},
		} {
			servedOff, missOff, dbOff := run(optsCfg{
				recSec: rInWin, liveSec: fInWin, recTag: "same", liveTag: "same",
				multiWord: shape.multiWord, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: false,
			})
			if !shape.multiWord {
				if servedOff == nil || missOff != nil || len(dbOff.consumed) != 1 || len(dbOff.consumed[0].Spec.LearnedBinds()["test case test-4"]) == 0 {
					t.Fatalf("%s rule-off strict echoed answer: served=%v miss=%+v; want bound to test-4 without rule", shape.name, servedOff != nil, missOff)
				}
			} else {
				if servedOff != nil || missOff == nil || !strings.Contains(missOff.res.Why, "test case test-4") {
					t.Fatalf("%s rule-off strict echoed answer: served=%v miss=%+v; want refused naming test-4 without rule", shape.name, servedOff != nil, missOff)
				}
			}

			servedOn, missOn, dbOn := run(optsCfg{
				recSec: rInWin, liveSec: fInWin, recTag: "same", liveTag: "same",
				multiWord: shape.multiWord, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
			})
			assertAnsweredEcho(t, shape.name+" rule-on strict echoed answer", servedOn, missOn, dbOn, ruleName, "test case test-4 body.replayId#7")
		}

		// Bindable in-scope earlier call (mock-7) + coincidental client test-4
		// + strict + clock rule: client test-4 is dropped and the field binds
		// to mock-7 (never relabeled as ClockRead and never stolen by test-4).
		servedDB, missDB, dbDB := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
			bindEarlierResp: true, echoAnswer: false, withBegin: true, timeForm: false, withClockRule: true,
		})
		if servedDB == nil || missDB != nil || len(dbDB.consumed) != 1 {
			t.Fatalf("in-scope bindable mock-7 strict: served=%v miss=%+v; want bound to mock-7", servedDB != nil, missDB)
		}
		if _, isClock := dbDB.consumed[0].Spec.Clocks["body.data.createdAt"]; isClock {
			t.Fatalf("in-scope bindable mock-7 strict: Clocks=%+v; want bound to mock-7, not ClockRead", dbDB.consumed[0].Spec.Clocks)
		}
		if len(dbDB.consumed[0].Spec.LearnedBinds()["mock-7"]) == 0 || len(dbDB.consumed[0].Spec.LearnedBinds()["test case test-4"]) != 0 {
			t.Fatalf("in-scope bindable mock-7 strict: LearnedBinds=%+v; want bound to mock-7, not test-4", dbDB.consumed[0].Spec.LearnedBinds())
		}

		// Out-of-window (1s before call window, inside rule) + echoed answer +
		// strict + clock rule (with and without client test-4): learning refuses
		// because the call's own window does not hold the read (never ClockRead
		// serving recorded moment, never blaming test-4's offset), names the
		// existing rule and what to change, and never tells the user to declare
		// the field noise.
		for _, tc := range []struct {
			name     string
			noClient bool
			want     []string
		}{
			{"with client", false, []string{ruleName, "mock.times", "call's window", "test case test-4 body.replayId#7"}},
			{"without client", true, []string{ruleName, "mock.times", "call's window"}},
		} {
			servedOOW, missOOW, dbOOW := run(optsCfg{
				recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
				noClient: tc.noClient, multiWord: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
			})
			if servedOOW != nil || missOOW == nil || len(dbOOW.consumed) != 0 {
				t.Fatalf("FALSE GREEN? out-of-window strict echoed answer (%s): served=%v miss=%+v; want refused", tc.name, servedOOW != nil, missOOW)
			}
			if strings.Contains(missOOW.res.Why, "stands at the moment test case test-4") {
				t.Fatalf("out-of-window strict echoed answer (%s): miss.Why = %q; must not blame set-aside client offset", tc.name, missOOW.res.Why)
			}
			if strings.Contains(missOOW.res.Why, "declare it noise") {
				t.Errorf("out-of-window strict echoed answer (%s): miss.Why = %q; must not advise declaring noise when rule %s exists", tc.name, missOOW.res.Why, ruleName)
			}
			for _, want := range tc.want {
				if !strings.Contains(missOOW.res.Why, want) {
					t.Errorf("out-of-window strict echoed answer (%s): miss.Why = %q; want it to name %q", tc.name, missOOW.res.Why, want)
				}
			}
		}

		// Out-of-range (2m before call, outside [-1s, 0] rule) + echoed answer
		// + strict + clock rule: leaving mock.times at its default would NOT
		// accept an out-of-range value, so the refusal names the rule and why
		// its range rejected the value without offering the mock.times remedy
		// or telling the user to declare noise.
		rFar := strconv.FormatInt(rCallAt.Add(-2*time.Minute).Unix(), 10)
		fFar := strconv.FormatInt(fCallAt.Add(-2*time.Minute).Unix(), 10)
		servedFar, missFar, dbFar := run(optsCfg{
			recSec: rFar, liveSec: fFar, recTag: "same", liveTag: "same",
			noClient: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
		})
		if servedFar != nil || missFar == nil || len(dbFar.consumed) != 0 {
			t.Fatalf("out-of-range strict echoed answer: served=%v miss=%+v; want refused", servedFar != nil, missFar)
		}
		for _, bad := range []string{"declare it noise", "mock.times at its default"} {
			if strings.Contains(missFar.res.Why, bad) {
				t.Errorf("out-of-range strict echoed answer: miss.Why = %q; must not contain %q", missFar.res.Why, bad)
			}
		}
		for _, want := range []string{ruleName, "the recorded value is no such read", "Adjust keploy.yml's rule " + ruleName} {
			if !strings.Contains(missFar.res.Why, want) {
				t.Errorf("out-of-range strict echoed answer: miss.Why = %q; want %q", missFar.res.Why, want)
			}
		}

		// No app clock reported by hooks (w == nil) + echoed answer + strict +
		// clock rule: must keep the true windowless diagnosis ("clock hooks did
		// not report") matching the time default, never replacing it with a
		// rule-edit or app-change remedy that cannot resolve a missing window.
		servedNoClk, missNoClk, dbNoClk := run(optsCfg{
			recSec: rInWin, liveSec: fInWin, recTag: "same", liveTag: "same",
			noClient: true, noAppClock: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
		})
		if servedNoClk != nil || missNoClk == nil || len(dbNoClk.consumed) != 0 {
			t.Fatalf("no-app-clock strict echoed answer: served=%v miss=%+v; want refused", servedNoClk != nil, missNoClk)
		}
		if !strings.Contains(missNoClk.res.Why, "clock hooks did not report") {
			t.Errorf("no-app-clock strict echoed answer: miss.Why = %q; want windowless hooks diagnosis", missNoClk.res.Why)
		}
		for _, bad := range []string{"Adjust keploy.yml's rule", "mock.times at its default"} {
			if strings.Contains(missNoClk.res.Why, bad) {
				t.Errorf("no-app-clock strict echoed answer: miss.Why = %q; must not offer %q when w is nil", missNoClk.res.Why, bad)
			}
		}

		// Incompatible layouts across runs (epoch-s recorded vs epoch-ms live)
		// + echoed answer + strict + clock rule: no rule edit can reconcile
		// different layouts, so the original diagnosis is kept unchanged.
		fSecMs := strconv.FormatInt(fTest4At.UnixMilli(), 10)
		servedApart, missApart, dbApart := run(optsCfg{
			recSec: rSec, liveSec: fSecMs, recTag: "same", liveTag: "same",
			noClient: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
		})
		if servedApart != nil || missApart == nil || len(dbApart.consumed) != 0 {
			t.Fatalf("incompatible-layout strict echoed answer: served=%v miss=%+v; want refused", servedApart != nil, missApart)
		}
		for _, bad := range []string{"Adjust keploy.yml's rule", "mock.times at its default"} {
			if strings.Contains(missApart.res.Why, bad) {
				t.Errorf("incompatible-layout strict echoed answer: miss.Why = %q; must not offer %q", missApart.res.Why, bad)
			}
		}
		if !strings.Contains(missApart.res.Why, "no clock read by the app matched it") {
			t.Errorf("incompatible-layout strict echoed answer: miss.Why = %q; want original diagnosis kept", missApart.res.Why)
		}

		// Earlier DB answer (mock-7) + echoed answer + strict + clock rule:
		// DeclaredClockStrictEchoRefusal must leave the non-unread mock-7
		// refusal untouched, naming mock-7, rule, and set-aside client place.
		servedStrictDB, missStrictDB, dbStrictDB := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
			dbRaw: true, echoAnswer: true, withBegin: true, timeForm: false, withClockRule: true,
		})
		if servedStrictDB != nil || missStrictDB == nil || len(dbStrictDB.consumed) != 0 {
			t.Fatalf("strict echoed answer with earlier mock-7: served=%v miss=%+v; want refused naming mock-7", servedStrictDB != nil, missStrictDB)
		}
		if strings.Contains(missStrictDB.res.Why, "declares it a read of the clock, but") {
			t.Errorf("strict echoed answer with earlier mock-7: miss.Why = %q; must keep mock-7 refusal untouched", missStrictDB.res.Why)
		}
		for _, want := range []string{"mock-7", ruleName, "test case test-4 body.replayId#7"} {
			if !strings.Contains(missStrictDB.res.Why, want) {
				t.Errorf("strict echoed answer with earlier mock-7: miss.Why = %q; want %q", missStrictDB.res.Why, want)
			}
		}

		// Under time default, the echoed declared clock sets the client aside so
		// an earlier DB answer (mock-7) stays eligible (refused naming mock-7 +
		// rule + test-4, never silently turned into a form-checked ClockRead),
		// and without mock-7 is answered by form naming both rule and client.
		servedTFDB, missTFDB, dbTFDB := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
			dbRaw: true, echoAnswer: true, withBegin: true, timeForm: true, withClockRule: true,
		})
		if servedTFDB != nil || missTFDB == nil || len(dbTFDB.consumed) != 0 {
			t.Fatalf("time default echoed answer with earlier mock-7: served=%v miss=%+v; want refused naming mock-7 (not form-checked ClockRead)", servedTFDB != nil, missTFDB)
		}
		for _, want := range []string{"mock-7", ruleName, "test case test-4 body.replayId#7"} {
			if !strings.Contains(missTFDB.res.Why, want) {
				t.Errorf("time default echoed answer with earlier mock-7: miss.Why = %q; want %q", missTFDB.res.Why, want)
			}
		}

		servedTF, missTF, dbTF := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "same", liveTag: "same",
			multiWord: false, echoAnswer: true, withBegin: true, timeForm: true, withClockRule: true,
		})
		if servedTF == nil || missTF != nil || len(dbTF.consumed) != 1 {
			t.Fatalf("time default echoed answer: served=%v miss=%+v; want answered by form", servedTF != nil, missTF)
		}
		if o, ok := dbTF.consumed[0].Spec.Clocks["body.data.createdAt"]; !ok || o.Form != "number 9999999999" || !strings.Contains(o.Why, ruleName) || !strings.Contains(o.Why, "test case test-4 body.replayId#7") {
			t.Fatalf("time default echoed answer: Clocks = %+v; want form-checked ClockRead naming %s and test-4", dbTF.consumed[0].Spec.Clocks, ruleName)
		}
		// In-window echoed answer under time default with 1-word client:
		// Clock.Why also names the rule and set-aside client place.
		servedTFIn, missTFIn, dbTFIn := run(optsCfg{
			recSec: rInWin, liveSec: fInWin, recTag: "same", liveTag: "same",
			multiWord: false, echoAnswer: true, withBegin: true, timeForm: true, withClockRule: true,
		})
		if servedTFIn == nil || missTFIn != nil || len(dbTFIn.consumed) != 1 {
			t.Fatalf("time default in-window echoed answer: served=%v miss=%+v; want answered by form", servedTFIn != nil, missTFIn)
		}
		if o, ok := dbTFIn.consumed[0].Spec.Clocks["body.data.createdAt"]; !ok || o.Form != "number 9999999999" || !strings.Contains(o.Why, ruleName) || !strings.Contains(o.Why, "test case test-4 body.replayId#7") {
			t.Fatalf("time default in-window echoed answer: Clocks = %+v; want form-checked ClockRead naming %s and test-4", dbTFIn.consumed[0].Spec.Clocks, ruleName)
		}
	})

	t.Run("out-of-range client timestamp stays bound to test-4", func(t *testing.T) {
		oldR := strconv.FormatInt(rTest4At.Add(-2*time.Minute).Unix(), 10)
		oldF := strconv.FormatInt(fTest4At.Add(-2*time.Minute).Unix(), 10)
		for _, tc := range []struct {
			name    string
			recSec  string
			liveSec string
		}{
			{"both runs 2m outside", oldR, oldF},
			{"only recorded run 2m outside", oldR, fSec},
			{"only round F 2m outside", rSec, oldF},
		} {
			for _, timeForm := range []bool{false, true} {
				served, miss, db := run(optsCfg{
					recSec: tc.recSec, liveSec: tc.liveSec, recTag: "same", liveTag: "same",
					multiWord: false, withBegin: true, timeForm: timeForm, withClockRule: true,
				})
				if served == nil || miss != nil || len(db.consumed) != 1 {
					t.Fatalf("%s (timeForm=%v): served=%v miss=%+v; want bound to test-4", tc.name, timeForm, served != nil, miss)
				}
				if _, isClock := db.consumed[0].Spec.Clocks["body.data.createdAt"]; isClock {
					t.Fatalf("FALSE GREEN? %s (timeForm=%v): learned ClockRead %+v; want bound to test-4", tc.name, timeForm, db.consumed[0].Spec.Clocks)
				}
				var boundCreatedAt bool
				for _, v := range db.consumed[0].Spec.Learned() {
					if v.Recorded == tc.recSec && len(v.Use) == 1 && v.Use[0] == "body.data.createdAt" {
						boundCreatedAt = true
					}
				}
				var fromTest4 bool
				for _, v := range db.consumed[0].Spec.LearnedBinds()["test case test-4"] {
					if v.Bind == "body.replayId#7" && v.Recorded == tc.recSec {
						fromTest4 = true
					}
				}
				if !boundCreatedAt || !fromTest4 {
					t.Fatalf("%s (timeForm=%v): Learned=%+v LearnedBinds=%+v; want body.data.createdAt bound to test-4 body.replayId#7", tc.name, timeForm, db.consumed[0].Spec.Learned(), db.consumed[0].Spec.LearnedBinds())
				}
				// Multi-word out-of-range also never becomes a ClockRead (stays refused naming test-4).
				servedM, missM, _ := run(optsCfg{
					recSec: tc.recSec, liveSec: tc.liveSec, recTag: "same", liveTag: "same",
					multiWord: true, withBegin: true, timeForm: timeForm, withClockRule: true,
				})
				if servedM != nil || missM == nil || !strings.Contains(missM.res.Why, "test case test-4") {
					t.Fatalf("FALSE GREEN? multi-word %s (timeForm=%v): served=%v miss=%+v; want refused naming test-4", tc.name, timeForm, servedM != nil, missM)
				}
			}
		}
	})

	t.Run("one-sided declared clock rule on HTTP does not drop client request", func(t *testing.T) {
		hourR := strconv.FormatInt(rCallAt.Add(-time.Hour).Unix(), 10)
		hourF := strconv.FormatInt(fCallAt.Add(-time.Hour).Unix(), 10)
		// Straddling -1h (-60m1s..-59m59s) gives Unit=1h so CheckClockRead
		// genuinely holds both runs; only declaredClockComparable prevents
		// dropping test-4 on a !ComparesOffsets HTTP window.
		oneSidedOff := models.ClockOffset{Min: -time.Hour - time.Second, Max: -time.Hour + time.Second, Res: time.Second}
		oneSidedCheck := oneSidedOff
		oneSidedCheck.Layout = "epoch-s"
		if okR, whyR := learn.CheckClockRead("body.data.createdAt", oneSidedCheck, learn.Leaf{Path: "body.data.createdAt", Type: learn.Number, Value: hourR}, learn.Span{From: rPrevAt, To: rCallAt}, true); !okR {
			t.Fatalf("test setup: one-sided rule must hold recorded run: %s", whyR)
		}
		if okF, whyF := learn.CheckClockRead("body.data.createdAt", oneSidedCheck, learn.Leaf{Path: "body.data.createdAt", Type: learn.Number, Value: hourF}, learn.Span{From: fPrevAt, To: fCallAt}, true); !okF {
			t.Fatalf("test setup: one-sided rule must hold round F: %s", whyF)
		}
		oneSidedRule := []models.DeclaredSource{{
			Rule:  "hour-lookback",
			Kind:  models.DeclaredSourceClock,
			Field: "body.data.createdAt",
			Clock: &oneSidedOff,
		}}
		for _, timeForm := range []bool{false, true} {
			served, miss, db := run(optsCfg{
				recSec: hourR, liveSec: hourF, recTag: "same", liveTag: "same",
				multiWord: false, withBegin: false, timeForm: timeForm, customClockRule: oneSidedRule,
			})
			if served == nil || miss != nil || len(db.consumed) != 1 {
				t.Fatalf("one-sided rule (timeForm=%v): served=%v miss=%+v; want bound to test-4 (not dropped to refusal)", timeForm, served != nil, miss)
			}
			if _, isClock := db.consumed[0].Spec.Clocks["body.data.createdAt"]; isClock {
				t.Fatalf("one-sided rule (timeForm=%v): learned ClockRead %+v; want bound to test-4", timeForm, db.consumed[0].Spec.Clocks)
			}
			var fromTest4 bool
			for _, v := range db.consumed[0].Spec.LearnedBinds()["test case test-4"] {
				if v.Bind == "body.replayId#7" && v.Recorded == hourR {
					fromTest4 = true
				}
			}
			if !fromTest4 {
				t.Fatalf("one-sided rule (timeForm=%v): LearnedBinds=%+v; want bound to test-4 body.replayId#7", timeForm, db.consumed[0].Spec.LearnedBinds())
			}
		}
	})

	t.Run("non-declared field on same HTTP call still binds to test-4", func(t *testing.T) {
		served, miss, db := run(optsCfg{
			recSec: rSec, liveSec: fSec, recTag: "tag-rec-1", liveTag: "tag-live-2",
			withBegin: true, timeForm: false, withClockRule: true,
		})
		if served == nil || miss != nil || len(db.consumed) != 1 {
			t.Fatalf("multi-field: served=%v miss=%+v; want PUT answered", served != nil, miss)
		}
		o, ok := db.consumed[0].Spec.Clocks["body.data.createdAt"]
		if !ok || !strings.Contains(o.Why, ruleName) || !strings.Contains(o.Why, "test case test-4 body.replayId#7") {
			t.Fatalf("multi-field: Clocks = %+v; want createdAt ClockRead naming %s and test-4", db.consumed[0].Spec.Clocks, ruleName)
		}
		var boundTag bool
		for _, v := range db.consumed[0].Spec.Learned() {
			if v.Recorded == "tag-rec-1" && len(v.Use) == 1 && v.Use[0] == "body.data.tag" {
				boundTag = true
			}
		}
		var fromTest4 bool
		for _, v := range db.consumed[0].Spec.LearnedBinds()["test case test-4"] {
			if v.Bind == "body.tag" && v.Recorded == "tag-rec-1" {
				fromTest4 = true
			}
		}
		if !boundTag || !fromTest4 {
			t.Fatalf("multi-field: Learned=%+v LearnedBinds=%+v; want body.data.tag bound to test-4 body.tag", db.consumed[0].Spec.Learned(), db.consumed[0].Spec.LearnedBinds())
		}
	})
}

func TestLearnPlanEchoReuseBackfillsWhy(t *testing.T) {
	tr := learn.NewTracker()
	tr.Begin()
	testNum, _, _, _ := tr.Test()
	m := learnRecorded("mock-0", 0, "PUT", "/v1/secret/data/keploy/personal-access-tokens/keploy.io/pat-1",
		`{"data":{"aDup":1794304892,"createdAt":1794304892}}`, nil,
		`{"data":{"createdAt":1794304892}}`)
	rec := learn.Shape{Fixed: map[string]string{"body": "json"}}
	ans := []learn.Leaf{{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: learn.Number, Value: "1794304892"}}
	const wantWhy = "set aside test case test-4 body.replayId#7 under keploy.yml's rule pat-created-at"
	learned := []learn.Learned{
		{
			Kind:     learn.KindEcho,
			Recorded: learn.Leaf{Path: "body.data.aDup", Noise: "body.data.aDup", Type: learn.Number, Value: "1794304892"},
			Live:     learn.Leaf{Path: "body.data.aDup", Noise: "body.data.aDup", Type: learn.Number, Value: "1794304982"},
			Echo:     ans,
		},
		{
			Kind:     learn.KindEcho,
			Recorded: learn.Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: learn.Number, Value: "1794304892"},
			Live:     learn.Leaf{Path: "body.data.createdAt", Noise: "body.data.createdAt", Type: learn.Number, Value: "1794304982"},
			Echo:     ans,
			Why:      wantWhy,
		},
	}
	p, why := learnPlan(tr, testNum, m, rec, true, learned, nil, nil)
	if why != "" {
		t.Fatalf("learnPlan failed: %s", why)
	}
	if len(p.vars) != 1 {
		t.Fatalf("expected 1 reused echo variable, got %+v", p.vars)
	}
	for _, v := range p.vars {
		if !v.Echo || v.Why != wantWhy {
			t.Errorf("reused echo variable = %+v; want Echo=true and backfilled Why=%q", v, wantWhy)
		}
		if len(v.Use) != 1 || v.Use[0] != "body.data.createdAt" {
			t.Errorf("reused echo variable Use = %v; want [body.data.createdAt]", v.Use)
		}
	}
}
