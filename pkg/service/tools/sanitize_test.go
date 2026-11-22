package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"gopkg.in/yaml.v3"
)

// sanitizedReq is the request of a test case file as keploy sanitize left it.
type sanitizedReq struct {
	Spec struct {
		Req struct {
			URL    string            `yaml:"url"`
			Header map[string]string `yaml:"header"`
			Body   string            `yaml:"body"`
		} `yaml:"req"`
	} `yaml:"spec"`
}

// WHAT keploy sanitize WRITES IN A REQUEST, A REPLAY SENDS BACK AS RECORDED.
// sanitize replaces the whole scalar that holds a secret with
// {{string .secret.K}} and keeps that scalar in secret.yaml as recorded: a
// URL whose query holds an API key, a form body that holds a token, a Bearer
// header. Inside a JSON body it redacts the JSON value alone and keeps it
// decoded. Rendering each request back gives the recorded request byte for
// byte, so a set sanitize wrote replays as it did before it was sanitized.
func TestSanitizeFileInPlace_ARequestRendersBackAsRecorded(t *testing.T) {
	// The fake key and token are split so the source holds no literal that
	// secret scanners read as a live credential; the values are unchanged.
	const (
		stripeKey  = "sk_" + "live_51H8xYzAbCdEfGhIjKlMnOpQrStUvWx"
		slackToken = "xo" + "xb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"
		reqURL     = "http://localhost:8080/v1/charges?api_key=" + stripeKey + "&note=a%2Bb+c"
		form       = "token=" + slackToken + "&user=alice&pw=p%40ss+w0rd"
		bearer     = "Bearer " + stripeKey
		jsonDoc    = `{"key":"` + stripeKey + `","user":"alice"}`
	)
	for _, c := range []struct {
		name, contentType, url, body, auth string
	}{
		{name: "a URL whose query holds a key", url: reqURL},
		{name: "a form body that holds a token", contentType: "application/x-www-form-urlencoded", url: "http://localhost:8080/login", body: form},
		{name: "a Bearer header", url: "http://localhost:8080/me", auth: bearer},
		{name: "a JSON body", contentType: "application/json", url: "http://localhost:8080/keys", body: jsonDoc},
	} {
		t.Run(c.name, func(t *testing.T) {
			header := map[string]string{}
			if c.contentType != "" {
				header["Content-Type"] = c.contentType
			}
			if c.auth != "" {
				header["Authorization"] = c.auth
			}
			doc := map[string]interface{}{
				"version": "api.keploy.io/v1beta1", "kind": "Http", "name": "test-1",
				"spec": map[string]interface{}{
					"req":  map[string]interface{}{"method": "POST", "url": c.url, "header": header, "body": c.body},
					"resp": map[string]interface{}{"status_code": 200, "body": `{"ok":true}`},
				},
			}
			src, err := yaml.Marshal(doc)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "test-1.yaml")
			require.NoError(t, os.WriteFile(path, src, 0o644))

			secrets := map[string]string{}
			require.NoError(t, SanitizeFileInPlace(path, "test-1", secrets))
			out, err := os.ReadFile(path)
			require.NoError(t, err)
			var got sanitizedReq
			require.NoError(t, yaml.Unmarshal(out, &got))
			req := got.Spec.Req
			require.NotEmpty(t, secrets, "sanitize found no secret")
			assert.NotContains(t, string(out), "sk_live_51H8xYz", "the key stays in the file")
			assert.NotContains(t, string(out), "xoxb-1234", "the token stays in the file")
			assert.True(t, templates.Contains(req.URL+req.Body+req.Header["Authorization"]), "no placeholder was written")

			sec := map[string]interface{}{}
			for k, v := range secrets {
				sec[k] = v
			}
			sent := templates.NewRun(nil, nil, sec).Request(models.HTTPReq{URL: req.URL, Header: req.Header, Body: req.Body})
			assert.Equal(t, c.url, sent.URL)
			assert.Equal(t, c.body, sent.Body)
			assert.Equal(t, c.auth, sent.Header["Authorization"])
			if c.body == jsonDoc {
				assert.True(t, strings.HasPrefix(req.Body, `{"key":"{{string .secret.`), "a JSON body is redacted value by value: %s", req.Body)
			}
		})
	}
}
