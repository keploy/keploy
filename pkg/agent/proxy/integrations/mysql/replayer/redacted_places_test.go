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
	if got := a.or(redactedPlaces{params: map[int]bool{0: true}}); !reflect.DeepEqual(got.params, map[int]bool{0: true, 2: true}) {
		t.Fatalf("params: %+v", got)
	}
}

// The places of a pool are every place any recording of it redacted; a
// log line with no places to read redacts the live statement whole.
func TestRedactedPlacesOfAPool(t *testing.T) {
	byKey := queryRecorded("by-key", "SELECT name FROM users WHERE api_key = 'xq7Lm2Pw9Rt4Zk' AND org = 'acme'", redactedNoise)
	plain := queryRecorded("plain", "SELECT name FROM users WHERE api_key = 'k1' AND org = 'acme'", nil)
	live := "SELECT name FROM users WHERE api_key = 'live' AND org = 'globex'"
	want := "select `name` from users where api_key = '(redacted)' and org = 'globex'"
	for _, pool := range [][]*models.Mock{{byKey, plain}, {plain, byKey}} {
		if got := poolPlaces(pool).liveStatement(live); got != want {
			t.Errorf("pool %s, %s: %q, want %q", pool[0].Name, pool[1].Name, got, want)
		}
	}
	if got := poolPlaces([]*models.Mock{plain}).liveStatement(live); got != live {
		t.Errorf("a pool that redacted nothing: %q, want the live statement", got)
	}
	if got := logPlaces(nil).live(live); got != valueRedacted {
		t.Errorf("no places read: %q, want the statement whole redacted", got)
	}
	read := 0
	if got := logPlaces(func() redactedPlaces { read++; return poolPlaces([]*models.Mock{byKey}) }).live(live); got != want || read != 1 {
		t.Errorf("places read: %q (read %d times), want %q", got, read, want)
	}
}

// The live statement and bound values as a report shows them.
func TestRedactedPlacesOfTheLiveCommand(t *testing.T) {
	const recorded = "SELECT /* app=web */ name FROM users WHERE org = 'acme' AND k = '" + redactedPlaceholder + "'"
	p := queryPlaces(recorded, util.NewNoiseChecker(redactedNoise))
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
			"select /* app=api */ `name` from users where org = 'globex' and k = '(redacted)'"},
		{"another template: every literal, comments dropped", p,
			"SELECT /* app=api */ name FROM users WHERE k = '" + liveSecret + "' AND org = 'globex'",
			"select `name` from users where k = '(redacted)' and org = '(redacted)'"},
		{"a live statement the parser cannot read", p, "SELEC name FRM users WHERE k = '" + liveSecret + "'", valueRedacted},
		{"the recording's comment redacted", queryPlaces("SELECT /* u="+redactedPlaceholder+" */ name FROM users WHERE k = 'acme'", util.NewNoiseChecker(redactedNoise)),
			"SELECT /* u=" + liveSecret + " */ name FROM users WHERE k = 'acme'", "select `name` from users where k = '(redacted)'"},
		{"whole", redactedPlaces{statement: true, whole: true}, "SELECT name FROM t_" + liveSecret, valueRedacted},
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
			[]string{"body.parameters.1.value", "body.parameters.1.value.$bin"},
			[]string{"body.query", "body.query.literals", "body.query.literals.0", "body.query.template", "body.parameters.0.value",
				"body.parameters.1.name", "body.parameters.1.type", "body.parameters.1.unsigned", "body.parameters.x.value"}},
		{"a literal, the recorded template", literalAt1, liveSame,
			[]string{"body.query", "body.query.literals", "body.query.literals.1"},
			[]string{"body.query.literals.0", "body.query.template", "body.query.comments", "body.parameters.1.value"}},
		{"a literal, another template", literalAt1, "SELECT name FROM users WHERE k = 'x'",
			[]string{"body.query.literals.0", "body.query.literals.1"},
			[]string{"body.query.template"}},
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
