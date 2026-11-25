package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMultipartDuplicateFilenameSlotLinking(t *testing.T) {
	tests := []struct {
		name      string
		form      []models.FormData
		fileBytes []string
		wantBytes [][]string
	}{
		{
			name: "distinct fields",
			form: []models.FormData{
				{Key: "first", FileNames: []string{"upload.txt"}, Paths: []string{"/agent/tmp/first-placeholder.bin"}},
				{Key: "second", FileNames: []string{"upload.txt"}, Paths: []string{"/agent/tmp/second-placeholder.bin"}},
			},
			fileBytes: []string{"first field bytes", "second field bytes"},
			wantBytes: [][]string{{"first field bytes"}, {"second field bytes"}},
		},
		{
			name: "two files in one field",
			form: []models.FormData{
				{Key: "upload", FileNames: []string{"upload.txt", "upload.txt"}, Paths: []string{"/agent/tmp/first-placeholder.bin", "/agent/tmp/second-placeholder.bin"}},
			},
			fileBytes: []string{"first file bytes", "second file bytes"},
			wantBytes: [][]string{{"first file bytes", "second file bytes"}},
		},
		{
			name: "single file",
			form: []models.FormData{
				{Key: "upload", FileNames: []string{"upload.txt"}, Paths: []string{"/agent/tmp/single-placeholder.bin"}},
			},
			fileBytes: []string{"only file bytes"},
			wantBytes: [][]string{{"only file bytes"}},
		},
		{
			name: "fallback unmatched filename",
			form: []models.FormData{
				{Key: "upload", FileNames: []string{"mismatched.txt"}, Paths: []string{"/agent/tmp/fallback-placeholder.bin"}},
			},
			fileBytes: []string{"fallback file bytes"},
			wantBytes: [][]string{{"fallback file bytes"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)

			p, err := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": []string{`form-data; name="metadata"`}})
			if err != nil {
				t.Fatalf("create metadata part: %v", err)
			}
			if err := json.NewEncoder(p).Encode(&models.TestCase{
				Name:          "multipart-duplicate",
				Kind:          models.HTTP,
				HasBinaryFile: true,
				HTTPReq:       models.HTTPReq{Method: "POST", Form: tt.form},
			}); err != nil {
				t.Fatalf("encode metadata: %v", err)
			}

			for _, content := range tt.fileBytes {
				header := textproto.MIMEHeader{"Content-Disposition": []string{`form-data; name="file"; filename="upload.txt"`}}
				f, err := mw.CreatePart(header)
				if err != nil {
					t.Fatalf("create file part: %v", err)
				}
				if _, err := f.Write([]byte(content)); err != nil {
					t.Fatalf("write file part: %v", err)
				}
			}

			if _, err := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": []string{`form-data; name="delimiter"`}}); err != nil {
				t.Fatalf("create delimiter: %v", err)
			}
			if err := mw.Close(); err != nil {
				t.Fatalf("close multipart body: %v", err)
			}

			res := &http.Response{Body: io.NopCloser(bytes.NewReader(body.Bytes()))}
			tcChan := make(chan *models.TestCase, 1)
			ended := &atomic.Bool{}
			agent := &AgentClient{logger: zap.NewNop()}
			agent.readIncomingParts(context.Background(), res, mw.Boundary(), tcChan, ended, nil)

			tc := <-tcChan
			if tc == nil {
				t.Fatal("received nil test case")
			}
			if len(tc.HTTPReq.Form) != len(tt.form) {
				t.Fatalf("got %d form fields, want %d", len(tc.HTTPReq.Form), len(tt.form))
			}

			allPaths := make(map[string]bool)
			for i := range tc.HTTPReq.Form {
				form := tc.HTTPReq.Form[i]
				if len(form.Paths) != len(tt.form[i].Paths) {
					t.Fatalf("field %d got %d paths, want %d", i, len(form.Paths), len(tt.form[i].Paths))
				}
				for j, path := range form.Paths {
					t.Cleanup(func(path string) func() {
						return func() { _ = os.Remove(path) }
					}(path))
					if allPaths[path] {
						t.Fatalf("captured path %q was assigned to more than one file slot", path)
					}
					allPaths[path] = true

					b, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("read captured path %q: %v", path, err)
					}
					if string(b) != tt.wantBytes[i][j] {
						t.Fatalf("field %d path %d holds %q, want %q", i, j, b, tt.wantBytes[i][j])
					}
				}
			}
		})
	}
}
