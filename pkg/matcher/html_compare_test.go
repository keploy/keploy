package matcher

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultTestConfig returns an HTMLCompareConfig that strips script and style
// — the baseline used by the production path.
func defaultTestConfig() HTMLCompareConfig {
	return HTMLCompareConfig{
		StripTags: map[string]bool{
			"script": true,
			"style":  true,
		},
	}
}

// ── Level 1: Whitespace Normalization ────────────────────────────────────────

func TestCanonicalizeHTML_Whitespace(t *testing.T) {
	config := defaultTestConfig()

	a := `<div>Hello   World</div>`
	b := `<div>Hello World</div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "extra whitespace should be collapsed to single space")
}

func TestCanonicalizeHTML_LeadingTrailingWhitespace(t *testing.T) {
	config := defaultTestConfig()

	a := `<p>  padded  </p>`
	b := `<p>padded</p>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "leading/trailing whitespace in text nodes should be trimmed")
}

// ── Level 1: Attribute Order ─────────────────────────────────────────────────

func TestCanonicalizeHTML_AttributeOrder(t *testing.T) {
	config := HTMLCompareConfig{}

	a := `<div id="1" class="a"></div>`
	b := `<div class="a" id="1"></div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "attribute ordering should not affect canonical form")
}

func TestCanonicalizeHTML_AttributeOrderMultiple(t *testing.T) {
	config := HTMLCompareConfig{}

	a := `<input type="text" name="q" placeholder="Search" required>`
	b := `<input placeholder="Search" required name="q" type="text">`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "any permutation of attributes should produce the same canonical form")
}

// ── Level 1: Dynamic Attribute Stripping ─────────────────────────────────────

func TestCanonicalizeHTML_StripDynamicAttr(t *testing.T) {
	re := regexp.MustCompile(`^id$`)
	config := HTMLCompareConfig{
		StripAttrRegex: []*regexp.Regexp{re},
	}

	a := `<div id="123">Hello</div>`
	b := `<div id="456">Hello</div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "dynamic id attribute should be stripped before comparison")
}

func TestCanonicalizeHTML_StripDataStar(t *testing.T) {
	re := regexp.MustCompile(`^data-.*$`)
	config := HTMLCompareConfig{
		StripAttrRegex: []*regexp.Regexp{re},
	}

	a := `<div data-session="abc" data-ts="1234">Content</div>`
	b := `<div data-session="xyz" data-ts="9999">Content</div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "all data-* attributes should be stripped")
}

// ── Level 1: Script Stripping ─────────────────────────────────────────────────

func TestCanonicalizeHTML_StripScript(t *testing.T) {
	config := HTMLCompareConfig{
		StripTags: map[string]bool{"script": true},
	}

	a := `<div>Hello<script>alert(1)</script></div>`
	b := `<div>Hello<script>alert(2)</script></div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "script tags should be completely stripped")
	assert.NotContains(t, ca, "alert", "script content must not appear in canonical form")
}

func TestCanonicalizeHTML_StripStyle(t *testing.T) {
	config := HTMLCompareConfig{
		StripTags: map[string]bool{"style": true},
	}

	a := `<html><head><style>.foo{color:red}</style></head><body>Hi</body></html>`
	b := `<html><head><style>.foo{color:blue}</style></head><body>Hi</body></html>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "style tags should be completely stripped")
}

// ── Level 1: Comment Stripping ────────────────────────────────────────────────

func TestCanonicalizeHTML_StripComments(t *testing.T) {
	config := defaultTestConfig()

	a := `<div><!-- this is dynamic: 2024-01-01 -->Hello</div>`
	b := `<div>Hello</div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "HTML comments should be stripped")
}

// ── Level 1: <pre> Whitespace Preservation ────────────────────────────────────

func TestCanonicalizeHTML_PreservesPreWhitespace(t *testing.T) {
	config := defaultTestConfig()

	// Whitespace inside <pre> must NOT be collapsed.
	a := `<pre>line1
	line2   spaced</pre>`
	b := `<pre>line1
line2 spaced</pre>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	// They differ in whitespace inside <pre>, so canonical forms should NOT match.
	assert.NotEqual(t, ca, cb, "<pre> whitespace must be preserved, not collapsed")
}

func TestCanonicalizeHTML_CodeDoesNotPreserveWhitespace(t *testing.T) {
	config := defaultTestConfig()

	// <code> is inline — whitespace IS collapsed (unlike <pre>).
	a := `<code>hello   world</code>`
	b := `<code>hello world</code>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.Equal(t, ca, cb, "<code> is inline — whitespace should be collapsed like any other element")
}

// ── Level 1: 1MB Size Guard ───────────────────────────────────────────────────

func TestCanonicalizeHTML_SizeGuard(t *testing.T) {
	config := defaultTestConfig()

	// Generate a body clearly above the 1MB threshold.
	big := strings.Repeat("<div>Hello</div>", 200_000) // ~3.2 MB

	_, err := CanonicalizeHTML(big, config)

	require.Error(t, err, "bodies above 1MB must be rejected with an error")
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

func TestCanonicalizeHTML_AtSizeBoundary(t *testing.T) {
	config := defaultTestConfig()

	// Body exactly at the limit — must succeed.
	atLimit := strings.Repeat("x", MaxHTMLBodySize)
	_, err := CanonicalizeHTML(atLimit, config)
	// html.Parse on binary content may fail but the size guard must NOT fire.
	// We simply verify the error is not the size-guard error.
	if err != nil {
		assert.NotContains(t, err.Error(), "exceeds maximum size",
			"body exactly at the limit should not trigger the size guard")
	}
}

// ── Level 1: Empty Input ──────────────────────────────────────────────────────

func TestCanonicalizeHTML_EmptyInput(t *testing.T) {
	config := defaultTestConfig()

	result, err := CanonicalizeHTML("", config)

	require.NoError(t, err)
	assert.Equal(t, "", result, "empty input should return empty string")
}

// ── Level 1: Malformed HTML ───────────────────────────────────────────────────

func TestCanonicalizeHTML_MalformedHTML(t *testing.T) {
	config := defaultTestConfig()

	// html.Parse is a spec-compliant error-correcting parser — it never errors
	// on malformed input. Two "equivalent" malformed documents should normalize
	// to the same canonical form.
	a := `<div><p>Unclosed`
	b := `<div><p>Unclosed</p></div>`

	ca, errA := CanonicalizeHTML(a, config)
	cb, errB := CanonicalizeHTML(b, config)

	require.NoError(t, errA)
	require.NoError(t, errB)
	assert.Equal(t, ca, cb, "error-corrected malformed HTML should produce the same canonical form")
}

// ── Level 1: Structural Difference ───────────────────────────────────────────

func TestCanonicalizeHTML_ContentDifference(t *testing.T) {
	config := defaultTestConfig()

	a := `<div>Expected Content</div>`
	b := `<div>Different Content</div>`

	ca, err := CanonicalizeHTML(a, config)
	require.NoError(t, err)
	cb, err := CanonicalizeHTML(b, config)
	require.NoError(t, err)

	assert.NotEqual(t, ca, cb, "different text content must produce different canonical forms")
}
