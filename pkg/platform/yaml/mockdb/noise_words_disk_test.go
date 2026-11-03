package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// a words pattern written by the learn step must survive the
// write to disk and the read back, or the next keploy test reads the place as
// noise whole.
func TestAWordsPatternSurvivesTheDisk(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			ctx := context.Background()
			set := "test-set-0"
			if err := ys.InsertMock(ctx, noiseTestMock(`{"note":"lead r-1 created"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{
				"mock-0": {Name: "mock-0", ReqBodyNoise: map[string][]string{"body.note": {"^keploy:words 1$"}}},
			}
			if _, err := ys.PersistLearned(ctx, set, states); err != nil {
				t.Fatal(err)
			}
			mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range append(mocks, filtered...) {
				if m.Name != "mock-0" {
					continue
				}
				if want := map[string][]string{"body.note": {"^keploy:words 1$"}}; !reflect.DeepEqual(m.Spec.ReqBodyNoise, want) {
					t.Fatalf("read back %#v, want %#v (the words pattern is lost: the place reads as noise whole)", m.Spec.ReqBodyNoise, want)
				}
				return
			}
			t.Fatal("mock-0 not read back")
		})
	}
}

func TestAWordsPatternSurvivesEncodeDecode(t *testing.T) {
	mock := noiseTestMock(`{"note":"lead r-1 created"}`)
	mock.Spec.ReqBodyNoise = map[string][]string{"body.note": {"^keploy:words 1$"}}
	doc, err := EncodeMock(mock, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"body.note": {"^keploy:words 1$"}}; !reflect.DeepEqual(decoded[0].Spec.ReqBodyNoise, want) {
		t.Fatalf("decoded %#v, want %#v", decoded[0].Spec.ReqBodyNoise, want)
	}
}

// persistAndRead writes learned noise onto the set's one mock (PersistLearned)
// and returns the mock as a replay reads it back, and the mock file's text.
func persistAndRead(t *testing.T, ys *MockYaml, set string, f yaml.Format, noise ...map[string][]string) (*models.Mock, string) {
	t.Helper()
	ctx := context.Background()
	for _, n := range noise {
		if _, err := ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", ReqBodyNoise: n}}); err != nil {
			t.Fatal(err)
		}
	}
	mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ys.MockPath, set, "mocks."+f.FileExtension()))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mocks {
		if m.Name == "mock-0" {
			return m, string(data)
		}
	}
	t.Fatal("mock-0 not read back")
	return nil, ""
}

// The mock file names a text learned by its words under noise.words, with
// the numbers of its unchecked words, and never under noise.req: a keploy
// that does not read noise.words compares the text whole instead of leaving
// all of it unchecked. A place learned whole stays under noise.req.
func TestTheMockFileNamesATextsUncheckedWordsApartFromItsWholeNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", f)
			set := "test-set-0"
			if err := ys.InsertMock(context.Background(), noiseTestMock(`{"note":"lead r-1 created","id":"a1"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			m, file := persistAndRead(t, ys, set, f, map[string][]string{"body.note": {"^keploy:words 1$"}, "body.id": {}})
			want := map[string][]string{"body.note": {"^keploy:words 1$"}, "body.id": {}}
			if !reflect.DeepEqual(m.Spec.ReqBodyNoise, want) {
				t.Fatalf("read back %#v, want %#v", m.Spec.ReqBodyNoise, want)
			}
			wantText := map[yaml.Format][]string{
				yaml.FormatYAML: {`(?m)^ *req:\n +- body\.id\n +words:\n +body\.note:\n +- 1\n`},
				yaml.FormatJSON: {`"req":\["body\.id"\],"words":\{"body\.note":\[1\]\}`},
			}[f]
			for _, w := range wantText {
				if !regexp.MustCompile(w).MatchString(file) {
					t.Fatalf("the mock file does not match %q:\n%s", w, file)
				}
			}
			for _, bad := range []string{"- body.note", `"body.note"]`, `"req":["body.id","body.note"]`, `"req":["body.note"`} {
				if strings.Contains(file, bad) {
					t.Fatalf("the mock file names the words place as whole noise (%q):\n%s", bad, file)
				}
			}
		})
	}
}

// A second learning that finds another word of the same text changed adds
// it on disk: the file's words read back and the new ones are their union,
// written whatever the proven switch.
func TestASecondLearningAddsItsWordsToTheFilesWords(t *testing.T) {
	for _, proven := range []string{"1", "0"} {
		for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
			t.Run(proven+"/"+string(f), func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", f)
				set := "test-set-0"
				if err := ys.InsertMock(context.Background(), noiseTestMock(`{"note":"lead r-1 created at 09:00"}`), set); err != nil {
					t.Fatal(err)
				}
				if err := ys.Close(); err != nil {
					t.Fatal(err)
				}
				m, _ := persistAndRead(t, ys, set, f, map[string][]string{"body.note": {"^keploy:words 1$"}})
				if want := []string{"^keploy:words 1$"}; !reflect.DeepEqual(m.Spec.ReqBodyNoise["body.note"], want) {
					t.Fatalf("first write read back %q, want %q", m.Spec.ReqBodyNoise["body.note"], want)
				}
				m, file := persistAndRead(t, ys, set, f, map[string][]string{"body.note": {"^keploy:words 4$"}})
				if want := []string{"^keploy:words 1,4$"}; !reflect.DeepEqual(m.Spec.ReqBodyNoise["body.note"], want) {
					t.Fatalf("second write read back %q, want %q (the union)\n%s", m.Spec.ReqBodyNoise["body.note"], want, file)
				}
			})
		}
	}
}

// A file an older keploy wrote names a text of more than one word as noise
// whole, under noise.req. A learning that finds the words of it that changed
// writes those words in its place: the text leaves noise.req for
// noise.words, and reads back as that words pattern alone.
func TestALearningReplacesAnOlderKeploysWholeNoiseOnDiskByItsWords(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, f := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(f), func(t *testing.T) {
			ys := NewWithFormat(zap.NewNop(), t.TempDir(), "mocks", f)
			set := "test-set-0"
			if err := ys.InsertMock(context.Background(), noiseTestMock(`{"note":"lead r-1 created","id":"a1"}`), set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			m, file := persistAndRead(t, ys, set, f, map[string][]string{"body.note": {}, "body.id": {}})
			if want := map[string][]string{"body.note": {}, "body.id": {}}; !reflect.DeepEqual(m.Spec.ReqBodyNoise, want) {
				t.Fatalf("the older keploy's file read back %#v, want %#v\n%s", m.Spec.ReqBodyNoise, want, file)
			}
			m, file = persistAndRead(t, ys, set, f, map[string][]string{"body.note": {"^keploy:words 1$"}})
			if want := map[string][]string{"body.note": {"^keploy:words 1$"}, "body.id": {}}; !reflect.DeepEqual(m.Spec.ReqBodyNoise, want) {
				t.Fatalf("read back %#v, want %#v\n%s", m.Spec.ReqBodyNoise, want, file)
			}
			for _, bad := range []string{"- body.note", `"body.note"]`, `"req":["body.id","body.note"]`, `"req":["body.note"`} {
				if strings.Contains(file, bad) {
					t.Fatalf("the mock file still names the text as whole noise (%q):\n%s", bad, file)
				}
			}
		})
	}
}
