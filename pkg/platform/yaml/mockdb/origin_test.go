package mockdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// A mock's origin (proven switch: its stamp's clock, the process that made
// it) is written as its document's top-level origin block and read back, for
// every kind keploy OSS writes, in YAML (in place or through the yaml.Node)
// and in JSON (direct, or from the YAML document). The block is read whatever
// the switch, so a run without it carries what a run with it recorded.
func TestAMocksOriginRoundTrips(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	origin := models.Origin{Clock: "proxy:run-1", Comm: "sqs", Exe: "./sqs"}
	readJSONKinds := map[models.Kind]bool{}
	for _, m := range goldenCorpus(t) {
		m.Origin = &origin
		t.Run(string(m.Kind)+"/"+m.Name, func(t *testing.T) {
			doc, err := EncodeMock(m, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			var node bytes.Buffer
			enc := yamlLib.NewEncoder(&node)
			if err := enc.Encode(doc); err != nil {
				t.Fatal(err)
			}
			_ = enc.Close()
			if !strings.Contains(node.String(), "\norigin:\n    clock: proxy:run-1\n    comm: sqs\n    exe: ./sqs\n") {
				t.Fatalf("the YAML document holds no origin block:\n%s", node.String())
			}
			if inPlace, ok := encodeMockInPlace(m); ok {
				var got bytes.Buffer
				enc := yamlLib.NewEncoder(&got)
				if err := enc.Encode(inPlace); err != nil {
					t.Fatal(err)
				}
				_ = enc.Close()
				if got.String() != node.String() {
					t.Fatalf("written in place, the mock differs from EncodeMock's:\n got: %s\nwant: %s", got.String(), node.String())
				}
			}
			read, err := yaml.UnmarshalDoc(yaml.FormatYAML, node.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			back, err := decodeMocks([]*yaml.NetworkTrafficDoc{read}, zap.NewNop(), false)
			if err != nil || len(back) != 1 {
				t.Fatalf("read back %d mocks, %v", len(back), err)
			}
			if back[0].Origin == nil || *back[0].Origin != origin {
				t.Fatalf("read back from YAML with origin %+v; want %+v", back[0].Origin, origin)
			}
			t.Setenv(provenswitch.Env, "")
			off, err := decodeMocks([]*yaml.NetworkTrafficDoc{read}, zap.NewNop(), false)
			if err != nil || len(off) != 1 || off[0].Origin == nil || *off[0].Origin != origin {
				t.Fatalf("read back without the switch: %d mocks, %v; want one, with origin %+v", len(off), err, origin)
			}
			t.Setenv(provenswitch.Env, "1")

			jsonDocs := []*yaml.NetworkTrafficDocJSON{}
			if direct, ok, err := EncodeMockJSON(m, zap.NewNop()); err != nil {
				t.Fatal(err)
			} else if ok {
				jsonDocs = append(jsonDocs, direct)
			}
			viaYAML, err := yaml.DocToJSON(doc)
			if err != nil {
				t.Fatal(err)
			}
			jsonDocs = append(jsonDocs, viaYAML)
			for _, jd := range jsonDocs {
				b, err := json.Marshal(jd)
				if err != nil {
					t.Fatal(err)
				}
				var readJSON yaml.NetworkTrafficDocJSON
				if err := json.Unmarshal(b, &readJSON); err != nil {
					t.Fatal(err)
				}
				back, err := decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&readJSON}, zap.NewNop(), false)
				if err != nil {
					t.Fatal(err)
				}
				if len(back) != 1 {
					continue // a kind the JSON reader leaves to another (a skip, as today)
				}
				if back[0].Origin == nil || *back[0].Origin != origin {
					t.Fatalf("read back from JSON with origin %+v; want %+v", back[0].Origin, origin)
				}
				t.Setenv(provenswitch.Env, "")
				off, err := decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{&readJSON}, zap.NewNop(), false)
				if err != nil || len(off) != 1 || off[0].Origin == nil || *off[0].Origin != origin {
					t.Fatalf("read back from JSON without the switch: %d mocks, %v; want one, with origin %+v", len(off), err, origin)
				}
				t.Setenv(provenswitch.Env, "1")
				readJSONKinds[m.Kind] = true
			}
		})
	}
	for _, k := range []models.Kind{models.HTTP, models.Mongo, models.MySQL, models.GENERIC} {
		if !readJSONKinds[k] {
			t.Errorf("no %s mock was read back from JSON: the JSON half checked nothing for it", k)
		}
	}
}

