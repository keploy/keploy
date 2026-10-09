package mockdb

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	yamlLib "gopkg.in/yaml.v3"
)

var cfT0 = time.Date(2026, 10, 8, 10, 21, 46, 511876000, time.UTC)

func cfMock(i int, phase models.ConnFailurePhase, outcome models.ConnFailureOutcome) *models.Mock {
	ts := cfT0.Add(time.Duration(i) * time.Second)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.ConnectionFailure,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			ConnFailure: &models.ConnFailureSpec{
				Address: "[2001:db8::10]:443",
				Host:    "api.example.com",
				Phase:   phase,
				Outcome: outcome,
				Cause:   "dial tcp [2001:db8::10]:443: connect: network is unreachable",
			},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(2 * time.Millisecond),
		},
	}
}

// cfCorpus is one connection failure of every phase/outcome pair the format
// has, then an ordinary mock.
func cfCorpus() []*models.Mock {
	return []*models.Mock{
		cfMock(0, models.ConnFailurePhaseConnect, models.ConnFailureRefused),
		cfMock(1, models.ConnFailurePhaseConnect, models.ConnFailureHostUnreachable),
		cfMock(2, models.ConnFailurePhaseConnect, models.ConnFailureNetUnreachable),
		cfMock(3, models.ConnFailurePhaseConnect, models.ConnFailureTimeout),
		cfMock(4, models.ConnFailurePhaseAccepted, models.ConnFailureClosed),
		cfMock(5, models.ConnFailurePhaseTLS, models.ConnFailureClosed),
		cfMock(6, models.ConnFailurePhaseRequest, models.ConnFailureClosed),
		pruneHTTPMock(7),
	}
}

func formatOf(format string) yaml.Format {
	if format == "json" {
		return yaml.FormatJSON
	}
	return yaml.FormatYAML
}

// readSet reads a test set's mocks back as a replay does, every pool.
func readSet(t *testing.T, logger *zap.Logger, dir, format string) []*models.Mock {
	t.Helper()
	ys := NewWithFormat(logger, dir, "mocks", formatOf(format))
	set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil)
	if err != nil {
		t.Fatalf("GetTestSetMocks: %v", err)
	}
	return append(append([]*models.Mock(nil), set.AllPerTest...), set.AllSession...)
}

func byName(mocks []*models.Mock) map[string]*models.Mock {
	out := map[string]*models.Mock{}
	for _, m := range mocks {
		out[m.Name] = m
	}
	return out
}

// A connection failure written in any storage format reads back as written,
// as a per-test mock.
func TestConnectionFailureRoundTripsThroughYAMLAndJSON(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, cfCorpus(), "")
			got := byName(readSet(t, zap.NewNop(), dir, format))
			for i, want := range cfCorpus() {
				name := "mock-" + string(rune('0'+i))
				m, ok := got[name]
				if !ok {
					t.Fatalf("%s (%s) did not read back; read %d mocks", name, want.Kind, len(got))
				}
				if want.Kind != models.ConnectionFailure {
					continue
				}
				if m.Kind != models.ConnectionFailure || m.Spec.ConnFailure == nil {
					t.Fatalf("%s read back as kind %q with spec %+v", name, m.Kind, m.Spec.ConnFailure)
				}
				if *m.Spec.ConnFailure != *want.Spec.ConnFailure {
					t.Fatalf("%s spec = %+v, want %+v", name, *m.Spec.ConnFailure, *want.Spec.ConnFailure)
				}
				if !m.Spec.ReqTimestampMock.Equal(want.Spec.ReqTimestampMock) || !m.Spec.ResTimestampMock.Equal(want.Spec.ResTimestampMock) {
					t.Fatalf("%s times = %v/%v, want %v/%v", name, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, want.Spec.ReqTimestampMock, want.Spec.ResTimestampMock)
				}
				if !reflect.DeepEqual(m.Spec.Metadata, want.Spec.Metadata) {
					t.Fatalf("%s metadata = %v, want %v", name, m.Spec.Metadata, want.Spec.Metadata)
				}
				if m.TestModeInfo.Lifetime != models.LifetimePerTest {
					t.Fatalf("%s lifetime = %v, want per-test", name, m.TestModeInfo.Lifetime)
				}
			}

			// The on-disk shape is the contract with other keploy versions.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"kind: ConnectionFailure", "address: '[2001:db8::10]:443'", "host: api.example.com", "phase: tls", "outcome: closed", "cause: 'dial tcp [2001:db8::10]:443: connect: network is unreachable'", "reqTimestampMock: 2026-10-08T10:21:46.511876Z"}
			if format == "json" {
				want = []string{`"kind":"ConnectionFailure"`, `"address":"[2001:db8::10]:443"`, `"host":"api.example.com"`, `"phase":"tls"`, `"outcome":"closed"`, `"cause":"dial tcp [2001:db8::10]:443: connect: network is unreachable"`, `"reqTimestampMock":"2026-10-08T10:21:46.511876Z"`}
			}
			for _, w := range want {
				if !strings.Contains(string(data), w) {
					t.Fatalf("the %s file does not hold %q:\n%s", format, w, data)
				}
			}
		})
	}
}

