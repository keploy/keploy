package integration

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
)

// stepInstr is an agent connection that accepts every call a step makes.
type stepInstr struct{ setupInstr }

func (*stepInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) { return nil, nil }

// sentRequest is a POST the app was sent.
type sentRequest struct{ contentType, body string }

// bodyApp is an app that keeps the POSTs it is sent.
type bodyApp struct {
	mu   sync.Mutex
	reqs []sentRequest
}

func (a *bodyApp) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			a.mu.Lock()
			a.reqs = append(a.reqs, sentRequest{contentType: r.Header.Get("Content-Type"), body: string(b)})
			a.mu.Unlock()
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (a *bodyApp) received() []sentRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sentRequest(nil), a.reqs...)
}

// formPart is one part of a multipart body: its file name, if it is a file,
// and its content.
type formPart struct{ fileName, content string }

// formParts reads the parts of a multipart body, by field name.
func formParts(r sentRequest) (map[string]formPart, error) {
	_, params, err := mime.ParseMediaType(r.contentType)
	if err != nil {
		return nil, err
	}
	parts := map[string]formPart{}
	mr := multipart.NewReader(strings.NewReader(r.body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(p)
		if err != nil {
			return nil, err
		}
		parts[p.FormName()] = formPart{fileName: p.FileName(), content: string(b)}
	}
}

// recordPost stores tc as test-1 of set test-set-0 in db, as a recording
// does: the testdb keeps a part of it over 1 MB in a file beside it.
func recordPost(t *testing.T, db *testdb.TestYaml, req models.HTTPReq) {
	t.Helper()
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	req.Method, req.ProtoMajor, req.ProtoMinor = models.Method(http.MethodPost), 1, 1
	req.URL, req.Timestamp = "http://localhost:8080/upload", at
	tc := &models.TestCase{
		Version:  models.GetVersion(),
		Kind:     models.HTTP,
		Name:     "test-1",
		HTTPReq:  req,
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok", Timestamp: at.Add(time.Millisecond)},
	}
	if err := db.InsertTestCase(context.Background(), tc, "test-set-0", false); err != nil {
		t.Fatalf("InsertTestCase: %v", err)
	}
}

func newBodyRunner(db TestCaseDB) Service {
	return New(zap.NewNop(), db, &stepInstr{}, &skippingMockDB{},
		&stubMappingDB{mappings: map[string][]models.MockEntry{}}, nil, nil)
}

// storedCaseDB serves one stored test case from a recording at root.
type storedCaseDB struct {
	tc   *models.TestCase
	root string
}

func (d storedCaseDB) GetTestCase(context.Context, string, string) (*models.TestCase, error) {
	tc := *d.tc
	return &tc, nil
}

func (d storedCaseDB) Root() string { return d.root }

// keptFile is a part of a request the testdb keeps in a file beside its test
// case: the request body, a form value, a form file.
type keptFile struct {
	name string
	// req is the request as recorded.
	req models.HTTPReq
	// path is where the stored test case names that file.
	path func(*models.TestCase) *string
	// wrong says how what the app was sent differs from the recorded
	// request, or is "" when it is the same.
	wrong func(sentRequest) string
}

// keptFiles are the three parts a test case keeps beside it, each of 2 MiB.
func keptFiles() []keptFile {
	text := strings.Repeat("0123456789abcdef", (2<<20)/16)
	bin := make([]byte, 2<<20)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	const multipartType = "multipart/form-data; boundary=recorded"
	return []keptFile{
		{
			name: "a body",
			req:  models.HTTPReq{Header: map[string]string{"Content-Type": "text/plain"}, Body: text},
			path: func(tc *models.TestCase) *string { return &tc.HTTPReq.BodyRef.Path },
			wrong: func(r sentRequest) string {
				if r.body != text {
					return fmt.Sprintf("a body of %d bytes, not the recorded one of %d", len(r.body), len(text))
				}
				return ""
			},
		},
		{
			name: "a form value",
			req: models.HTTPReq{
				Header: map[string]string{"Content-Type": multipartType},
				Form:   []models.FormData{{Key: "note", Values: []string{"small"}}, {Key: "doc", Values: []string{text}}},
			},
			path: func(tc *models.TestCase) *string { return &tc.HTTPReq.Form[1].Paths[0] },
			wrong: func(r sentRequest) string {
				parts, err := formParts(r)
				if err != nil {
					return "a body that is not a form: " + err.Error()
				}
				doc, note := parts["doc"], parts["note"]
				if len(parts) != 2 || doc.fileName != "" || doc.content != text || note.content != "small" {
					return fmt.Sprintf("a form of %d parts, doc of %d bytes (file name %q), note %q; not the recorded one",
						len(parts), len(doc.content), doc.fileName, note.content)
				}
				return ""
			},
		},
		{
			name: "a form file",
			req: models.HTTPReq{
				Header: map[string]string{"Content-Type": multipartType},
				Form:   []models.FormData{{Key: "upload", FileNames: []string{"data.bin"}, FileContents: [][]byte{bin}}},
			},
			path: func(tc *models.TestCase) *string { return &tc.HTTPReq.Form[0].Paths[0] },
			wrong: func(r sentRequest) string {
				parts, err := formParts(r)
				if err != nil {
					return "a body that is not a form: " + err.Error()
				}
				up := parts["upload"]
				if len(parts) != 1 || up.fileName != "data.bin" || up.content != string(bin) {
					return fmt.Sprintf("a form of %d parts, upload of %d bytes named %q; not the recorded data.bin",
						len(parts), len(up.content), up.fileName)
				}
				return ""
			},
		},
	}
}

// recordKept records k in a testdb at root and returns the testdb and the
// stored test case, which names its file by a path relative to root.
func recordKept(t *testing.T, root string, k keptFile) (*testdb.TestYaml, *models.TestCase) {
	t.Helper()
	db := testdb.New(zap.NewNop(), root)
	recordPost(t, db, k.req)
	stored, err := db.GetTestCase(context.Background(), "test-set-0", "test-1")
	if err != nil {
		t.Fatalf("GetTestCase: %v", err)
	}
	p := *k.path(stored)
	if p == "" || filepath.IsAbs(p) {
		t.Fatalf("the testdb named the file %q; want it beside the test case, by a relative path", p)
	}
	if _, err := os.Stat(filepath.Join(root, p)); err != nil {
		t.Fatalf("the testdb did not keep the file under its root: %v", err)
	}
	return db, stored
}

// The testdb keeps a body, a form value or a form file over 1 MB beside its
// test case and names it by a path relative to the recording's root. The
// runner (the sandbox's and test generation's, which lay a set out in a
// directory of their own and pass the runner no path of the recording) sends
// each whole, read from its testdb's root. The working directory holds a
// different file at the same path, and it is never what the app is sent.
func TestTheRunnerSendsEachFileKeptBesideItsTestCase(t *testing.T) {
	for _, k := range keptFiles() {
		t.Run(k.name, func(t *testing.T) {
			tmp := t.TempDir()
			db, stored := recordKept(t, filepath.Join(tmp, "keploy"), k)
			work := filepath.Join(tmp, "work")
			decoy := filepath.Join(work, *k.path(stored))
			if err := os.MkdirAll(filepath.Dir(decoy), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(decoy, []byte("the working directory's, not the recording's"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(work)

			app := &bodyApp{}
			res := newBodyRunner(db).RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: app.start(t)})
			if res.Error != "" {
				t.Fatalf("RunTest: %s", res.Error)
			}
			got := app.received()
			if len(got) != 1 {
				t.Fatalf("the app was sent %d POSTs; want one", len(got))
			}
			if why := k.wrong(got[0]); why != "" {
				t.Fatalf("the app was sent %s", why)
			}
		})
	}
}

// A stored test case cannot make the runner read a file outside its
// recording: a body, form value or form file path that leads out of the
// testdb's directory is refused with why, and the app is sent nothing. A
// testdb with no directory has its test cases' files refused too, whatever
// the working directory holds.
func TestTheRunnerReadsNoFileOutsideItsRecording(t *testing.T) {
	for _, k := range keptFiles() {
		t.Run(k.name, func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "keploy")
			if err := os.WriteFile(filepath.Join(tmp, "outside.txt"), []byte("not the recording's"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, stored := recordKept(t, root, k)

			for _, c := range []struct {
				name, path, root, why string
			}{
				{"up and out of the recording", "../outside.txt", root, "is read only inside its recording"},
				{"a testdb with no directory", "outside.txt", "", "no recording directory was given"},
			} {
				t.Run(c.name, func(t *testing.T) {
					t.Chdir(tmp) // outside.txt is under the working directory
					tc := *stored
					tc.HTTPReq.Form = append([]models.FormData(nil), stored.HTTPReq.Form...)
					for i := range tc.HTTPReq.Form {
						tc.HTTPReq.Form[i].Paths = append([]string(nil), stored.HTTPReq.Form[i].Paths...)
					}
					*k.path(&tc) = c.path
					app := &bodyApp{}
					res := newBodyRunner(storedCaseDB{tc: &tc, root: c.root}).RunTest(context.Background(),
						RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: app.start(t)})
					if res.Passed || !strings.Contains(res.Error, c.path) || !strings.Contains(res.Error, c.why) {
						t.Fatalf("RunTest: passed %v, %q; want it refused, naming %q, with %q", res.Passed, res.Error, c.path, c.why)
					}
					if got := app.received(); len(got) != 0 {
						t.Fatalf("the app was sent %d POSTs; want none", len(got))
					}
				})
			}
		})
	}
}
