package mockdb

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