func TestConnectionFailureRoundTripsThroughGob(t *testing.T) {
	roundTrip(t, "ConnectionFailure", cfMock(2, models.ConnFailurePhaseRequest, models.ConnFailureClosed))
}

// cfDocYAML is a connection failure as another keploy may have written it.
func cfDocYAML(name, phase, outcome string, timed bool) string {
	d := "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: " + name + "\nspec:\n    address: 10.0.3.7:443\n    phase: " + phase + "\n    outcome: " + outcome + "\n"
	if timed {
		d += "    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n"
	}
	return d
}

func cfDocJSON(name, phase, outcome string, timed bool) string {
	d := `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"` + name + `","spec":{"address":"10.0.3.7:443","phase":"` + phase + `","outcome":"` + outcome + `"`
	if timed {
		d += `,"reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"`
	}
	return d + "}}"
}

// A connection failure this keploy cannot replay — a value a newer keploy
// wrote, or a malformed one — is skipped with an ERROR that says why, the way
// a mock of an unknown kind is, and every other mock still loads.
func TestAConnectionFailureThisKeployCannotReplayIsSkippedLoudly(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			raw := "---\n" + cfDocYAML("cf-newer", "request", "reset", true) +
				"---\n" + cfDocYAML("cf-untimed", "connect", "refused", false) +
				"---\n" + cfDocYAML("cf-ok", "connect", "refused", true)
			if format == "json" {
				raw = cfDocJSON("cf-newer", "request", "reset", true) + "\n" +
					cfDocJSON("cf-untimed", "connect", "refused", false) + "\n" +
					cfDocJSON("cf-ok", "connect", "refused", true) + "\n"
			}
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, raw)

			core, logs := observer.New(zapcore.DebugLevel)
			got := byName(readSet(t, zap.New(core), dir, format))
			if _, ok := got["cf-ok"]; !ok || len(got) != 3 {
				t.Fatalf("want the valid connection failure and both HTTP mocks, got %v", keysOf(got))
			}
			for _, skipped := range []string{"cf-newer", "cf-untimed"} {
				if _, ok := got[skipped]; ok {
					t.Fatalf("%s must be skipped", skipped)
				}
			}
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			if len(errs) != 2 {
				t.Fatalf("want one ERROR per skipped document, got %d: %v", len(errs), errs)
			}
			want := map[string]string{
				"cf-newer":   `the connection failure uses outcome "reset", which this keploy does not support; upgrade keploy`,
				"cf-untimed": "cannot be placed in the test that saw it",
			}
			for _, e := range errs {
				fields := e.ContextMap()
				name, _ := fields["mock"].(string)
				msg, _ := fields["error"].(string)
				if w, ok := want[name]; !ok || !strings.Contains(msg, w) {
					t.Fatalf("ERROR for mock %q says %q; want it to say %q", name, msg, want[name])
				}
				if _, ok := fields["keploy_version"]; !ok {
					t.Fatalf("the ERROR must name this keploy's version: %v", fields)
				}
				if fields["mock_file"] != path {
					t.Fatalf("the ERROR must name the mock file %s, since mock names repeat in every test set: %v", path, fields)
				}
			}
			// The set's WARN counts the one it read, not the two it skipped.
			if ws := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("connection-failure").All(); len(ws) != 1 || !strings.HasPrefix(ws[0].Message, "1 connection-failure mock in ") {
				t.Fatalf("want one WARN counting the one connection failure read, got %v", ws)
			}
		})
	}
}

// gob has no per-kind decoder; the read path validates a connection failure
// itself. The file is written with a bare encoder, as an older or newer
// keploy's writer would, since this keploy's writer refuses such a mock.
func TestAConnectionFailureThisKeployCannotReplayIsSkippedLoudlyFromGob(t *testing.T) {
	newer := cfMock(0, models.ConnFailurePhaseRequest, "reset")
	newer.Name = "cf-newer"
	untimed := cfMock(1, models.ConnFailurePhaseConnect, models.ConnFailureRefused)
	untimed.Name, untimed.Spec.ReqTimestampMock = "cf-untimed", time.Time{}
	ok := cfMock(2, models.ConnFailurePhaseConnect, models.ConnFailureRefused)
	ok.Name = "cf-ok"
	h1, h2 := pruneHTTPMock(1), pruneHTTPMock(2)
	h1.Name, h2.Name = "mock-1", "mock-2"

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, pruneSet), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(gobMockMagic)
	enc := gob.NewEncoder(&buf)
	for _, m := range []*models.Mock{h1, newer, untimed, ok, h2} {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, pruneSet, "mocks.gob"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	got := byName(readSet(t, zap.New(core), dir, "yaml"))
	if _, found := got["cf-ok"]; !found || len(got) != 3 {
		t.Fatalf("want the valid connection failure and both HTTP mocks, got %v", keysOf(got))
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 2 {
		t.Fatalf("want one ERROR per skipped mock, got %d: %v", len(errs), errs)
	}
	gobSet, err := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML).GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[string]models.Kind{"cf-newer": models.ConnectionFailure, "cf-untimed": models.ConnectionFailure}); !reflect.DeepEqual(gobSet.Skipped, want) {
		t.Fatalf("Skipped = %v, want %v", gobSet.Skipped, want)
	}
	gobPath := filepath.Join(dir, pruneSet, "mocks.gob")
	for _, e := range errs {
		if n := e.ContextMap()["mock"]; n != "cf-newer" && n != "cf-untimed" {
			t.Fatalf("unexpected ERROR for %v: %v", n, e.ContextMap())
		}
		if f := e.ContextMap()["mock_file"]; f != gobPath {
			t.Fatalf("the ERROR must name the mock file %s: %v", gobPath, e.ContextMap())
		}
	}
	if ws := logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("connection-failure").All(); len(ws) != 1 || !strings.HasPrefix(ws[0].Message, "1 connection-failure mock in ") {
		t.Fatalf("want one WARN counting the one connection failure read, got %v", ws)
	}
}

