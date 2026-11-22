package templates

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// Cookie names are HTTP tokens, so punctuation can start a second cookie
// just as a letter can. Check against net/http parsing the original lines.
func TestSplitSetCookie_AllTokenStarts(t *testing.T) {
	for _, first := range "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz" {
		t.Run(string(first), func(t *testing.T) {
			name := string(first) + "session"
			lines := []string{"theme=dark; Expires=Wed, 21 Oct 2037 07:28:00 GMT; Path=/", name + "=LIVE; Path=/; HttpOnly"}
			reference := (&http.Response{Header: http.Header{"Set-Cookie": lines}}).Cookies()
			require.Len(t, reference, 2)
			for _, join := range []string{",", ", ", "\n"} {
				value := lines[0] + join + lines[1]
				cookies := SplitSetCookie(value)
				require.Len(t, cookies, 2, "separator %q", join)
				assert.Equal(t, reference[1].Name, cookies[1].Name)
				assert.Equal(t, reference[1].Value, cookies[1].Value(value))
				attr, ok := cookies[0].Attr("Expires")
				require.True(t, ok)
				assert.Equal(t, "Wed, 21 Oct 2037 07:28:00 GMT", value[attr.ValueStart:attr.ValueEnd])
			}
		})
	}
}

func TestExpected_DotPrefixedCookieCaptureReachesNextRequest(t *testing.T) {
	r := newRun(t, map[string]interface{}{"sid": "REC"})
	exp := models.HTTPResp{Header: map[string]string{"Set-Cookie": "theme=dark; Path=/,.AspNetCore.Cookies={{capture string .sid}}; Path=/; HttpOnly"}}
	ans := models.HTTPResp{Header: map[string]string{"Set-Cookie": "theme=dark; Path=/,.AspNetCore.Cookies=LIVE; Path=/; HttpOnly"}}
	res := r.Expected(exp, ans, ExpectOptions{})
	require.Empty(t, res.Failures)
	require.Len(t, res.Captures, 1)
	assert.Equal(t, "LIVE", r.Values()["sid"])
	assert.Equal(t, ans.Header["Set-Cookie"], res.Resp.Header["Set-Cookie"])
}

func TestKeep_DotPrefixedCookieKeepsPlaceholderAtItsOwnCookie(t *testing.T) {
	r := newRun(t, map[string]interface{}{"sid": "REC"})
	disk := models.HTTPResp{Header: map[string]string{"Set-Cookie": "theme=dark; Path=/,.AspNetCore.Cookies={{capture string .sid}}; Path=/; HttpOnly"}}
	live := models.HTTPResp{Header: map[string]string{"Set-Cookie": "theme=light; Path=/,.AspNetCore.Cookies=LIVE; Path=/; HttpOnly"}}
	kept, dropped := r.Keep(disk, live, live)
	assert.Empty(t, dropped)
	assert.Equal(t, "theme=light; Path=/,.AspNetCore.Cookies={{capture string .sid}}; Path=/; HttpOnly", kept.Header["Set-Cookie"])
}
