package replayer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// Datadog DBM (sqlcommenter) comments, in the shape a traced service emits.
// Every executed statement carries one, and two properties matter:
//
//   - traceparent is unique per request, so the recorded query text can NEVER
//     be byte-equal to the replayed one.
//   - ddh names the database endpoint actually used. A cluster hands out a
//     writer ("cluster-") and a reader ("cluster-ro-") endpoint, so the SAME
//     statement carries a comment three bytes longer on whichever run happened
//     to be routed to the reader.
//
// The two constants therefore differ in BOTH content and length, which is the
// whole point: neither text equality nor payload length can identify a
// statement once a tracer is in front of it.
const (
	dbmWriter = "/*dde='sandbox',ddps='web-service',ddpv='1a2b3c4d'," +
		"ddh='demo.cluster-abcdefghijkl.us-east-1.rds.amazonaws.com'," +
		"dddb='appdb',traceparent='00-1111111111111111aaaaaaaaaaaaaaaa-1111111111111111-01'*/ "
	dbmReader = "/*dde='sandbox',ddps='web-service',ddpv='5e6f7a8b'," +
		"ddh='demo.cluster-ro-abcdefghijkl.us-east-1.rds.amazonaws.com'," +
		"dddb='appdb',traceparent='00-2222222222222222bbbbbbbbbbbbbbbb-2222222222222222-01'*/ "
)

// queryBundle builds a COM_QUERY PacketBundle whose PayloadLength is the real
// wire length (1 command byte + the SQL text), which is what matchQuery
// compares.
func queryBundle(sql string) mysql.PacketBundle {
	return mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1), SequenceID: 0},
			Type:   "COM_QUERY",
		},
		Message: &mysql.QueryPacket{Command: 0x03, Query: sql},
	}
}

// TestMatchQueryPacket_TraceCommentDoesNotDefeatMatching pins the contract that
// a comment the server discards is not part of a statement's identity.
//
// A COM_QUERY carrying a Datadog DBM / sqlcommenter comment is the same
// statement as the recorded one whenever the SQL around the comment is
// identical, wherever the tracer chose to put it.
//
// Before the fix it was not: the exact-text path needs byte equality and the
// unique traceparent guarantees inequality, so matching fell through to the
// only remaining signal — equal MySQL PayloadLength — which the writer/reader
// endpoint swap destroys. The candidate then scored 0, matchCommand recorded no
// closest mock, and the connection was torn down as a missing recording.
func TestMatchQueryPacket_TraceCommentDoesNotDefeatMatching(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	const body = "SELECT `invoices`.* FROM `invoices` WHERE `invoices`.`deleted_at` IS NULL " +
		"AND `invoices`.`external_id` = 'aaaaaaaaaaaaaaaaaaaaaaaa' LIMIT 1"

	t.Run("comment differs in content only", func(t *testing.T) {
		// Same endpoint, different traceparent — equal PayloadLength.
		live := queryBundle(strings.Replace(dbmWriter,
			"traceparent='00-1111111111111111aaaaaaaaaaaaaaaa-1111111111111111-01'",
			"traceparent='00-3333333333333333cccccccccccccccc-3333333333333333-01'", 1) + body)
		recorded := queryBundle(dbmWriter + body)

		if recorded.Header.Header.PayloadLength != live.Header.Header.PayloadLength {
			t.Fatalf("test setup broken: this subtest must hold PayloadLength equal")
		}
		if ok, score := matchQueryPacket(ctx, logger, recorded, live); !ok {
			t.Fatalf("same statement with a different traceparent must match; got ok=false score=%d", score)
		}
	})

	t.Run("comment differs in length (writer vs reader endpoint)", func(t *testing.T) {
		recorded := queryBundle(dbmWriter + body)
		live := queryBundle(dbmReader + body)

		if len(dbmWriter) == len(dbmReader) {
			t.Fatalf("test setup broken: the two comments must differ in length")
		}
		if ok, score := matchQueryPacket(ctx, logger, recorded, live); !ok {
			t.Fatalf("same statement behind a different-length trace comment must match; got ok=false score=%d "+
				"(score 0 means matchCommand sees no candidate at all)", score)
		}
	})

	t.Run("comment in the middle of the statement", func(t *testing.T) {
		recorded := queryBundle("SELECT /* traceparent=aaaa */ `total` FROM `invoices` WHERE `id` = 1")
		live := queryBundle("SELECT /* traceparent=bbbb */ `total` FROM `invoices` WHERE `id` = 1")

		if ok, score := matchQueryPacket(ctx, logger, recorded, live); !ok {
			t.Fatalf("a tracer may inject its comment anywhere, not just in front; got ok=false score=%d", score)
		}
	})
}