func keysOf(m map[string]*models.Mock) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The encoders refuse what the decoders would skip, so a recording never
// holds a connection failure no keploy can replay.
func TestEncodersRefuseAnInvalidConnectionFailure(t *testing.T) {
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			if format == "gob" {
				t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
			}
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
			bad := cfMock(0, models.ConnFailurePhaseConnect, "reset")
			err := ys.InsertMock(context.Background(), bad, pruneSet)
			if !errors.Is(err, models.ErrMockEncode) || !strings.Contains(err.Error(), "upgrade keploy") {
				t.Fatalf("InsertMock = %v, want a skippable ErrMockEncode naming the value", err)
			}
			untimed := cfMock(0, models.ConnFailurePhaseConnect, models.ConnFailureRefused)
			untimed.Spec.ReqTimestampMock = time.Time{}
			if err := ys.InsertMock(context.Background(), untimed, pruneSet); !errors.Is(err, models.ErrMockEncode) {
				t.Fatalf("InsertMock of an untimed connection failure = %v, want ErrMockEncode", err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			if got := readSetOrEmpty(t, dir, format); len(got) != 0 {
				t.Fatalf("nothing should have been written, read back %d mocks", len(got))
			}
		})
	}
}

func readSetOrEmpty(t *testing.T, dir, format string) []*models.Mock {
	t.Helper()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
	set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		return nil
	}
	return append(set.AllPerTest, set.AllSession...)
}

// A mock of a kind this keploy cannot read says which kind, which mock in
// which file, which keploy read it, and what to do. Which of three causes it
// is, this keploy cannot tell (a newer keploy's kind, an enterprise-only kind,
// or a legacy kind like the v1 Postgres and SQL mocks in old recordings), so
// it names all three rather than telling a user with an old recording to
// upgrade.
func TestUnknownKindErrorNamesTheVersionAndEveryWayOut(t *testing.T) {
	old := utils.Version
	utils.Version = "v3.6.999"
	t.Cleanup(func() { utils.Version = old })

	for _, kind := range []string{"FutureKind", "SQL", "Postgres"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)},
				"---\nversion: api.keploy.io/v1beta1\nkind: "+kind+"\nname: mock-9\nspec: {a: 1}\n")
			core, logs := observer.New(zapcore.DebugLevel)
			if got := readSet(t, zap.New(core), dir, "yaml"); len(got) != 2 {
				t.Fatalf("want the two HTTP mocks and the %s document skipped, read %d", kind, len(got))
			}
			errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
			if len(errs) != 1 {
				t.Fatalf("want one ERROR, got %v", logs.All())
			}
			msg, f := errs[0].Message, errs[0].ContextMap()
			for _, want := range []string{`mock kind "` + kind + `"`, "newer keploy", "keploy enterprise", "no longer reads"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("the ERROR must say %q; got %q", want, msg)
				}
			}
			if next, _ := f["next_step"].(string); !strings.Contains(next, "upgrade keploy") || !strings.Contains(next, "enterprise") || !strings.Contains(next, "re-record") {
				t.Fatalf("the ERROR must name every way out; next_step = %q", next)
			}
			if f["kind"] != kind || f["mock"] != "mock-9" || f["keploy_version"] != "v3.6.999" || f["mock_file"] != path {
				t.Fatalf("the ERROR must name the kind, the mock, the mock file and this keploy's version; got %v", f)
			}
		})
	}

	utils.Version = ""
	if v := keployVersion(); v == "" || !strings.Contains(v, "development") {
		t.Fatalf("a build without a version must say so, got %q", v)
	}
}

