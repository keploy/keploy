package replayer

import (
	"reflect"
	"testing"
)

// tokenByKey finds the first token with the given key. Helper for assertions.
func tokenByKey(toks []queryLiteralToken, key string) (queryLiteralToken, bool) {
	for _, tk := range toks {
		if tk.Key == key {
			return tk, true
		}
	}
	return queryLiteralToken{}, false
}

// (a) extractor: UPDATE SET literals are eligible, WHERE literal is not.
func TestExtractQueryLiterals_UpdateSetEligibleWhereNot(t *testing.T) {
	toks, err := extractQueryLiterals("update t set updated_at='x', n=5 where id=42")
	if err != nil {
		t.Fatalf("extractQueryLiterals: %v", err)
	}

	updatedAt, ok := tokenByKey(toks, "set:updated_at#0")
	if !ok {
		t.Fatalf("missing set:updated_at#0 token; got %+v", toks)
	}
	if !updatedAt.Eligible {
		t.Errorf("set:updated_at literal should be Eligible")
	}
	if updatedAt.Val != "x" {
		t.Errorf("set:updated_at val: want %q, got %q", "x", updatedAt.Val)
	}

	nTok, ok := tokenByKey(toks, "set:n#0")
	if !ok || !nTok.Eligible {
		t.Errorf("set:n literal should exist and be Eligible; got %+v", toks)
	}

	// The WHERE literal (42) must be present but NOT eligible (default-deny).
	var sawWhere bool
	for _, tk := range toks {
		if tk.Val == "42" {
			sawWhere = true
			if tk.Eligible {
				t.Errorf("WHERE literal 42 must NOT be Eligible (never learnable)")
			}
		}
	}
	if !sawWhere {
		t.Errorf("WHERE literal 42 not collected at all; got %+v", toks)
	}
}

// (b) detectQueryNoise: UPDATE with a drifting updated_at SET literal learns
// exactly that key and nothing else.
func TestDetectQueryNoise_UpdateLearnsOnlyDriftingSetLiteral(t *testing.T) {
	recorded := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	live := "update orders set views=5, updated_at='2026-06-17 14:00:24' where region='north'"

	noise, ok := detectQueryNoise(recorded, live)
	if !ok {
		t.Fatalf("detectQueryNoise returned ok=false for structurally-identical queries")
	}
	want := map[string][]string{
		"set:updated_at#0": {"2026-01-01 12:48:36"},
	}
	if !reflect.DeepEqual(noise, want) {
		t.Fatalf("learned noise mismatch:\n want %v\n got  %v", want, noise)
	}
}

// detectQueryNoise must NOT learn a non-eligible (WHERE) drift, and returns the
// empty (but ok==true) map when only WHERE drifts.
func TestDetectQueryNoise_DoesNotLearnWhereDrift(t *testing.T) {
	recorded := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	live := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='south'"

	noise, ok := detectQueryNoise(recorded, live)
	if !ok {
		t.Fatalf("detectQueryNoise returned ok=false")
	}
	if len(noise) != 0 {
		t.Fatalf("WHERE drift must not be learned; got %v", noise)
	}
}

// (c) INSERT VALUES: learns the drifting generated-id position.
func TestDetectQueryNoise_InsertLearnsValuesPosition(t *testing.T) {
	recorded := "insert into events (user_id, code) values (101,'code-1001')"
	live := "insert into events (user_id, code) values (101,'code-2002')"

	noise, ok := detectQueryNoise(recorded, live)
	if !ok {
		t.Fatalf("detectQueryNoise returned ok=false")
	}
	want := map[string][]string{
		"values:0:1": {"code-1001"},
	}
	if !reflect.DeepEqual(noise, want) {
		t.Fatalf("learned noise mismatch:\n want %v\n got  %v", want, noise)
	}
}

// detectQueryNoise returns (nil,false) when the queries are not structurally
// comparable (different literal counts) or one fails to parse.
func TestDetectQueryNoise_NonComparable(t *testing.T) {
	// Different literal counts (one extra SET column) -> not comparable.
	if _, ok := detectQueryNoise(
		"update t set a=1 where id=2",
		"update t set a=1, b=3 where id=2",
	); ok {
		t.Errorf("expected ok=false for differing literal counts")
	}
	// Unparseable live query -> ok=false.
	if _, ok := detectQueryNoise("update t set a=1 where id=2", "this is not sql"); ok {
		t.Errorf("expected ok=false for unparseable query")
	}
}

// (d) ENFORCE: with the learned set from (b), queryMatchesWithinNoise tolerates
// the updated_at drift.
func TestQueryMatchesWithinNoise_ToleratesLearnedSetDrift(t *testing.T) {
	recorded := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	live := "update orders set views=5, updated_at='2026-06-17 14:00:24' where region='north'"
	learned := map[string][]string{
		"set:updated_at#0": {"2026-01-01 12:48:36"},
	}
	if !queryMatchesWithinNoise(recorded, live, learned) {
		t.Fatalf("expected match: only the learned-noise updated_at literal drifted")
	}
}