// TestMatchQueryPacket_EqualLengthDifferentStatementsNeverMatch pins the other
// half of the same defect, which is the dangerous half: matching must not fall
// back to "the payload happens to be the same size".
//
// With a ~200-byte comment in front of every statement, unrelated queries
// collide on total length constantly — every `SHOW FULL FIELDS FROM <table>`
// with an equal-length table name measures the same. Serving one table's column
// metadata in answer to another's makes an ORM build a model with the wrong
// columns, which surfaces downstream as an application bug rather than a mock
// mismatch.
func TestMatchQueryPacket_EqualLengthDifferentStatementsNeverMatch(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	cases := []struct{ recorded, live string }{
		{"SHOW FULL FIELDS FROM `invoices`", "SHOW FULL FIELDS FROM `couriers`"},
		{"SHOW FULL FIELDS FROM `payments`", "SHOW FULL FIELDS FROM `receipts`"},
		{"SELECT `id` FROM `invoices` WHERE `id` = 1", "SELECT `id` FROM `couriers` WHERE `id` = 1"},
		// Same shape, different COLUMN: an identifier, so never interchangeable.
		{
			"SELECT `total` FROM `invoices` WHERE `external_id` = 'aaaaaaaaaaaaaaaaaaaaaaaa'",
			"SELECT `total` FROM `invoices` WHERE `internal_id` = 'aaaaaaaaaaaaaaaaaaaaaaaa'",
		},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%.44s", tc.live), func(t *testing.T) {
			recorded := queryBundle(dbmWriter + tc.recorded)
			live := queryBundle(dbmWriter + tc.live)

			if recorded.Header.Header.PayloadLength != live.Header.Header.PayloadLength {
				t.Fatalf("test setup broken: the two statements must have equal PayloadLength")
			}

			ok, score := matchQueryPacket(ctx, logger, recorded, live)
			if ok {
				t.Fatalf("different statements must never match")
			}
			if score != 0 {
				t.Fatalf("a different statement of equal byte length must score 0, got %d — "+
					"a non-zero score makes it a servable candidate in matchCommand and cross-serves "+
					"one statement's result set for another", score)
			}
		})
	}
}

// TestMatchQueryPacket_ExecutableCommentsAreNeverStripped guards the one way
// comment-tolerant matching could go wrong: two comment forms are not comments
// to the server at all.
//
//	/*! ... */  version-gated SQL — the server RUNS the contents, and the
//	            version gate decides WHICH statement runs
//	/*+ ... */  optimizer hint — changes the plan
//
// Ignoring either would equate statements the server treats differently.
func TestMatchQueryPacket_ExecutableCommentsAreNeverStripped(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	cases := []struct{ name, recorded, live string }{
		{"different version gate", "/*!40101 SET NAMES utf8 */", "/*!80000 SET NAMES utf8 */"},
		{"version gate vs none", "/*!40101 SET NAMES utf8 */", "SET NAMES utf8"},
		{"leading hint vs none", "/*+ MAX_EXECUTION_TIME(1) */ SELECT a FROM t", "SELECT a FROM t"},
		// Equal byte length, so the old payload-length tier would have made this
		// a servable candidate.
		{"leading hint differs", "/*+ MAX_EXECUTION_TIME(1) */ SELECT a FROM t", "/*+ MAX_EXECUTION_TIME(2) */ SELECT a FROM t"},
		{"inline hint differs", "SELECT /*+ MAX_EXECUTION_TIME(1) */ a FROM t", "SELECT /*+ MAX_EXECUTION_TIME(9999) */ a FROM t"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, score := matchQueryPacket(ctx, logger, queryBundle(tc.recorded), queryBundle(tc.live))
			if ok || score != 0 {
				t.Errorf("executable comments differ, so these are different statements; got ok=%v score=%d", ok, score)
			}
		})
	}
}

