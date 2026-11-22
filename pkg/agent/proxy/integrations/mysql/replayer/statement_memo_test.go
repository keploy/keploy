package replayer

import (
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
)

// The statements the memo rows read, by what their read holds.
var (
	// memoShort reads into a few hundred bytes.
	memoShort = "SELECT name FROM users WHERE org = 'acme' AND id = 7"
	// memoPadded is a short statement padded past memoMaxBytes with
	// blanks: its read is small, its text is not.
	memoPadded = "SELECT name FROM users WHERE id = 7" + strings.Repeat(" ", memoMaxBytes)
	// memoNames is 40 KB of names and one literal: its shape's template is
	// as long as its text, its literals few.
	memoNames = "SELECT " + strings.Repeat("c, ", 40<<10/3) + "1 FROM t"
	// memoDigits is 4 KB of one-digit literals: two bytes of text a literal.
	memoDigits = "SELECT 1" + strings.Repeat(",1", 2<<10)
	// memoString25 and memoString40 are one string literal of 25 and 40 KB.
	memoString25 = "SELECT '" + strings.Repeat("a", 25<<10) + "'"
	memoString40 = "SELECT '" + strings.Repeat("a", 40<<10) + "'"
	// memoComment is a 40 KB comment and one literal: its shape keeps the
	// comment beside the template.
	memoComment = "SELECT /* " + strings.Repeat("a", 40<<10) + " */ 1"
)

// A statement is kept while it and what it is read into take at most
// memoMaxBytes, and not a byte more.
func TestMemoKeepsAStatementUpToTheBound(t *testing.T) {
	sql := strings.Repeat("a", 100)
	if !memoKeeps(sql, memoMaxBytes-100) {
		t.Error("a statement read into exactly the bound is not kept")
	}
	if memoKeeps(sql, memoMaxBytes-99) {
		t.Error("a statement read into a byte past the bound is kept")
	}
}

// queryShapeCache keeps a statement only while its text and its shape take
// at most memoMaxBytes; one larger is read again each time, the same.
func TestQueryShapeCacheKeepsAStatementByItsSize(t *testing.T) {
	cases := []struct {
		name, sql string
		kept      bool
	}{
		{"a short statement", memoShort, true},
		{"a short statement padded past the bound with blanks", memoPadded, false},
		{"40 KB of names: a template as long as the text", memoNames, false},
		{"4 KB of one-digit literals", memoDigits, false},
		{"a 25 KB string literal", memoString25, true},
		{"a 40 KB string literal", memoString40, false},
		{"a 40 KB comment", memoComment, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queryShapeCache.Purge()
			t.Cleanup(queryShapeCache.Purge)
			got := extractQueryShape(tc.sql)
			if !got.ok {
				t.Fatalf("the statement does not parse")
			}
			if kept := queryShapeCache.Contains(tc.sql); kept != tc.kept {
				t.Errorf("kept %v, want %v: %d bytes of text read into about %d", kept, tc.kept, len(tc.sql), got.bytes())
			}
			if want := computeQueryShape(tc.sql); !reflect.DeepEqual(got, want) {
				t.Errorf("read as %+v, want %+v", got, want)
			}
			if again := extractQueryShape(tc.sql); !reflect.DeepEqual(again, got) {
				t.Errorf("read again as %+v, want %+v", again, got)
			}
		})
	}
}

// sqlLiteralsCache keeps a statement only while its text and its literals
// take at most memoMaxBytes; one larger is read again each time, the same.
func TestSQLLiteralsCacheKeepsAStatementByItsSize(t *testing.T) {
	cases := []struct {
		name, sql string
		kept      bool
	}{
		{"a short statement", memoShort, true},
		{"a short statement padded past the bound with blanks", memoPadded, false},
		{"40 KB of names: few literals", memoNames, true},
		{"4 KB of one-digit literals", memoDigits, false},
		{"a 25 KB string literal, read and written", memoString25, false},
		{"a 40 KB comment: no literal of it", memoComment, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlLiteralsCache.Purge()
			t.Cleanup(sqlLiteralsCache.Purge)
			got, ok := readSQLLiterals(tc.sql)
			if !ok || !got.matched {
				t.Fatalf("the statement is not read: %v, matched %v", ok, got.matched)
			}
			if kept := sqlLiteralsCache.Contains(tc.sql); kept != tc.kept {
				t.Errorf("kept %v, want %v: %d bytes of text read into about %d", kept, tc.kept, len(tc.sql), got.bytes())
			}
			if want, wantOK := scanSQLLiterals(tc.sql, sqlKinds); !reflect.DeepEqual(got, want) || wantOK != ok {
				t.Errorf("read as %+v, want %+v", got, want)
			}
			if again, againOK := readSQLLiterals(tc.sql); !reflect.DeepEqual(again, got) || againOK != ok {
				t.Errorf("read again as %+v, want %+v", again, got)
			}
		})
	}
}

// A read the tokenizer stops at its first literal (a kind it does not
// know: scanSQLLiterals' seam) holds the literals the parser read, and is
// sized as any other: their headers count, so it is not kept.
func TestSQLLiteralsCountTheLiteralsTheParserRead(t *testing.T) {
	sql := "SELECT 1" + strings.Repeat(",1", 4<<10)
	got, ok := scanSQLLiterals(sql, sqlTokenKinds{text: sqlKinds.text})
	if ok || len(got.tokens) != 0 || len(got.parsed) <= 4<<10 {
		t.Fatalf("read as %v with %d tokens and %d literals, want no token read and every literal parsed", ok, len(got.tokens), len(got.parsed))
	}
	if memoKeeps(sql, got.bytes()) {
		t.Errorf("%d literals parsed, %d bytes of text, read into about %d: kept, want not", len(got.parsed), len(sql), got.bytes())
	}
}

