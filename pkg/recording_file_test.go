package pkg

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

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// recordingDirs lays out a recording at <tmp>/keploy and a file outside it,
// <tmp>/outside.txt, with a symbolic link to that file inside the recording.
func recordingDirs(t *testing.T) (root, outside string) {
	t.Helper()
	tmp := t.TempDir()
	root = filepath.Join(tmp, "keploy")
	if err := os.MkdirAll(filepath.Join(root, "test-set-0", "assets", "test-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(tmp, "outside.txt")
	if err := os.WriteFile(outside, []byte("not the recording's"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A machine that cannot make a link (Windows without the privilege)
	// leaves the link's row out.
	if err := os.Symlink(outside, filepath.Join(root, "test-set-0", "assets", "test-1", "link.txt")); err != nil {
		t.Logf("no symbolic link: %v", err)
	}
	return root, outside
}

// hasLink says whether recordingDirs made its link.
func hasLink(root string) bool {
	_, err := os.Lstat(filepath.Join(root, "test-set-0", "assets", "test-1", "link.txt"))
	return err == nil
}

func writeRecordingFile(t *testing.T, root, rel, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, rel), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// partsApp is an app that keeps each request it is sent: its body, and the
// parts of a multipart one as "name|file name|text".
type partsApp struct {
	mu       sync.Mutex
	requests int
	bodies   []string
	parts    [][]string
}

func (a *partsApp) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests++
		if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && mt == "multipart/form-data" {
			var got []string
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(p)
				got = append(got, p.FormName()+"|"+p.FileName()+"|"+string(b))
			}
			a.parts = append(a.parts, got)
		} else {
			b, _ := io.ReadAll(r.Body)
			a.bodies = append(a.bodies, string(b))
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (a *partsApp) sent() (int, []string, [][]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests, append([]string(nil), a.bodies...), append([][]string(nil), a.parts...)
}

func bodyRefCase(url, path string) *models.TestCase {
	return &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPReq: models.HTTPReq{
		Method: http.MethodPost, URL: url + "/upload", ProtoMajor: 1, ProtoMinor: 1,
		Header:  map[string]string{"Content-Type": "text/plain"},
		BodyRef: models.BodyRef{Path: path, Size: 2 << 20},
	}}
}

func formCase(url string, form models.FormData) *models.TestCase {
	return &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPReq: models.HTTPReq{
		Method: http.MethodPost, URL: url + "/upload", ProtoMajor: 1, ProtoMinor: 1,
		Header: map[string]string{"Content-Type": "multipart/form-data; boundary=x"},
		Form:   []models.FormData{form},
	}}
}

// A test case's file is read only inside its recording, wherever the
// process's working directory is: a body, a form value and a form file kept
// beside the test case are sent from the recording's directory, and a path
// that leads out of it (by "..", as an absolute path, or through a symbolic
// link), or any path with no recording directory, is refused with why, and
// nothing is read or sent.
func TestATestCasesFileIsReadOnlyInsideItsRecording(t *testing.T) {
	root, outside := recordingDirs(t)
	body := strings.Repeat("b", 2<<20)
	writeRecordingFile(t, root, "test-set-0/assets/test-1/body.txt", body)
	writeRecordingFile(t, root, "test-set-0/assets/test-1/value_0.txt", "a value kept beside")
	writeRecordingFile(t, root, "test-set-0/assets/test-1/report.csv", "a,b\n1,2\n")
	// The working directory holds a file at each path too: it is never read.
	cwd := t.TempDir()
	t.Chdir(cwd)
	for _, rel := range []string{"body.txt", "value.txt", "report.csv"} {
		writeRecordingFile(t, cwd, rel, "the working directory's")
	}

	const inside = "is read only inside its recording"
	const noRoot = "no recording directory was given"
	link := "test-set-0/assets/test-1/link.txt"
	refused := []struct {
		name, root, path, why string
	}{
		// A path that leads out by its own name is said plainly too: where
		// it leads, or that it is absolute, and what to name instead.
		{"up and out", root, "../outside.txt", fmt.Sprintf("%s %q: it names %q, a place outside it; name a place inside it", inside, root, filepath.Join(filepath.Dir(root), "outside.txt"))},
		{"absolute, outside", root, outside, fmt.Sprintf("%s %q: it names %q, a place outside it; name a place inside it", inside, root, outside)},
		{"absolute, inside", root, filepath.Join(root, "test-set-0/assets/test-1/body.txt"), fmt.Sprintf("%s %q: it names %q by an absolute path, which keploy does not open; name it relative to %q", inside, root, filepath.Join(root, "test-set-0/assets/test-1/body.txt"), root)},
		{"no recording directory", "", "body.txt", noRoot},
		{"a recording directory that is not there", filepath.Join(root, "gone"), "body.txt", "open the recording directory"},
	}
	if hasLink(root) {
		// A link out is said as one, after what any refusal of the file says.
		refused = append(refused, struct{ name, root, path, why string }{"a link out", root, link, inside + fmt.Sprintf(" %q: it is a link that leads out of it", root)})
	}

	t.Run("body", func(t *testing.T) {
		app := &partsApp{}
		url := app.start(t)
		resp, err := SimulateHTTP(context.Background(), bodyRefCase(url, "test-set-0/assets/test-1/body.txt"), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: root})
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("SimulateHTTP: %v", err)
		}
		if n, bodies, _ := app.sent(); n != 1 || len(bodies) != 1 || bodies[0] != body {
			t.Fatalf("the app was sent %d requests; want one with the %d bytes kept beside the test case", n, len(body))
		}
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				app := &partsApp{}
				url := app.start(t)
				_, err := SimulateHTTP(context.Background(), bodyRefCase(url, c.path), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: c.root})
				if err == nil || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), fmt.Sprintf("%q", c.path)) {
					t.Fatalf("SimulateHTTP: %v; want it refused, naming %q, with %q", err, c.path, c.why)
				}
				if strings.Contains(err.Error(), "escapes from parent") {
					t.Fatalf("SimulateHTTP: %v; want it said without os.Root's words", err)
				}
				if n, _, _ := app.sent(); n != 0 {
					t.Fatalf("the app was sent %d requests; want none", n)
				}
			})
		}
	})

	t.Run("form value", func(t *testing.T) {
		app := &partsApp{}
		url := app.start(t)
		form := models.FormData{Key: "note", Values: []string{"", "small"}, Paths: []string{"test-set-0/assets/test-1/value_0.txt", ""}}
		if _, err := SimulateHTTP(context.Background(), formCase(url, form), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: root}); err != nil {
			t.Fatalf("SimulateHTTP: %v", err)
		}
		if n, _, parts := app.sent(); n != 1 || len(parts) != 1 || strings.Join(parts[0], ";") != "note||a value kept beside;note||small" {
			t.Fatalf("the app was sent %d requests, parts %q; want the value kept beside the test case, then the small one", n, parts)
		}
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				app := &partsApp{}
				url := app.start(t)
				form := models.FormData{Key: "note", Values: []string{""}, Paths: []string{c.path}}
				_, err := SimulateHTTP(context.Background(), formCase(url, form), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: c.root})
				if err == nil || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), fmt.Sprintf("%q", c.path)) {
					t.Fatalf("SimulateHTTP: %v; want it refused, naming %q, with %q", err, c.path, c.why)
				}
				if n, _, _ := app.sent(); n != 0 {
					t.Fatalf("the app was sent %d requests; want none", n)
				}
			})
		}
	})

	t.Run("form file", func(t *testing.T) {
		app := &partsApp{}
		url := app.start(t)
		form := models.FormData{Key: "report", FileNames: []string{"report.csv"}, Paths: []string{"test-set-0/assets/test-1/report.csv"}}
		if _, err := SimulateHTTP(context.Background(), formCase(url, form), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: root}); err != nil {
			t.Fatalf("SimulateHTTP: %v", err)
		}
		if n, _, parts := app.sent(); n != 1 || len(parts) != 1 || strings.Join(parts[0], ";") != "report|report.csv|a,b\n1,2\n" {
			t.Fatalf("the app was sent %d requests, parts %q; want the file kept beside the test case", n, parts)
		}
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				app := &partsApp{}
				url := app.start(t)
				form := models.FormData{Key: "report", FileNames: []string{"report.csv"}, Paths: []string{c.path}}
				_, err := SimulateHTTP(context.Background(), formCase(url, form), "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 10, KeployPath: c.root})
				if err == nil || !strings.Contains(err.Error(), c.why) || !strings.Contains(err.Error(), fmt.Sprintf("%q", c.path)) {
					t.Fatalf("SimulateHTTP: %v; want it refused, naming %q, with %q", err, c.path, c.why)
				}
				if n, _, _ := app.sent(); n != 0 {
					t.Fatalf("the app was sent %d requests; want none", n)
				}
			})
		}
	})
}

