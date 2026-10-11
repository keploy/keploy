package bodydigest

import (
	"crypto/md5" //nolint:gosec // an ETag form, not security
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recorded, other = `{"id":"usr_REC01"}`, `{"id":"usr_LIVE002"}`

func b64Of(sum []byte) string { return base64.StdEncoding.EncodeToString(sum) }

func md5b64(b string) string    { s := md5.Sum([]byte(b)); return b64Of(s[:]) } //nolint:gosec
func sha1b64(b string) string   { s := sha1.Sum([]byte(b)); return b64Of(s[:]) }
func sha256b64(b string) string { s := sha256.Sum256([]byte(b)); return b64Of(s[:]) }
func sha512b64(b string) string { s := sha512.Sum512([]byte(b)); return b64Of(s[:]) }
func sha1hex(b string) string   { s := sha1.Sum([]byte(b)); return hex.EncodeToString(s[:]) }
func sha256hex(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }
func md5hex(b string) string    { s := md5.Sum([]byte(b)); return hex.EncodeToString(s[:]) } //nolint:gosec

// EACH DIGEST FORM a server writes over the body it sends is recognised in the
// value recorded for a body, and rewritten, in the same form, for another
// body: the letter case of a hex ETag, its weakness, an Express, Rails or
// Spring tag, Content-MD5, and every item of a Digest list, sha-512 included.
func TestForm_RecomputesEachDigestFormForAnotherBody(t *testing.T) {
	for _, c := range []struct {
		name, header string
		value        func(body string) string
	}{
		{"a lower-case hex SHA-1 ETag", "etag", func(b string) string { return `"` + sha1hex(b) + `"` }},
		{"an upper-case hex SHA-256 ETag", "etag", func(b string) string { return `"` + strings.ToUpper(sha256hex(b)) + `"` }},
		{"an upper-case hex MD5 weak ETag", "etag", func(b string) string { return `W/"` + strings.ToUpper(md5hex(b)) + `"` }},
		{"Express's ETag", "etag", func(b string) string { return fmt.Sprintf(`W/"%x-%s"`, len(b), sha1b64(b)[:27]) }},
		{"Rails' Rack::ETag", "etag", func(b string) string { return `W/"` + sha256hex(b)[:32] + `"` }},
		{"Spring's ShallowEtagHeaderFilter", "etag", func(b string) string { return `"0` + md5hex(b) + `"` }},
		{"Content-MD5", "content-md5", md5b64},
		{"a Digest of sha-512", "digest", func(b string) string { return "sha-512=" + sha512b64(b) }},
		{"a Digest list of two items", "digest", func(b string) string { return "SHA-256=" + sha256b64(b) + ", md5=" + md5b64(b) }},
		{"a Digest of sha (SHA-1)", "digest", func(b string) string { return "sha=" + sha1b64(b) }},
		{"a Content-Digest", "content-digest", func(b string) string { return "sha-512=:" + sha512b64(b) + ":" }},
		{"a Repr-Digest list", "repr-digest", func(b string) string { return "sha-256=:" + sha256b64(b) + ":, sha-512=:" + sha512b64(b) + ":" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, IsHeader(c.header))
			form, ok := Form(c.header, c.value(recorded), recorded)
			require.True(t, ok, "%s is a digest of the body", c.value(recorded))
			assert.Equal(t, c.value(other), form(other))
		})
	}
}

// A VALUE THAT IS NO DIGEST OF THE BODY is not one: a resource version, a
// hash of another body, a list one item of which is not a digest of it, an
// algorithm keploy does not know, an unquoted ETag.
func TestForm_AValueThatIsNoDigestOfTheBodyIsNotOne(t *testing.T) {
	for _, c := range []struct{ name, header, value string }{
		{"a version ETag", "etag", `"7"`},
		{"a hash of another body", "etag", `"` + sha1hex(other) + `"`},
		{"an unquoted ETag", "etag", sha1hex(recorded)},
		{"a list with one foreign item", "digest", "sha-256=" + sha256b64(recorded) + ", md5=" + md5b64(other)},
		{"an unknown algorithm", "digest", "crc32c=AAAAAA=="},
		{"an item that is no alg=value", "digest", "sha-256"},
		{"Content-MD5 of another body", "content-md5", md5b64(other)},
		{"a header that is no digest", "content-type", "application/json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ok := Form(c.header, c.value, recorded)
			assert.False(t, ok)
		})
	}
	assert.False(t, IsHeader("content-length"), "Content-Length follows the body by length alone")
}
