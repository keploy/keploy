package replayer

import (
	"fmt"

	"vitess.io/vitess/go/vt/sqlparser"
)

// queryLiteralToken is one literal value extracted from a SQL statement in a
// deterministic traversal order. Eligible reports whether this literal lives
// in a position where request-literal noise may be LEARNED — true only for
// UPDATE SET expressions and INSERT VALUES tuples. Every other literal
// (WHERE / ON / HAVING / subquery / CASE / LIMIT / anywhere else) is collected
// with Eligible=false so the enforce step can default-deny any drift there.
type queryLiteralToken struct {
	// Key encodes the eligible literal position so the learned-noise map can
	// be keyed by clause role rather than by raw traversal index. For
	// non-eligible literals the Key carries the traversal index only (it is
	// never written to the learned-noise map, so its exact value is unused by
	// enforcement — only Eligible and Val matter there).
	Key string
	// Val is the literal's raw value as the parser captured it.
	Val string
	// Eligible is true only for UPDATE SET / INSERT VALUES literals.
	Eligible bool
}

// newSQLParser builds a vitess parser with default options. Mirrors
// getQueryStructure so behaviour is consistent across the matcher.
func newSQLParser() (*sqlparser.Parser, error) {
	parser, err := sqlparser.New(sqlparser.Options{})
	if err != nil {
		return nil, fmt.Errorf("failed to create MySQL query parser: %w", err)
	}
	return parser, nil
}

// extractQueryLiterals parses sql and returns its literals in a deterministic
// traversal order. The SAME extractor is run on both the recorded and the live
// query, so the only invariant that matters for zipping is that two
// structurally-identical statements yield the same ordered token list — which
// a single AST Walk guarantees.
//
// Eligibility is computed by first collecting the set of *sqlparser.Literal
// pointers that sit directly under an UPDATE SET expression or an INSERT VALUES
// tuple, then marking each Walk-visited literal eligible iff its pointer is in
// that set. This is clause-aware (default-deny): a literal anywhere else —
// including an UPDATE's WHERE, a SELECT, a DELETE, a subquery, or a CASE — is
// collected with Eligible=false.
func extractQueryLiterals(sql string) ([]queryLiteralToken, error) {
	parser, err := newSQLParser()
	if err != nil {
		return nil, err
	}
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SQL: %w", err)
	}

	// eligible maps an eligible *Literal pointer to its clause-role key.
	eligible := map[*sqlparser.Literal]string{}

	switch s := stmt.(type) {
	case *sqlparser.Update:
		// UPDATE ... SET col = <literal>, ... — every literal that appears in
		// a SET expression's RHS is learnable. We index by the target column
		// name plus an occurrence counter so two SETs of the same column (rare
		// but legal) get distinct keys, and key collisions across rows can't
		// silently merge.
		colCounts := map[string]int{}
		for _, ue := range s.Exprs {
			col := ""
			if ue.Name != nil {
				col = ue.Name.Name.String()
			}
			n := colCounts[col]
			colCounts[col] = n + 1
			key := fmt.Sprintf("set:%s#%d", col, n)
			markLiteralsUnder(ue.Expr, key, eligible)
		}
	case *sqlparser.Insert:
		// INSERT ... VALUES (<literal>, ...), (...) — every literal in a
		// VALUES tuple is learnable, keyed by (rowIdx, colIdx).
		if rows, ok := s.Rows.(sqlparser.Values); ok {
			for rowIdx, tuple := range rows {
				for colIdx, expr := range tuple {
					key := fmt.Sprintf("values:%d:%d", rowIdx, colIdx)
					markLiteralsUnder(expr, key, eligible)
				}
			}
		}
		// INSERT ... SELECT (Rows is *Select/*Union) is intentionally not
		// treated as eligible: those literals live in a SELECT, never learnable.

		// TODO(querynoise): COM_STMT_EXECUTE (prepared statements) is out of
		// scope. Prepared-statement parameters arrive as bound values rather
		// than inline literals and would need a separate per-parameter noise
		// path; this extractor only handles plaintext COM_QUERY SQL.
	}

	var tokens []queryLiteralToken
	idx := 0
	walkErr := sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		lit, ok := node.(*sqlparser.Literal)
		if !ok {
			return true, nil
		}
		key, isEligible := eligible[lit]
		if !isEligible {
			key = fmt.Sprintf("lit#%d", idx)
		}
		tokens = append(tokens, queryLiteralToken{
			Key:      key,
			Val:      lit.Val,
			Eligible: isEligible,
		})
		idx++
		return true, nil
	}, stmt)
	if walkErr != nil {
		return nil, fmt.Errorf("failed to walk the AST: %w", walkErr)
	}

	return tokens, nil
}

// markLiteralsUnder records every DIRECT-value *sqlparser.Literal reachable
// from expr into the eligible set under key. A SET RHS / VALUES element is
// usually a bare literal, but it can also be a wrapping expression (e.g. a
// tuple or an arithmetic expression) — walking the subtree captures whatever
// literals it actually contains while keeping non-literal positions out.
//
// Crucially, eligibility must NOT leak into nested query/predicate contexts:
// a literal inside a subquery, a SELECT/UNION, or a CASE expression under the
// SET RHS (e.g. `set score=(select max(score) from scores where tenant_id=1)`)
// lives in a predicate position that is never learnable. We stop descending at
// those node types so their literals remain default-deny (they are still
// collected as non-eligible by the main extractor Walk).
func markLiteralsUnder(expr sqlparser.Expr, key string, eligible map[*sqlparser.Literal]string) {
	if expr == nil {
		return
	}
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch n := node.(type) {
		case *sqlparser.Subquery, *sqlparser.Select, *sqlparser.Union, *sqlparser.CaseExpr:
			// Stop descending: literals under a nested query / CASE predicate
			// are never directly-set values and must stay non-eligible.
			return false, nil
		case *sqlparser.Literal:
			eligible[n] = key
		}
		return true, nil
	}, expr)
}

