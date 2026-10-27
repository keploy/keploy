package mockdb

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// unknown is one key a newer keploy might write, as this keploy keeps it.
func unknown(t *testing.T, text string) models.UnknownKeys {
	t.Helper()
	var u models.UnknownKeys
	if err := yamlLib.Unmarshal([]byte(text), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

// aboutMock is variablesMock with an ignored field, a clock read, and where
// each decision came from: a learned one with every field, a hand one, an
// orphan (an about whose path is no longer ignored), and keys this keploy
// does not know in every block that holds them.
func aboutMock(t *testing.T) *models.Mock {
	m := variablesMock()
	m.Spec.ReqBodyNoise = map[string][]string{"body.eventId": {}, "header.x-trace-id": {}}
	m.Spec.NoiseAbout = map[string]models.About{
		"body.eventId": {
			By:     models.AboutByLearned,
			Why:    "changed between the recording and the full round",
			Seen:   &models.AboutSeen{Record: "3f2b9c1e", Full: "9a01d4be", Other: unknown(t, "keploy-test: 77aa\n")},
			Round:  models.AboutRoundFull,
			Run:    "2026-10-27T05:00:00Z",
			Test:   "api::TestAppCRUD",
			Call:   "http store POST /orders #1",
			Keploy: "3.8.91",
			Undo:   "delete body.eventId from noise.req and from noise.about",
			Sum:    "3f9a1c0e",
			Other:  unknown(t, "x-future:\n    rule: 4\n    places: [a, b]\n"),
		},
		"header.x-trace-id": {By: models.AboutByHand, Why: "our tracing middleware sets it at random"},
		"body.gone":         {By: models.AboutByLearned, Why: "an orphan: no longer ignored"},
	}
	m.Spec.NoiseOther = unknown(t, "x-match:\n    body.eventId: ['^[0-9a-f]+$']\n")
	v := m.Spec.Variables["orderId"]
	v.About = &models.About{By: models.AboutByLearned, Why: "the app makes this value anew each run", Round: models.AboutRoundFull, Other: unknown(t, "x-seen-in: 3\n")}
	v.Other = unknown(t, "x-scope: test\n")
	m.Spec.Variables["orderId"] = v
	m.Spec.Clocks = map[string]models.ClockOffset{"body.createdAt": {Res: time.Millisecond}, "body.at": {Res: time.Second}}
	m.Spec.ClockAbout = map[string]models.About{"body.createdAt": {By: models.AboutByLearned, Why: "a read of the app's clock", Round: models.AboutRoundFull}}
	m.Spec.ClockOther = map[string]models.UnknownKeys{"body.createdAt": unknown(t, "x-skew: 2ms\n")}
	return m
}

// provenance is, of m, what this format adds: where each decision came
// from and the keys this keploy does not know.
type provenance struct {
	NoiseAbout map[string]models.About
	NoiseOther models.UnknownKeys
	Variables  models.MockVariables
	ClockAbout map[string]models.About
	ClockOther map[string]models.UnknownKeys
}

func provenanceOf(m *models.Mock) provenance {
	return provenance{m.Spec.NoiseAbout, m.Spec.NoiseOther, m.Spec.Variables, m.Spec.ClockAbout, m.Spec.ClockOther}
}

// sameProvenance reports whether a and b say the same. A JSON file keeps an
// unknown key's value, not its YAML presentation (JSON has no flow style
// and orders a map's keys), so in JSON they are compared as JSON; in YAML
// and gob, as written.
func sameProvenance(t *testing.T, format string, a, b provenance) bool {
	t.Helper()
	if format != string(yaml.FormatJSON) {
		return reflect.DeepEqual(a, b)
	}
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ja, jb)
}

// Where each decision came from, and the keys a newer keploy wrote that this
// one does not know, are written and read back as written in each of the
// three formats a recording is kept in (YAML, JSON, gob), orphans
// included. In YAML they are legible blocks next to the decisions; the
// decisions and the recorded bytes are unchanged by them.
func TestWhereEachDecisionCameFromSurvivesEveryFormat(t *testing.T) {
	for _, format := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			m := aboutMock(t)
			want := provenanceOf(aboutMock(t))
			ys := clockStore(dir, format)
			if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			got := readBack(t, clockStore(dir, format), "test-set-0", "mock-0")
			if !sameProvenance(t, format, provenanceOf(got), want) {
				t.Fatalf("read back\n%+v\nwant\n%+v", provenanceOf(got), want)
			}
			if !reflect.DeepEqual(got.Spec.ReqBodyNoise, map[string][]string{"body.eventId": {}, "header.x-trace-id": {}}) || len(got.Spec.Clocks) != 2 {
				t.Fatalf("the decisions changed: noise %v clocks %v", got.Spec.ReqBodyNoise, got.Spec.Clocks)
			}
			if got.Spec.HTTPReq.Body != m.Spec.HTTPReq.Body || got.Spec.HTTPResp.Body != m.Spec.HTTPResp.Body {
				t.Fatal("the recorded bytes changed")
			}
			if format != string(yaml.FormatYAML) {
				return
			}
			text, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mocks.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{
				"noise:\n", "    about:\n", "        body.eventId:\n", "            by: learned\n",
				"            round: full\n", "            x-future:\n", "    x-match:\n",
				"        about:\n", "        x-scope: test\n", "            x-seen-in: 3\n",
				"        x-skew: 2ms\n", "        body.gone:\n",
			} {
				if !bytes.Contains(text, []byte(line)) {
					t.Fatalf("the file lacks %q:\n%s", line, text)
				}
			}
			if strings.Count(string(text), "    about:\n") < 3 {
				t.Fatalf("an about block is missing:\n%s", text)
			}
		})
	}
}

