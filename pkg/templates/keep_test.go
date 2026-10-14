package templates

import (
	"crypto/md5"  //nolint:gosec // the digest forms servers send
	"crypto/sha1" //nolint:gosec // as above
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy normalize keeps each placeholder where the run resolved it to what
// the app answered, and writes the answer, with a reason, where it did not.
func TestKeep_PutsBackEachPlaceholderTheAnswerAgreesWith(t *testing.T) {
	onDisk := models.HTTPResp{
		StatusCode: 201,
		Header:     map[string]string{"Location": "/users/{{string .id}}", "X-Ver": "{{string .ver}}"},
		Body:       `{"id":"{{capture string .id}}","n":"{{int .n}}","shape":"v1","owner":"{{string .owner}}"}`,
	}
	resolved := models.HTTPResp{
		Header: map[string]string{"Location": "/users/usr_LIVE", "X-Ver": "1"},
		Body:   `{"id":"usr_LIVE","n":7654321,"shape":"v1","owner":"alice"}`,
	}
	answer := models.HTTPResp{
		StatusCode: 201,
		Header:     map[string]string{"location": "/users/usr_LIVE", "X-Ver": "2", "Content-Type": "application/json"},
		Body:       `{"id":"usr_LIVE","n":7654321,"shape":"v2","owner":"bob"}`,
	}
	got, dropped := NewRun(nil, nil, nil).Keep(onDisk, resolved, answer)
	assert.Equal(t, `{"id":"{{capture string .id}}","n":"{{int .n}}","shape":"v2","owner":"bob"}`, got.Body)
	assert.Equal(t, "/users/{{string .id}}", got.Header["Location"])
	_, stillLower := got.Header["location"]
	assert.False(t, stillLower, "the header is written once, under the test case's name for it")
	assert.Equal(t, "2", got.Header["X-Ver"])
	assert.Equal(t, "application/json", got.Header["Content-Type"])
	require.Len(t, dropped, 2)
	assert.Contains(t, dropped[0], "body.owner")
	assert.Contains(t, dropped[1], "header X-Ver")
}

// A test case without a placeholder is normalized exactly as it always was:
// the answer, byte for byte.
func TestKeep_NoPlaceholderIsTheAnswerAsItIs(t *testing.T) {
	answer := models.HTTPResp{StatusCode: 200, Header: map[string]string{"A": "1"}, Body: `{"b": 2, "a":1}`}
	got, dropped := NewRun(nil, nil, nil).Keep(models.HTTPResp{Body: `{"a":1}`, Header: map[string]string{"A": "0"}}, models.HTTPResp{}, answer)
	assert.Equal(t, answer, got)
	assert.Empty(t, dropped)
}

func TestKeep_ABodyThatIsNoLongerJSONDropsItsPlaceholders(t *testing.T) {
	got, dropped := NewRun(nil, nil, nil).Keep(models.HTTPResp{Body: `{"id":"{{string .id}}"}`}, models.HTTPResp{Body: `{"id":"x"}`}, models.HTTPResp{Body: "gone"})
	assert.Equal(t, "gone", got.Body)
	require.Len(t, dropped, 1)
	assert.Contains(t, dropped[0], "no longer JSON")
	// An answer encoding/json rejects is no longer JSON either, a
	// placeholder the app answered where a value goes included.
	for _, answer := range []string{`{"id":{{string .id}}}`, "{\"id\":\"a\tb\"}"} {
		got, dropped = NewRun(nil, nil, nil).Keep(models.HTTPResp{Body: `{"id":"{{string .id}}"}`}, models.HTTPResp{Body: answer}, models.HTTPResp{Body: answer})
		assert.Equal(t, answer, got.Body)
		require.Len(t, dropped, 1, "%q", answer)
		assert.Contains(t, dropped[0], "no longer JSON")
	}
}

// Set-Cookie is kept cookie by cookie: a cookie whose value the run resolved
// to what the app set keeps its placeholder, with the answer's attributes
// (a fresh Expires among them); one the app now sets otherwise is dropped.
func TestKeep_SetCookieIsKeptCookieByCookie(t *testing.T) {
	onDisk := models.HTTPResp{Header: map[string]string{"Set-Cookie": "csrftoken={{capture string .csrf}}; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT,sessionid={{capture string .sid}}; Path=/; HttpOnly"}}
	resolved := models.HTTPResp{Header: map[string]string{"Set-Cookie": "csrftoken=C1; Path=/; Expires=Fri, 24 Oct 2026 11:00:00 GMT,sessionid=S1; Path=/; HttpOnly"}}
	answer := models.HTTPResp{Header: map[string]string{"set-cookie": "csrftoken=C1; Path=/; Expires=Fri, 24 Oct 2026 11:00:00 GMT,sessionid=S9; Path=/app; HttpOnly"}}
	got, dropped := NewRun(nil, nil, nil).Keep(onDisk, resolved, answer)
	assert.Equal(t, "csrftoken={{capture string .csrf}}; Path=/; Expires=Fri, 24 Oct 2026 11:00:00 GMT,sessionid=S9; Path=/app; HttpOnly", got.Header["Set-Cookie"])
	_, stillLower := got.Header["set-cookie"]
	assert.False(t, stillLower)
	require.Len(t, dropped, 1)
	assert.Contains(t, dropped[0], "header Set-Cookie.sessionid")
}

// A PLACEHOLDER IN AN OBJECT KEY is not put back: the answer's keys are
// written, and normalize says which placeholder it dropped.
func TestKeep_AnObjectKeyPlaceholderIsDroppedAndSaid(t *testing.T) {
	onDisk := models.HTTPResp{Body: `{"{{string .id}}":true,"n":"{{int .n}}"}`}
	got, dropped := NewRun(nil, nil, nil).Keep(onDisk, models.HTTPResp{Body: `{"usr_1":true,"n":7}`}, models.HTTPResp{Body: `{"usr_1":true,"n":7}`})
	assert.Equal(t, `{"usr_1":true,"n":"{{int .n}}"}`, got.Body)
	assert.Equal(t, []string{"body key {{string .id}}: \"{{string .id}}\" (the answer's keys are written)"}, dropped)
}

// THE DIGEST INVARIANT. A digest header on disk is a digest of the body with
// each placeholder at its config.yaml value: every run's digest rule starts
// from that body. So a kept body's ETag (Express, Rails Rack::ETag, Spring's
// ShallowEtagHeaderFilter, hex), Content-MD5 and Digest are written as the
// same digest of the kept body rendered with the recorded values; a version
// ETag, and a body that keeps no placeholder, keep the answer's.
func TestKeep_ADigestHeaderIsTheDigestOfTheBodyAsRecorded(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "usr_REC"}, nil)
	onDisk := models.HTTPResp{Body: `{"id":"{{capture string .id}}","name":"alice"}`}
	answerBody := `{"id":"usr_LIVE","name":"bob"}`
	recorded := `{"id":"usr_REC","name":"bob"}`
	resolved := models.HTTPResp{Body: `{"id":"usr_LIVE","name":"alice"}`}
	sha1b64 := func(b string) string { s := sha1.Sum([]byte(b)); return base64.StdEncoding.EncodeToString(s[:]) }
	md5hex := func(b string) string { s := md5.Sum([]byte(b)); return hex.EncodeToString(s[:]) }
	sha256hex := func(b string) string { s := sha256.Sum256([]byte(b)); return hex.EncodeToString(s[:]) }
	md5b64 := func(b string) string { s := md5.Sum([]byte(b)); return base64.StdEncoding.EncodeToString(s[:]) }
	for _, c := range []struct {
		name, header string
		of           func(body string) string
	}{
		{"Express", "ETag", func(b string) string { return fmt.Sprintf(`W/"%x-%s"`, len(b), sha1b64(b)[:27]) }},
		{"Rails Rack::ETag", "ETag", func(b string) string { return `W/"` + sha256hex(b)[:32] + `"` }},
		{"Spring ShallowEtagHeaderFilter", "ETag", func(b string) string { return `"0` + md5hex(b) + `"` }},
		{"hex SHA-256", "ETag", func(b string) string { return `"` + sha256hex(b) + `"` }},
		{"Content-MD5", "Content-MD5", md5b64},
		{"Digest", "Digest", func(b string) string {
			return "sha-256=" + func() string { s := sha256.Sum256([]byte(b)); return base64.StdEncoding.EncodeToString(s[:]) }()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := models.HTTPResp{Header: map[string]string{c.header: c.of(answerBody), "X-Version": `"7"`}, Body: answerBody}
			got, _ := r.Keep(onDisk, resolved, answer)
			require.Equal(t, `{"id":"{{capture string .id}}","name":"bob"}`, got.Body)
			assert.Equal(t, c.of(recorded), got.Header[c.header], "the digest of the kept body at the recorded values")
			assert.Equal(t, `"7"`, got.Header["X-Version"])
		})
	}
	version := models.HTTPResp{Header: map[string]string{"ETag": `"v7"`}, Body: answerBody}
	got, _ := r.Keep(onDisk, resolved, version)
	assert.Equal(t, `"v7"`, got.Header["ETag"], "a version ETag is no digest of the body")
	lost := models.HTTPResp{Header: map[string]string{"ETag": `"` + md5hex(`{"id":7}`) + `"`}, Body: `{"id":7}`}
	got, _ = r.Keep(onDisk, resolved, lost)
	assert.Equal(t, lost.Header["ETag"], got.Header["ETag"], "a body that keeps no placeholder is the answer's, digest included")
}
