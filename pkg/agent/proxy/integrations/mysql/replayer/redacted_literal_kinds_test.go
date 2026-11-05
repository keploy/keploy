package replayer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"vitess.io/vitess/go/vt/sqlparser"
)

// A literal the client wrote as a bit number (0b...) at a place the
// recording redacted is redacted, as every other literal kind is.
func TestRedactedPlacesOfABitNumberAtARedactedPlace(t *testing.T) {
	const liveBits = "0b0111001101101011"
	p := queryPlaces("SELECT name FROM users WHERE k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	q := queryPlaces("SELECT name FROM users WHERE org = 'acme' AND k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	for _, tc := range []struct {
		name   string
		places redactedPlaces
		live   string
	}{
		{"the recorded template, the redacted literal written as 0b", p, "SELECT name FROM users WHERE k = " + liveBits},
		{"another template, every literal redacted, one written as 0b", q, "SELECT name FROM users WHERE k = " + liveBits + " AND n = 1"},
		{"the recorded template, a hex number (control)", p, "SELECT name FROM users WHERE k = 0x736b394c495645"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.places.liveStatement(tc.live)
			t.Logf("live %q -> %q", tc.live, got)
			if strings.Contains(got, liveBits) || strings.Contains(got, "0x736b394c495645") {
				t.Errorf("LEAK: %q shows the live literal at the redacted place", got)
			}
		})
	}
}

// A token of a kind readSQLLiterals does not know, as a literal kind it
// were not told of would be, redacts the statement whole, wherever it
// stands: at a redacted place, at a place no recording redacted, and where
// the parser's walk does not reach. A literal kind it took for text leaves
// a literal the parser reads with no token of its own, and the statement
// is redacted whole too: no literal can show for a kind not listed.
func TestRedactedPlacesOfALiteralOfAKindNotKnown(t *testing.T) {
	const bits = "0b0111001101101011"
	without := sqlTokenKinds{literal: map[int]bool{}, text: sqlKinds.text}
	for k := range sqlKinds.literal {
		if k != sqlparser.BITNUM {
			without.literal[k] = true
		}
	}
	asText := sqlTokenKinds{literal: without.literal, text: map[int]bool{sqlparser.BITNUM: true}}
	for k := range sqlKinds.text {
		asText.text[k] = true
	}
	plainKey := queryPlaces("SELECT name FROM users WHERE org = 'acme' AND k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	for _, tc := range []struct {
		name  string
		kinds sqlTokenKinds
		live  string
		want  string
	}{
		{"every kind known: the redacted literal alone", sqlKinds,
			"SELECT name FROM users WHERE org = 'globex' AND k = " + bits,
			"SELECT name FROM users WHERE org = 'globex' AND k = '(redacted)'"},
		{"a kind not known, at the redacted place", without,
			"SELECT name FROM users WHERE org = 'globex' AND k = " + bits, valueRedacted},
		{"a kind not known, at a place no recording redacted", without,
			"SELECT name FROM users WHERE org = " + bits + " AND k = 'x'", valueRedacted},
		{"a kind not known, where the walk does not reach", without,
			"CREATE TABLE tokens (k BIT DEFAULT " + bits + ")", valueRedacted},
		{"a literal kind taken for text, as many literals written as read", asText,
			"SELECT CAST(name AS CHAR(10)) FROM users WHERE org = 'globex' AND k = " + bits, valueRedacted},
		{"a literal kind taken for text", asText,
			"SELECT name FROM users WHERE org = 'globex' AND k = " + bits, valueRedacted},
		{"a literal kind taken for text, its value written as another literal", asText,
			"SELECT CAST(name AS CHAR(10)) FROM users WHERE org = '" + bits + "' AND k = " + bits, valueRedacted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lits, ok := scanSQLLiterals(tc.live, tc.kinds)
			if got := plainKey.shownStatement(tc.live, lits, ok); got != tc.want {
				t.Fatalf("shownStatement(%q) = %q, want %q (ok=%v matched=%v)", tc.live, got, tc.want, ok, lits.matched)
			}
		})
	}
	t.Run("the tokenizer's kinds", func(t *testing.T) {
		if _, ok := scanSQLLiterals("SELECT name FROM users WHERE k = "+bits, without); ok {
			t.Fatalf("a token of a kind not known read")
		}
	})
}

// A statement is read once (readSQLLiterals): a pool's places, read for
// each command a debug line or a miss shows, cost no new read of a
// recording read before.
func TestReadSQLLiteralsReadsAStatementOnce(t *testing.T) {
	const sql = "SELECT /* once */ name FROM users WHERE k = 'read-once-7Qz'"
	a, okA := readSQLLiterals(sql)
	b, okB := readSQLLiterals(sql)
	if !okA || !okB || len(a.tokens) == 0 || len(b.tokens) != len(a.tokens) || &a.tokens[0] != &b.tokens[0] {
		t.Fatalf("read twice: %+v %v, then %+v %v", a, okA, b, okB)
	}
}