// TestStripInertSQLComments covers the primitive directly, including the forms
// that must survive and the quoting rules that stop it eating real SQL.
func TestStripInertSQLComments(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"leading block comment", "/* trace */ SELECT 1", "SELECT 1"},
		{"stacked comments", "/* a */ /* b */\n SELECT 1", "SELECT 1"},
		{"dbm comment", dbmWriter + "SELECT 1", "SELECT 1"},
		{"inline comment", "SELECT /* t */ a FROM b", "SELECT a FROM b"},
		{"trailing comment", "SELECT a FROM b /* t */", "SELECT a FROM b"},
		{"comment glues tokens", "SELECT/*c*/1", "SELECT 1"},
		{"line comment", "-- trace id\nSELECT 1", "SELECT 1"},
		{"hash comment", "# trace id\nSELECT 1", "SELECT 1"},
		{"double unary minus is not a comment", "SELECT --1 + 2", "SELECT --1 + 2"},
		{"unterminated block comment is left alone", "SELECT /* oops", "SELECT /* oops"},
		{"version gate survives", "/*!40101 SET NAMES utf8 */", "/*!40101 SET NAMES utf8 */"},
		{"leading hint survives", "/*+ HINT */ SELECT 1", "/*+ HINT */ SELECT 1"},
		{"inline hint survives", "SELECT /*+ HINT */ a FROM b", "SELECT /*+ HINT */ a FROM b"},
		{"comment-only has no body", "/* ping */", ""},

		// Quote awareness: these are data, not comments.
		{"block marker in string literal", "SELECT '/* not a comment */' FROM t", "SELECT '/* not a comment */' FROM t"},
		{"dashes in string literal", "SELECT 'a -- b' FROM t", "SELECT 'a -- b' FROM t"},
		{"hash in string literal", "SELECT '#tag' FROM t", "SELECT '#tag' FROM t"},
		{"block marker in identifier", "SELECT `we/*ird` FROM t", "SELECT `we/*ird` FROM t"},
		{"escaped quote inside literal", `SELECT 'it\'s /* fine */' FROM t`, `SELECT 'it\'s /* fine */' FROM t`},
		{"doubled quote inside literal", "SELECT 'it''s /* fine */' FROM t", "SELECT 'it''s /* fine */' FROM t"},
		{"comment after a literal is still stripped", "SELECT 'x' /* c */ FROM t", "SELECT 'x' FROM t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripInertSQLComments(tc.in); got != tc.want {
				t.Errorf("stripInertSQLComments(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}

	// A probe that is nothing but a comment keeps its raw text, so two different
	// probes stay distinguishable instead of both collapsing to "".
	if got := sqlStatementIdentity("/* ping */"); got != "/* ping */" {
		t.Errorf("sqlStatementIdentity(%q) = %q, want the raw text", "/* ping */", got)
	}
	if sqlStatementIdentity("/* ping */") == sqlStatementIdentity("/* health */") {
		t.Error("two different comment-only probes must not share an identity")
	}
}

// TestMatchQueryPacket_LiteralTypeIsPartOfTheStatement pins that a numeric
// literal and a quoted one are different statements.
//
// MySQL's "= 1" is a numeric comparison that also matches the strings '01',
// ' 1' and '1 '; "= '1'" is a string comparison that matches none of them. Any
// normalisation that parameterises literals away — comparing "same shape plus
// the same literal TEXT" — loses the distinction and would serve one query's
// rows for the other.
func TestMatchQueryPacket_LiteralTypeIsPartOfTheStatement(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	cases := []struct {
		recorded, live string
		// DML still reaches the pre-existing parse-tree tier, which is blind to
		// both identifiers and literal types and scores this pair weakly. That
		// tier is unchanged by this fix and never returns a definitive match, so
		// the contract asserted here is only that the pair is not EXACT.
		wantScore int
	}{
		// Every pair is deliberately EQUAL in byte length, so the payload-length
		// scoring this fix removed would have made them interchangeable.
		{recorded: "SELECT * FROM `invoices` WHERE `x` = 111", live: "SELECT * FROM `invoices` WHERE `x` = '1'"},
		{recorded: "SELECT * FROM `invoices` WHERE `x` = 1.500", live: "SELECT * FROM `invoices` WHERE `x` = '1.5'"},
		{
			recorded:  "INSERT INTO `invoices` (`a`) VALUES (000)",
			live:      "INSERT INTO `invoices` (`a`) VALUES ('0')",
			wantScore: scoreQueryStructure + 1, // structure tier plus its equal-length tie-break
		},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%.44s", tc.live), func(t *testing.T) {
			recorded := queryBundle(dbmWriter + tc.recorded)
			live := queryBundle(dbmWriter + tc.live)
			if recorded.Header.Header.PayloadLength != live.Header.Header.PayloadLength {
				t.Fatalf("test setup broken: the pair must be equal length, else the old scoring rejected it anyway")
			}
			ok, score := matchQueryPacket(ctx, logger, recorded, live)
			if ok {
				t.Errorf("a numeric literal and a quoted one are different statements; "+
					"matching them definitively serves %q's rows for %q", tc.recorded, tc.live)
			}
			if score != tc.wantScore {
				t.Errorf("score = %d, want %d", score, tc.wantScore)
			}
		})
	}
}

