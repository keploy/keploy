package mockdb

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func copyRowMock(url string, at time.Time) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "connection", "connID": "c1"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: url, Timestamp: at},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: "name: not-the-name\n---\n" + strings.Repeat("y", 300), Timestamp: at.Add(time.Millisecond)},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
	}
}

// Mocks copied from one test set into another, as a recording that rolls
// over copies them (record.setRoll), read back the same: the same names, the
// same specs, in either format. Only the named documents are copied, a body
// that holds a "name:" line or a separator does not confuse the read, and a
// name the set does not hold is not returned.
func TestCopiedMocksReadBackTheSame(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)
			at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
			var names []string
			for i, url := range []string{"http://a/1", "http://a/2", "http://a/3"} {
				m := copyRowMock(url, at.Add(time.Duration(i)*time.Second))
				if err := ys.InsertMock(ctx, m, "from"); err != nil {
					t.Fatal(err)
				}
				names = append(names, m.Name)
			}
			// The second set has a mock of its own first.
			if err := ys.InsertMock(ctx, copyRowMock("http://b/own", at), "to"); err != nil {
				t.Fatal(err)
			}
			want := map[string]struct{}{names[0]: {}, names[2]: {}, "mock-404": {}}
			docs, got, err := ys.ReadMockDocs(ctx, "from", want)
			if err != nil {
				t.Fatal(err)
			}
			if got != format.FileExtension() {
				t.Fatalf("format %q; want %q", got, format.FileExtension())
			}
			if len(docs) != 2 || docs[names[0]] == nil || docs[names[2]] == nil {
				t.Fatalf("read %d documents; want %s and %s", len(docs), names[0], names[2])
			}
			n, err := ys.AppendMockDocs(ctx, "to", got, [][]byte{docs[names[0]], docs[names[2]]})
			if err != nil || n == 0 {
				t.Fatalf("append: %d bytes, %v", n, err)
			}
			from, err := ys.GetUnFilteredMocks(ctx, "from", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			to, err := ys.GetUnFilteredMocks(ctx, "to", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			byName := func(ms []*models.Mock) map[string]*models.Mock {
				out := map[string]*models.Mock{}
				for _, m := range ms {
					out[m.Name] = m
				}
				return out
			}
			f, tt := byName(from), byName(to)
			if len(tt) != 3 {
				t.Fatalf("the second set holds %d mocks; want its own and the two copied", len(tt))
			}
			for _, name := range []string{names[0], names[2]} {
				if tt[name] == nil {
					t.Fatalf("%s was not copied", name)
				}
				if !reflect.DeepEqual(f[name].Spec, tt[name].Spec) {
					t.Fatalf("%s reads back otherwise after the copy:\n%+v\nvs\n%+v", name, f[name].Spec, tt[name].Spec)
				}
			}
		})
	}
}

// Copying into a set whose mock file is of the other format is refused: the
// documents would not read back.
func TestCopiedMocksRefuseAnotherFormat(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	if err := ys.InsertMock(ctx, copyRowMock("http://a/1", time.Now()), "to"); err != nil {
		t.Fatal(err)
	}
	if _, err := ys.AppendMockDocs(ctx, "to", "json", [][]byte{[]byte(`{"name":"mock-9"}`)}); err == nil {
		t.Fatal("a json document was appended to a yaml mock file")
	}
	_, _, err := ys.ReadMockDocs(ctx, "absent", map[string]struct{}{"mock-1": {}})
	if err == nil || errors.Is(err, ErrMockDocsGob) {
		t.Fatalf("reading a set with no mock file: %v", err)
	}
}
