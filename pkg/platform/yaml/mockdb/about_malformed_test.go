package mockdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	yamlLib "gopkg.in/yaml.v3"
)

// An about block a person wrote in the wrong shape never fails the file
// (main ignored about): the set loads, its decisions read as they did
// without the block (so it replays the same), the block reads as a
// person's, and a learn rewrite of the same call writes it back unchanged.
// A noise.about that is not a mapping of paths holds every noise place of
// its call, so a learned noise field there is blocked.
func TestAnAboutOfTheWrongShapeNeverFailsTheFile(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	shapes := []struct {
		name  string
		at    []string
		value any
		hand  func(spec *models.MockSpec) models.AboutState
		holds bool
	}{
		{"a variable's about is a text", []string{"variables", "orderId", "about"}, "written by our team", variableState, false},
		{"a variable's about has by as a list", []string{"variables", "orderId", "about"}, map[string]any{"by": []any{"hand"}}, variableState, false},
		{"a variable's about has seen as a text", []string{"variables", "orderId", "about"}, map[string]any{"by": "learned", "seen": "some text"}, variableState, false},
		{"a noise about entry is a text", []string{"noise", "about", "body.item"}, "our tracing sets it", func(spec *models.MockSpec) models.AboutState {
			return models.NoiseAboutState(spec, "body.item")
		}, false},
		{"noise.about is a list", []string{"noise", "about"}, []any{"body.item"}, func(spec *models.MockSpec) models.AboutState {
			return models.NoiseAboutState(spec, "body.extra")
		}, true},
	}
	for _, format := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON)} {
		for _, sh := range shapes {
			t.Run(format+"/"+sh.name, func(t *testing.T) {
				dir := t.TempDir()
				ctx := context.Background()
				ys := clockStore(dir, format)
				m := variablesMock()
				m.Spec.ReqBodyNoise = map[string][]string{"body.item": {}}
				if err := ys.InsertMock(ctx, m, "s"); err != nil {
					t.Fatal(err)
				}
				if err := ys.Close(); err != nil {
					t.Fatal(err)
				}
				before := readBack(t, clockStore(dir, format), "s", "mock-0")
				path := mockFile(t, dir)
				editDoc(t, path, format, func(doc map[string]any) { setAt(t, doc, sh.at, sh.value) })

				got := readBack(t, clockStore(dir, format), "s", "mock-0")
				if !reflect.DeepEqual(decisions(got), decisions(before)) || !reflect.DeepEqual(got.Spec.HTTPReq, before.Spec.HTTPReq) || !reflect.DeepEqual(got.Spec.HTTPResp, before.Spec.HTTPResp) {
					t.Fatalf("the block changed what replay reads:\n%+v\nwant\n%+v", decisions(got), decisions(before))
				}
				if s := sh.hand(&got.Spec); s != models.AboutHand {
					t.Fatalf("the block reads as %v, want hand", s)
				}

				added, blocked, err := clockStore(dir, format).PersistLearned(ctx, "s", map[string]models.MockState{
					"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.extra": {}}, NoiseAbout: map[string]models.About{"body.extra": {Why: "it changed"}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if held := len(blocked["mock-0"]) == 1 && len(added["mock-0"]) == 0; held != sh.holds || !held && len(added["mock-0"]) != 1 {
					t.Fatalf("added %v, blocked %v; want held=%v", added, blocked, sh.holds)
				}
				var doc map[string]any
				editDoc(t, path, format, func(d map[string]any) { doc = d })
				if v := getAt(doc, sh.at); !reflect.DeepEqual(v, sh.value) {
					t.Fatalf("the rewrite changed the block: %#v, want %#v", v, sh.value)
				}
				learned := getAt(doc, []string{"noise", "about", "body.extra", "by"})
				if wantBy := any(models.AboutByLearned); sh.holds && learned != nil || !sh.holds && learned != wantBy {
					t.Fatalf("the learned noise's about: by=%v (held=%v)", learned, sh.holds)
				}
			})
		}
	}
}

func variableState(spec *models.MockSpec) models.AboutState {
	return models.VariableAboutState("orderId", spec.Variables["orderId"])
}

// decisions is what replay reads of m's learned blocks: its noise paths and
// its variables without their abouts.
func decisions(m *models.Mock) any {
	vars := m.Spec.Variables.Clone()
	for n, v := range vars {
		v.About, v.Other = nil, nil
		vars[n] = v
	}
	paths := make([]string, 0, len(m.Spec.ReqBodyNoise))
	for p := range m.Spec.ReqBodyNoise {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return struct {
		Noise []string
		Vars  models.MockVariables
	}{paths, vars}
}

// mockFile is the one mocks file of set s under dir.
func mockFile(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "s", "mocks.*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("mocks file: %v %v", files, err)
	}
	return files[0]
}

// editDoc calls edit on the first document of the mocks file at path,
// generically decoded, and writes the file back with the edit.
func editDoc(t *testing.T, path, format string, edit func(map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	if format == string(yaml.FormatJSON) {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var d map[string]any
			if err := json.Unmarshal([]byte(line), &d); err != nil {
				t.Fatal(err)
			}
			docs = append(docs, d)
		}
	} else {
		dec := yamlLib.NewDecoder(bytes.NewReader(b))
		for {
			var d map[string]any
			if err := dec.Decode(&d); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			docs = append(docs, d)
		}
	}
	if len(docs) != 1 {
		t.Fatalf("want one document, got %d", len(docs))
	}
	edit(docs[0])
	var out []byte
	if format == string(yaml.FormatJSON) {
		out, err = json.Marshal(docs[0])
		out = append(out, '\n')
	} else {
		out, err = yamlLib.Marshal(docs[0])
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func setAt(t *testing.T, doc map[string]any, at []string, v any) {
	t.Helper()
	for _, k := range at[:len(at)-1] {
		next, ok := doc[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			doc[k] = next
		}
		doc = next
	}
	doc[at[len(at)-1]] = v
}

func getAt(doc map[string]any, at []string) any {
	var v any = doc
	for _, k := range at {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}
