package matcher

import "testing"

// TestSubstringKeyMatch_CaseInsensitive verifies that SubstringKeyMatch treats
// both the header key and the noise-pattern key as case-insensitive. This
// guards against a historical regression where the incoming key was lower-
// cased by callers but the map key was compared verbatim, so CamelCase HTTP
// headers silently failed to match lowercase noise entries (and vice-versa).
func TestSubstringKeyMatch_CaseInsensitive(t *testing.T) {
	tests := []struct {
		name      string
		s         string
		noise     map[string][]string
		wantMatch bool
		wantVals  []string
	}{
		{
			name:      "CamelCase header matches lowercase noise pattern",
			s:         "X-Correlation-Id",
			noise:     map[string][]string{"x-correlation-id": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "lowercase header matches CamelCase noise pattern",
			s:         "x-correlation-id",
			noise:     map[string][]string{"X-Correlation-Id": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "Content-Type vs content-type matches",
			s:         "Content-Type",
			noise:     map[string][]string{"content-type": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
		{
			name:      "unrelated header does not match correlation-id pattern",
			s:         "X-Different",
			noise:     map[string][]string{"x-correlation-id": {}},
			wantMatch: false,
			wantVals:  []string{},
		},
		{
			name:      "mixed-case header with regex payload preserves value",
			s:         "X-Request-Id",
			noise:     map[string][]string{"x-request-id": {".*"}},
			wantMatch: true,
			wantVals:  []string{".*"},
		},
		{
			name:      "empty noise map never matches",
			s:         "X-Correlation-Id",
			noise:     map[string][]string{},
			wantMatch: false,
			wantVals:  []string{},
		},
		{
			name:      "substring semantics: short noise key matches longer header",
			s:         "X-My-Correlation-Id-Extra",
			noise:     map[string][]string{"CORRELATION-ID": {}},
			wantMatch: true,
			wantVals:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SubstringKeyMatch(tt.s, tt.noise)
			if ok != tt.wantMatch {
				t.Fatalf("SubstringKeyMatch(%q, %v) matched=%v, want %v",
					tt.s, tt.noise, ok, tt.wantMatch)
			}
			if len(got) != len(tt.wantVals) {
				t.Fatalf("SubstringKeyMatch(%q, ...) returned vals=%v, want %v",
					tt.s, got, tt.wantVals)
			}
			for i := range got {
				if got[i] != tt.wantVals[i] {
					t.Fatalf("SubstringKeyMatch(%q, ...) vals[%d]=%q, want %q",
						tt.s, i, got[i], tt.wantVals[i])
				}
			}
		})
	}
}

// TestSubstringKeyMatch_GlobSuffixLiteral documents that SubstringKeyMatch does
// NOT interpret glob metacharacters (e.g. "x-*") — it does a literal substring
// match. A pattern like "x-*" will therefore only match a header that literally
// contains the "*" character, never an arbitrary "x-..." header. This is an
// intentional simplification; callers that want glob matching should use the
// regex-aware code paths (JSONDiffWithNoiseControl / noiseIndex.match).
func TestSubstringKeyMatch_GlobSuffixLiteral(t *testing.T) {
	noise := map[string][]string{"x-*": {}}

	// An arbitrary "x-..." header must NOT match the glob pattern — the "*" is
	// treated as a literal character, not a wildcard.
	if _, ok := SubstringKeyMatch("X-Correlation-Id", noise); ok {
		t.Fatalf("SubstringKeyMatch should not treat '*' as a glob; got match for X-Correlation-Id vs x-*")
	}

	// But when the header genuinely contains "x-*" (literal), it does match.
	if _, ok := SubstringKeyMatch("some-X-*-thing", noise); !ok {
		t.Fatalf("SubstringKeyMatch should match literal '*' substring; missed some-X-*-thing vs x-*")
	}
}
