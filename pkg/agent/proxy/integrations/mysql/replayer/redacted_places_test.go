package replayer

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The places of a recording the obfuscator redacted are read from its
// statement's parts and its bound values, by the mock's patterns.
func TestRedactedPlacesOfARecording(t *testing.T) {
	nc := util.NewNoiseChecker(redactedNoise)
	for _, tc := range []struct {
		name string
		sql  string
		nc   *util.NoiseChecker
		want redactedPlaces
	}{
		{"a literal holds the value", "SELECT name FROM t WHERE org = 'acme' AND k = '" + redactedPlaceholder + "'", nc,
			redactedPlaces{template: extractQueryShape("SELECT name FROM t WHERE org = 'a' AND k = 'b'").template, literals: map[int]bool{1: true}}},
		{"no part holds the value", "SELECT name FROM t WHERE org = 'acme'", nc, redactedPlaces{}},
		{"no statement, a pattern that matches an empty text", "", util.NewNoiseChecker([]string{`^.*$`}), redactedPlaces{}},
		{"a statement the parser cannot read", "SELEC name FRM t WHERE k = '" + redactedPlaceholder + "'", nc,
			redactedPlaces{statement: true, whole: true}},
		{"the template holds the value too", "SELECT name FROM t_" + redactedPlaceholder + " WHERE k = '" + redactedPlaceholder + "'", nc,
			redactedPlaces{template: extractQueryShape("SELECT name FROM t_" + redactedPlaceholder + " WHERE k = 'x'").template, literals: map[int]bool{0: true}, statement: true, whole: true}},
		{"the value spans a literal and the text after it", "SELECT x FROM t WHERE a = 1 AND b = 2", util.NewNoiseChecker([]string{`^1 AND$`}),
			redactedPlaces{template: extractQueryShape("SELECT x FROM t WHERE a = 1 AND b = 2").template, statement: true, whole: true}},
		{"a comment and a literal hold the value", "SELECT /* u=" + redactedPlaceholder + " */ name FROM t WHERE k = '" + redactedPlaceholder + "'", nc,
			redactedPlaces{template: extractQueryShape("SELECT name FROM t WHERE k = 'x'").template, literals: map[int]bool{0: true}, statement: true}},
		{"a comment alone holds the value", "SELECT /* u=" + redactedPlaceholder + " */ name FROM t WHERE k = 'acme'", nc,
			redactedPlaces{template: extractQueryShape("SELECT name FROM t WHERE k = 'x'").template, statement: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := queryPlaces(tc.sql, tc.nc); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("queryPlaces(%q) = %+v, want %+v", tc.sql, got, tc.want)
			}
		})
	}

	t.Run("a mock: its PREPARE's statement and its EXECUTE's bound values", func(t *testing.T) {
		const stored = "SELECT name FROM users WHERE org = 'acme' AND id = ? AND api_key = ?"
		if p := placesOf(nil); p.inStatement() || len(p.params) > 0 {
			t.Fatalf("placesOf(nil) = %+v, want none", p)
		}
		unredacted := queryRecorded("q", "SELECT 1 FROM t WHERE k = '"+redactedPlaceholder+"'", nil)
		if p := placesOf(unredacted); p.inStatement() || len(p.params) > 0 {
			t.Fatalf("a mock with no patterns has places %+v", p)
		}
		prep := preparedMock("p", "0", 1, "SELECT name FROM users WHERE api_key = '"+redactedPlaceholder+"' AND id = ?", stmtT0)
		prep.Noise = redactedNoise
		if p := placesOf(prep); !p.literals[0] || p.statement {
			t.Fatalf("a PREPARE's redacted literal: places %+v, want literal 0", p)
		}
		exec := executedMock("e", 1, stmtT0, "row", stored, "41", redactedPlaceholder)
		exec.Noise = redactedNoise
		exec.Spec.MySQLRequests[0].Message.(*mysql.StmtExecutePacket).Parameters[0].Value = []byte(redactedPlaceholder + "x")
		if p := placesOf(exec); p.inStatement() || !reflect.DeepEqual(p.params, map[int]bool{0: true, 1: true}) {
			t.Fatalf("an EXECUTE's redacted values (one of them bytes): places %+v, want params 0 and 1 alone", p)
		}
		chunk := sldMockOf("c", []byte(`{"token":"`+redactedPlaceholder+`"}`))
		chunk.Noise = redactedNoise
		if p := placesOf(chunk); !p.data || p.inStatement() || len(p.params) > 0 {
			t.Fatalf("a chunk's redacted value: places %+v, want the chunk alone", p)
		}
		plainChunk := sldMockOf("c", []byte(`{"token":"other"}`))
		plainChunk.Noise = redactedNoise
		if p := placesOf(plainChunk); p.redactsAny() {
			t.Fatalf("a chunk holding no redacted value has places %+v", p)
		}
	})
}