// redactedSkeleton returns sql with every literal replaced by a placeholder
// while preserving table/column/operator/keyword structure — a canonical
// skeleton produced by vitess' RedactSQLQuery. The bool is false when the SQL
// fails to parse/normalize. Two queries whose skeletons are byte-equal share
// ALL non-literal semantics (same tables, columns, operators, clause shape);
// only their literal VALUES may differ.
func redactedSkeleton(sql string) (string, bool) {
	parser, err := newSQLParser()
	if err != nil {
		return "", false
	}
	skeleton, err := parser.RedactSQLQuery(sql)
	if err != nil {
		return "", false
	}
	return skeleton, true
}

// skeletonsEqual reports whether two queries redact to byte-identical
// skeletons. This is the authoritative structural gate for request-literal
// noise: getQueryStructure (the matchQuery structure gate) records only AST
// node TYPES, so it treats `update users ... where id=1` and
// `update admins ... where id=1` (or `where id=1` vs `where other_id=1`) as
// comparable when they are NOT. The redacted skeleton preserves identifiers,
// operators and clause shape, so requiring it to be equal means per-literal
// value comparison only decides tolerance — never structural identity.
func skeletonsEqual(a, b string) bool {
	sa, oka := redactedSkeleton(a)
	if !oka {
		return false
	}
	sb, okb := redactedSkeleton(b)
	if !okb {
		return false
	}
	return sa == sb
}

// detectQueryNoise diffs a recorded COM_QUERY against a structurally-identical
// live COM_QUERY and learns request-literal noise. It parses both; if either
// fails to parse, or the two produce a different number of literal tokens
// (i.e. not actually structurally identical at the literal level), it returns
// (nil, false). Otherwise it zips the token lists by index and, for every index
// where the values differ AND the token is in an eligible (SET / VALUES)
// position, records key -> [recordedVal] as noise.
//
// Differing literals in NON-eligible positions are deliberately NOT added: they
// make the queries "drift" in a place that is never learnable, and the strict
// enforce step (queryMatchesWithinNoise) rejects the mock on them.
func detectQueryNoise(recordedSQL, liveSQL string) (map[string][]string, bool) {
	// Authoritative structural gate: the two queries must redact to the SAME
	// skeleton (same tables/columns/operators/clause shape). getQueryStructure
	// only compares AST node TYPES, so without this an UPDATE on `users` and on
	// `admins`, or `where id=1` vs `where other_id=1`, would be treated as
	// comparable. Only literal VALUES may differ past this point.
	if !skeletonsEqual(recordedSQL, liveSQL) {
		return nil, false
	}
	recTokens, err := extractQueryLiterals(recordedSQL)
	if err != nil {
		return nil, false
	}
	liveTokens, err := extractQueryLiterals(liveSQL)
	if err != nil {
		return nil, false
	}
	if len(recTokens) != len(liveTokens) {
		return nil, false
	}

	noise := map[string][]string{}
	for i := range recTokens {
		rt := recTokens[i]
		lt := liveTokens[i]
		if rt.Val == lt.Val {
			continue
		}
		if !rt.Eligible {
			// A non-eligible literal drifted. Detection never learns it; the
			// enforce path will reject any mock on this drift. Skip it here.
			continue
		}
		noise[rt.Key] = []string{rt.Val}
	}
	return noise, true
}

// queryMatchesWithinNoise reports whether a structurally-identical live
// COM_QUERY is consumable by a recorded mock under STRICT enforcement, given
// the mock's learned noise set. It parses both and zips tokens; for every index
// whose value differs:
//   - if the token is NOT in an eligible (SET / VALUES) position -> no match
//     (WHERE / predicate / subquery drift is never tolerated), and
//   - if the token's clause-role Key is not present in learned -> no match
//     (the literal drifted at an eligible position that was never learned as
//     noise — e.g. a column whose value changed but was never flagged).
//
// When every differing literal is both eligible AND learned-noise, it returns
// true. If either side fails to parse, or the token counts differ, it returns
// false (not structurally identical -> caller must not treat it as a match).
func queryMatchesWithinNoise(recordedSQL, liveSQL string, learned map[string][]string) bool {
	// Authoritative structural gate (see detectQueryNoise): a differing
	// table/column/operator/clause shape is NOT a literal-noise drift and must
	// reject outright, never be tolerated as "within noise".
	if !skeletonsEqual(recordedSQL, liveSQL) {
		return false
	}
	recTokens, err := extractQueryLiterals(recordedSQL)
	if err != nil {
		return false
	}
	liveTokens, err := extractQueryLiterals(liveSQL)
	if err != nil {
		return false
	}
	if len(recTokens) != len(liveTokens) {
		return false
	}

	for i := range recTokens {
		rt := recTokens[i]
		lt := liveTokens[i]
		if rt.Val == lt.Val {
			continue
		}
		if !rt.Eligible {
			return false
		}
		if _, ok := learned[rt.Key]; !ok {
			return false
		}
	}
	return true
}