// The parser's walk (readSQLLiterals) and the rewriter (extractQueryShape,
// whose positions the places are) read a statement's literals in the same
// order, over every SQL text the integrations' sources hold.
func TestReadSQLLiteralsWalksInTheShapesOrder(t *testing.T) {
	var corpus []string
	root := "../.."
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil {
					u := strings.ToUpper(strings.TrimSpace(s))
					for _, kw := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "WITH", "SET", "CREATE", "ALTER", "CALL", "SHOW"} {
						if strings.HasPrefix(u, kw+" ") {
							corpus = append(corpus, s)
							break
						}
					}
				}
			}
			return true
		})
		return nil
	})
	corpus = append(corpus,
		"SELECT a FROM t WHERE x IN (1, 'b', 0x41) AND y BETWEEN 2 AND 3 ORDER BY 4 LIMIT 5 OFFSET 6",
		"SELECT CASE WHEN a = 1 THEN 'x' ELSE 'y' END, (SELECT 2 FROM u WHERE u.z = 'q' LIMIT 1) FROM t WHERE b LIKE 'p%' ESCAPE '!' GROUP BY 1 HAVING COUNT(*) > 7",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y') ON DUPLICATE KEY UPDATE b = 'z', c = c + 3",
		"INSERT INTO t SELECT 1, 'a' FROM u WHERE k = 'v' UNION ALL SELECT 2, 'b' FROM w WHERE k = 'r' LIMIT 9",
		"UPDATE t SET a = 'x', b = b + 1 WHERE c = 'y' ORDER BY d DESC LIMIT 10",
		"DELETE FROM t WHERE a = 'x' AND created < NOW() - INTERVAL 5 DAY LIMIT 100",
		"WITH c AS (SELECT 1 AS n, 'a' AS s) SELECT n, s, 'k' FROM c WHERE n > 0 LIMIT 2, 3",
		"SELECT JSON_EXTRACT(doc, '$.a.b'), CAST('12' AS SIGNED), CONVERT('x' USING utf8mb4), DATE '2020-01-02', _utf8mb4'z', N'nn', X'4142', b'0101', 0b0101 FROM t",
		"SELECT ROW_NUMBER() OVER (PARTITION BY a ORDER BY b ROWS BETWEEN 1 PRECEDING AND 2 FOLLOWING) FROM t WHERE c = 'w'",
		"SELECT a FROM t1 JOIN t2 ON t1.id = t2.id AND t2.k = 'j' LEFT JOIN t3 USING (id) WHERE t1.v = 8 FOR UPDATE",
		"SET @a = 'x', @b = 2, SESSION sql_mode = 'TRADITIONAL'",
		"SELECT IF(a > 1, 'p', 'q'), COALESCE(b, 'c'), SUBSTRING('abcdef', 2, 3), LPAD('x', 5, '0') FROM t WHERE d IN (SELECT e FROM f WHERE g = 'h')",
	)
	compared := 0
	for _, sql := range corpus {
		shape := extractQueryShape(sql)
		lits, ok := readSQLLiterals(sql)
		if !shape.ok || !ok {
			continue
		}
		if len(shape.literals) == 0 && len(lits.parsed) == 0 {
			continue
		}
		compared++
		if !reflect.DeepEqual(shape.literals, lits.parsed) {
			t.Errorf("ORDER: %q\n shape %q\n walk  %q", sql, shape.literals, lits.parsed)
		}
	}
	t.Logf("%d statements read, %d with literals compared", len(corpus), compared)
	// The sources hold hundreds: a walk that found few read the wrong tree.
	if compared < 100 {
		t.Fatalf("only %d statements with literals compared", compared)
	}
}

// A strict field diff of the template of a live statement whose template
// keeps a literal the parser's walk does not reach, written as a bit
// number (a column's DEFAULT), is redacted, as one written as a string or
// a hex number is.
func TestRedactedPlacesOfATemplateKeepingABitNumber(t *testing.T) {
	const liveSame = "SELECT name FROM users WHERE org = 'globex' AND k = 'x'"
	literalAt1 := redactedPlaces{template: extractQueryShape(liveSame).template, literals: map[int]bool{1: true}}
	for _, live := range []string{
		"CREATE TABLE tokens (k BIT(64) DEFAULT 0b0111001101101011)",
		"CREATE TABLE tokens (k BIT DEFAULT 0b0111001101101011)",
		"CREATE TABLE tokens (k BIT(64) DEFAULT 0x736b394c)",
	} {
		t.Run(live[len(live)-12:], func(t *testing.T) {
			d := literalAt1.fieldDiffs([]models.MockFieldDiff{{Path: "body.query.template", Expected: "rec", Actual: extractQueryShape(live).template, Kind: models.DiffKindValueChanged}}, live)
			if d[0].Actual != valueRedacted {
				t.Errorf("LEAK: the template diff shows %q", d[0].Actual)
			}
		})
	}
}
