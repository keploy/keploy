// Package proxy — the one agent-wide mock-mismatch log line must not assert a
// negative it never checked.
//
// destination_scope is only meaningful for a report whose parser supplied
// destination evidence. HTTP is the only protocol that does today (nothing
// else records an upstream authority in its mocks), so an unconditional field
// printed a negative verdict on every mongo/mysql/generic/pulsar miss — which
// reads as "we checked, and it was fine" for a check that never ran.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// mismatchLogFields is the fields of the one mock-mismatch line a miss logs.
// Its level is the proven switch's to decide (a WARN with the switch off; at
// Debug under it, where keploy's verdict names each miss): its fields are the
// same either way, so the line is found by its message, at any level, whatever
// switch the test runs under.
func mismatchLogFields(t *testing.T, report *models.MockMismatchReport) map[string]any {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 4)}

	p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, report))

	entries := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one mismatch log entry, got %d: %v", len(entries), logs.All())
	}
	return entries[0].ContextMap()
}

func TestSendMockNotFoundError_ScopeFieldOmittedWithoutEvidence(t *testing.T) {
	// A Mongo miss: no destination evidence was ever gathered.
	fields := mismatchLogFields(t, &models.MockMismatchReport{
		Protocol:      "MongoDB",
		Destination:   "mongo.example.com:27017",
		ActualSummary: "find users",
		MatchPhase:    models.MatchPhaseExhausted,
	})

	if _, present := fields["destination_scope"]; present {
		t.Errorf("unchecked destination scope must be absent from the log, got %v", fields["destination_scope"])
	}
	if fields["match_phase"] != models.MatchPhaseExhausted {
		t.Errorf("match_phase = %v, want %q", fields["match_phase"], models.MatchPhaseExhausted)
	}
}

func TestSendMockNotFoundError_ScopeFieldReflectsTheVerdict(t *testing.T) {
	for _, scope := range []string{
		models.DestinationScopeNotInComparedSet,
		models.DestinationScopeInComparedSet,
	} {
		t.Run(scope, func(t *testing.T) {
			fields := mismatchLogFields(t, &models.MockMismatchReport{
				Protocol:         "HTTP",
				Destination:      "192.0.2.20",
				ActualSummary:    "GET /v1/metrics",
				MatchPhase:       models.MatchPhaseSchema,
				DestinationScope: scope,
			})
			got, present := fields["destination_scope"]
			if !present {
				t.Fatalf("an answered scope check must be logged; fields: %v", fields)
			}
			if got != scope {
				t.Errorf("destination_scope = %v, want %v", got, scope)
			}
			// The cascade's stopping point survives alongside the verdict.
			if fields["match_phase"] != models.MatchPhaseSchema {
				t.Errorf("match_phase = %v, want %q", fields["match_phase"], models.MatchPhaseSchema)
			}
		})
	}
}

// The mismatch line, logged at WARN with the switch off, names the call as a
// log names it (mismatch.LogCall): an HTTP call by its method and its target's
// shape, a statement with its literals "?", a generic exchange by keploy's
// counts, a DNS question by its type and name, a MongoDB command by its
// command and namespace, and a call of a protocol keploy has no shape for by
// nothing: the field is left out, not logged empty. No stretch of a query
// value, a path segment's value, a literal or a bound parameter appears in
// the log in any encoding. It logged the summary whole.
func TestSendMockNotFoundError_LogsTheCallInItsShape(t *testing.T) {
	const token, email, account = "sk_live_Q7wZr9Kf", "grace.vault@example.net", "acct_9Lq2Zx7WbN4"
	for _, c := range []struct {
		protocol, summary, want string
	}{
		{"HTTP", "POST /pay/" + account + "?token=" + token + "&email=" + email, "POST /pay/{}?token={}&email={}"},
		{"MySQL", "SELECT id FROM users WHERE email = '" + email + "' AND token = ? params=[" + token + "]", "SELECT id FROM users WHERE email = ? AND token = ?"},
		{"Generic", "opaque TCP exchange (2 request buffer(s), first 40 bytes)", "opaque TCP exchange (2 request buffer(s), first 40 bytes)"},
		{"DNS", "A api.payments.internal.", "A api.payments.internal."},
		{"MongoDB", `find on users {"email":"` + email + `","token":"` + token + `"}`, "find on users"},
		{"Redis", "GET session:" + email + ":" + token, ""},
	} {
		t.Run(c.protocol, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
			p.sendMockNotFoundError(models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
				Protocol: c.protocol, Destination: "db.internal:3306", ActualSummary: c.summary, MatchPhase: models.MatchPhaseExhausted,
			}))
			entries := logs.FilterMessageSnippet("mock mismatch: no matching mock for outgoing call").All()
			if len(entries) != 1 {
				t.Fatalf("expected exactly one mismatch log entry, got %d: %v", len(entries), logs.All())
			}
			got, present := entries[0].ContextMap()["actual"]
			if c.want == "" && present {
				t.Errorf("a call keploy has no shape for is left out of the line, got actual = %q", got)
			}
			if c.want != "" && got != c.want {
				t.Errorf("actual = %q, want the call's shape %q", got, c.want)
			}
			log := out.String()
			if !strings.Contains(log, "mock mismatch: no matching mock for outgoing call") {
				t.Fatalf("the rendered log is not the one checked: %s", log)
			}
			for what, content := range map[string]string{"the token": token, "the e-mail": email, "the account in the path": account} {
				if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
					t.Errorf("the log holds %s: %v", what, found)
				}
			}
		})
	}
}