// Why ConnectionFailure has no hyphen. A keploy too old to know the kind
// treats it as it treats any kind it does not know, and which path that is
// depends on the spelling alone: a hyphenated kind is skipped at Debug, as
// "enterprise-only", before the unknown-kind branch; an unhyphenated one
// reaches that branch and logs an ERROR naming it. The released readers have
// the same two paths (v3.6.107 mockdb/util.go:666-670 and :826-828).
func TestConnectionFailureKindReachesTheUnknownKindError(t *testing.T) {
	if strings.Contains(string(models.ConnectionFailure), "-") {
		t.Fatalf("%q has a hyphen: a keploy that cannot read it would drop it at Debug level, silently", models.ConnectionFailure)
	}
	decode := func(kind string) []observer.LoggedEntry {
		core, logs := observer.New(zapcore.DebugLevel)
		var doc yaml.NetworkTrafficDoc
		if err := yamlLib.Unmarshal([]byte("version: api.keploy.io/v1beta1\nkind: "+kind+"\nname: m\nspec: {a: 1}\n"), &doc); err != nil {
			t.Fatal(err)
		}
		if mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.New(core)); err != nil || len(mocks) != 0 {
			t.Fatalf("decode %s: %d mocks, %v", kind, len(mocks), err)
		}
		return logs.FilterLevelExact(zapcore.ErrorLevel).All()
	}
	if errs := decode("Connection-Failure-v2"); len(errs) != 0 {
		t.Fatalf("precondition: a hyphenated unknown kind is skipped without an ERROR; got %v", errs)
	}
	if errs := decode("ConnectionFailureV2"); len(errs) != 1 || errs[0].ContextMap()["kind"] != "ConnectionFailureV2" {
		t.Fatalf("an unhyphenated unknown kind must log one ERROR naming it; got %v", errs)
	}
}

// The connection-failure format belongs to keploy: an out-of-tree mapper must
// not be able to reshape it.
func TestAMapperCannotShadowTheConnectionFailureFormat(t *testing.T) {
	clearRegistry(t)
	RegisterMockYAMLMapper(models.ConnectionFailure, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(*yaml.NetworkTrafficDoc, *models.Mock) error { return nil },
	})
	if hasMapperForKind(models.ConnectionFailure) {
		t.Fatal("a mapper was registered for ConnectionFailure")
	}
}

// The prune drops what no test consumed, and nothing consumes a connection
// failure at replay yet: pruning one would delete it from a recording made by
// a keploy that replays it.
func TestThePruneKeepsConnectionFailures(t *testing.T) {
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, cfCorpus(), "")
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
			// Nothing consumed, the replay started after every mock, no
			// startup cutoff: every ordinary per-test mock goes.
			if err := ys.UpdateMocks(context.Background(), pruneSet, nil, cfT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatalf("UpdateMocks: %v", err)
			}
			var got []*models.Mock
			if format == "gob" {
				var err error
				if got, err = readGobMocks(filepath.Join(dir, pruneSet, "mocks.gob")); err != nil {
					t.Fatal(err)
				}
			} else {
				got = readSet(t, zap.NewNop(), dir, format)
			}
			kinds := map[models.Kind]int{}
			for _, m := range got {
				kinds[m.Kind]++
			}
			if kinds[models.ConnectionFailure] != 7 || kinds[models.HTTP] != 0 {
				t.Fatalf("after the prune: %v; want the 7 connection failures kept and the unconsumed HTTP mock pruned", kinds)
			}
		})
	}
}

// cfNewerFormat is a connection failure in a format this keploy does not
// have, as a newer keploy may write it (or a person may edit it): a field the
// format lacks (count, and acceptedBefore, which the first replayer may add),
// or a field whose type changed. Each is skipped with one ERROR that says so
// and says to upgrade, the rest of the set still loads, and a rewrite keeps
// the document exactly as it was.
var cfNewerFormat = []struct {
	name, yaml, json, want string
}{
	{
		name: "unknown field",
		yaml: "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-newer\nspec:\n    address: 10.0.3.7:443\n    phase: connect\n    outcome: refused\n    count: 3\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n",
		json: `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-newer","spec":{"address":"10.0.3.7:443","phase":"connect","outcome":"refused","count":3,"reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`,
		want: `the connection failure uses field "count", which this keploy does not support; upgrade keploy`,
	},
	{
		name: "a field the replayer adds",
		yaml: "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-newer\nspec:\n    address: 10.0.3.7:443\n    phase: connect\n    outcome: refused\n    acceptedBefore: 2\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n",
		json: `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-newer","spec":{"acceptedBefore":2,"address":"10.0.3.7:443","phase":"connect","outcome":"refused","reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`,
		want: `the connection failure uses field "acceptedBefore", which this keploy does not support; upgrade keploy`,
	},
	{
		name: "a field name in another case",
		yaml: "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-newer\nspec:\n    Address: 10.0.3.7:443\n    phase: connect\n    outcome: refused\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n",
		json: `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-newer","spec":{"Address":"10.0.3.7:443","phase":"connect","outcome":"refused","reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`,
		want: `the connection failure uses field "Address", which this keploy does not support; upgrade keploy`,
	},
	{
		name: "a field whose type changed",
		yaml: "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-newer\nspec:\n    address: 10.0.3.7:443\n    phase: connect\n    outcome: refused\n    cause: {errno: 111, text: refused}\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n",
		json: `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-newer","spec":{"address":"10.0.3.7:443","phase":"connect","outcome":"refused","cause":{"errno":111,"text":"refused"},"reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`,
		want: "does not decode as this keploy's format, so it was written by a newer keploy or edited by hand",
	},
	{
		name: "a time that is not a time",
		yaml: "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-newer\nspec:\n    address: 10.0.3.7:443\n    phase: connect\n    outcome: refused\n    reqTimestampMock: yesterday\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n",
		json: `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-newer","spec":{"address":"10.0.3.7:443","phase":"connect","outcome":"refused","reqTimestampMock":"yesterday","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`,
		want: "does not decode as this keploy's format, so it was written by a newer keploy or edited by hand",
	},
}

