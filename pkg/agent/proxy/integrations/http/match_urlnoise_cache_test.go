package http

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// compileURLNoise memoizes per pattern, returns the SAME compiled regexp on a
// repeat, and caches an invalid pattern as a skip rather than recompiling it.
func TestCompileURLNoise_Memoizes(t *testing.T) {
	logger := zap.NewNop()
	pat := `^cache-probe-[0-9]+$`

	first := compileURLNoise(logger, []string{pat})
	require.Len(t, first, 1)
	second := compileURLNoise(logger, []string{pat})
	require.Len(t, second, 1)
	require.Same(t, first[0], second[0], "the compiled pattern should be reused, not recompiled")

	// An invalid pattern is skipped (not returned) on every call.
	bad := compileURLNoise(logger, []string{`^cache-probe-[`, pat})
	require.Len(t, bad, 1)
	require.Same(t, first[0], bad[0])
	require.Nil(t, compileURLNoise(logger, nil))
}

// The configured-noise path must not compile anything while every value matches
// exactly — that fast path runs once per candidate mock on every request.
func TestQueryParamsMatch_NoiseNotCompiledOnExactMatch(t *testing.T) {
	h := newHTTP()
	// A pattern that is only ever compiled if the value-mismatch branch runs.
	pat := `^never-compiled-probe-[0-9]+$`
	require.True(t, h.QueryParamsMatch(
		map[string]string{"id": "A", "page": "2"},
		url.Values{"id": {"A"}, "page": {"2"}},
		[]string{pat}))
	_, cached := urlNoiseCache.Load(pat)
	require.False(t, cached, "url noise must not be compiled when no value differs")
}