// querySigCache keeps every statement, each entry within memoMaxBytes: a
// long text by its digest, a long signature as its digest. A signature is
// only compared with another, and two compare equal exactly when the
// statements' trees do, whatever their sizes.
func TestQuerySigCacheKeepsAStatementByItsSize(t *testing.T) {
	padded := func(sql string) string { return sql + strings.Repeat(" ", memoMaxBytes) }
	digits := func(n int, d string) string { return "SELECT " + d + strings.Repeat(","+d, n) }
	cases := []struct {
		name, sql            string
		keyByText, sigAsText bool
	}{
		{"a short statement", memoShort, true, true},
		{"4 KB of one-digit literals: a long signature", memoDigits, true, false},
		{"a short statement padded past the bound: a long text", memoPadded, false, true},
		{"64 KB of one-digit literals: both long", digits(32<<10, "1"), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			querySigCache.Purge()
			t.Cleanup(querySigCache.Purge)
			got, err := getQueryStructureCached(tc.sql)
			if err != nil {
				t.Fatalf("not read: %v", err)
			}
			if keys := querySigCache.Keys(); len(keys) != 1 || (keys[0] == tc.sql) != tc.keyByText || len(keys[0]) > memoMaxBytes/2 {
				t.Errorf("kept by a key of %d bytes (by the text: %v), want by the text: %v, within %d", len(keys[0]), keys[0] == tc.sql, tc.keyByText, memoMaxBytes/2)
			}
			sig, _ := getQueryStructure(tc.sql)
			if (got == sig) != tc.sigAsText || len(got) > memoMaxBytes/2 {
				t.Errorf("signature of %d bytes kept as %d bytes (as itself: %v), want as itself: %v, within %d", len(sig), len(got), got == sig, tc.sigAsText, memoMaxBytes/2)
			}
			if again, _ := getQueryStructureCached(tc.sql); again != got || querySigCache.Len() != 1 {
				t.Errorf("read again as %.80q (%d entries), want %.80q from the one entry", again, querySigCache.Len(), got)
			}
		})
	}
	querySigCache.Purge()
	t.Cleanup(querySigCache.Purge)
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"long signatures, other literals", digits(2<<10, "1"), digits(2<<10, "2"), true},
		{"long signatures, one more literal", digits(2<<10, "1"), digits(2<<10+1, "1"), false},
		{"a long text and a short one", padded("SELECT name FROM users WHERE id = 7"), "SELECT name FROM users WHERE id = 8", true},
		{"long texts of other trees", padded("SELECT name FROM users WHERE id = 7"), padded("SELECT name FROM users WHERE id = 7 AND k = 1"), false},
	} {
		if got := sameQueryStructure(tc.a, tc.b); got != tc.same {
			t.Errorf("%s: the same structure %v, want %v", tc.name, got, tc.same)
		}
	}
}

// stmtIdentityCache keeps a statement only while its text and its identity
// take at most memoMaxBytes; one larger is read again each time, the same.
// An identity shorter than its text holds none of the text: kept, and as a
// key of the other caches, it holds its own bytes alone.
func TestStmtIdentityCacheKeepsAStatementByItsSize(t *testing.T) {
	for _, tc := range []struct {
		name, sql, want string
		kept            bool
	}{
		{"a short statement after a comment", "/* traceparent=00-ab */ " + memoShort, memoShort, true},
		{"a short statement padded with 40 KB of blanks", memoShort + strings.Repeat(" ", 40<<10), memoShort, true},
		{"40 KB of names after a comment: an identity as long as the text", "/* c */ " + memoNames, memoNames, false},
		{"a statement padded past the bound", "/* traceparent=00-ab */ " + memoPadded, strings.TrimSpace(memoPadded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stmtIdentityCache.Purge()
			t.Cleanup(stmtIdentityCache.Purge)
			got := sqlStatementIdentity(tc.sql)
			if got != tc.want {
				t.Errorf("identity %.80q, want %.80q", got, tc.want)
			}
			if at, from := uintptr(unsafe.Pointer(unsafe.StringData(got))), uintptr(unsafe.Pointer(unsafe.StringData(tc.sql))); at >= from && at < from+uintptr(len(tc.sql)) {
				t.Errorf("the identity of %d bytes holds the text of %d", len(got), len(tc.sql))
			}
			if kept := stmtIdentityCache.Contains(tc.sql); kept != tc.kept {
				t.Errorf("kept %v, want %v", kept, tc.kept)
			}
			if again := sqlStatementIdentity(tc.sql); again != got {
				t.Errorf("read again as %.80q, want %.80q", again, got)
			}
		})
	}
}

// The places a miss's report shows its command by are read once, however
// often they are asked for (logPlaces). No logPlaces reads every place.
func TestLogPlacesReadsThePlacesOnce(t *testing.T) {
	byKey := queryRecorded("by-key", "SELECT name FROM users WHERE api_key = 'xq7Lm2Pw9Rt4Zk' AND org = 'acme'", redactedNoise)
	want := poolPlaces([]*models.Mock{byKey})
	read := 0
	r := &logPlaces{read: func() redactedPlaces { read++; return poolPlaces([]*models.Mock{byKey}) }}
	for i := 0; i < 3; i++ {
		if got := r.places(); !reflect.DeepEqual(got, want) {
			t.Fatalf("read %d: %+v, want %+v", i, got, want)
		}
	}
	if read != 1 {
		t.Errorf("the places were read %d times, want once", read)
	}
	if got := (*logPlaces)(nil).places(); !reflect.DeepEqual(got, everyPlace) {
		t.Errorf("no logPlaces reads %+v, want every place", got)
	}
}