// cfRaw is doc as writePruneFixture appends it to a mock file.
func cfRaw(format, doc string) string {
	if format == "json" {
		return doc + "\n"
	}
	return "---\n" + doc
}

// assertDocVerbatim fails unless doc is in the mock file at path exactly
// once, byte for byte, as a document (YAML) or a line (JSON) of its own.
func assertDocVerbatim(t *testing.T, path, format, doc string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the rewritten mock file: %v", err)
	}
	var parts []string
	if format == "json" {
		parts = strings.Split(string(data), "\n")
	} else {
		parts = yamlDocuments(string(data))
	}
	n := 0
	for _, p := range parts {
		if p == doc {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the rewrite did not keep the document as written, exactly once (found %d).\nwant:\n%s\nfile:\n%s", n, doc, data)
	}
}

func TestAConnectionFailureInANewerFormatIsSkippedAndKeptAsWritten(t *testing.T) {
	old := utils.Version
	utils.Version = "v3.6.999"
	t.Cleanup(func() { utils.Version = old })

	for _, tc := range cfNewerFormat {
		for _, format := range []string{"yaml", "json"} {
			doc := tc.yaml
			if format == "json" {
				doc = tc.json
			}
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				// The read: the document is skipped, loudly, and the set loads.
				dir := t.TempDir()
				path := writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, cfRaw(format, doc))
				core, logs := observer.New(zapcore.DebugLevel)
				got := byName(readSet(t, zap.New(core), dir, format))
				if _, ok := got["cf-newer"]; ok || len(got) != 2 {
					t.Fatalf("want the two HTTP mocks and the connection failure skipped, read %v", keysOf(got))
				}
				errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
				if len(errs) != 1 {
					t.Fatalf("want one ERROR for the skipped document, got %d: %v", len(errs), errs)
				}
				f := errs[0].ContextMap()
				msg, _ := f["error"].(string)
				if !strings.Contains(msg, tc.want) || !strings.Contains(msg, "upgrade keploy") {
					t.Fatalf("the ERROR says %q; want it to say %q and to upgrade keploy", msg, tc.want)
				}
				if f["mock"] != "cf-newer" || f["keploy_version"] != "v3.6.999" || f["mock_file"] != path {
					t.Fatalf("the ERROR must name the mock, its file and this keploy's version: %v", f)
				}

				// The prune and the noise write-back keep it as written.
				ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
				if err := ys.UpdateMocks(context.Background(), pruneSet, map[string]models.MockState{"mock-1": {Name: "mock-1"}}, pruneT0.Add(time.Hour), time.Time{}); err != nil {
					t.Fatalf("UpdateMocks: %v", err)
				}
				assertDocVerbatim(t, path, format, doc)

				dir = t.TempDir()
				path = writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, cfRaw(format, doc))
				ys = NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
				states := map[string]models.MockState{"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.request_id": {}}}}
				if err := ys.PersistMockNoise(context.Background(), pruneSet, states); err != nil {
					t.Fatalf("PersistMockNoise: %v", err)
				}
				if data, _ := os.ReadFile(path); !strings.Contains(string(data), "body.request_id") {
					t.Fatalf("precondition: the write-back did not rewrite the file:\n%s", data)
				}
				assertDocVerbatim(t, path, format, doc)
			})
		}
	}
}

// A connection failure this keploy reads is still copied through a rewrite as
// written, not re-encoded: nothing in this keploy changes one, and a re-encode
// would respell another keploy's document in this keploy's way. The quoting,
// the key order and the comment survive only a copy.
const (
	cfSpelledYAML = "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-spelled\nspec:\n    phase: connect   # the step that failed\n    outcome: \"refused\"\n    address: \"10.0.3.7:443\"\n    reqTimestampMock: 2026-10-08T11:12:00.01Z\n    resTimestampMock: 2026-10-08T11:12:00.041Z\n"
	cfSpelledJSON = `{"kind":"ConnectionFailure","version":"api.keploy.io/v1beta1","name":"cf-spelled","spec":{"phase":"connect", "outcome":"refused","address":"10.0.3.7:443","reqTimestampMock":"2026-10-08T11:12:00.01Z","resTimestampMock":"2026-10-08T11:12:00.041Z"}}`
)

func TestTheRewriteCopiesAConnectionFailureAsWritten(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		doc := cfSpelledYAML
		if format == "json" {
			doc = cfSpelledJSON
		}
		for _, op := range []string{"prune", "noise write-back"} {
			t.Run(format+"/"+op, func(t *testing.T) {
				dir := t.TempDir()
				path := writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, cfRaw(format, doc))
				if m, ok := byName(readSet(t, zap.NewNop(), dir, format))["cf-spelled"]; !ok || m.Kind != models.ConnectionFailure {
					t.Fatal("precondition: this keploy reads the connection failure")
				}
				ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
				if op == "prune" {
					if err := ys.UpdateMocks(context.Background(), pruneSet, nil, pruneT0.Add(time.Hour), time.Time{}); err != nil {
						t.Fatalf("UpdateMocks: %v", err)
					}
				} else {
					states := map[string]models.MockState{"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.request_id": {}}}}
					if err := ys.PersistMockNoise(context.Background(), pruneSet, states); err != nil {
						t.Fatalf("PersistMockNoise: %v", err)
					}
				}
				assertDocVerbatim(t, path, format, doc)
			})
		}
	}
}