// TestMatchQueryPacket_InlineLiteralDriftIsServable is the case keploy's own
// go-memory-load-mysql suite exercises: a client that interpolates its values
// instead of binding them re-issues the same statement with a freshly generated
// id on every run.
//
//	recorded: SELECT ... FROM customers WHERE id = '<uuid A>'
//	replayed: SELECT ... FROM customers WHERE id = '<uuid B>'
//
// There is no exact match and a SELECT never reaches the DML parse-tree tier, so
// without a literal-drift tier the candidate scores 0, matchCommand finds no
// candidate at all, and the MySQL connection is torn down mid-suite.
//
// It must be servable — but only as a last resort, never as a definitive match,
// because the recorded response is a different row.
func TestMatchQueryPacket_InlineLiteralDriftIsServable(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	const shape = "SELECT `id`, `email`, `full_name` FROM `customers` WHERE `id` = '%s'"
	recorded := queryBundle(dbmWriter + fmt.Sprintf(shape, "1dc32a3c-a50c-5e92-9797-a6b6c4c7156e"))
	live := queryBundle(dbmReader + fmt.Sprintf(shape, "2a22d715-4799-5150-80d6-a0dd935fbda2"))

	ok, score := matchQueryPacket(ctx, logger, recorded, live)
	if ok {
		t.Error("a different id is a different row: this must not be a definitive match")
	}
	if score == 0 {
		t.Fatal("a re-issued statement whose inline literal drifted must stay servable as a last " +
			"resort; scoring 0 leaves matchCommand with no candidate and tears the connection down")
	}
	if score >= scoreQueryStructure {
		t.Errorf("score = %d, must rank below the DML parse-tree tier (%d)", score, scoreQueryStructure)
	}
}

// TestMaskSQLLiterals pins what the literal-drift tier will and will not equate.
func TestMaskSQLLiterals(t *testing.T) {
	sameShape := func(t *testing.T, a, b string) {
		t.Helper()
		if maskSQLLiterals(a) != maskSQLLiterals(b) {
			t.Errorf("must be the same shape:\n  %s -> %s\n  %s -> %s", a, maskSQLLiterals(a), b, maskSQLLiterals(b))
		}
	}
	differs := func(t *testing.T, a, b string) {
		t.Helper()
		if maskSQLLiterals(a) == maskSQLLiterals(b) {
			t.Errorf("must NOT be equated (both mask to %q):\n  %s\n  %s", maskSQLLiterals(a), a, b)
		}
	}

	t.Run("drifted values are the same shape", func(t *testing.T) {
		sameShape(t, "SELECT * FROM t WHERE id = 'aaa'", "SELECT * FROM t WHERE id = 'bbb'")
		sameShape(t, "SELECT * FROM t WHERE id = 5", "SELECT * FROM t WHERE id = 61234")
		sameShape(t, "SELECT * FROM t WHERE a = 1.5 AND b = 'x'", "SELECT * FROM t WHERE a = 9.25 AND b = 'y'")
		sameShape(t, "SELECT * FROM t WHERE h = 0x1F", "SELECT * FROM t WHERE h = 0xAB09")
	})

	t.Run("identifiers are never masked", func(t *testing.T) {
		differs(t, "SHOW FULL FIELDS FROM `invoices`", "SHOW FULL FIELDS FROM `couriers`")
		differs(t, "SELECT a FROM t1", "SELECT a FROM t2")
		differs(t, "SELECT `col1` FROM t", "SELECT `col2` FROM t")
		// Under sql_mode=ANSI_QUOTES a double-quoted token is an IDENTIFIER,
		// and nothing on the wire says which mode the session is in. Masking it
		// as a string made two DIFFERENT TABLES mask alike, so one table's rows
		// were served for another's — the very cross-serve this matcher exists
		// to prevent, reached through a different quoting style.
		differs(t, `SELECT id FROM "customers" WHERE id = 1`, `SELECT id FROM "orders" WHERE id = 1`)
		differs(t, `SELECT "col1" FROM t`, `SELECT "col2" FROM t`)
		// A digit inside an identifier is part of the name, not a literal.
		if got := maskSQLLiterals("SET NAMES utf8mb4"); got != "SET NAMES utf8mb4" {
			t.Errorf("identifier digits must survive, got %q", got)
		}
	})

	t.Run("literal type is preserved", func(t *testing.T) {
		differs(t, "SELECT * FROM t WHERE x = 1", "SELECT * FROM t WHERE x = '1'")
	})

	t.Run("quote-awareness", func(t *testing.T) {
		// A quote inside a literal must not end it early.
		sameShape(t, `SELECT * FROM t WHERE s = 'it\'s a'`, `SELECT * FROM t WHERE s = 'other'`)
		sameShape(t, "SELECT * FROM t WHERE s = 'it''s a'", "SELECT * FROM t WHERE s = 'other'")
		// Backquoted identifiers containing digits or quotes stay intact.
		differs(t, "SELECT `we'ird1` FROM t", "SELECT `we'ird2` FROM t")
	})

	t.Run("executable comments are copied through", func(t *testing.T) {
		differs(t, "/*!40101 SET NAMES utf8 */", "/*!80000 SET NAMES utf8 */")
	})
}