// A mocks.gob carries every field of a mock: its reader reads the origin
// whatever the switch, as the YAML and JSON readers do.
func TestAGobMocksOriginIsReadWhateverTheSwitch(t *testing.T) {
	origin := models.Origin{Clock: "ktime:boot-1.anchor-1", Comm: "sqs", Exe: "./sqs"}
	mocks := inPlaceCorpus(t)
	for _, m := range mocks {
		m.Origin = &origin
	}
	path := writePruneFixture(t, t.TempDir(), "gob", mocks, "")
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		back, err := readGobMocks(path)
		if err != nil || len(back) != len(mocks) {
			t.Fatalf("switch=%v: read %d of %d mocks back, %v", on, len(back), len(mocks), err)
		}
		for _, m := range back {
			if m.Origin == nil || *m.Origin != origin {
				t.Errorf("switch=%v: %s read back with origin %+v; want %+v", on, m.Name, m.Origin, origin)
			}
		}
	}
}

// A kind an out-of-tree mapper decodes keeps its document's origin, whatever
// the switch, even when the mapper rebuilds the mock whole.
func TestAMappedMocksOriginIsReadWhateverTheSwitch(t *testing.T) {
	const kind = models.Kind("EnterpriseKind")
	clearRegistry(t)
	RegisterMockYAMLMapper(kind, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(doc *yaml.NetworkTrafficDoc, m *models.Mock) error {
			*m = models.Mock{Version: doc.Version, Name: doc.Name, Kind: doc.Kind}
			return nil
		},
	})
	origin := models.Origin{Clock: "proxy:run-1", Comm: "worker", Exe: "/app/worker"}
	doc, err := EncodeMock(&models.Mock{Version: models.GetVersion(), Name: "mock-0", Kind: kind, Origin: &origin}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	for _, on := range []bool{true, false} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		back, err := decodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop(), false)
		if err != nil || len(back) != 1 {
			t.Fatalf("switch=%v: read back %d mocks, %v", on, len(back), err)
		}
		if back[0].Origin == nil || *back[0].Origin != origin {
			t.Fatalf("switch=%v: the mapped mock read back with origin %+v; want %+v", on, back[0].Origin, origin)
		}
	}
}

// A rewrite of a set's mocks without the proven switch keeps every origin a
// run with it recorded (S12 compat audit A6): the prune (--remove-unused-mocks)
// and the noise write-back (--schema-noise-detection without the prune)
// re-encode each mock they keep from what the readers decoded, so a reader
// that dropped the block would strip it from the file for good. Every
// format: YAML, JSON and gob.
func TestAMocksOriginSurvivesARewriteWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			mocks := pruneMixedFixture()
			origins := map[string]models.Origin{}
			for i, m := range mocks {
				if i%2 == 1 {
					// Half the mocks have none: they must stay without one.
					continue
				}
				o := models.Origin{Clock: "proxy:run-1", Comm: "app", Exe: fmt.Sprintf("/bin/app-%d", i)}
				m.Origin = &o
				// InsertMock names the i-th mock it records mock-<i>.
				origins[fmt.Sprintf("mock-%d", i)] = o
			}
			dir := t.TempDir()
			path := writePruneFixture(t, dir, format, mocks, "")
			f := yaml.FormatYAML
			if format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()

			// The noise write-back first: it rewrites the file, pruning nothing.
			noise := map[string]models.MockState{}
			for _, name := range []string{"mock-1", "mock-2", "mock-4"} {
				noise[name] = models.MockState{Name: name, ReqBodyNoise: map[string][]string{"body.n": {}}}
			}
			if err := ys.PersistMockNoise(ctx, pruneSet, noise); err != nil {
				t.Fatal(err)
			}
			checkOrigins(t, "after the noise write-back", path, origins, nil)

			// Then the prune.
			consumed, pruneBefore, startupCutoff := pruneMixedArgs()
			if err := ys.UpdateMocks(ctx, pruneSet, consumed, pruneBefore, startupCutoff); err != nil {
				t.Fatal(err)
			}
			checkOrigins(t, "after the prune", path, origins, consumed)
		})
	}
}