// The misses keploy drops (made while no test's capture window was open, or
// of a test whose window was never read) are named by the calls as a log
// names them (mismatch.LogCall), a miss with no report by its kind: no
// statement's literal, path value, document or error text of the call is in
// the WARN that says they were dropped. It wrote each one's summary, or its
// error's text, as it was.
func TestLogDroppedMisses_NamesTheCallsByTheirShape(t *testing.T) {
	const email, account, doc, errText = "ada.brgnhf@mail.test", "acct_7Q2xLp9Zr4mW", "zqvkxjwplmtr", "grace.qzvk@mail.test"
	miss := func(protocol, summary string) error {
		return models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: models.ErrNoMockMatched,
			MismatchReport: &models.MockMismatchReport{Protocol: protocol, ActualSummary: summary}}
	}
	errs := []error{
		miss("MySQL", "SELECT * FROM users WHERE email = '"+email+"'"),
		miss("HTTP", "GET /accounts/"+account+"/keys"),
		miss("Redis", "GET token:"+doc),
		models.ParserError{ParserErrorType: models.ErrMockNotFound, Err: errors.New("no mock for " + errText)},
	}
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	p := &Proxy{logger: logger}
	p.logDroppedMisses("mock mismatches dropped", errs)
	lines := logs.FilterMessage("mock mismatches dropped").All()
	if len(lines) != 1 || lines[0].Level != zapcore.WarnLevel {
		t.Fatalf("want one WARN naming the dropped misses; got %v", logs.All())
	}
	want := []interface{}{"SELECT * FROM users WHERE email = ?", "GET /accounts/{}/keys", "a Redis call", "a miss with no report (mock not found)"}
	if calls, _ := lines[0].ContextMap()["calls"].([]interface{}); !reflect.DeepEqual(calls, want) {
		t.Errorf("the line names the calls %q; want %q", calls, want)
	}
	for what, content := range map[string]string{"a statement's literal": email, "a path's account": account, "a document's value": doc, "an error's text": errText} {
		if found := logleak.Find(t, out.String(), []byte(content)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}

// A miss whose parser gave no report is logged by its error's length: the
// parser's own text may quote the call (a statement, a key, a document), as
// missSummaries says of it. The line logged the error whole, at WARN.
func TestSendMockNotFoundError_AMissWithNoReportLogsNoErrorText(t *testing.T) {
	const token, email = "sk_live_Q7wZr9Kf", "grace.vault@example.net"
	obs, logs := observer.New(zapcore.DebugLevel)
	logger, out := logleak.Logger(obs)
	p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
	err := fmt.Errorf(`no mock for find users {"email":"%s","token":"%s"}`, email, token)
	p.sendMockNotFoundError(err)
	entries := logs.FilterMessage("mock mismatch: no matching mock for outgoing call (no structured report)").All()
	if len(entries) != 1 {
		t.Fatalf("expected exactly one mismatch log entry, got %d: %v", len(entries), logs.All())
	}
	if got := entries[0].ContextMap()["error_bytes"]; got != int64(len(err.Error())) {
		t.Errorf("error_bytes = %v, want %d", got, len(err.Error()))
	}
	log := out.String()
	if !strings.Contains(log, "no structured report") {
		t.Fatalf("the rendered log is not the one checked: %s", log)
	}
	for what, content := range map[string]string{"the token": token, "the e-mail": email} {
		if found := logleak.Find(t, log, []byte(content)); len(found) > 0 {
			t.Errorf("the log holds %s: %v", what, found)
		}
	}
}

// A miss with no report names its cause, never its text: a genuine miss by
// models.ErrNoMockMatched, keploy's own failure by the standard error it ends
// on (a decode cut short, a closed connection), so the WARN still tells them
// apart with the text left out. The line named only its error's length, and a
// keploy failure could not be diagnosed. A fabricated address names no
// server: it is not the line's destination, as a report's destination leaves
// it out.
func TestSendMockNotFoundError_AMissWithNoReportNamesItsCauseNotItsText(t *testing.T) {
	const token = "sk_live_Q7wZr9Kf"
	cases := []struct {
		name string
		err  error
		dst  *models.ConditionalDstCfg
		want string
		addr string
	}{
		{"a genuine miss", fmt.Errorf("no mock for get %s: %w", token, models.ErrNoMockMatched), &models.ConditionalDstCfg{Addr: "10.0.0.7:6379"}, "no mock matched", "10.0.0.7:6379"},
		{"a decode cut short", fmt.Errorf("decode OP_MSG after %s: %w", token, io.ErrUnexpectedEOF), nil, "unexpected EOF", ""},
		{"a closed connection", fmt.Errorf("read reply to %s: %w", token, net.ErrClosed), &models.ConditionalDstCfg{Addr: "169.254.0.9:80", AddrFabricated: true}, "closed connection", ""},
		{"a timeout", fmt.Errorf("wait for %s: %w", token, context.DeadlineExceeded), nil, "timeout", ""},
		{"none of them", fmt.Errorf("no mock for %s", token), nil, "other", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			p := &Proxy{logger: logger, errChannel: make(chan error, 4)}
			p.sendMockNotFoundErrorFrom(tc.err, 0, tc.dst)
			entries := logs.FilterMessage("mock mismatch: no matching mock for outgoing call (no structured report)").All()
			if len(entries) != 1 {
				t.Fatalf("expected exactly one mismatch log entry, got %d: %v", len(entries), logs.All())
			}
			ctx := entries[0].ContextMap()
			if got := ctx["cause"]; got != tc.want {
				t.Errorf("cause = %v, want %q", got, tc.want)
			}
			if got, _ := ctx["destination"].(string); got != tc.addr {
				t.Errorf("destination = %q, want %q", got, tc.addr)
			}
			if found := logleak.Find(t, out.String(), []byte(token)); len(found) > 0 {
				t.Errorf("the log holds the error's token: %v", found)
			}
		})
	}
}

// missSummaries names a call keploy has no shape for as every CLI line does
// (models.LoggedCallName): by its protocol, and a report with none as "a
// call". The proxy's own copy of the rule named it "a  call".
func TestMissSummariesNameACallAsTheCLIDoes(t *testing.T) {
	for _, r := range []*models.MockMismatchReport{
		{ActualSummary: "get session:Q7wZr9Kf"},
		{Protocol: "Redis", ActualSummary: "get session:Q7wZr9Kf"},
		{Protocol: "MongoDB", ActualSummary: "find orders", LogSummary: "find orders"},
	} {
		got := missSummaries([]error{models.ParserError{MismatchReport: r}})
		want := models.LoggedCallName(models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, LogSummary: r.LogSummary})
		if len(got) != 1 || got[0] != want {
			t.Errorf("missSummaries(%+v) = %q, want [%q]", *r, got, want)
		}
	}
	if got := missSummaries([]error{models.ParserError{MismatchReport: &models.MockMismatchReport{}}}); got[0] != "a call" {
		t.Errorf("a report with no protocol is named %q, want \"a call\"", got[0])
	}
}
