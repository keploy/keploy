package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
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
			assert.NotContains(t, string(out), "sk_"+"live_"+"51H8xYz", "the key stays in the file")
			assert.NotContains(t, string(out), "xox"+"b-1234", "the token stays in the file")
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

type cancelOnNthCheckContext struct {
	done     chan struct{}
	err      error
	mu       sync.Mutex
	checks   int
	cancelOn int
}

func newCancelOnNthCheckContext(cancelOn int) *cancelOnNthCheckContext {
	return &cancelOnNthCheckContext{
		done:     make(chan struct{}),
		cancelOn: cancelOn,
	}
}

func (c *cancelOnNthCheckContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c *cancelOnNthCheckContext) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	if c.checks >= c.cancelOn && c.err == nil {
		c.err = context.Canceled
		close(c.done)
	}
	return c.done
}

func (c *cancelOnNthCheckContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *cancelOnNthCheckContext) Value(key any) any {
	return nil
}

func TestSanitizeTestSetDir_CancellationSafeSecretPersistence(t *testing.T) {
	const (
		secretToken  = "Bearer " + "sk_" + "live_" + "51H8xYzAbCdEfGhIjKlMnOpQrStUvWx"
		file1Content = `version: api.keploy.io/v1beta1
kind: Http
name: test-1
spec:
  req:
    method: GET
    url: http://localhost:8080/api/1
    header:
      Authorization: ` + secretToken + `
  resp:
    status_code: 200
`
		file2Content = `version: api.keploy.io/v1beta1
kind: Http
name: test-2
spec:
  req:
    method: GET
    url: http://localhost:8080/api/2
    header:
      Authorization: ` + secretToken + `
  resp:
    status_code: 200
`
	)

	t.Run("cancelling mid-loop persists secret.yaml and leaves rewritten file with placeholders", func(t *testing.T) {
		tmpDir := t.TempDir()
		setDir := filepath.Join(tmpDir, "test-set-0")
		testsDir := filepath.Join(setDir, "tests")
		require.NoError(t, os.MkdirAll(testsDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(testsDir, "test-1.yaml"), []byte(file1Content), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(testsDir, "test-2.yaml"), []byte(file2Content), 0o644))

		// Cancel on the 2nd loop check (when test-1 is done, before test-2 starts)
		ctx := newCancelOnNthCheckContext(2)
		tools := &Tools{logger: zap.NewNop()}

		err := tools.SanitizeTestSetDir(ctx, setDir)
		require.ErrorIs(t, err, context.Canceled)

		// test-1.yaml must be rewritten with placeholders (no raw secret)
		out1, err := os.ReadFile(filepath.Join(testsDir, "test-1.yaml"))
		require.NoError(t, err)
		assert.NotContains(t, string(out1), "sk_"+"live_"+"51H8xYz")
		assert.Contains(t, string(out1), "{{string .secret.")

		// test-2.yaml must remain untouched
		out2, err := os.ReadFile(filepath.Join(testsDir, "test-2.yaml"))
		require.NoError(t, err)
		assert.Equal(t, file2Content, string(out2))

		// secret.yaml must exist and contain the original secret value
		secretPath := filepath.Join(setDir, "secret.yaml")
		secretBytes, err := os.ReadFile(secretPath)
		require.NoError(t, err, "secret.yaml must be persisted on context cancellation")
		var secrets map[string]string
		require.NoError(t, yaml.Unmarshal(secretBytes, &secrets))
		require.NotEmpty(t, secrets)

		found := false
		for _, v := range secrets {
			if v == secretToken {
				found = true
				break
			}
		}
		assert.True(t, found, "secret.yaml must contain the exact original secret value %q, got: %v", secretToken, secrets)
	})

	t.Run("cancelling before any file is sanitized does not create secret.yaml", func(t *testing.T) {
		tmpDir := t.TempDir()
		setDir := filepath.Join(tmpDir, "test-set-0")
		testsDir := filepath.Join(setDir, "tests")
		require.NoError(t, os.MkdirAll(testsDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(testsDir, "test-1.yaml"), []byte(file1Content), 0o644))

		// Cancel on the 1st loop check (before any file is sanitized)
		ctx := newCancelOnNthCheckContext(1)
		tools := &Tools{logger: zap.NewNop()}

		err := tools.SanitizeTestSetDir(ctx, setDir)
		require.ErrorIs(t, err, context.Canceled)

		// test-1.yaml should remain untouched
		out1, err := os.ReadFile(filepath.Join(testsDir, "test-1.yaml"))
		require.NoError(t, err)
		assert.Equal(t, file1Content, string(out1))

		// secret.yaml must NOT exist
		_, err = os.Stat(filepath.Join(setDir, "secret.yaml"))
		assert.True(t, os.IsNotExist(err), "secret.yaml should not be created when no secrets are sanitized")
	})
}

