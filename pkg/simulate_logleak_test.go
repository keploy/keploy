package pkg

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// Sending a test's request to the app logs its shape and size, never the
// request: its headers carry the test's credentials, its URL a token, its
// body a password. A request the app's connection drops fails with the
// client's error, which quotes the URL: withheld in the log line, and kept
// in the error returned, which the test's own report shows (beside the
// request it records anyway). It logged the whole *http.Request at debug
// level.
func TestSimulateHTTPLogsNoneOfTheRequest(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct {
		name  string
		fails bool
		app   http.HandlerFunc
	}{
		{"an answered request", false, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }},
		{"a request whose connection the app drops", true, func(w http.ResponseWriter, _ *http.Request) {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := httptest.NewServer(tc.app)
			t.Cleanup(app.Close)
			logger, logs := logleak.Observe()
			testCase := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{
				Method: "POST", URL: app.URL + "/v1/reset/" + s + "?access_token=" + s,
				Header: map[string]string{"Authorization": "Bearer " + s, "Content-Type": "application/json"},
				Body:   `{"password":"` + s + `"}`,
			}}
			_, err := SimulateHTTP(context.Background(), testCase, "s", logger, SimulationConfig{APITimeout: 5})
			if logs.Len() == 0 {
				t.Fatal("precondition: nothing was logged; the test drove none of the lines")
			}
			logleak.Check(t, logs, s, "SimulateHTTP")
			if tc.fails {
				if err == nil || logs.FilterMessage("failed to send testcase request to app").Len() == 0 {
					t.Fatalf("precondition: the dropped request did not fail and log (%v)", err)
				}
				if !logleak.Holds(err.Error(), s) {
					t.Error("the error returned for the test's report lost the request's URL")
				}
			}
		})
	}
}

// A test's multipart file part is sent from the file the recording kept for
// it, assets/<test>/<field>/<name>, whose name is the one the app's client
// gave the file (filename=): a user's name, a document's title. keploy's
// lines about it, and the replay's line for a test whose file is missing,
// name the file's folder and its name's size, never the name; the error
// returned, which the test's report keeps, names it whole.
func TestSimulateHTTPLogsNoMultipartFileName(t *testing.T) {
	const s = logleak.Canary
	name := "passport-" + s + ".pdf"
	for _, at := range []string{"kept", "missing", "folder"} {
		t.Run(map[string]string{"kept": "the file is kept", "missing": "the file is missing", "folder": "the part names a folder"}[at], func(t *testing.T) {
			kept := at == "kept"
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
			t.Cleanup(app.Close)
			root := t.TempDir()
			rel := filepath.Join("assets", "test-1", "upload", name)
			switch at {
			case "kept":
				if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, rel), []byte("the bytes a user uploaded"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "folder":
				// A hand-edited test points the part at a folder: it opens,
				// and its read fails naming the path (read ...: is a
				// directory).
				if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			logger, logs := logleak.Observe()
			testCase := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{
				Method: "POST", URL: app.URL + "/upload",
				Header: map[string]string{"Content-Type": "multipart/form-data; boundary=xyz"},
				Form:   []models.FormData{{Key: "upload", FileNames: []string{name}, Paths: []string{rel}}},
			}}
			_, err := SimulateHTTP(context.Background(), testCase, "s", logger, SimulationConfig{APITimeout: 5, KeployPath: root})
			if logs.FilterMessage("multipart file path").Len() == 0 {
				t.Fatal("precondition: the multipart part was not sent; the test drove none of its lines")
			}
			if at == "missing" && (err == nil || logs.FilterMessage("failed to open the multipart file kept beside the test case").Len() == 0) {
				t.Fatalf("precondition: a missing file did not fail and log (%v)", err)
			}
			if at == "folder" && (err == nil || logs.FilterMessage("failed to write multipart file part").Len() == 0) {
				t.Fatalf("precondition: a folder's read did not fail and log (%v)", err)
			}
			logleak.Check(t, logs, s, "SimulateHTTP of a multipart file part")
			// The error names the file whole, for the test's report; the
			// replay's line for the test names it by its folder.
			if kept && err != nil {
				t.Fatalf("a kept file's replay failed: %v", err)
			}
			if err != nil {
				if !logleak.Holds(err.Error(), s) {
					t.Errorf("the error %q, which the test's report keeps, lost the file's name", err)
				}
				if line := LoggableAnswerError(fmt.Errorf("test-1: %w", err)); logleak.Holds(line, s) || !strings.Contains(line, "(file name withheld, ") {
					t.Errorf("the replay's line names the error as %q, want the file's folder and its name's size", line)
				}
			}
		})
	}
}

// Each line keploy logs as it resolves a test's target names the target by
// its shape (ResolveTestTarget) and the test case it is the request of, which
// the report keeps the request whole under: the line alone names no call a
// reader can find.
func TestSimulateHTTPNamesTheTestOnItsTargetLines(t *testing.T) {
	const s = logleak.Canary
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	t.Cleanup(app.Close)
	logger, logs := logleak.Observe()
	testCase := &models.TestCase{Name: "test-7", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://recorded.example/v1/reset/" + s}}
	host, port, err := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = SimulateHTTP(context.Background(), testCase, "s", logger, SimulationConfig{APITimeout: 5, ConfigHost: host, URLReplacements: map[string]string{"recorded.example": host + ":" + port}})
	if err != nil {
		t.Fatalf("SimulateHTTP: %v", err)
	}
	lines := logs.FilterMessageSnippet("replaceWith substitution").All()
	lines = append(lines, logs.FilterMessageSnippet("Replaced host with config host").All()...)
	if len(lines) == 0 {
		t.Fatal("precondition: the target was resolved with no line; the test drove none")
	}
	for _, e := range lines {
		if e.ContextMap()["testcase"] != "test-7" {
			t.Errorf("the line %q names no test case: %v", e.Message, e.ContextMap())
		}
	}
	logleak.Check(t, logs, s, "SimulateHTTP's target lines")
}