// (e) REGRESSION GUARD: a changed WHERE predicate is never tolerated, and an
// eligible literal that was not learned (views) also rejects.
func TestQueryMatchesWithinNoise_RejectsWhereAndUnlearnedDrift(t *testing.T) {
	recorded := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='north'"
	learned := map[string][]string{
		"set:updated_at#0": {"2026-01-01 12:48:36"},
	}

	// Changed WHERE predicate (region) — WHERE is never learnable.
	whereDrift := "update orders set views=5, updated_at='2026-01-01 12:48:36' where region='south'"
	if queryMatchesWithinNoise(recorded, whereDrift, learned) {
		t.Errorf("WHERE drift must reject (WHERE never learnable)")
	}

	// Changed eligible SET literal (views) that is NOT a learned-noise key.
	viewsDrift := "update orders set views=9, updated_at='2026-01-01 12:48:36' where region='north'"
	if queryMatchesWithinNoise(recorded, viewsDrift, learned) {
		t.Errorf("unlearned eligible SET drift (views) must reject")
	}
}

// queryMatchesWithinNoise returns true when the queries are byte-identical
// (no drift at all), regardless of the learned set.
func TestQueryMatchesWithinNoise_IdenticalQueriesMatch(t *testing.T) {
	q := "update t set a=1, b='x' where id=2"
	if !queryMatchesWithinNoise(q, q, nil) {
		t.Errorf("identical queries must match even with no learned noise")
	}
}

// queryMatchesWithinNoise returns false when the queries are not structurally
// comparable (different literal counts).
func TestQueryMatchesWithinNoise_NonComparable(t *testing.T) {
	if queryMatchesWithinNoise(
		"update t set a=1 where id=2",
		"update t set a=1, b=3 where id=2",
		map[string][]string{"set:b#0": {"3"}},
	) {
		t.Errorf("differing literal counts must not match")
	}
}

// (Finding 1) A difference in a non-literal part of the query — table, column,
// or operator — is NOT a literal-noise drift. The redacted skeletons differ, so
// detection refuses to learn (ok=false) and strict never tolerates it, even if
// every eligible position were already learned. getQueryStructure alone (AST
// node types) would wrongly treat these as comparable.
func TestQueryNoise_NonLiteralDriftRejected(t *testing.T) {
	cases := []struct{ name, recorded, live string }{
		{"table", "update users set name='x' where id=1", "update admins set name='x' where id=1"},
		{"column", "update t set a=1 where id=1", "update t set a=1 where other_id=1"},
		{"operator", "update t set a=1 where id=1", "update t set a=1 where id>1"},
	}
	// Deliberately generous: pretend every eligible position is learned noise.
	learned := map[string][]string{"set:a#0": {"1"}, "set:name#0": {"x"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := detectQueryNoise(c.recorded, c.live); ok {
				t.Errorf("%s drift must be non-comparable (detectQueryNoise ok=false)", c.name)
			}
			if queryMatchesWithinNoise(c.recorded, c.live, learned) {
				t.Errorf("%s drift must reject under strict (queryMatchesWithinNoise=false)", c.name)
			}
		})
	}
}

// (Finding 2) A literal inside a subquery/predicate under a SET expression is
// NOT learnable. The skeleton still matches (only the nested literal differs),
// but because the literal is non-eligible, detection learns nothing and strict
// rejects the drift.
func TestQueryNoise_SubqueryUnderSetNotLearnable(t *testing.T) {
	recorded := "update t set score=(select max(score) from scores where tenant_id=1) where id=7"
	live := "update t set score=(select max(score) from scores where tenant_id=2) where id=7"

	// The subquery's tenant_id literal must be collected as NON-eligible.
	toks, err := extractQueryLiterals(recorded)
	if err != nil {
		t.Fatalf("extractQueryLiterals: %v", err)
	}
	for _, tk := range toks {
		if tk.Val == "1" && tk.Eligible {
			t.Errorf("subquery literal tenant_id=1 must NOT be Eligible; got %+v", tk)
		}
	}

	// Skeletons are equal (only the nested literal differs), so detection runs
	// but learns nothing (the drift is non-eligible).
	noise, ok := detectQueryNoise(recorded, live)
	if !ok {
		t.Fatalf("skeletons should be equal -> detectQueryNoise ok=true")
	}
	if len(noise) != 0 {
		t.Errorf("subquery-predicate drift must not be learned; got %v", noise)
	}
	// And strict rejects it even with a generous learned set.
	if queryMatchesWithinNoise(recorded, live, map[string][]string{"set:score#0": {"whatever"}}) {
		t.Errorf("subquery-predicate drift must reject under strict")
	}
}
