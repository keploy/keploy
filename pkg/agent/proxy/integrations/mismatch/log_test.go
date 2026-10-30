package mismatch

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// A miss report, as a log line names it (LogCall, LogDiff), holds no stretch
// of a value the call sent or its recording holds, in any encoding, for each
// parser family the OSS agent reports misses for (HTTP, MySQL, generic, DNS;
// Postgres's parser is not in the OSS agent): a field diff gives the field's
// shape, how it differs and its values' lengths, and a diff written as text
// its length, but for the generic parser's, which keploy writes from a
// recording's name and a similarity. The report itself keeps the values, so
// each row checks it held them. The log line printed the summary and the diff
// whole.
func TestLogCallAndLogDiff_HoldNoValue(t *testing.T) {
	const token, email, account = "sk_live_Q7wZr9Kf", "grace.vault@example.net", "acct_9Lq2Zx7WbN4"
	planted := map[string]string{"the token": token, "the e-mail": email, "the account": account}
	for _, c := range []struct {
		name               string
		report             *models.MockMismatchReport
		wantCall, wantDiff string
		// keeps: the report holds a planted value the log form must not.
		keeps bool
	}{
		{"HTTP, field diffs",
			NewReport(ProtocolHTTP, "POST /pay/"+account+"?token="+token).
				WithClosest("mock-3", []models.MockFieldDiff{
					{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "ada.vault@example.org", Actual: email},
					{Path: "query.token", Kind: models.DiffKindMissingInMock, Actual: token},
					{Path: "body.accounts." + account, Kind: models.DiffKindMissingInLive, Expected: "1"},
				}).Build(),
			"POST /pay/{}?token={}",
			"body.email: recorded 21 bytes, live 23 bytes; query.token: live (16 bytes), absent in recording; body.accounts.{}: recorded (1 bytes), absent in live call", true},
		{"HTTP, a diff written as text",
			NewReport(ProtocolHTTP, "GET /users/"+account).WithDiff(`mock mock-3: variable user is "` + email + `" in this test`).Build(),
			"GET /users/{}", "68 bytes, in the report", true},
		{"MySQL",
			NewReport(ProtocolMySQL, "COM_STMT_EXECUTE SELECT id FROM users WHERE email = ? params=["+email+"]").
				WithDiff("actual: SELECT id FROM users WHERE email = ? params=[" + email + "]\nclosest: SELECT id FROM users WHERE email = ? params=[" + token + "]").Build(),
			"COM_STMT_EXECUTE SELECT id FROM users WHERE email = ?", "149 bytes, in the report", true},
		{"generic",
			NewReport(ProtocolGeneric, "opaque TCP exchange (1 request buffer(s), first 40 bytes)").
				WithDiff(`closest recorded exchange "mock-7" has Jaccard similarity 0.83 (thresholds: 0.9 per-test, 0.4 session)`).Build(),
			"opaque TCP exchange (1 request buffer(s), first 40 bytes)",
			`closest recorded exchange "mock-7" has Jaccard similarity 0.83 (thresholds: 0.9 per-test, 0.4 session)`, false},
		{"DNS", NewReport(ProtocolDNS, "A api.payments.internal.").Build(), "A api.payments.internal.", "", false},
		// MongoDB's shape (models.CallShape) is its command and namespace,
		// each by its look; the document is left out.
		{"MongoDB", NewReport(ProtocolMongo, `find on users {"email":"`+email+`"}`).Build(), "find on users", "", true},
		// A protocol keploy has no shape for is not named by its summary.
		{"Redis", NewReport(ProtocolRedis, "GET session:"+email).Build(), "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			call, diff := LogCall(c.report), LogDiff(c.report)
			if call != c.wantCall {
				t.Errorf("LogCall = %q, want %q", call, c.wantCall)
			}
			if diff != c.wantDiff {
				t.Errorf("LogDiff = %q, want %q", diff, c.wantDiff)
			}
			for what, v := range planted {
				if found := logleak.Find(t, call+"\n"+diff, []byte(v)); len(found) > 0 {
					t.Errorf("the log form holds %s: %v", what, found)
				}
			}
			whole := c.report.ActualSummary + " " + c.report.Diff
			held := false
			for _, v := range planted {
				held = held || strings.Contains(whole, v)
			}
			if c.keeps && !held {
				t.Errorf("the report holds none of the planted values, so the row checks nothing: %q", whole)
			}
		})
	}
}