// The places of two recordings together.
func TestRedactedPlacesTogether(t *testing.T) {
	a := redactedPlaces{template: "select a from t where k = ?", literals: map[int]bool{0: true}, params: map[int]bool{2: true}}
	sameTemplate := redactedPlaces{template: a.template, literals: map[int]bool{1: true}}
	if got := a.or(sameTemplate); got.statement || !reflect.DeepEqual(got.literals, map[int]bool{0: true, 1: true}) || !got.params[2] {
		t.Fatalf("same template: %+v, want literals 0 and 1, param 2", got)
	}
	if got := (redactedPlaces{}).or(sameTemplate); got.template != a.template || !got.literals[1] {
		t.Fatalf("onto none: %+v, want the other's", got)
	}
	if got := a.or(redactedPlaces{template: "select b from u where k = ?", literals: map[int]bool{0: true}}); !got.statement || got.whole {
		t.Fatalf("two templates: %+v, want every literal (statement), not whole", got)
	}
	if got := a.or(redactedPlaces{statement: true}); !got.statement || got.whole {
		t.Fatalf("with a statement: %+v", got)
	}
	if got := a.or(redactedPlaces{statement: true, whole: true}); !got.whole {
		t.Fatalf("with a whole statement: %+v", got)
	}
	if got := a.or(redactedPlaces{params: map[int]bool{0: true}}); !reflect.DeepEqual(got.params, map[int]bool{0: true, 2: true}) || got.data {
		t.Fatalf("params: %+v", got)
	}
	if got := a.or(redactedPlaces{data: true}); !got.data || !got.params[2] {
		t.Fatalf("a chunk: %+v", got)
	}
	if got := (redactedPlaces{data: true}).or(a); !got.data || !got.params[2] {
		t.Fatalf("onto a chunk: %+v", got)
	}
	if got := a.or(redactedPlaces{everyParam: true}); !got.everyParam || !got.params[2] {
		t.Fatalf("every bound value: %+v", got)
	}
	if got := (redactedPlaces{everyParam: true}).or(a); !got.everyParam {
		t.Fatalf("onto every bound value: %+v", got)
	}
}

// everyPlace, the places of a pool that cannot be read, redacts the live
// command whole: its statement, every bound value and every diff of it.
func TestRedactedPlacesEveryPlace(t *testing.T) {
	if !everyPlace.redactsAny() || !(redactedPlaces{everyParam: true}).redactsAny() {
		t.Fatalf("everyPlace redacts nothing")
	}
	exec := &mysql.StmtExecutePacket{StatementID: 1, Parameters: []mysql.Parameter{
		{Type: 254, Value: liveSecret}, {Type: 3, Value: int32(7)}, {Type: 254, Value: nil}}}
	dctx := newDecodeCtx()
	dctx.StmtIDToQuery[1] = "SELECT name FROM users WHERE api_key = ? AND id = ? AND org = 'acme'"
	miss := &mockMiss{places: everyPlace}
	if got := miss.liveCommand(exec, dctx); got != valueRedacted+" "+formatExecParams([]mysql.Parameter{
		{Type: 254, Value: valueRedacted}, {Type: 3, Value: valueRedacted}, {Type: 254, Value: nil}}) {
		t.Errorf("an EXECUTE shows %q; want its statement and every bound value redacted", got)
	}
	if got := miss.liveCommand(&mysql.QueryPacket{Query: "SELECT 1 FROM t WHERE k = '" + liveSecret + "'"}, nil); got != valueRedacted {
		t.Errorf("a COM_QUERY shows %q; want it redacted whole", got)
	}
	diffs := everyPlace.fieldDiffs([]models.MockFieldDiff{
		{Path: "body.parameters.1.value", Kind: models.DiffKindValueChanged, Expected: "k1", Actual: liveSecret},
		{Path: "body.parameters.1.type", Kind: models.DiffKindValueChanged, Expected: "253", Actual: "254"},
		{Path: "body.query.literals.3", Kind: models.DiffKindValueChanged, Expected: "k1", Actual: liveSecret},
		{Path: "body.data.token", Kind: models.DiffKindValueChanged, Expected: "k1", Actual: liveSecret},
	}, "")
	for i, want := range []string{valueRedacted, "254", valueRedacted, valueRedacted} {
		if diffs[i].Actual != want {
			t.Errorf("the diff of %s shows %q; want %q", diffs[i].Path, diffs[i].Actual, want)
		}
	}
}

