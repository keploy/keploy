package mockdb

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A read gives each document its decoders skipped whose request time it can
// read as test windows see it (models.TestSetMocks.SkippedWindowed): its name,
// kind and times, and whether it is a test mock by the metadata every kind's
// spec carries. One whose time it cannot read is left out.
func TestAReadGivesTheWindowsOfTheDocumentsItSkipped(t *testing.T) {
	docs := map[string]string{
		"yaml": "---\nversion: api.keploy.io/v1beta1\nkind: Acme-Queue\nname: q-data\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.02Z\n" +
			"---\nversion: api.keploy.io/v1beta1\nkind: Acme-Queue\nname: q-hello\nspec:\n    metadata:\n        type: config\n    reqTimestampMock: 2026-10-08T11:12:00.03Z\n" +
			"---\nversion: api.keploy.io/v1beta1\nkind: Acme-Queue\nname: q-untimed\nspec:\n    metadata:\n        type: mocks\n",
		"json": `{"version":"api.keploy.io/v1beta1","kind":"Acme-Queue","name":"q-data","spec":{"metadata":{"type":"mocks"},"reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.02Z"}}` + "\n" +
			`{"version":"api.keploy.io/v1beta1","kind":"Acme-Queue","name":"q-hello","spec":{"metadata":{"type":"config"},"reqTimestampMock":"2026-10-08T11:12:00.03Z"}}` + "\n" +
			`{"version":"api.keploy.io/v1beta1","kind":"Acme-Queue","name":"q-untimed","spec":{"metadata":{"type":"mocks"}}}` + "\n",
	}
	at := func(ms int) time.Time { return time.Date(2026, 10, 8, 11, 12, 0, ms*int(time.Millisecond), time.UTC) }
	for format, raw := range docs {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, raw)
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
			set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Skipped) != 3 {
				t.Fatalf("Skipped = %v, want the three Acme-Queue documents", set.Skipped)
			}
			got := map[string]models.WindowedMock{}
			for _, w := range set.SkippedWindowed {
				got[w.Name] = w
			}
			if len(got) != 2 {
				t.Fatalf("SkippedWindowed = %v, want q-data and q-hello", set.SkippedWindowed)
			}
			if w := got["q-data"]; !w.TestMock || !w.At.Equal(at(10)) || !w.Res.Equal(at(20)) || w.Kind != "Acme-Queue" {
				t.Fatalf("q-data = %+v, want a test mock at 11:12:00.01", w)
			}
			if w := got["q-hello"]; w.TestMock || !w.At.Equal(at(30)) {
				t.Fatalf("q-hello = %+v, want a protocol-phase mock at 11:12:00.03", w)
			}
		})
	}
}

// Of a skipped document, window ownership reads an async-egress one (async is
// a key of the document) as no test mock, the lowercase timestamp keys older
// recordings write, metadata values that are not strings, and the whole lines
// of an incomplete last YAML document.
func TestSkippedWindowedReadsWhatEveryRecordingWrites(t *testing.T) {
	at := time.Date(2026, 10, 8, 11, 12, 0, 10*int(time.Millisecond), time.UTC)
	for _, tc := range []struct {
		name       string
		raw        string
		json       bool
		incomplete bool
		testMock   bool
	}{
		{"async yaml", "kind: Acme\nname: q\nasync:\n    lane: kafka\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n", false, false, false},
		{"async json", `{"kind":"Acme","name":"q","async":{"lane":"kafka"},"spec":{"metadata":{"type":"mocks"},"reqTimestampMock":"2026-10-08T11:12:00.01Z"}}`, true, false, false},
		{"lowercase keys", "kind: Acme\nname: q\nspec:\n    metadata:\n        type: mocks\n    reqtimestampmock: 2026-10-08T11:12:00.01Z\n", false, false, true},
		{"a number in the metadata", `{"kind":"Acme","name":"q","spec":{"metadata":{"type":"mocks","retries":3},"reqTimestampMock":"2026-10-08T11:12:00.01Z"}}`, true, false, true},
		{"incomplete last document", "kind: Acme\nname: q\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:1", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := skippedWindowed([]byte(tc.raw), tc.json, tc.incomplete, "q", "Acme")
			if !ok || !w.At.Equal(at) || w.TestMock != tc.testMock {
				t.Fatalf("got %+v ok %v, want at %v, test mock %v", w, ok, at, tc.testMock)
			}
		})
	}
}