// This keploy reads connection failures but cannot replay them: reading a set
// that holds some says so, once, with how many, the set, the mock file, this
// keploy's version and what to do. Only the ones it read count; one it
// skipped has its own ERROR.
func TestReadingASetWithConnectionFailuresWarnsOnce(t *testing.T) {
	old := utils.Version
	utils.Version = "v3.6.999"
	t.Cleanup(func() { utils.Version = old })

	warnings := func(t *testing.T, dir, format string) []observer.LoggedEntry {
		t.Helper()
		core, logs := observer.New(zapcore.DebugLevel)
		readSet(t, zap.New(core), dir, format)
		return logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("connection-failure").All()
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			raw := ""
			if format != "gob" {
				raw = cfRaw(format, cfNewerFormat[0].yaml)
				if format == "json" {
					raw = cfRaw(format, cfNewerFormat[0].json)
				}
			}
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, cfCorpus(), raw)
			ws := warnings(t, dir, format)
			if len(ws) != 1 {
				t.Fatalf("want one WARN for the set, got %d: %v", len(ws), ws)
			}
			want := "7 connection-failure mocks in set-0: keploy v3.6.999 reads them but cannot replay them; tests that depend on a refused or failed connection may fail; upgrade keploy"
			if ws[0].Message != want {
				t.Fatalf("WARN = %q\nwant %q", ws[0].Message, want)
			}
			if f := ws[0].ContextMap(); f["mock_file"] != path || f["connectionFailures"] != int64(7) {
				t.Fatalf("the WARN must name the mock file and the count: %v", f)
			}
		})
	}

	t.Run("one", func(t *testing.T) {
		dir := t.TempDir()
		writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(1), cfMock(2, models.ConnFailurePhaseConnect, models.ConnFailureRefused)}, "")
		ws := warnings(t, dir, "yaml")
		want := "1 connection-failure mock in set-0: keploy v3.6.999 reads it but cannot replay it; tests that depend on a refused or failed connection may fail; upgrade keploy"
		if len(ws) != 1 || ws[0].Message != want {
			t.Fatalf("want one WARN %q, got %v", want, ws)
		}
	})
	t.Run("none", func(t *testing.T) {
		dir := t.TempDir()
		writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, "")
		if ws := warnings(t, dir, "yaml"); len(ws) != 0 {
			t.Fatalf("a set without connection failures must not warn about them: %v", ws)
		}
	})
}

// The rewrites decode the file too, after a read of the set has already said,
// at ERROR, what it skips. They keep those documents as written and say so at
// Debug only, naming the file as the read does: mock names repeat in every
// test set.
func TestTheRewritesReportWhatTheyKeepAtDebug(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		for _, op := range []string{"prune", "noise write-back"} {
			t.Run(format+"/"+op, func(t *testing.T) {
				// And, in YAML, a document with no kind (commented out).
				raw := cfRaw(format, cfDocYAML("cf-newer", "request", "reset", true)) + "---\n" + undecodableYAMLFuture + "---\n# kind: Http\n# name: mock-old\n"
				if format == "json" {
					raw = cfRaw(format, cfDocJSON("cf-newer", "request", "reset", true)) + undecodableJSONFuture + "\n"
				}
				dir := t.TempDir()
				path := writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, raw)
				core, logs := observer.New(zapcore.DebugLevel)
				ys := NewWithFormat(zap.New(core), dir, "mocks", formatOf(format))
				if op == "prune" {
					if err := ys.UpdateMocks(context.Background(), pruneSet, nil, pruneT0.Add(time.Hour), time.Time{}); err != nil {
						t.Fatalf("UpdateMocks: %v", err)
					}
				} else {
					states := map[string]models.MockState{"mock-1": {Name: "mock-1", ReqBodyNoise: map[string][]string{"body.request_id": {}}}}
					if err := ys.PersistMockNoise(context.Background(), pruneSet, states); err != nil {
						t.Fatalf("PersistMockNoise: %v", err)
					}
				}
				if loud := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zapcore.WarnLevel }).All(); len(loud) != 0 {
					t.Fatalf("a rewrite must not repeat, at WARN or above, what the read reported: %v", loud)
				}
				kept := logs.FilterMessageSnippet("kept as written").All()
				named := map[any]bool{}
				for _, e := range kept {
					if e.Level != zapcore.DebugLevel || e.ContextMap()["mock_file"] != path {
						t.Fatalf("want a Debug line naming the mock file %s: %s %s %v", path, e.Level, e.Message, e.ContextMap())
					}
					named[e.ContextMap()["mock"]] = true
				}
				// The JSON reader skips an unknown kind at Debug on a read
				// too, so only the YAML rewrite has its line to look for.
				if !named["cf-newer"] || (format == "yaml" && !named["mock-future"]) {
					t.Fatalf("precondition: the rewrite read the connection failure it cannot replay (and, in YAML, the unknown kind) and said, at Debug, that it kept them; got %v", kept)
				}
			})
		}
	}
}