// A name that passes by ".." and stays inside the recording, as a test case
// may name its file (test-set-0/assets/deep/../body.txt), reads as before:
// os.Root walks it, and nothing is refused.
func TestANameThatPassesByDotDotInsideItsRecordingReads(t *testing.T) {
	root, _ := recordingDirs(t)
	writeRecordingFile(t, root, "test-set-0/assets/body.txt", "kept beside")
	if err := os.MkdirAll(filepath.Join(root, "test-set-0", "assets", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "test-set-0/assets/deep/../body.txt"
	if b, err := ReadRecordingFile(root, name); err != nil || string(b) != "kept beside" {
		t.Fatalf("ReadRecordingFile(%q) = %q, %v; want the file", name, b, err)
	}
	f, err := OpenRecordingFile(root, name)
	if err != nil {
		t.Fatalf("OpenRecordingFile(%q): %v", name, err)
	}
	defer func() { _ = f.Close() }()
	if b, err := io.ReadAll(f); err != nil || string(b) != "kept beside" {
		t.Fatalf("OpenRecordingFile(%q) read %q, %v; want the file", name, b, err)
	}
}

// Reading a test case's file leaves no file open: neither the recording's
// directory nor the file.
func TestReadingATestCasesFileLeavesNothingOpen(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd to count open files")
	}
	root, _ := recordingDirs(t)
	writeRecordingFile(t, root, "test-set-0/assets/test-1/body.txt", "kept beside")
	open := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(ents)
	}
	before := open()
	for i := 0; i < 50; i++ {
		if b, err := ReadRecordingFile(root, "test-set-0/assets/test-1/body.txt"); err != nil || string(b) != "kept beside" {
			t.Fatalf("ReadRecordingFile: %q, %v", b, err)
		}
		if _, err := ReadRecordingFile(root, "../outside.txt"); err == nil {
			t.Fatal("ReadRecordingFile read a file out of the recording")
		}
		f, err := OpenRecordingFile(root, "test-set-0/assets/test-1/body.txt")
		if err != nil {
			t.Fatalf("OpenRecordingFile: %v", err)
		}
		// The file stays readable once its recording's directory is closed.
		if b, err := io.ReadAll(f); err != nil || string(b) != "kept beside" {
			t.Fatalf("read the opened file: %q, %v", b, err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenRecordingFile(root, "../outside.txt"); err == nil {
			t.Fatal("OpenRecordingFile opened a file out of the recording")
		}
	}
	if after := open(); after > before+5 {
		t.Fatalf("%d files open after 50 reads, %d before; want none left open", after, before)
	}
}

// A recording directory that cannot be opened is said so; the read neither
// panics nor reads anything.
func TestARecordingDirectoryThatCannotBeOpenedIsSaid(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	for _, read := range []func() error{
		func() error { _, err := ReadRecordingFile(gone, "body.txt"); return err },
		func() error { _, err := OpenRecordingFile(gone, "body.txt"); return err },
	} {
		err := func() (err error) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("the read panicked: %v", p)
				}
			}()
			return read()
		}()
		if err == nil || !strings.Contains(err.Error(), "open the recording directory") || !strings.Contains(err.Error(), "body.txt") {
			t.Fatalf("read in a directory that is not there: %v; want it said", err)
		}
	}
}

