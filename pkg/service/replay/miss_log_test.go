package replay

import (
	"encoding/json"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
)

// A call that fails a set (made before its first test, or after its last,
// that no mock answered) is logged at ERROR by its shape and its diff's
// field shapes and lengths (models.LoggedCall, LoggedDiff), as the agent
// logs the same miss: no stretch of the path's account, the query's token
// or a body value appears in any encoding. The test report keeps the call.
// Both lines logged the call's summary and its diff whole.
func TestTheSetFailingCallLinesLogNoValue(t *testing.T) {
	const token, email, account = "sk_live_Q7wZr9Kf", "grace.vault@example.net", "acct_9Lq2Zx7WbN4"
	call := models.UnmatchedCall{
		Protocol:      "HTTP",
		ActualSummary: "POST /pay/" + account + "?token=" + token,
		Destination:   "payments.internal:443",
		Diff:          `body.email: recorded "old@example.org", live "` + email + `"`,
		FieldDiffs:    []models.MockFieldDiff{{Path: "body.email", Expected: "old@example.org", Actual: email, Kind: models.DiffKindValueChanged}},
	}
	for name, log := range map[string]func(r *Replayer){
		"before the set's first test": func(r *Replayer) { r.logSetCalls("test-set-0", []models.UnmatchedCall{call}) },
		"after the set's last test":   func(r *Replayer) { r.logUnclaimed("test-set-0", []models.UnmatchedCall{call}) },
	} {
		t.Run(name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			log(&Replayer{logger: logger})
			got := out.String()
			if !strings.Contains(got, "POST /pay/{}?token={}") || !strings.Contains(got, "body.email: recorded 15 bytes, live 23 bytes") {
				t.Errorf("the line does not name the call and its diff by their shapes: %s", got)
			}
			for what, v := range map[string]string{"the token": token, "the e-mail": email, "the account": account} {
				if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}

// A call whose parser stated its log summary (models.UnmatchedCall.LogSummary,
// carried from the agent's report over the agent's JSON) is named by it on the
// CLI's lines, as the agent's own line names it: never by its summary, which
// holds the key. Both lines named such a call by its protocol alone ("a
// Redis call"), or not at all.
func TestTheSetFailingCallLinesNameACallByItsStatedLogSummary(t *testing.T) {
	const key = "user_7Hq2Lw9Zx@example.org"
	sent, err := json.Marshal(models.UnmatchedCall{
		Protocol:      "Redis",
		ActualSummary: "GET session:" + key,
		LogSummary:    "GET session:{}",
		Destination:   "cache.internal:6379",
	})
	if err != nil {
		t.Fatal(err)
	}
	var call models.UnmatchedCall
	if err := json.Unmarshal(sent, &call); err != nil {
		t.Fatal(err)
	}
	if got := models.LoggedCallName(call); got != "GET session:{}" {
		t.Fatalf("the call as the agent sent it is named %q, want its stated log summary %q", got, "GET session:{}")
	}
	for name, log := range map[string]func(r *Replayer){
		"before the set's first test": func(r *Replayer) { r.logSetCalls("test-set-0", []models.UnmatchedCall{call}) },
		"after the set's last test":   func(r *Replayer) { r.logUnclaimed("test-set-0", []models.UnmatchedCall{call}) },
	} {
		t.Run(name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			log(&Replayer{logger: logger})
			got := out.String()
			if !strings.Contains(got, "GET session:{}") {
				t.Errorf("the line does not name the call by its stated log summary: %s", got)
			}
			if found := logleak.Find(t, got, []byte(key)); len(found) > 0 {
				t.Errorf("the log holds the document's key: %v", found)
			}
		})
	}
}

// A template's capture and a template failure are logged by their key, their
// place's shape and their lengths: no stretch of the value the app issued (a
// cookie, a token), of what the answer held there, or of a templated key's
// live value appears in any encoding. The report keeps them. The capture line
// logged the value, the failure line its why whole, which quotes the answer;
// a place was shaped after its last space only (a key holding a space, a
// person's name, was logged), and the set check's refusal, uncaptured-key and
// refused-test lines logged their places raw and quoted the recorded value.
func TestTemplateLinesLogNoValue(t *testing.T) {
	const token, cookie, liveKey, person, recorded = "sk_live_Q7wZr9Kf", "sess_Hq2Lw9ZxP4", "u_LIVE7Qz9Wk", "Ada Lovelace", "/jobs/j_REC7Hq2Lw9"
	logger, out := logleak.Logger(nil)
	r := &Replayer{logger: logger}
	r.logCaptures("login", []templates.Capture{{Key: "token", Value: token, Where: "body.token"}})
	r.logTemplateFailures("profile", "test-set-0", []templates.Failure{
		{Where: "header Set-Cookie.sid", Placeholder: "{{string .sid}}", Why: `needs the answer to read "sid="…"" around it; it reads "sid=` + cookie + `"`},
		{Where: "body key roles." + liveKey, Placeholder: "{{string .id}}", Why: "must name the key " + liveKey + " there; the answer has no such key"},
		{Where: "body.users." + person + ".id", Placeholder: "{{string .id}}", Why: "needs a string there; the answer has none"},
	}, nil, true)
	// The set check's lines: a refusal (its why quotes the recorded value),
	// a key no earlier test case captures, and the error a refused test case
	// fails with.
	refusal := templates.Refusal{TestCase: "poll", Where: "body.users." + person + ".url", Placeholder: "{{string .loc}}", Why: "the URL is read by its parts, and the recorded value \"" + recorded + "\" holds \"/\""}
	r.logTemplateCheck("test-set-0", templates.CheckResult{
		Refused:    map[string][]templates.Refusal{"poll": {refusal}},
		Uncaptured: []templates.Undefined{{Key: "uid", TestCase: "read", Where: "body.users." + person + ".uid", Placeholder: "{{string .uid}}", Recorded: recorded}},
	}, nil)
	logger.Error("failed the test without sending it", zap.Error(refusalError([]templates.Refusal{refusal})))
	got := out.String()
	for _, want := range []string{`"value_bytes":16`, "header Set-Cookie.sid", "body key roles.{}", "bytes, in the report's template_failures", "body.users.{}.id", "body.users.{}.url", "body.users.{}.uid"} {
		if !strings.Contains(got, want) {
			t.Errorf("the lines do not say %q: %s", want, got)
		}
	}
	for what, v := range map[string]string{"the captured token": token, "the answer's cookie": cookie, "the key's live value": liveKey, "a key that names a user": person, "the recorded value": recorded} {
		if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}

// The Warn line that a noise entry covers a template variable's place names
// the entry by its shape (models.LearnedFieldShape), as it names the place
// (templates.PlaceShape): the entry covers the place, so its keys are the
// place's, and a key may be a user's name or ID. The line shaped the place
// and wrote the entry whole.
func TestNoiseOverAVariableLineNamesTheEntryByItsShape(t *testing.T) {
	const person, id = "Ada Lovelace", "u_7Hq2Lw9ZxK"
	logger, out := logleak.Logger(nil)
	r := &Replayer{logger: logger, config: &config.Config{}}
	tc := &models.TestCase{Name: "profile", Noise: map[string][]string{"body.users." + person + "." + id: {}}}
	place := templates.Place{
		TestCase:    "profile",
		Where:       "body.users." + person + "." + id + ".token",
		Placeholder: templates.Placeholder{Text: "{{string .token}}"},
		Path:        []templates.PathSeg{{Key: "users"}, {Key: person}, {Key: id}, {Key: "token"}},
	}
	r.warnNoiseOverVariables("test-set-0", []templates.Place{place}, []*models.TestCase{tc}, nil)
	got := out.String()
	for _, want := range []string{"a noise entry covers a template variable's place", `"noise":"body.users.{}.{} (this test case)"`, `"where":"body.users.{}.{}.token"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the line does not say %q: %s", want, got)
		}
	}
	for what, v := range map[string]string{"a key that names a user": person, "a key that is a user's ID": id} {
		if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
			t.Errorf("the log holds %s: %v\n%s", what, found, got)
		}
	}
}

// The learn step's lines name each learned field by its shape
// (models.LearnedFieldShape): a body keyed by a user's ID learns noise under
// it, and the line wrote the ID whole, at Info.
func TestLearnedFieldLinesNameAFieldByItsShape(t *testing.T) {
	const id = "u_7Hq2Lw9Zx"
	mapping := &models.Mapping{TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-1"}}}}}
	lines := learnedByTest(map[string][]string{"mock-1": {"body.users." + id + ".lastSeen (noise)"}}, mapping)
	if len(lines) != 1 || len(lines[0].fields) != 1 || lines[0].fields[0] != "mock-1 body.users.{}.lastSeen (noise)" {
		t.Fatalf("learnedByTest = %+v, want test-1: mock-1 body.users.{}.lastSeen (noise)", lines)
	}
}
