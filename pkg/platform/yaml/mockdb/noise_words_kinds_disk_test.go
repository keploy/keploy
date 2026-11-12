package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// mongoWordsMock is a Mongo recording of an insert whose note is a text of
// three words; the Mongo learn step names its places body.<path>.
func mongoWordsMock() *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			Metadata: map[string]string{"operation": "OP_MSG"},
			MongoRequests: []models.MongoRequest{{
				Header:  &models.MongoHeader{Length: 80, RequestID: 1, Opcode: 2013},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"insert":"leads","note":"lead r-1 created"} }`}},
			}},
			MongoResponses: []models.MongoResponse{{
				Header:  &models.MongoHeader{Length: 60, RequestID: 2, ResponseTo: 1, Opcode: 2013},
				Message: &models.MongoOpMessage{Sections: []string{`{ SectionSingle msg: {"ok":1} }`}},
			}},
			ReqTimestampMock: time.Date(2026, 9, 30, 8, 42, 54, 0, time.UTC),
			ResTimestampMock: time.Date(2026, 9, 30, 8, 42, 55, 0, time.UTC),
		},
	}
}

// postgresWordsMock is a Postgres recording of a statement with a text of
// three words at its first bind and a stamp at its second; the Postgres
// learn step names those places bind.$1 and bind.$2.
func postgresWordsMock() *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks", "class": "APP"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					Class:         "APP",
					Lifetime:      "perTest",
					SQLAstHash:    "sha256:abcd",
					SQLNormalized: "INSERT INTO notes (note, at) VALUES ($1, $2)",
					InvocationID:  "0:7",
					ParamOIDs:     []uint32{25, 25},
					BindValues:    models.PostgresV3Cells{models.NewValueCell("lead r-1 created"), models.NewValueCell("2026-09-30T08:42:54Z")},
					Response:      &models.PostgresV3Response{CommandComplete: "INSERT 0 1"},
				},
			},
		},
	}
}

// readMockZero is mock-0 of the set as a replay reads it, from either pool, and
// the mock file's text.
func readMockZero(t *testing.T, ys *MockYaml, set string, f yaml.Format) (*models.Mock, string) {
	t.Helper()
	ctx := context.Background()
	end := models.BaseTime.AddDate(100, 0, 0)
	session, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, end, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	perTest, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, end, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ys.MockPath, set, "mocks."+f.FileExtension()))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range append(session, perTest...) {
		if m.Name == "mock-0" {
			return m, string(data)
		}
	}
	t.Fatalf("mock-0 not read back:\n%s", data)
	return nil, ""
}

// A Mongo and a Postgres mock's text learned by its words reach the mock file
// under noise.words, never under noise.req, and read back as the same words
// pattern, in YAML and JSON, whatever the proven switch: read back as noise
// whole, the next keploy test would leave every word of the text unchecked.
// A second learning that finds another word changed adds it to the file's.
func TestAMongoAndAPostgresTextsWordsSurviveTheDisk(t *testing.T) {
	kinds := []struct {
		name  string
		mock  func() *models.Mock
		first map[string][]string
		then  map[string][]string
		want  map[string][]string
		inReq string
	}{
		{
			name:  "mongo",
			mock:  mongoWordsMock,
			first: map[string][]string{"body.cmd.note": {"^keploy:words 1$"}, "body.cmd.lsid": {}},
			then:  map[string][]string{"body.cmd.note": {"^keploy:words 2$"}},
			want:  map[string][]string{"body.cmd.note": {"^keploy:words 1,2$"}, "body.cmd.lsid": {}},
			inReq: "body.cmd.lsid",
		},
		{
			name:  "postgres",
			mock:  postgresWordsMock,
			first: map[string][]string{"bind.$1": {"^keploy:words 1$"}, "bind.$2": {}},
			then:  map[string][]string{"bind.$1": {"^keploy:words 2$"}},
			want:  map[string][]string{"bind.$1": {"^keploy:words 1,2$"}, "bind.$2": {}},
			inReq: "bind.$2",
		},
	}
	for _, k := range kinds {
		for _, proven := range []string{"1", "0"} {
			for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
				t.Run(k.name+"/"+proven+"/"+string(f), func(t *testing.T) {
					t.Setenv(provenswitch.Env, proven)
					ctx := context.Background()
					ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", f)
					set := "test-set-0"
					if err := ys.InsertMock(ctx, k.mock(), set); err != nil {
						t.Fatal(err)
					}
					if err := ys.Close(); err != nil {
						t.Fatal(err)
					}
					if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: k.first}}); err != nil {
						t.Fatal(err)
					}
					m, file := readMockZero(t, ys, set, f)
					if !reflect.DeepEqual(m.Spec.ReqBodyNoise, k.first) {
						t.Fatalf("read back %#v, want %#v (a words place read as noise whole leaves the text unchecked)\n%s", m.Spec.ReqBodyNoise, k.first, file)
					}
					doc := fileNoise(t, file, f)
					if !reflect.DeepEqual(doc.Req, []string{k.inReq}) {
						t.Fatalf("noise.req is %q, want only %q: a words place under req is noise whole to an older keploy\n%s", doc.Req, k.inReq, file)
					}
					for path, patterns := range k.first {
						if path == k.inReq {
							continue
						}
						if _, ok := doc.Words[path]; !ok {
							t.Fatalf("noise.words has no %q (patterns %q)\n%s", path, patterns, file)
						}
					}
					if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: k.then}}); err != nil {
						t.Fatal(err)
					}
					m, file = readMockZero(t, ys, set, f)
					if !reflect.DeepEqual(m.Spec.ReqBodyNoise, k.want) {
						t.Fatalf("second write read back %#v, want %#v (the union of the words)\n%s", m.Spec.ReqBodyNoise, k.want, file)
					}
				})
			}
		}
	}
}

// fileNoise is the one noise block of a mock file holding one mock.
func fileNoise(t *testing.T, file string, f yaml.Format) *yaml.DocNoise {
	t.Helper()
	var docs []*yaml.NetworkTrafficDoc
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mocks."+f.FileExtension()), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := yaml.NewMockReaderF(context.Background(), zap.NewNop(), dir, "mocks", f)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for {
		doc, err := reader.ReadNextDoc()
		if err != nil {
			break
		}
		docs = append(docs, doc)
	}
	if len(docs) != 1 || docs[0].Noise == nil {
		t.Fatalf("want one mock with a noise block, got %d docs:\n%s", len(docs), file)
	}
	if strings.Contains(file, "req_body_noise") {
		t.Fatalf("the legacy req_body_noise key is written:\n%s", file)
	}
	return docs[0].Noise
}
