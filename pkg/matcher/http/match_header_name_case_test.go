package http

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A header assertion names its header in any case, as HTTP header names are
// case-insensitive. The answer's header map is keyed by Go's canonical form
// (X-Request-Id, Www-Authenticate), so a test case that wrote the header as
// the server documents it (X-Request-ID, WWW-Authenticate) or in lower case
// (content-type) failed with "header not found" on an answer that carried it.
func TestAssertionMatchHeaderNameIsCaseInsensitive(t *testing.T) {
	answer := &models.HTTPResp{
		StatusCode: 200,
		Header: map[string]string{
			"Content-Type":     "application/json; charset=utf-8",
			"X-Request-Id":     "req-123",
			"Www-Authenticate": `Bearer realm="api"`,
		},
		Body: `{"id":"abc"}`,
	}
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		want       bool
	}{
		{"header_equal, upper-case acronym", map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-Request-ID": "req-123"}}, true},
		{"header_equal, lower case", map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"content-type": "application/json; charset=utf-8"}}, true},
		{"header_equal, wrong value still fails", map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-Request-ID": "req-999"}}, false},
		{"header_contains", map[models.AssertionType]interface{}{models.HeaderContains: map[string]interface{}{"content-type": "json"}}, true},
		{"header_matches", map[models.AssertionType]interface{}{models.HeaderMatches: map[string]interface{}{"WWW-Authenticate": "^Bearer "}}, true},
		{"header_exists, a list", map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"x-request-id", "WWW-Authenticate"}}, true},
		{"header_exists, a map", map[models.AssertionType]interface{}{models.HeaderExists: map[string]interface{}{"X-Request-ID": true}}, true},
		{"header_exists, an absent header still fails", map[models.AssertionType]interface{}{models.HeaderExists: []interface{}{"x-trace-id"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "test-1", HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"id":"abc"}`}, Assertions: c.assertions}
			pass, _ := AssertionMatch(tc, answer, zap.NewNop())
			require.Equal(t, c.want, pass)
		})
	}
}