// The recording's directory is opened from the project: a keploy folder a
// cloned repository carries as a link out of the project, by ".." or to an
// absolute path, is refused with why, and no file a test case names is read
// from where it leads. A keploy folder that links inside the project is read.
func TestAKeployFolderThatLinksOutOfTheProjectIsNotReadFrom(t *testing.T) {
	const rel = "test-set-0/assets/test-1/body.txt"
	for _, how := range []string{"up", "absolute", "inside"} {
		base := t.TempDir()
		target := filepath.Join(base, "outside")
		if how == "inside" {
			target = filepath.Join(base, "project", "recordings")
		}
		writeAt := filepath.Join(target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(writeAt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(writeAt, []byte("SECRET-"+how), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(base, "project"), 0o755); err != nil {
			t.Fatal(err)
		}
		keploy := filepath.Join(base, "project", "keploy")
		to := target
		switch how {
		case "up":
			to = filepath.Join("..", "outside")
		case "inside":
			to = "recordings"
		}
		if err := os.Symlink(to, keploy); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		b, rerr := ReadRecordingFile(keploy, rel)
		f, oerr := OpenRecordingFile(keploy, rel)
		if f != nil {
			_ = f.Close()
		}
		if how == "inside" {
			if rerr != nil || oerr != nil || string(b) != "SECRET-inside" {
				t.Fatalf("a keploy folder linking inside the project: %q, %v, %v; want it read", b, rerr, oerr)
			}
			continue
		}
		for _, err := range []error{rerr, oerr} {
			if err == nil || !strings.Contains(err.Error(), "open the recording directory") || !strings.Contains(err.Error(), "only inside") {
				t.Fatalf("%s: a keploy folder that links out of the project: %v; want it refused with why", how, err)
			}
		}
		if len(b) != 0 {
			t.Fatalf("%s: read %q from outside the project", how, b)
		}
	}
}
