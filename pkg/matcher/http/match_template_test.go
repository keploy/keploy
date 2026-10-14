package http

import (
	"crypto/md5"  //nolint:gosec // an ETag form, not security
	"crypto/sha1" //nolint:gosec // Express's ETag is a SHA-1
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A value written out in an expected response is compared as written: Match
// has no template input, so these pairs fail, the verdict is the body
// result's, and the test case is left as it was given. (This passes on main
// as written, since no template value is set here; the false pass reproduces
// through a run: TestRunTestSet_LaterResponseAssertsTheValueWrittenOut.)
func TestMatch_AValueWrittenOutIsComparedAsWritten(t *testing.T) {
	cases := []struct {
		name             string
		expected, actual string
	}{
		{"string id written out in a later response", `{"id":"usr_01J9REC","name":"alice"}`, `{"id":"usr_OTHER01","name":"alice"}`},
		{"a number equal to a short template value", `{"count":1,"name":"alice"}`, `{"count":2,"name":"alice"}`},
		{"a string field equal to an int template value", `{"qty":"42"}`, `{"qty":"43"}`},
		{"an array element equal to a template value", `{"ids":["item_0001","item_0002"]}`, `{"ids":["item_9999","item_0002"]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{
				Name:     "get-user-2",
				HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: c.expected},
			}
			before := *tc
			actual := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: c.actual}
			for i := 0; i < 2; i++ { // a quiet pre-match and the loud one see the same thing
				pass, res := Match(tc, actual, map[string]map[string][]string{}, false, false, zap.NewNop(), i == 1)
				assert.False(t, pass, "the app answered another value where the test case says %s", c.expected)
				require.NotNil(t, res)
				assert.Equal(t, res.BodyResult[0].Normal, pass, "the verdict and the body result disagree")
			}
			assert.Equal(t, before, *tc, "Match changed the test case it was given")
		})
	}
}

// expressETag is the weak ETag Express sends by default: W/"<length in hex>-<SHA-1
// of the body, base64, 27 chars>".
func expressETag(body string) string {
	sum := sha1.Sum([]byte(body))
	return fmt.Sprintf(`W/"%x-%s"`, len(body), base64.StdEncoding.EncodeToString(sum[:])[:27])
}

const (
	recordedBody = `{"id":"usr_01J9REC","name":"alice"}`
	liveBody     = `{"id":"usr_LIVE001","name":"alice"}`
)

// etagCase is a templated read-back: its expected body rendered to the live
// id, recorded with the given ETag, answered with liveBody and another.
func etagCase(header, recorded, answered string) (*models.TestCase, *models.HTTPResp) {
	return &models.TestCase{Name: "t", HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{header: recorded}, Body: liveBody}},
		&models.HTTPResp{StatusCode: 200, Header: map[string]string{header: answered}, Body: liveBody}
}

func hexMD5(body string) string {
	sum := md5.Sum([]byte(body)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

func b64SHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// THE DIGEST OF A RENDERED BODY. A templated expected body resolves to this
// run's values; its digest headers were computed over the RECORDED body.
// WithRecordedBody says which: a header that is a digest of the recorded body
// is asserted as the same digest of the answer's body; any other value (a
// resource version) is compared as written. Neither depends on
// --disableAutoHeaderNoise.
func TestMatch_ADigestOfARenderedBodyIsAssertedOverTheAnswersBody(t *testing.T) {
	cases := []struct {
		name, header, recorded, answered string
		pass                             bool
	}{
		{"Express ETag, the answer's digest", "Etag", expressETag(recordedBody), expressETag(liveBody), true},
		{"Express ETag, a stale digest", "Etag", expressETag(recordedBody), expressETag(recordedBody), false},
		{"Express ETag, a digest of another body", "Etag", expressETag(recordedBody), expressETag(`{"id":"usr_OTHER"}`), false},
		{"strong hex MD5 ETag", "Etag", `"` + hexMD5(recordedBody) + `"`, `"` + hexMD5(liveBody) + `"`, true},
		{"strong hex MD5 ETag, stale", "Etag", `"` + hexMD5(recordedBody) + `"`, `"` + hexMD5(recordedBody) + `"`, false},
		{"Digest sha-256", "Digest", "sha-256=" + b64SHA256(recordedBody), "sha-256=" + b64SHA256(liveBody), true},
		{"Content-Digest sha-256, stale", "Content-Digest", "sha-256=:" + b64SHA256(recordedBody) + ":", "sha-256=:" + b64SHA256(recordedBody) + ":", false},
		{"Rails Rack::ETag, the answer's digest", "Etag", rackETag(recordedBody), rackETag(liveBody), true},
		{"Rails Rack::ETag, stale", "Etag", rackETag(recordedBody), rackETag(recordedBody), false},
		{"Spring ShallowEtagHeaderFilter, the answer's digest", "Etag", springETag(recordedBody, false), springETag(liveBody, false), true},
		{"Spring ShallowEtagHeaderFilter, a digest of another body", "Etag", springETag(recordedBody, false), springETag(`{"id":"usr_OTHER"}`, false), false},
		{"Spring weak ETag, the answer's digest", "Etag", springETag(recordedBody, true), springETag(liveBody, true), true},
		{"Spring weak ETag, stale", "Etag", springETag(recordedBody, true), springETag(recordedBody, true), false},
		{"a version ETag that stayed", "Etag", `"1"`, `"1"`, true},
		{"a version ETag that changed", "Etag", `"1"`, `"7"`, false},
		{"an opaque ETag that changed", "Etag", `W/"23-recorded"`, `W/"23-live"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, auto := range []bool{true, false} {
				tc, actual := etagCase(c.header, c.recorded, c.answered)
				pass, res := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(auto), WithRecordedBody(recordedBody))
				assert.Equal(t, c.pass, pass, "auto header noise %v: %+v", auto, res.HeadersResult)
			}
		})
	}

	// The body still has to match: a wrong id fails, its digest as well.
	tc, actual := etagCase("Etag", expressETag(recordedBody), expressETag(`{"id":"usr_OTHER","name":"alice"}`))
	actual.Body = `{"id":"usr_OTHER","name":"alice"}`
	pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true), WithRecordedBody(recordedBody))
	assert.False(t, pass)

	// The user's noise on the header still wins.
	tc, actual = etagCase("Etag", expressETag(recordedBody), `"anything"`)
	tc.Noise = map[string][]string{"header.Etag": {}}
	pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false), WithRecordedBody(recordedBody))
	assert.True(t, pass, "an ETag the user noised is not asserted")

	// And so does noise over every header (MH3).
	for _, noise := range []map[string][]string{{"header": {}}, {"header": {"*"}}} {
		tc, actual = etagCase("Etag", expressETag(recordedBody), expressETag(recordedBody))
		tc.Noise = noise
		pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false), WithRecordedBody(recordedBody))
		assert.True(t, pass, "a stale ETag under noise %v over every header is not asserted", noise)
	}
}