// The places of a pool are every place any recording of it redacted.
func TestRedactedPlacesOfAPool(t *testing.T) {
	byKey := queryRecorded("by-key", "SELECT name FROM users WHERE api_key = 'xq7Lm2Pw9Rt4Zk' AND org = 'acme'", redactedNoise)
	plain := queryRecorded("plain", "SELECT name FROM users WHERE api_key = 'k1' AND org = 'acme'", nil)
	live := "SELECT name FROM users WHERE api_key = 'live' AND org = 'globex'"
	want := "SELECT name FROM users WHERE api_key = '(redacted)' AND org = 'globex'"
	for _, pool := range [][]*models.Mock{{byKey, plain}, {plain, byKey}} {
		if got := poolPlaces(pool).liveStatement(live); got != want {
			t.Errorf("pool %s, %s: %q, want %q", pool[0].Name, pool[1].Name, got, want)
		}
	}
	if got := poolPlaces([]*models.Mock{plain}).liveStatement(live); got != live {
		t.Errorf("a pool that redacted nothing: %q, want the live statement", got)
	}
	if got := everyPlace.liveStatement(live); got != valueRedacted {
		t.Errorf("every place: %q, want the statement whole redacted", got)
	}
}

// The live statement and bound values as a report shows them.
func TestRedactedPlacesOfTheLiveCommand(t *testing.T) {
	const recorded = "SELECT /* app=web */ name FROM users WHERE org = 'acme' AND k = '" + redactedPlaceholder + "'"
	p := queryPlaces(recorded, util.NewNoiseChecker(redactedNoise))
	cast := queryPlaces("SELECT CAST(name AS CHAR(10)) FROM users WHERE org = 'acme' AND k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	lowerP := queryPlaces("SELECT /* app=web */ name FROM users WHERE org = ? AND k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	const liveNumber = "4111111111111111"
	signed := queryPlaces("SELECT name FROM users WHERE org = 'acme' AND n = -1234567890123456", util.NewNoiseChecker([]string{"^1234567890123456$"}))
	limit := queryPlaces("SELECT name FROM users LIMIT 10 OFFSET 7", util.NewNoiseChecker([]string{"^7$"}))
	special := queryPlaces("SELECT name FROM users WHERE org = 'acme' /*!50000 AND k = '"+redactedPlaceholder+"' */", util.NewNoiseChecker(redactedNoise))
	plainKey := queryPlaces("SELECT name FROM users WHERE org = 'acme' AND k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	setVars := queryPlaces("SET @y = 'k1', @x = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	midComment := queryPlaces("SELECT name /* k="+redactedPlaceholder+" */ FROM users WHERE k = '"+redactedPlaceholder+"'", util.NewNoiseChecker(redactedNoise))
	for _, tc := range []struct {
		name   string
		places redactedPlaces
		live   string
		want   string
	}{
		{"no place redacted", redactedPlaces{}, "SELECT name FROM users WHERE k = '" + liveSecret + "'", "SELECT name FROM users WHERE k = '" + liveSecret + "'"},
		{"no statement", p, "", ""},
		{"the recorded template: the redacted literal alone, comments kept", p,
			"SELECT /* app=api */ name FROM users WHERE org = 'globex' AND k = '" + liveSecret + "'",
			"SELECT /* app=api */ name FROM users WHERE org = 'globex' AND k = '(redacted)'"},
		{"another template: every literal, comments dropped", p,
			"SELECT /* app=api */ name FROM users WHERE k = '" + liveSecret + "' AND org = 'globex'",
			"SELECT   name FROM users WHERE k = '(redacted)' AND org = '(redacted)'"},
		{"a live statement the parser cannot read", p, "SELEC name FRM users WHERE k = '" + liveSecret + "'", valueRedacted},
		{"a live statement the parser cannot read, with no literal", p, "SELEC name FRM users_" + liveSecret, valueRedacted},
		{"the recording's comment redacted", queryPlaces("SELECT /* u="+redactedPlaceholder+" */ name FROM users WHERE k = 'acme'", util.NewNoiseChecker(redactedNoise)),
			"SELECT /* u=" + liveSecret + " */ name FROM users WHERE k = 'acme'", "SELECT   name FROM users WHERE k = '(redacted)'"},
		{"whole", redactedPlaces{statement: true, whole: true}, "SELECT name FROM t_" + liveSecret, valueRedacted},
		// A literal the parser's walk does not reach has no parsed
		// literal of its own: the statement is redacted whole.
		{"another template: a string DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k TEXT DEFAULT '" + liveSecret + "')", valueRedacted},
		{"another template: a hex DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k TEXT DEFAULT x'736b394c495645')", valueRedacted},
		{"another template: a hex number DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k INT DEFAULT 0x736b394c)", valueRedacted},
		{"another template: a numeric DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k BIGINT DEFAULT 4111111111111111)", valueRedacted},
		{"another template: a decimal DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k DOUBLE DEFAULT 4111.1111)", valueRedacted},
		{"the recorded template with a number in no literal: whole", cast,
			"SELECT CAST(name AS CHAR(10)) FROM users WHERE org = 'globex' AND k = '" + liveSecret + "'", valueRedacted},
		// The statement keeps the client's text: its keywords' case, its
		// spacing, its ? markers and its comments, only the literal at the
		// redacted place replaced where it is written.
		{"the recorded template, written in another case", lowerP,
			"select /* app=api */ name\n  from users where org = ? AND k='" + liveSecret + "'",
			"select /* app=api */ name\n  from users where org = ? AND k='(redacted)'"},
		{"another template: every literal where written, ? kept", p,
			"Select name From users Where k = ? And org = \"" + liveSecret + "\" And n = -42 Limit 5",
			"Select name From users Where k = ? And org = '(redacted)' And n = -'(redacted)' Limit '(redacted)'"},
		{"another template: a comment between tokens dropped, the tokens kept apart", p,
			"SELECT name FROM users WHERE k='" + liveSecret + "'/* u=" + liveSecret + " */AND org = 'acme'",
			"SELECT name FROM users WHERE k='(redacted)' AND org = '(redacted)'"},
		{"another template: a comment to the end of its line dropped", p,
			"SELECT name FROM users -- u=" + liveSecret + "\nWHERE k = 'acme'",
			"SELECT name FROM users  WHERE k = '(redacted)'"},
		{"a literal written with a sign: redacted where written, the sign kept", signed,
			"SELECT name FROM users WHERE org = 'globex' AND n = -" + liveNumber,
			"SELECT name FROM users WHERE org = 'globex' AND n = -'(redacted)'"},
		{"a literal the parser reads before one written ahead of it: redacted wherever its value is written", limit,
			"SELECT name FROM users LIMIT 3 OFFSET 9",
			"SELECT name FROM users LIMIT 3 OFFSET '(redacted)'"},
		// A literal in a /*! */ comment, which the parser reads, and a
		// literal of no token of its own, have no span: whole.
		{"the recording's /*! */ comment redacted: whole", special,
			"SELECT name FROM users WHERE org = 'globex' /*!50000 AND k = '" + liveSecret + "' */", valueRedacted},
		{"the recorded template, the redacted literal in a /*! */ comment the parser reads: whole", plainKey,
			"SELECT name FROM users WHERE org = 'globex' /*!50000 AND k = '" + liveSecret + "' */", valueRedacted},
		{"the recorded template, the redacted literal in a /*! */ comment, its value written elsewhere: whole", cast,
			"SELECT CAST(name AS CHAR(10)) FROM users WHERE org = 'globex' /*!50000 AND k = 10 */", valueRedacted},
		{"a /*! */ comment that holds no literal: whole", plainKey,
			"SELECT name FROM users WHERE org = 'globex' /*!50000 AND k IS NOT NULL */", valueRedacted},
		{"the recorded template, the redacted literal written as two strings, its value written elsewhere: whole", setVars,
			"SET @y = '" + liveSecret + "', @x = 'sk9LIVE' 'secret77'", valueRedacted},
		{"another template: a national string DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k TEXT DEFAULT N'" + liveSecret + "')", valueRedacted},
		{"another template: a float DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k DOUBLE DEFAULT 4.1e15)", valueRedacted},
		{"another template: a bit DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k BIT DEFAULT b'10110111')", valueRedacted},
		{"another template: a bit number DEFAULT the rewriter does not reach: whole", p,
			"CREATE TABLE tokens (k BIT DEFAULT 0b10110111)", valueRedacted},
		// A literal of each kind is redacted where it is written, and text
		// of each kind that holds none is kept.
		{"another template: a literal of each kind, where written", plainKey,
			"SELECT name FROM users WHERE k = N'nn' AND a = X'4142' AND b = 0x41 AND c = b'0101' AND d = 0b0101 AND e = 1.5 AND f = 4.1e15 AND g = 7 AND h = 'x'",
			"SELECT name FROM users WHERE k = '(redacted)' AND a = '(redacted)' AND b = '(redacted)' AND c = '(redacted)' AND d = '(redacted)' AND e = '(redacted)' AND f = '(redacted)' AND g = '(redacted)' AND h = '(redacted)'"},
		{"another template: text of each kind kept", plainKey,
			"SELECT @a := 1, @@version, a != 2, b <> 3, c <= 4, d >= 5, e <=> 6, f << 7, g >> 8, doc->'$.a', doc->>'$.b', h && i, j || k FROM t WHERE k = ? AND id IN ::ids AND n = :1",
			"SELECT @a := '(redacted)', @@version, a != '(redacted)', b <> '(redacted)', c <= '(redacted)', d >= '(redacted)', e <=> '(redacted)', f << '(redacted)', g >> '(redacted)', doc->'(redacted)', doc->>'(redacted)', h && i, j || k FROM t WHERE k = ? AND id IN ::ids AND n = :1"},
		{"the recorded template, the redacted literal written as b''", plainKey,
			"SELECT name FROM users WHERE org = 'globex' AND k = b'0111001101101011'",
			"SELECT name FROM users WHERE org = 'globex' AND k = '(redacted)'"},
		// A literal written as a bit number is a literal as any other.
		{"the recorded template, the redacted literal written as a bit number", plainKey,
			"SELECT name FROM users WHERE org = 'globex' AND k = 0b0111001101101011",
			"SELECT name FROM users WHERE org = 'globex' AND k = '(redacted)'"},
		{"another template: every literal, one written as a bit number", plainKey,
			"SELECT name FROM users WHERE k = 0b0111001101101011 AND n = 1",
			"SELECT name FROM users WHERE k = '(redacted)' AND n = '(redacted)'"},
		{"a recorded comment after the first token redacted: every literal, comments dropped", midComment,
			"SELECT name /* k=" + liveSecret + " */ FROM users WHERE k = '" + liveSecret + "'",
			"SELECT name   FROM users WHERE k = '(redacted)'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.places.liveStatement(tc.live); got != tc.want {
				t.Fatalf("liveStatement(%q) = %q, want %q", tc.live, got, tc.want)
			}
		})
	}

	t.Run("bound values", func(t *testing.T) {
		params := []mysql.Parameter{{Value: "42"}, {Value: liveSecret}, {Value: nil}}
		if got := (redactedPlaces{}).liveParams(params); !reflect.DeepEqual(got, params) {
			t.Fatalf("no place: %+v", got)
		}
		got := redactedPlaces{params: map[int]bool{1: true, 2: true}}.liveParams(params)
		want := []mysql.Parameter{{Value: "42"}, {Value: valueRedacted}, {Value: nil}}
		if !reflect.DeepEqual(got, want) || params[1].Value != liveSecret {
			t.Fatalf("liveParams = %+v, want %+v, its input unchanged", got, want)
		}
	})

	t.Run("the live command's statement", func(t *testing.T) {
		q := &mysql.PacketBundle{Message: &mysql.QueryPacket{Query: "q"}}
		prep := &mysql.PacketBundle{Message: &mysql.StmtPreparePacket{Query: "p"}}
		exec := &mysql.PacketBundle{Message: &mysql.StmtExecutePacket{}}
		for b, want := range map[*mysql.PacketBundle]string{q: "q", prep: "p", exec: "e", nil: "e"} {
			if got := liveStatementOf(b, "e"); got != want {
				t.Fatalf("liveStatementOf(%+v) = %q, want %q", b, got, want)
			}
		}
	})
}

