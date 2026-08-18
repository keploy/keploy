package replayer

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestMatchCommand_ShowFullFields_TraceCommentCrossServe reproduces the
// cross-serving bug behind the auto-replay 500s.
//
// The app runs a Datadog DBM / sqlcommenter build that appends a unique
// /*traceparent=...*/ comment to every statement, so the recorded and replayed
// text of the SAME query never match byte-for-byte. For a non-DML statement like
// SHOW FULL FIELDS, matchQuery then can't reach its exact-text return and falls
// to equal-PayloadLength-alone (score 1). matchCommand serves the FIRST mock that
// scored 1 — so a SHOW FULL FIELDS on one table is answered with another table's
// column metadata, which surfaces in the app as `undefined method '...' for an
// instance of <Model>`.
//
// Before the trace-comment normalization this test fails (the first same-length
// mock's columns are served); after it, the correct table's mock matches
// exactly and is served.
func TestMatchCommand_ShowFullFields_TraceCommentCrossServe(t *testing.T) {
	logger := zap.NewNop()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	// Two non-DML SHOW FULL FIELDS statements on different tables, each recorded
	// with its own trace comment. readbackMock stamps the same PayloadLength on
	// both — the equal-length condition the writer/reader prologues produce.
	mockVersions := readbackMock(
		"versions",
		"SHOW FULL FIELDS FROM `versions` /*traceparent='00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1111111111111111-01'*/",
		"COLS=versions", base)
	mockAccounts := readbackMock(
		"accounts",
		"SHOW FULL FIELDS FROM `accounts` /*traceparent='00-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-2222222222222222-01'*/",
		"COLS=accounts", base.Add(time.Second))

	// versions is recorded FIRST, so a length-only score-1 match serves it.
	db := &fakeMockDb{session: []*models.Mock{mockVersions, mockAccounts}}

	// The live call is SHOW FULL FIELDS FROM accounts, carrying yet another
	// trace comment (the replay-time span differs from the recorded one).
	live := comQueryReq("SHOW FULL FIELDS FROM `accounts` /*traceparent='00-cccccccccccccccccccccccccccccccc-3333333333333333-01'*/")

	resp, ok, _, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), nil, nil)
	if err != nil || !ok || resp == nil {
		t.Fatalf("expected a match for SHOW FULL FIELDS FROM accounts, got ok=%v err=%v", ok, err)
	}
	if resp.Payload != "COLS=accounts" {
		t.Fatalf("cross-serve: SHOW FULL FIELDS FROM `accounts` was answered with %q (a different table's columns) — the trace comment defeated exact matching and equal-length-alone served the first mock", resp.Payload)
	}
}

func TestStripSQLComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no comment", "SELECT 1", "SELECT 1"},
		{"trailing sqlcommenter", "SELECT 1 /*traceparent='00-abc-def-01'*/", "SELECT 1"},
		{"leading comment", "/*app='web'*/ SELECT 1", "SELECT 1"},
		{"two show-full-fields differ only by comment collapse equal",
			"SHOW FULL FIELDS FROM `accounts` /*t=1*/", "SHOW FULL FIELDS FROM `accounts`"},
		{"preserve executable comment", "SELECT /*!40001 SQL_NO_CACHE */ 1", "SELECT /*!40001 SQL_NO_CACHE */ 1"},
		{"preserve optimizer hint", "SELECT /*+ NO_INDEX(t) */ a FROM t", "SELECT /*+ NO_INDEX(t) */ a FROM t"},
		{"do not strip inside string literal", "SELECT '/*not a comment*/' AS x", "SELECT '/*not a comment*/' AS x"},
		{"do not strip inside backtick identifier", "SELECT `/*weird*/col` FROM t", "SELECT `/*weird*/col` FROM t"},
		{"unterminated comment drops remainder", "SELECT 1 /*oops", "SELECT 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripSQLComments(c.in); got != c.want {
				t.Errorf("stripSQLComments(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}

	// The property that actually matters for matching: the same base statement
	// with two different trace comments normalizes to the same string, and two
	// different base statements do not.
	a := stripSQLComments("SHOW FULL FIELDS FROM `accounts` /*traceparent='00-aaaa-1111-01'*/")
	b := stripSQLComments("SHOW FULL FIELDS FROM `accounts` /*traceparent='00-bbbb-2222-01',dddbs='web'*/")
	if a != b {
		t.Errorf("same base query with different trace comments must normalize equal: %q vs %q", a, b)
	}
	c := stripSQLComments("SHOW FULL FIELDS FROM `versions` /*traceparent='00-cccc-3333-01'*/")
	if a == c {
		t.Errorf("different base queries must not normalize equal: %q == %q", a, c)
	}
}

// TestMatchCommand_ShowFullFields_DifferentTableStillDistinct guards the fix
// against over-normalization: stripping the trace comment must not make two
// genuinely different SHOW FULL FIELDS statements collapse into one. The live
// call for `users` must be served the users mock, not accounts.
func TestMatchCommand_ShowFullFields_DifferentTableStillDistinct(t *testing.T) {
	logger := zap.NewNop()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	mockAccounts := readbackMock("accounts",
		"SHOW FULL FIELDS FROM `accounts` /*traceparent='00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-1111111111111111-01'*/",
		"COLS=accounts", base)
	mockUsers := readbackMock("users",
		"SHOW FULL FIELDS FROM `users` /*traceparent='00-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-2222222222222222-01'*/",
		"COLS=users", base.Add(time.Second))
	db := &fakeMockDb{session: []*models.Mock{mockAccounts, mockUsers}}

	live := comQueryReq("SHOW FULL FIELDS FROM `users` /*traceparent='00-cccccccccccccccccccccccccccccccc-3333333333333333-01'*/")
	resp, ok, _, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), nil, nil)
	if err != nil || !ok || resp == nil {
		t.Fatalf("expected a match for SHOW FULL FIELDS FROM users, got ok=%v err=%v", ok, err)
	}
	if resp.Payload != "COLS=users" {
		t.Fatalf("expected COLS=users, got %q — normalization must not merge distinct tables", resp.Payload)
	}
}