// cfSpecDoc is a connection failure, in YAML and in JSON, with the given spec
// fields over the valid base ones (an empty value drops the field), or with
// rawSpec (YAML, JSON) in place of the whole spec when it is set; "-" leaves
// the spec out.
func cfSpecDoc(over map[string][2]string, rawSpec *[2]string) (string, string) {
	base := [][3]string{
		{"address", "10.0.3.7:443", `"10.0.3.7:443"`},
		{"phase", "connect", `"connect"`},
		{"outcome", "refused", `"refused"`},
		{"reqTimestampMock", "2026-10-08T11:12:00.01Z", `"2026-10-08T11:12:00.01Z"`},
		{"resTimestampMock", "2026-10-08T11:12:00.041Z", `"2026-10-08T11:12:00.041Z"`},
	}
	fields := map[string][2]string{}
	order := []string{}
	for _, f := range base {
		fields[f[0]] = [2]string{f[1], f[2]}
		order = append(order, f[0])
	}
	// Fields the base does not have go last, in reverse order of name, so
	// that a document's order is not its fields' sorted order.
	extra := make([]string, 0, len(over))
	for k, v := range over {
		if _, ok := fields[k]; !ok {
			extra = append(extra, k)
		}
		fields[k] = v
	}
	slices.Sort(extra)
	slices.Reverse(extra)
	order = append(order, extra...)
	y := "version: api.keploy.io/v1beta1\nkind: ConnectionFailure\nname: cf-x\n"
	j := `{"version":"api.keploy.io/v1beta1","kind":"ConnectionFailure","name":"cf-x"`
	switch {
	case rawSpec != nil && rawSpec[0] == "-":
	case rawSpec != nil:
		y += "spec: " + rawSpec[0] + "\n"
		j += `,"spec":` + rawSpec[1]
	default:
		y += "spec:\n"
		var parts []string
		for _, k := range order {
			v := fields[k]
			if v[0] == "" {
				continue
			}
			y += "    " + k + ": " + v[0] + "\n"
			parts = append(parts, `"`+k+`":`+v[1])
		}
		j += `,"spec":{` + strings.Join(parts, ",") + "}"
	}
	return y, j + "}"
}

// A connection failure gets the same verdict, with the same message, from the
// YAML and the JSON reader: yaml.v3 alone would take `cause: 111` as text and
// a date as a time where encoding/json refuses both, and the two readers used
// to call a missing spec malformed in one and undecodable in the other.
func TestYAMLAndJSONGiveAConnectionFailureTheSameVerdict(t *testing.T) {
	spec := func(y, j string) *[2]string { return &[2]string{y, j} }
	for _, tc := range []struct {
		name    string
		over    map[string][2]string
		rawSpec *[2]string
		want    string // "" means the mock is read
	}{
		{name: "valid"},
		{name: "cause as a number", over: map[string][2]string{"cause": {"111", "111"}}, want: `field "cause" is not text`},
		{name: "cause as a boolean", over: map[string][2]string{"cause": {"true", "true"}}, want: `field "cause" is not text`},
		{name: "address as a number", over: map[string][2]string{"address": {"443", "443"}}, want: `field "address" is not text`},
		{name: "cause null", over: map[string][2]string{"cause": {"~", "null"}}},
		{name: "a quoted time", over: map[string][2]string{"reqTimestampMock": {"'2026-10-08T11:12:00.01Z'", `"2026-10-08T11:12:00.01Z"`}}},
		{name: "a date with no time", over: map[string][2]string{"reqTimestampMock": {"2026-10-08", `"2026-10-08"`}}, want: `field "reqTimestampMock" is not an RFC 3339 time`},
		{name: "a time as a number", over: map[string][2]string{"reqTimestampMock": {"1791466344", "1791466344"}}, want: `field "reqTimestampMock" is not an RFC 3339 time`},
		{name: "metadata", over: map[string][2]string{"metadata": {"{type: mocks}", `{"type":"mocks"}`}}},
		{name: "metadata holding a map", over: map[string][2]string{"metadata": {"{type: {a: b}}", `{"type":{"a":"b"}}`}}, want: `field "metadata" is not a map of text`},
		{name: "metadata holding a number", over: map[string][2]string{"metadata": {"{n: 1}", `{"n":1}`}}, want: `field "metadata" is not a map of text`},
		// Two faults: both readers name the first field in sorted order,
		// whatever the document's order (here outcome, then cause).
		{name: "two fields of the wrong type", over: map[string][2]string{"outcome": {"1", "1"}, "cause": {"2", "2"}}, want: `field "cause" is not text`},
		{name: "two fields the format lacks", over: map[string][2]string{"zeta": {"1", "1"}, "alpha": {"1", "1"}}, want: `uses field "alpha"`},
		{name: "no spec", rawSpec: spec("-", "-"), want: "the connection failure has no spec"},
		{name: "a null spec", rawSpec: spec("~", "null"), want: "the connection failure has no spec"},
		{name: "a list for a spec", rawSpec: spec("[1, 2]", "[1,2]"), want: "its spec is not a map"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y, j := cfSpecDoc(tc.over, tc.rawSpec)
			verdict := func(format string) (int, string) {
				t.Helper()
				core, logs := observer.New(zapcore.DebugLevel)
				var mocks []*models.Mock
				var err error
				if format == "yaml" {
					var doc yaml.NetworkTrafficDoc
					if uerr := yamlLib.Unmarshal([]byte(y), &doc); uerr != nil {
						t.Fatal(uerr)
					}
					mocks, err = DecodeMocks([]*yaml.NetworkTrafficDoc{&doc}, zap.New(core))
				} else {
					var doc yaml.NetworkTrafficDocJSON
					if uerr := json.Unmarshal([]byte(j), &doc); uerr != nil {
						t.Fatal(uerr)
					}
					mocks, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&doc}, zap.New(core))
				}
				if err != nil {
					t.Fatalf("%s: the set must not fail: %v", format, err)
				}
				msg := ""
				for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
					m, _ := e.ContextMap()["error"].(string)
					msg += m
				}
				return len(mocks), msg
			}
			yn, ymsg := verdict("yaml")
			jn, jmsg := verdict("json")
			if yn != jn || ymsg != jmsg {
				t.Fatalf("YAML read %d mock(s) and said %q; JSON read %d and said %q", yn, ymsg, jn, jmsg)
			}
			if tc.want == "" && yn != 1 {
				t.Fatalf("want the mock read, got %d mocks and %q", yn, ymsg)
			}
			if tc.want != "" && (yn != 0 || !strings.Contains(ymsg, tc.want)) {
				t.Fatalf("want the mock skipped with %q, got %d mocks and %q", tc.want, yn, ymsg)
			}
		})
	}
}