// A path segment the report says holds a value (a variable's place, or a
// url.path[<n>] field diff) is {} in the logged call, whatever its look: a
// lowercase name, a user's handle, reads as a name to models.PathShape. It
// was logged.
func TestLogCall_WritesAMarkedSegmentAsAValue(t *testing.T) {
	for name, r := range map[string]*models.MockMismatchReport{
		"a field diff's place": {Protocol: ProtocolHTTP, ActualSummary: "GET /users/gracevault/orders",
			FieldDiffs: []models.MockFieldDiff{{Path: "url.path[1]", Expected: "adalovel", Actual: "gracevault"}}},
		"a variable's place": {Protocol: ProtocolHTTP, ActualSummary: "GET /users/gracevault/orders", VariablePlaces: []string{"url.path[1]"}},
	} {
		if got := LogCall(r); got != "GET /users/{}/orders" {
			t.Errorf("%s: LogCall = %q, want GET /users/{}/orders", name, got)
		}
	}
}

// A next step names the fields that differed, and a map's key can be a value
// the app sent (an e-mail address, a user ID): the logged next step writes
// each field path in its shape (models.FieldShape), as a proven run's
// evidence does. The rest of the text is keploy's own words, and stays.
func TestLogNextStep_WritesEachFieldPathInItsShape(t *testing.T) {
	const email = "grace.vault@example.net"
	r := &models.MockMismatchReport{Protocol: ProtocolHTTP,
		NextSteps:  "keploy's learn step: body.users." + email + ".score changed, and header.X-Trace changed. Re-record the test.",
		FieldDiffs: []models.MockFieldDiff{{Path: "body.users." + email + ".score"}, {Path: "header.X-Trace"}}}
	got := LogNextStep(r)
	if found := logleak.Find(t, got, []byte(email)); len(found) > 0 {
		t.Errorf("the logged next step holds the key's value: %v in %q", found, got)
	}
	for _, kept := range []string{"keploy's learn step: body.users.", ".{}.", "header.X-Trace changed. Re-record the test."} {
		if !strings.Contains(got, kept) {
			t.Errorf("the logged next step lost %q: %q", kept, got)
		}
	}
	if LogNextStep(nil) != "" || LogNextStep(&models.MockMismatchReport{}) != "" {
		t.Error("no report, or a report with no next step, logs no next step")
	}
}

// A body path the next step names but no field diff carries (a learn step's
// reason names the fields it learned, which need not have differed in this
// call) is written in its shape too: the text is read for body paths, not
// only for the diffs' paths.
func TestLogNextStep_WritesABodyPathNoDiffCarriesInItsShape(t *testing.T) {
	const id = "u_7Hq2Lw9ZxK"
	r := &models.MockMismatchReport{Protocol: ProtocolHTTP,
		NextSteps:  "keploy's learn step: body.users." + id + ".lastSeen changed between replays, so it is noise.",
		FieldDiffs: []models.MockFieldDiff{{Path: "header.X-Trace"}}}
	got := LogNextStep(r)
	if found := logleak.Find(t, got, []byte(id)); len(found) > 0 {
		t.Errorf("the logged next step holds the key's value: %v in %q", found, got)
	}
	if want := "keploy's learn step: body.users.{}.lastSeen changed between replays, so it is noise."; got != want {
		t.Errorf("LogNextStep = %q, want %q", got, want)
	}
}

// A parser that states the call's log summary (WithLogSummary: a command and
// a collection, no document) has it logged as written, in preference to
// keploy's own shaping, so two misses of a protocol keploy has no shape for
// no longer read alike ("a Redis call"); a report without one is logged as
// before: an HTTP call by its shape, a Redis call not at all. LogCall shaped
// every report and logged no Redis call.
func TestLogCall_PrefersTheParsersLogSummary(t *testing.T) {
	const email, token = "grace.vault@example.net", "sk_live_Q7wZr9Kf"
	getSummary := "GET session:" + email
	httpSummary := "POST /pay/acct_9Lq2Zx7WbN4?token=" + token
	for _, c := range []struct {
		name   string
		report *models.MockMismatchReport
		want   string
	}{
		{"Redis, stated", NewReport(ProtocolRedis, getSummary).WithLogSummary("GET session:{}").Build(), "GET session:{}"},
		{"Redis, not stated", NewReport(ProtocolRedis, getSummary).Build(), ""},
		{"HTTP, stated", NewReport(ProtocolHTTP, httpSummary).WithLogSummary("POST payments.charge").Build(), "POST payments.charge"},
		{"HTTP, not stated", NewReport(ProtocolHTTP, httpSummary).Build(), "POST /pay/{}?token={}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := LogCall(c.report)
			if got != c.want {
				t.Errorf("LogCall = %q, want %q", got, c.want)
			}
			for _, v := range []string{email, token} {
				if found := logleak.Find(t, got, []byte(v)); len(found) > 0 {
					t.Errorf("the logged call holds a planted value: %v in %q", found, got)
				}
			}
		})
	}
}