func TestDesanitizeFileInPlace_A4Restoration(t *testing.T) {
	createTestFile := func(t *testing.T, filename, content string) string {
		t.Helper()
		tmpDir := t.TempDir()
		setDir := filepath.Join(tmpDir, "test-set-0")
		testsDir := filepath.Join(setDir, "tests")
		require.NoError(t, os.MkdirAll(testsDir, 0o755))
		filePath := filepath.Join(testsDir, filename)
		require.NoError(t, os.WriteFile(filePath, []byte(content), 0o644))
		return filePath
	}

	t.Run("1. double-quoted YAML header with quotes and backslashes round-trips cleanly", func(t *testing.T) {
		const rawSecret = `abc"def\ghi`
		yamlContent := `version: api.keploy.io/v1beta1
kind: Http
name: test-1
spec:
  req:
    header:
      Authorization: "{{string .secret.TOKEN }}"
`
		testPath := createTestFile(t, "test-1.yaml", yamlContent)
		secrets := map[string]string{"TOKEN": rawSecret}

		require.NoError(t, DesanitizeFileInPlace(testPath, secrets))

		out, err := os.ReadFile(testPath)
		require.NoError(t, err)

		var req sanitizedReq
		require.NoError(t, yaml.Unmarshal(out, &req), "desanitized YAML must be syntactically valid")
		assert.Equal(t, rawSecret, req.Spec.Req.Header["Authorization"])
		assert.Contains(t, string(out), `Authorization: "abc\"def\\ghi"`)
	})

	t.Run("2. single-quoted YAML scalar with single and double quotes round-trips cleanly", func(t *testing.T) {
		const rawSecret = `foo'bar"baz`
		yamlContent := `version: api.keploy.io/v1beta1
kind: Http
name: test-2
spec:
  req:
    header:
      Authorization: '{{string .secret.TOKEN }}'
`
		testPath := createTestFile(t, "test-2.yaml", yamlContent)
		secrets := map[string]string{"TOKEN": rawSecret}

		require.NoError(t, DesanitizeFileInPlace(testPath, secrets))

		out, err := os.ReadFile(testPath)
		require.NoError(t, err)

		var req sanitizedReq
		require.NoError(t, yaml.Unmarshal(out, &req), "desanitized YAML must be syntactically valid")
		assert.Equal(t, rawSecret, req.Spec.Req.Header["Authorization"])
		assert.Contains(t, string(out), `Authorization: 'foo''bar"baz'`)
	})

	t.Run("3. embedded JSON body with quotes, backslashes, 64-bit int, and HTML chars", func(t *testing.T) {
		const rawSecret = `abc"def\ghi`
		yamlContent := `version: api.keploy.io/v1beta1
kind: Http
name: test-3
spec:
  req:
    body: '{"auth":"{{string .secret.TOKEN }}","big_int":9007199254740993,"html":"a < b & c > d"}'
`
		testPath := createTestFile(t, "test-3.yaml", yamlContent)
		secrets := map[string]string{"TOKEN": rawSecret}

		require.NoError(t, DesanitizeFileInPlace(testPath, secrets))

		out, err := os.ReadFile(testPath)
		require.NoError(t, err)

		var req sanitizedReq
		require.NoError(t, yaml.Unmarshal(out, &req), "desanitized YAML must be syntactically valid")

		bodyStr := req.Spec.Req.Body
		require.True(t, json.Valid([]byte(bodyStr)), "body must be valid JSON: %s", bodyStr)

		// Exact 64-bit int check - must not be rounded to 9007199254740992
		assert.NotContains(t, bodyStr, "9007199254740992")
		assert.Contains(t, bodyStr, "9007199254740993")

		// HTML characters must NOT be escaped to \u003c, \u003e, \u0026
		assert.NotContains(t, bodyStr, `\u003c`)
		assert.NotContains(t, bodyStr, `\u003e`)
		assert.NotContains(t, bodyStr, `\u0026`)
		assert.Contains(t, bodyStr, "a < b & c > d")

		dec := json.NewDecoder(strings.NewReader(bodyStr))
		dec.UseNumber()
		var jsonObj map[string]interface{}
		require.NoError(t, dec.Decode(&jsonObj))

		assert.Equal(t, rawSecret, jsonObj["auth"])
		assert.Equal(t, json.Number("9007199254740993"), jsonObj["big_int"])
		assert.Equal(t, "a < b & c > d", jsonObj["html"])
	})

	t.Run("4. untouched JSON body without placeholders remains byte-for-byte unchanged", func(t *testing.T) {
		const originalBody = `{"foo":  "bar",   "spacing":   123,  "nested":  {"a":  [1,  2]}}`
		yamlContent := `version: api.keploy.io/v1beta1
kind: Http
name: test-4
spec:
  req:
    header:
      Authorization: "{{string .secret.TOKEN }}"
    body: '` + originalBody + `'
`
		testPath := createTestFile(t, "test-4.yaml", yamlContent)
		secrets := map[string]string{"TOKEN": "restored-token"}

		require.NoError(t, DesanitizeFileInPlace(testPath, secrets))

		out, err := os.ReadFile(testPath)
		require.NoError(t, err)

		var req sanitizedReq
		require.NoError(t, yaml.Unmarshal(out, &req))

		// Authorization header was desanitized
		assert.Equal(t, "restored-token", req.Spec.Req.Header["Authorization"])

		// The JSON body without placeholders must remain byte-for-byte identical to originalBody
		assert.Equal(t, originalBody, req.Spec.Req.Body)
	})

	t.Run("5. newline secrets in YAML scalars and JSON bodies round-trip exactly", func(t *testing.T) {
		const rawSecret = "line1\nline2"
		yamlContent := `version: api.keploy.io/v1beta1
kind: Http
name: test-5
spec:
  req:
    header:
      Authorization: "{{string .secret.TOKEN }}"
    body: '{"auth":"{{string .secret.TOKEN }}","html":"a < b & c > d"}'
`
		testPath := createTestFile(t, "test-5.yaml", yamlContent)
		secrets := map[string]string{"TOKEN": rawSecret}

		require.NoError(t, DesanitizeFileInPlace(testPath, secrets))

		out, err := os.ReadFile(testPath)
		require.NoError(t, err)

		var req sanitizedReq
		require.NoError(t, yaml.Unmarshal(out, &req), "desanitized YAML must be syntactically valid")
		assert.Equal(t, rawSecret, req.Spec.Req.Header["Authorization"])

		require.True(t, json.Valid([]byte(req.Spec.Req.Body)), "restored embedded JSON must be valid: %s", req.Spec.Req.Body)
		dec := json.NewDecoder(strings.NewReader(req.Spec.Req.Body))
		var jsonObj map[string]interface{}
		require.NoError(t, dec.Decode(&jsonObj))
		assert.Equal(t, rawSecret, jsonObj["auth"])
		assert.Equal(t, "a < b & c > d", jsonObj["html"])
	})
}