// connFailureShapes types every field of the format, no more.
func TestConnFailureShapesCoverTheFormat(t *testing.T) {
	full := models.ConnFailureSchema{
		Metadata:         map[string]string{"type": "mocks"},
		ConnFailureSpec:  *cfMock(0, models.ConnFailurePhaseConnect, models.ConnFailureRefused).Spec.ConnFailure,
		ReqTimestampMock: cfT0,
		ResTimestampMock: cfT0,
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != len(connFailureShapes) {
		t.Fatalf("the format writes %d fields, connFailureShapes types %d", len(keys), len(connFailureShapes))
	}
	for k := range keys {
		if _, ok := connFailureShapes[k]; !ok {
			t.Fatalf("connFailureShapes does not type field %q", k)
		}
	}
}

// A document with no kind is empty or commented out, not a mock a newer keploy
// wrote: it says so, and does not tell anyone to upgrade.
func TestADocumentWithNoKindSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := writePruneFixture(t, dir, "yaml", []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)},
		"---\n# version: api.keploy.io/v1beta1\n# kind: Http\n# name: mock-old\n")
	core, logs := observer.New(zapcore.DebugLevel)
	if got := readSet(t, zap.New(core), dir, "yaml"); len(got) != 2 {
		t.Fatalf("want the two HTTP mocks, read %d", len(got))
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "no kind (empty or commented out)") ||
		strings.Contains(errs[0].Message, "newer keploy") || errs[0].ContextMap()["mock_file"] != path {
		t.Fatalf("want one ERROR saying the document has no kind, naming the file, without the upgrade advice; got %v", errs)
	}
}

// The JSON reader skips a kind it does not know at Debug, as it always has:
// JSON recordings made before #4036 hold Redis and Kafka mocks OSS skips, and
// an ERROR (which CI greps for) would turn their green replays red.
func TestTheJSONReaderSkipsAnUnknownKindAtDebug(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	var doc yaml.NetworkTrafficDocJSON
	if err := json.Unmarshal([]byte(`{"version":"api.keploy.io/v1beta1","kind":"Redis","name":"mock-r","spec":{"metadata":{"type":"mocks"}}}`), &doc); err != nil {
		t.Fatal(err)
	}
	mocks, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&doc}, zap.New(core))
	if err != nil || len(mocks) != 0 {
		t.Fatalf("DecodeMocksJSON = %d mocks, %v; want the document skipped", len(mocks), err)
	}
	all := logs.All()
	if len(all) != 1 || all[0].Level != zapcore.DebugLevel || all[0].ContextMap()["kind"] != "Redis" {
		t.Fatalf("want one Debug line naming the kind, got %v", all)
	}
}

// A read lists, by name and kind, the documents its decoders skipped: a
// mapping entry, or the name of a mock appended to the set, can still refer to
// one. A document with no name (commented out) is not listed.
func TestAReadListsTheDocumentsItSkipped(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			raw := undecodableRaw(format)
			if format == "yaml" {
				raw += "---\n# kind: Http\n"
			}
			dir := t.TempDir()
			writePruneFixture(t, dir, format, []*models.Mock{pruneHTTPMock(1), pruneHTTPMock(2)}, raw)
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", formatOf(format))
			set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Now(), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]models.Kind{"mock-acme": "Acme-Queue", "mock-future": "FutureKind", "cf-newer": models.ConnectionFailure}
			if !reflect.DeepEqual(set.Skipped, want) {
				t.Fatalf("Skipped = %v, want %v", set.Skipped, want)
			}
		})
	}
}