// A strict miss's field diffs are redacted on both sides at a redacted
// place, the side a field is absent from left empty, and kept elsewhere.
func TestRedactedPlacesOfFieldDiffs(t *testing.T) {
	const liveSame = "SELECT name FROM users WHERE org = 'globex' AND k = 'x'"
	literalAt1 := redactedPlaces{template: extractQueryShape(liveSame).template, literals: map[int]bool{1: true}}
	diff := func(path string, kind models.MockFieldDiffKind) models.MockFieldDiff {
		return models.MockFieldDiff{Path: path, Expected: "rec", Actual: "live", Kind: kind}
	}
	const changed = models.DiffKindValueChanged
	for _, tc := range []struct {
		name     string
		places   redactedPlaces
		live     string
		redacted []string
		kept     []string
	}{
		{"bound values alone", redactedPlaces{params: map[int]bool{1: true}}, liveSame,
			[]string{"body.parameters.1.value", "body.parameters.1.value.$bin", "body.data"},
			[]string{"body.query", "body.query.literals", "body.query.literals.0", "body.query.template", "body.parameters.0.value",
				"body.parameters.1.name", "body.parameters.1.type", "body.parameters.1.unsigned", "body.parameters.x.value"}},
		{"a literal, the recorded template", literalAt1, liveSame,
			[]string{"body.query", "body.query.literals", "body.query.literals.1"},
			[]string{"body.query.literals.0", "body.query.template", "body.query.comments", "body.parameters.1.value"}},
		{"a literal, another template", literalAt1, "SELECT name FROM users WHERE k = 'x'",
			[]string{"body.query.literals.0", "body.query.literals.1"},
			[]string{"body.query.template"}},
		{"a literal, a live statement the parser cannot read", literalAt1, "SELEC name FRM users WHERE k = 'x'",
			[]string{"body.query.template", "body.query.literals.0"},
			nil},
		{"a literal, another template keeping a literal in it", literalAt1, "SELECT name FROM users WHERE k = 'x' INTO OUTFILE '/tmp/x'",
			[]string{"body.query.template", "body.query.literals.0"},
			nil},
		{"a chunk", redactedPlaces{data: true}, liveSame,
			[]string{"body.data", "body.data.token", "body.data.n.0"},
			[]string{"body.query", "body.query.literals.0", "body.query.template", "body.parameters.0.value", "body.database"}},
		{"none", redactedPlaces{}, liveSame,
			nil,
			[]string{"body.data", "body.query", "body.query.literals.0", "body.parameters.0.value"}},
		{"the statement", redactedPlaces{statement: true}, liveSame,
			[]string{"body.query", "body.query.template", "body.query.comments", "body.query.literals.0"},
			[]string{"body.parameters.0.value"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in []models.MockFieldDiff
			for _, p := range append(append([]string{}, tc.redacted...), tc.kept...) {
				in = append(in, diff(p, changed))
			}
			out := tc.places.fieldDiffs(in, tc.live)
			for i, d := range out {
				want := "live"
				if i < len(tc.redacted) {
					want = valueRedacted
				}
				if d.Actual != want || (d.Expected == valueRedacted) != (want == valueRedacted) {
					t.Errorf("%s: %q / %q, want %s", d.Path, d.Expected, d.Actual, want)
				}
			}
		})
	}
	t.Run("a field absent from one side", func(t *testing.T) {
		p := redactedPlaces{params: map[int]bool{0: true}}
		out := p.fieldDiffs([]models.MockFieldDiff{
			{Path: "body.parameters.0.value", Actual: "live", Kind: models.DiffKindMissingInMock},
			{Path: "body.parameters.0.value", Expected: "rec", Kind: models.DiffKindMissingInLive},
			{Path: "body.parameters.x.value", Expected: "rec", Actual: "live", Kind: models.DiffKindValueChanged},
		}, "")
		if out[0].Expected != "" || out[0].Actual != valueRedacted || out[1].Expected != valueRedacted || out[1].Actual != "" || out[2].Actual != "live" {
			t.Fatalf("diffs %+v: want the absent side empty, the other redacted, a path of no index kept", out)
		}
	})
}