// TestIsSessionControlStatement pins the exclusion that keeps the literal-drift
// tier away from statements whose literal is their meaning.
func TestIsSessionControlStatement(t *testing.T) {
	for _, q := range []string{"SET NAMES utf8mb4", "set autocommit = 0", "SET\n@@sql_mode = 'X'", "SET\t@@x = 1", "SET"} {
		if !isSessionControlStatement(q) {
			t.Errorf("%q is a session-control statement", q)
		}
	}
	for _, q := range []string{"SELECT 1", "SETTINGS", "SETUP", "settle up"} {
		if isSessionControlStatement(q) {
			t.Errorf("%q is not a session-control statement", q)
		}
	}
}

// TestMatchCommand_AnsiQuotedIdentifierIsNeverCrossServed is the regression for
// the literal-drift tier's one blind spot.
//
// Under sql_mode=ANSI_QUOTES a double-quoted token is an IDENTIFIER, not a
// string, and the wire carries nothing that says which mode the session is in.
// While maskSQLLiterals masked `"..."` as a value, `SELECT id FROM "customers"`
// and `SELECT id FROM "orders"` both became `SELECT id FROM ?s ...`, so a query
// against one table was answered with the other table's recording — the same
// silent wrong-table serve that equal-byte-length scoring used to cause, which
// is exactly what this matcher exists to prevent.
//
// The backquoted form is asserted alongside it as the control: it was always
// treated as an identifier and must stay that way.
func TestMatchCommand_AnsiQuotedIdentifierIsNeverCrossServed(t *testing.T) {
	logger := zap.NewNop()
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct{ name, recorded, live string }{
		{
			name:     "ansi quoted identifiers",
			recorded: `SELECT id FROM "orders" WHERE id = 1`,
			live:     `SELECT id FROM "customers" WHERE id = 1`,
		},
		{
			name:     "backquoted identifiers",
			recorded: "SELECT id FROM `orders` WHERE id = 1",
			live:     "SELECT id FROM `customers` WHERE id = 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeMockDb{session: []*models.Mock{
				readbackMock("orders", tc.recorded, "ROWS=orders", base),
			}}

			resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(tc.live), db, newDecodeCtx(), nil, nil)
			if err != nil {
				t.Fatalf("matchCommand: %v", err)
			}
			if ok && resp != nil {
				t.Fatalf("cross-serve: a query against a different table was answered with %q; only the literal may drift, never the table", resp.Payload)
			}
		})
	}

	// The tier must still do its job: same table, drifted single-quoted literal.
	db := &fakeMockDb{session: []*models.Mock{
		readbackMock("orders", `SELECT id FROM "orders" WHERE ref = 'aaa'`, "ROWS=orders", base),
	}}
	resp, ok, _, err := matchCommand(context.Background(), logger,
		comQueryReq(`SELECT id FROM "orders" WHERE ref = 'bbb'`), db, newDecodeCtx(), nil, nil)
	if err != nil || !ok || resp == nil {
		t.Fatalf("a drifted single-quoted literal on the SAME table must still resolve, got ok=%v err=%v", ok, err)
	}
}