// A test set Keploy Enterprise recorded holds Redis mocks this keploy cannot
// read: Keploy records and mocks Redis only in Keploy Enterprise, so reading
// them is expected, not an error. The read says so once for the set, at
// Warn, with how many and where to get Redis recorded and mocked, and logs
// no ERROR, however many Redis documents the set holds.
func TestAReadOfRedisMocksSaysOnceThatItReadsNone(t *testing.T) {
	redisDoc := map[string]func(name string) string{
		"yaml": func(name string) string {
			return "---\nversion: api.keploy.io/v1beta1\nkind: Redis\nname: " + name + "\nspec:\n    metadata:\n        type: mocks\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n"
		},
		"json": func(name string) string {
			return `{"version":"api.keploy.io/v1beta1","kind":"Redis","name":"` + name + `","spec":{"metadata":{"type":"mocks"},"reqTimestampMock":"2026-10-08T11:12:00.01Z"}}` + "\n"
		},
	}
	for format, doc := range redisDoc {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, doc("mock-r1")+doc("mock-r2")+doc("mock-r3"))
			core, logs := observer.New(zapcore.DebugLevel)
			if got := readSet(t, zap.New(core), dir, format); len(got) != 2 {
				t.Fatalf("want the two HTTP mocks read and the Redis ones skipped, read %d", len(got))
			}
			if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
				t.Fatalf("reading Redis mocks logged %d ERRORs, want none: %v", len(errs), errs)
			}
			warns := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("Redis").All()
			if len(warns) != 1 {
				t.Fatalf("want one warning for the set's Redis mocks, got %v", logs.All())
			}
			msg, f := warns[0].Message, warns[0].ContextMap()
			for _, want := range []string{pruneSet, "3 Redis mocks", "only in Keploy Enterprise", "none of them is replayed", models.RedisEnterpriseURL} {
				if !strings.Contains(msg, want) {
					t.Errorf("the warning must say %q; got %q", want, msg)
				}
			}
			if f["mocks"] != int64(3) || f["testSetID"] != pruneSet {
				t.Errorf("the warning's fields are %v, want mocks=3 and the set", f)
			}
		})
	}
}

// A build that reads Redis mocks (Keploy Enterprise, whose YAML mapper
// decodes them) says nothing of not reading them, of a json mock file's
// either, which the mapper does not read; and a Redis document the file ends
// part-way through is said as a cut document, not as a Redis mock this
// keploy does not read.
func TestAReadSaysNothingOfRedisMocksABuildThatReadsThemSkipped(t *testing.T) {
	redisJSON := func(name string) string {
		return `{"version":"api.keploy.io/v1beta1","kind":"Redis","name":"` + name + `","spec":{"metadata":{"type":"mocks"},"reqTimestampMock":"2026-10-08T11:12:00.01Z"}}` + "\n"
	}
	t.Run("a build with a Redis mapper", func(t *testing.T) {
		clearRegistry(t)
		RegisterMockYAMLMapper(models.REDIS, MockYAMLMapper{
			Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
			Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { return nil },
		})
		dir := t.TempDir()
		writePruneFixture(t, dir, "json", []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, redisJSON("mock-r1")+redisJSON("mock-r2"))
		core, logs := observer.New(zapcore.DebugLevel)
		if got := readSet(t, zap.New(core), dir, "json"); len(got) != 2 {
			t.Fatalf("precondition: want the two HTTP mocks read, read %d", len(got))
		}
		if said := logs.FilterMessageSnippet("this keploy does not read").All(); len(said) != 0 {
			t.Fatalf("a build that reads Redis mocks said it does not read them: %v", said)
		}
	})
	t.Run("a cut Redis document", func(t *testing.T) {
		dir := t.TempDir()
		path := writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, "")
		fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fh.WriteString("---\nversion: api.keploy.io/v1beta1\nkind: Redis\nname: mock-cut\nspec:\n    metadata:\n        type: moc"); err != nil {
			t.Fatal(err)
		}
		_ = fh.Close()
		core, logs := observer.New(zapcore.DebugLevel)
		_ = readSet(t, zap.New(core), dir, "yaml")
		if said := logs.FilterMessageSnippet("this keploy does not read").All(); len(said) != 0 {
			t.Fatalf("a cut Redis document was said as a Redis mock this keploy does not read: %v", said)
		}
		cut := 0
		for _, e := range incompleteWarnings(logs) {
			if e.ContextMap()["mock"] == "mock-cut" {
				cut++
			}
		}
		if cut != 1 {
			t.Fatalf("precondition: the cut Redis document was said cut %d times, want once: %v", cut, logs.All())
		}
	})
}