// checkOrigins reads the mock file at path back, with the switch on, and
// checks that every mock it holds has the origin it was recorded with (none
// for one recorded without), and, when kept is set, that the prune kept at
// least one mock with an origin it re-encoded (a consumed one).
func checkOrigins(t *testing.T, when, path string, origins map[string]models.Origin, kept map[string]models.MockState) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	defer t.Setenv(provenswitch.Env, "")
	var back []*models.Mock
	switch filepath.Ext(path) {
	case ".gob":
		var err error
		if back, err = readGobMocks(path); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
	default:
		f := yaml.FormatYAML
		if filepath.Ext(path) == ".json" {
			f = yaml.FormatJSON
		}
		reader, err := yaml.NewMockReaderF(context.Background(), zap.NewNop(), filepath.Dir(path), "mocks", f)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		defer reader.Close()
		for {
			var decoded []*models.Mock
			if f == yaml.FormatJSON {
				d, _, err := reader.ReadNextDocJSONBytes()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				if decoded, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{d}, zap.NewNop()); err != nil {
					t.Fatalf("%s: %v", when, err)
				}
			} else {
				d, _, err := reader.ReadNextDocBytes()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				if decoded, err = DecodeMocks([]*yaml.NetworkTrafficDoc{d}, zap.NewNop()); err != nil {
					t.Fatalf("%s: %v", when, err)
				}
			}
			back = append(back, decoded...)
		}
	}
	if len(back) == 0 {
		t.Fatalf("%s: the file holds no mock", when)
	}
	keptWithOrigin := 0
	for _, m := range back {
		want, had := origins[m.Name]
		switch {
		case had && (m.Origin == nil || *m.Origin != want):
			t.Errorf("%s: %s has origin %+v; want %+v", when, m.Name, m.Origin, want)
		case !had && m.Origin != nil:
			t.Errorf("%s: %s, recorded without an origin, has %+v", when, m.Name, m.Origin)
		}
		if _, consumed := kept[m.Name]; consumed && had {
			keptWithOrigin++
		}
	}
	if kept != nil && keptWithOrigin == 0 {
		t.Fatalf("%s: no consumed mock with an origin was kept: the prune checked nothing", when)
	}
}

// Why keploy named no process (models.Origin.Unnamed, PID: it runs in a PID
// namespace of its own) travels with a mock to keploy record and is never
// written to its file, in any format: the origin is written as its clock
// alone, and one with nothing else is no origin at all, as a mock recorded
// without the switch has.
func TestWhyNoProcessWasNamedIsNeverWritten(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			mocks := pruneMixedFixture()
			origins := map[string]models.Origin{}
			for i, m := range mocks {
				if i%2 == 0 {
					m.Origin = &models.Origin{Clock: "proxy:run-1", Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
					origins[fmt.Sprintf("mock-%d", i)] = models.Origin{Clock: "proxy:run-1"}
				} else {
					m.Origin = &models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
				}
			}
			path := writePruneFixture(t, t.TempDir(), format, mocks, "")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{models.UnnamedPIDNamespace, "2912345", "origin: {}"} {
				if bytes.Contains(raw, []byte(leak)) {
					t.Fatalf("the %s file holds %q", format, leak)
				}
			}
			checkOrigins(t, "as written", path, origins, nil)
			for _, m := range mocks {
				if m.Origin == nil || m.Origin.Unnamed == "" {
					t.Fatalf("writing %s edited the recorded mock's origin: %+v", m.Name, m.Origin)
				}
			}
		})
	}
}

// The gob writer's sync fallback (gobWriteSync: the async queue full) writes
// a mock as the stream writer does: why keploy named no process
// (models.Origin.Unnamed, PID) never reaches the file, and an origin with
// nothing else is none.
func TestTheSyncGobWriterNeverWritesWhyNoProcessWasNamed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	mocks := pruneMixedFixture()[:2]
	mocks[0].Origin = &models.Origin{Clock: "proxy:run-1", Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
	mocks[1].Origin = &models.Origin{Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
	mockPath := filepath.Join(dir, pruneSet)
	if err := os.MkdirAll(mockPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range mocks {
		if err := ys.gobWriteSync(context.Background(), m, mockPath, "mocks"); err != nil {
			t.Fatal(err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mockPath, "mocks.gob")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(models.UnnamedPIDNamespace)) || bytes.Contains(raw, []byte("2912345")) {
		t.Fatal("the sync gob writer wrote why no process was named")
	}
	back, err := readGobMocks(path)
	if err != nil || len(back) != 2 {
		t.Fatalf("read back %d mocks, %v", len(back), err)
	}
	if back[0].Origin == nil || *back[0].Origin != (models.Origin{Clock: "proxy:run-1"}) || back[1].Origin != nil {
		t.Fatalf("read back with origins %+v, %+v; want the clock alone, and none", back[0].Origin, back[1].Origin)
	}
	for _, m := range mocks {
		if m.Origin.Unnamed == "" {
			t.Fatalf("writing %s edited the recorded mock's origin: %+v", m.Name, m.Origin)
		}
	}
}