// A mock with no about writes none: keploy never invents where a decision
// came from, and a recording from before this format writes as before.
func TestAMockWithoutAboutWritesNone(t *testing.T) {
	m := variablesMock()
	m.Spec.ReqBodyNoise = map[string][]string{"body.eventId": {}}
	m.Spec.Clocks = map[string]models.ClockOffset{"body.createdAt": {Res: time.Millisecond}}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	out, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("about")) {
		t.Fatalf("a mock without about wrote one:\n%s", out)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("decode: %v", err)
	}
	if p := provenanceOf(mocks[0]); p.NoiseAbout != nil || p.NoiseOther != nil || p.ClockAbout != nil || p.ClockOther != nil {
		t.Fatalf("an about appeared on read: %+v", p)
	}
	again, err := EncodeMock(mocks[0], zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	out2, err := yamlLib.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, out2) {
		t.Fatalf("a read and a write changed the document:\n%s\nwas\n%s", out2, out)
	}
}

// A file the learn step rewrites keeps every mock's about blocks and every
// key this keploy does not know as written, a hand entry and an orphan
// included, on the mocks it learned nothing on.
func TestALearnRewriteKeepsAboutAndUnknownKeys(t *testing.T) {
	for _, format := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON)} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			ys := clockStore(dir, format)
			other := variablesMock()
			other.Name = "mock-0"
			other.Spec.Variables = nil
			if err := ys.InsertMock(ctx, other, "test-set-0"); err != nil {
				t.Fatal(err)
			}
			if err := ys.InsertMock(ctx, aboutMock(t), "test-set-0"); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			want := provenanceOf(readBack(t, clockStore(dir, format), "test-set-0", "mock-1"))
			if len(want.NoiseAbout) != 3 || len(want.NoiseOther) != 1 || len(want.ClockAbout) != 1 || len(want.ClockOther) != 1 || want.Variables["orderId"].About == nil {
				t.Fatalf("the file before the rewrite lacks what the test keeps: %+v", want)
			}
			ys = clockStore(dir, format)
			added, err := ys.PersistLearned(ctx, "test-set-0", map[string]models.MockState{
				"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.item": {}}},
			})
			if err != nil || len(added["mock-0"]) != 1 {
				t.Fatalf("persist: %v %v", added, err)
			}
			back := clockStore(dir, format)
			if got := readBack(t, back, "test-set-0", "mock-0"); len(got.Spec.ReqBodyNoise) != 1 {
				t.Fatalf("the learned noise was not written: %v", got.Spec.ReqBodyNoise)
			}
			if got := provenanceOf(readBack(t, back, "test-set-0", "mock-1")); !sameProvenance(t, format, got, want) {
				t.Fatalf("the rewrite changed mock-1's provenance:\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// An older keploy's decoder (the document shapes before this format) loads a
// file this format wrote: no decoder rejects a key it does not know, and the
// noise list stays a list of paths.
func TestAnOlderKeployLoadsANewFile(t *testing.T) {
	doc, err := EncodeMock(aboutMock(t), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	type oldNoise struct {
		Req   []string `json:"req,omitempty" yaml:"req,omitempty"`
		Value []string `json:"value,omitempty" yaml:"value,omitempty"`
	}
	type oldVariable struct {
		Recorded string   `json:"recorded" yaml:"recorded"`
		Bind     string   `json:"bind,omitempty" yaml:"bind,omitempty"`
		Use      []string `json:"use,omitempty" yaml:"use,omitempty"`
		Answer   []string `json:"answer,omitempty" yaml:"answer,omitempty"`
		Echo     bool     `json:"echo,omitempty" yaml:"echo,omitempty"`
	}
	type oldDoc struct {
		Name      string                        `json:"name" yaml:"name"`
		Noise     *oldNoise                     `json:"noise,omitempty" yaml:"noise,omitempty"`
		Variables map[string]oldVariable        `json:"variables,omitempty" yaml:"variables,omitempty"`
		Clocks    map[string]models.ClockOffset `json:"clocks,omitempty" yaml:"clocks,omitempty"`
	}
	check := func(t *testing.T, old oldDoc) {
		t.Helper()
		if old.Name != "mock-1" || old.Noise == nil || !reflect.DeepEqual(old.Noise.Req, []string{"body.eventId", "header.x-trace-id"}) {
			t.Fatalf("noise: %+v", old.Noise)
		}
		if v := old.Variables["orderId"]; v.Recorded != "o1" || v.Bind != "body.id" {
			t.Fatalf("variables: %+v", old.Variables)
		}
		if old.Clocks["body.createdAt"] != (models.ClockOffset{Res: time.Millisecond}) {
			t.Fatalf("clocks: %+v", old.Clocks)
		}
	}
	y, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML oldDoc
	if err := yamlLib.Unmarshal(y, &fromYAML); err != nil {
		t.Fatalf("an older keploy fails on the YAML: %v", err)
	}
	check(t, fromYAML)
	j, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON oldDoc
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatalf("an older keploy fails on the JSON: %v", err)
	}
	check(t, fromJSON)
}

// The key of the about block is spelled in one place (models.AboutKey): every
// block that holds one uses it, so renaming it is one change.
func TestAboutKeyIsTheOnlySpelling(t *testing.T) {
	for _, f := range []reflect.StructField{
		field(t, reflect.TypeOf(models.MockVariable{}), "About"),
		field(t, reflect.TypeOf(yaml.DocNoise{}), "About"),
		field(t, reflect.TypeOf(yaml.DocClock{}), "About"),
	} {
		for _, codec := range []string{"json", "yaml"} {
			name, _, _ := strings.Cut(f.Tag.Get(codec), ",")
			if name != models.AboutKey {
				t.Errorf("%s tag of %s is %q, want models.AboutKey %q", codec, f.Name, name, models.AboutKey)
			}
		}
	}
}

func field(t *testing.T, typ reflect.Type, name string) reflect.StructField {
	t.Helper()
	f, ok := typ.FieldByName(name)
	if !ok {
		t.Fatalf("%s has no field %s", typ, name)
	}
	return f
}

// The in-place encoder InsertMock uses writes the about blocks and the keys
// this keploy does not know byte for byte as EncodeMock does.
func TestEncodeMockInPlaceWritesAboutAsEncodeMockDoes(t *testing.T) {
	m := aboutMock(t)
	inPlace, ok := encodeMockInPlace(m)
	if !ok {
		t.Fatalf("kind %s is not written in place", m.Kind)
	}
	got, err := yamlLib.Marshal(inPlace)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	want, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written in place:\n%s\nthrough EncodeMock:\n%s", got, want)
	}
	if !bytes.Contains(got, []byte("x-skew: 2ms")) || !bytes.Contains(got, []byte("body.gone:")) {
		t.Fatalf("the in-place document lacks the about blocks:\n%s", got)
	}
}

// A noise block that holds only where paths came from (orphans: their paths
// are no longer ignored) or only keys this keploy does not know is still
// written, so neither is dropped, and the noise paths stay empty.
func TestANoiseBlockOfOnlyOrphansIsKept(t *testing.T) {
	m := variablesMock()
	m.Spec.NoiseAbout = map[string]models.About{"body.gone": {By: models.AboutByHand, Why: "no longer ignored"}}
	m.Spec.NoiseOther = unknown(t, "x-match: {}\n")
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	out, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(mocks) != 1 {
		t.Fatalf("decode: %v", err)
	}
	if got := mocks[0].Spec; len(got.NoiseAbout) != 1 || len(got.NoiseOther) != 1 || got.ReqBodyNoise != nil {
		t.Fatalf("read back about %v other %v noise %v:\n%s", got.NoiseAbout, got.NoiseOther, got.ReqBodyNoise, out)
	}
}