// With no rendered body nothing changes. Identical bytes with a
// differing digest is a non-deterministic ETag, a real defect, and fails as
// it always has.
func TestMatch_ADigestWithoutARenderedBodyIsAssertedAsBefore(t *testing.T) {
	tc, actual := etagCase("Etag", `W/"23-recorded"`, `W/"23-live"`)
	pass, _ := Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	assert.False(t, pass, "identical bytes with another ETag must still fail")

	// A templated body whose values this run are the recorded ones is no
	// rendering: its digest is compared as written. With the body passing
	// through noise and --disableAutoHeaderNoise, a digest that changed with
	// the noised field fails, exactly as it does for a set without templates.
	noised := `{"id":"usr_LIVE001","name":"bob"}`
	tc = &models.TestCase{Name: "t", Noise: map[string][]string{"body.name": {}}, HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Etag": expressETag(liveBody)}, Body: liveBody}}
	actual = &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Etag": expressETag(noised)}, Body: noised}
	pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false), WithRecordedBody(liveBody))
	assert.False(t, pass, "a template that rendered nothing must leave the digest compared as written")
	pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false))
	assert.False(t, pass, "control: the same without WithRecordedBody")

	// --disableAutoHeaderNoise, a body that passes only through noise: the
	// digest is asserted, as it always was when the user opts out.
	tc, actual = etagCase("Etag", `W/"23-recorded"`, `W/"23-live"`)
	tc.HTTPResp.Body = recordedBody
	tc.Noise = map[string][]string{"body.id": {}}
	pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(false))
	assert.False(t, pass, "a noised body difference let the ETag off with auto header noise disabled")
	tc, actual = etagCase("Etag", `W/"23-recorded"`, `W/"23-live"`)
	tc.HTTPResp.Body = recordedBody
	tc.Noise = map[string][]string{"body.id": {}}
	pass, _ = Match(tc, actual, nil, false, false, zap.NewNop(), false, WithAutoHeaderNoise(true))
	assert.True(t, pass, "control: with auto header noise on, a noised body difference lets its digest off")
}

// rackETag is the ETag Rails sends by default (Rack::ETag): weak, the first 32
// hex digits of the body's SHA-256.
func rackETag(body string) string {
	sum := sha256.Sum256([]byte(body))
	return `W/"` + hex.EncodeToString(sum[:])[:32] + `"`
}

// springETag is the ETag of Spring's ShallowEtagHeaderFilter: "0" and the
// body's hex MD5, weak when writeWeakETag is set.
func springETag(body string, weak bool) string {
	sum := md5.Sum([]byte(body)) //nolint:gosec
	tag := `"0` + hex.EncodeToString(sum[:]) + `"`
	if weak {
		return "W/" + tag
	}
	return tag
}

// TEMPLATE-SHAPED TEXT IN A HEADER IS COMPARED AS WRITTEN (disclosed verdict
// change 4). In a set without templates, a response header holding
// "Hello {{ .Name }}" that the app answers "Hello Bob" fails, as any other
// header that differs. main rendered both sides of the header diff with its
// process-global template map, turned each into "<no value>", and passed it
// (22 of 20,000 seeded differential cases).
func TestMatch_TemplateShapedTextInAHeaderIsComparedAsWritten(t *testing.T) {
	for _, c := range []struct {
		name, expected, actual string
		pass                   bool
	}{
		{"the app answers other text", "Hello {{ .Name }}", "Hello Bob", false},
		{"a typed placeholder's text, answered another", "n={{int .n}}", "n=7", false},
		{"the app answers the text as recorded", "Hello {{ .Name }}", "Hello {{ .Name }}", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "t", HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Tpl": c.expected}, Body: `{"ok":true}`}}
			actual := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Tpl": c.actual}, Body: `{"ok":true}`}
			pass, res := Match(tc, actual, map[string]map[string][]string{}, false, false, zap.NewNop(), false)
			assert.Equal(t, c.pass, pass)
			require.NotNil(t, res)
		})
	}
}
