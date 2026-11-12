package mockdb

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// The places the learn step learned as a read of the application's clock
// (models.MockSpec.Clocks: a plain read, and one plus thirty minutes) are a
// top-level `clocks:` block of the document, written in durations a person
// reads ("res: 1ms", "min: 29m59.99s"), and they survive a YAML and a JSON
// round trip with the recorded bytes unchanged. A mock without them writes
// no such key, so a recording without the field reads and writes as before.
func TestAClocksBlockRoundTrips(t *testing.T) {
	m := variablesMock()
	m.Spec.Clocks = map[string]models.ClockOffset{
		"body.createdAt": {Res: time.Millisecond},
		"body.expiresAt": {Min: 30*time.Minute - 10*time.Millisecond, Max: 30*time.Minute + 5*time.Millisecond, Res: time.Millisecond},
	}
	want := map[string]models.ClockOffset{}
	for k, v := range m.Spec.Clocks {
		want[k] = v
	}
	doc, err := EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var buf bytes.Buffer
	enc := yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	text := buf.String()
	if !strings.Contains(text, "\nclocks:\n") || !strings.Contains(text, "res: 1ms") || !strings.Contains(text, "min: 29m59.99s") {
		t.Fatalf("the block is not a legible top-level key of the document:\n%s", text)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil || len(out) != 1 {
		t.Fatalf("decode: err=%v n=%d", err, len(out))
	}
	if !reflect.DeepEqual(out[0].Spec.Clocks, want) {
		t.Fatalf("yaml round-trip: %+v, want %+v", out[0].Spec.Clocks, want)
	}
	if out[0].Spec.HTTPReq.Body != m.Spec.HTTPReq.Body || out[0].Spec.HTTPResp.Body != m.Spec.HTTPResp.Body {
		t.Fatal("the recorded bytes changed")
	}
	jsonBytes, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
	if err != nil {
		t.Fatalf("MarshalDoc(json): %v", err)
	}
	jdoc, err := yaml.UnmarshalDoc(yaml.FormatJSON, jsonBytes)
	if err != nil {
		t.Fatalf("UnmarshalDoc(json): %v", err)
	}
	jout, err := DecodeMocks([]*yaml.NetworkTrafficDoc{jdoc}, zap.NewNop())
	if err != nil || len(jout) != 1 || !reflect.DeepEqual(jout[0].Spec.Clocks, want) {
		t.Fatalf("json round-trip: err=%v %+v, want %+v", err, jout, want)
	}

	m.Spec.Clocks = nil
	doc, err = EncodeMock(m, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	enc = yamlLib.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	_ = enc.Close()
	if strings.Contains(buf.String(), "clocks") {
		t.Fatalf("a mock without clocks wrote the key:\n%s", buf.String())
	}
}

// clockStore is a store of one format of the three a recording is kept in
// (YAML, JSON, gob), in dir.
func clockStore(dir, format string) *MockYaml {
	if format == mockFormatGob {
		ys := New(zap.NewNop(), dir, "mocks")
		ys.MockFormat = mockFormatGob
		return ys
	}
	return NewWithFormat(zap.NewNop(), dir, "mocks", yaml.Format(format))
}

// readBack is the mock named name of set, read back from ys.
func readBack(t *testing.T, ys *MockYaml, set, name string) *models.Mock {
	t.Helper()
	ctx := context.Background()
	mocks, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range append(mocks, filtered...) {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("%s not read back", name)
	return nil
}

// What record's full round learned as reads of the application's clock
// (MockState.Clocks) is written onto the recording as its clock reads, said
// per mock, read back, and never added twice, in every storage format. A
// place that is a clock read is never noise: noise there, learned in the
// same round or on disk from a keploy that learned a clock read as noise,
// is dropped, so the place is compared again (a usage query whose
// createdAt.$lte moved thirty days replays red). A later learning of the
// same place meets the one on disk: one that narrows its offset is added,
// and one that shares no offset with it leaves an Empty read that refuses
// every value there.
func TestPersistLearnedWritesClockReadsAndDropsNoiseThere(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	plain := models.ClockOffset{Res: time.Millisecond}
	ttl := models.ClockOffset{Min: 30*time.Minute - 20*time.Millisecond, Max: 30*time.Minute + 20*time.Millisecond, Res: time.Millisecond}
	for _, f := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()
			ys := clockStore(dir, f)
			ctx := context.Background()
			set := "test-set-0"
			m := noiseTestMock(`{"createdAt":"2026-10-26T10:00:00.123Z","expiresAt":"2026-10-26T10:30:00.123Z","other":"o"}`)
			m.Name = "mock-0"
			m.Spec.ReqBodyNoise = map[string][]string{"body.createdAt": {}, "body.other": {}}
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{"mock-0": {Name: "mock-0",
				ReqBodyNoise: map[string][]string{"body.expiresAt": {}},
				Clocks:       map[string]models.ClockOffset{"body.createdAt": plain, "body.expiresAt": ttl}}}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string][]string{"mock-0": {"body.createdAt (clock)", "body.expiresAt (" + ttl.String() + ")"}}
			if !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			got := readBack(t, ys, set, "mock-0")
			if want := (map[string]models.ClockOffset{"body.createdAt": plain, "body.expiresAt": ttl}); !reflect.DeepEqual(got.Spec.Clocks, want) {
				t.Fatalf("clock reads read back %+v, want %+v", got.Spec.Clocks, want)
			}
			if want := (map[string][]string{"body.other": {}}); !reflect.DeepEqual(got.Spec.ReqBodyNoise, want) {
				t.Fatalf("noise read back %v, want %v: a clock read's place is never noise", got.Spec.ReqBodyNoise, want)
			}
			if again, err := ys.PersistLearned(ctx, set, states); err != nil || len(again) != 0 {
				t.Fatalf("what a mock already carries is not added again: %v %v", again, err)
			}

			narrower := models.ClockOffset{Min: ttl.Min + 10*time.Millisecond, Max: ttl.Max, Res: time.Millisecond}
			added, err = ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
				Clocks: map[string]models.ClockOffset{"body.expiresAt": narrower, "body.createdAt": ttl}}})
			if err != nil {
				t.Fatal(err)
			}
			empty := plain.Meet(ttl)
			if !empty.Empty() {
				t.Fatalf("a plain read met with an offset is %+v, not Empty", empty)
			}
			want = map[string][]string{"mock-0": {"body.createdAt (" + empty.String() + ")", "body.expiresAt (" + narrower.String() + ")"}}
			if !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			got = readBack(t, ys, set, "mock-0")
			if want := (map[string]models.ClockOffset{"body.createdAt": empty, "body.expiresAt": narrower}); !reflect.DeepEqual(got.Spec.Clocks, want) {
				t.Fatalf("met clock reads read back %+v, want %+v", got.Spec.Clocks, want)
			}
		})
	}
}

// A read of the clock learned as one word of a text (models.ClockOffset.Word:
// keploy mock's client test naming an app "[E2E] stop-<UnixNano>", read
// since its test began) is written onto the recording with where the word
// stands (no byte of the text), read back the same in every storage format,
// and never added twice: two learnings of the word compare equal. A later
// learning that reads the same place as a read whole is another form of the
// value: their meet is an Empty read, which refuses every value there.
func TestPersistLearnedKeepsAClockWordInEveryFormat(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const name = "[E2E] stop-1793336961102639022"
	word := models.ClockOffset{Res: time.Nanosecond, Since: models.SinceTestBegin, Word: models.ClockWordOf(name, len("[E2E] stop-"), len(name))}
	whole := models.ClockOffset{Res: time.Nanosecond, Since: models.SinceTestBegin}
	for _, f := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()
			ys := clockStore(dir, f)
			ctx := context.Background()
			set := "test-set-0"
			m := noiseTestMock(`{"name":"` + name + `"}`)
			m.Name = "mock-0"
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			states := map[string]models.MockState{"mock-0": {Name: "mock-0", Clocks: map[string]models.ClockOffset{"body.name": word}}}
			added, err := ys.PersistLearned(ctx, set, states)
			if err != nil {
				t.Fatal(err)
			}
			if want := (map[string][]string{"mock-0": {"body.name (" + word.String() + ")"}}); !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			got := readBack(t, ys, set, "mock-0")
			if want := (map[string]models.ClockOffset{"body.name": word}); !reflect.DeepEqual(got.Spec.Clocks, want) {
				t.Fatalf("the word read back %+v, want %+v", got.Spec.Clocks, want)
			}
			if again, err := ys.PersistLearned(ctx, set, states); err != nil || len(again) != 0 {
				t.Fatalf("a word the mock already carries is not added again: %v %v", again, err)
			}

			added, err = ys.PersistLearned(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0", Clocks: map[string]models.ClockOffset{"body.name": whole}}})
			if err != nil {
				t.Fatal(err)
			}
			empty := word.Meet(whole)
			if !empty.Empty() {
				t.Fatalf("a word met with a read whole is %+v, not Empty", empty)
			}
			if want := (map[string][]string{"mock-0": {"body.name (" + empty.String() + ")"}}); !reflect.DeepEqual(added, want) {
				t.Fatalf("added %v, want %v", added, want)
			}
			if got := readBack(t, ys, set, "mock-0"); !reflect.DeepEqual(got.Spec.Clocks, map[string]models.ClockOffset{"body.name": empty}) {
				t.Fatalf("the met read read back %+v, want %+v", got.Spec.Clocks, empty)
			}
		})
	}
}

// Compat: with the switch off nothing learns a clock read, and every rewrite
// of a recording (the noise write-back, the prune of keploy test
// --remove-unused-mocks) carries the clock reads a recording holds, as they
// are, in every storage format, and merges noise as it always has, at a
// clock read's place too: only the switch reads them.
func TestAClockReadSurvivesASwitchOffRewrite(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	clocks := map[string]models.ClockOffset{"body.createdAt": {Res: time.Millisecond}}
	for _, f := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		t.Run(f, func(t *testing.T) {
			dir := t.TempDir()
			ys := clockStore(dir, f)
			ctx := context.Background()
			set := "test-set-0"
			m := noiseTestMock(`{"createdAt":"2026-10-26T10:00:00.123Z","other":"o"}`)
			m.Name = "mock-0"
			m.Spec.Clocks = clocks
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatal(err)
			}
			unused := noiseTestMock(`{"unused":"u"}`)
			unused.Name = "mock-1"
			if err := ys.InsertMock(ctx, unused, set); err != nil {
				t.Fatal(err)
			}
			if err := ys.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ys.PersistMockNoise(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0",
				ReqBodyNoise: map[string][]string{"body.other": {}, "body.createdAt": {}}}}); err != nil {
				t.Fatal(err)
			}
			got := readBack(t, ys, set, "mock-0")
			if !reflect.DeepEqual(got.Spec.Clocks, clocks) {
				t.Fatalf("switch off: the noise write-back left clock reads %+v, want %+v", got.Spec.Clocks, clocks)
			}
			if want := (map[string][]string{"body.createdAt": {}, "body.other": {}}); !reflect.DeepEqual(got.Spec.ReqBodyNoise, want) {
				t.Fatalf("switch off: noise written %v, want %v (merged as it always has)", got.Spec.ReqBodyNoise, want)
			}
			future := time.Now().Add(time.Hour)
			if err := ys.UpdateMocks(ctx, set, map[string]models.MockState{"mock-0": {Name: "mock-0"}}, future, models.BaseTime); err != nil {
				t.Fatal(err)
			}
			got = readBack(t, ys, set, "mock-0")
			if !reflect.DeepEqual(got.Spec.Clocks, clocks) {
				t.Fatalf("switch off: the prune left clock reads %+v, want %+v", got.Spec.Clocks, clocks)
			}
			left, err := ys.GetUnFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			filtered, err := ys.GetFilteredMocks(ctx, set, models.BaseTime, models.BaseTime.AddDate(100, 0, 0), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(left) + len(filtered); n != 1 {
				t.Fatalf("the prune kept %d mocks, want 1: it did not rewrite the file", n)
			}
		})
	}
}

// A kind an out-of-tree mapper decodes (the Mongo parser's, in integrations)
// keeps its document's clock reads, though the mapper rebuilds the mock's
// spec whole: a replay of it still checks each one, rather than read the
// place as recorded.
func TestAMappedKindsClockReadsSurviveItsDecode(t *testing.T) {
	const kind = models.Kind("EnterpriseKind")
	clearRegistry(t)
	RegisterMockYAMLMapper(kind, MockYAMLMapper{
		Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
		Decode: func(doc *yaml.NetworkTrafficDoc, m *models.Mock) error {
			*m = models.Mock{Version: doc.Version, Name: doc.Name, Kind: doc.Kind}
			return nil
		},
	})
	clocks := map[string]models.ClockOffset{"body.createdAt": {Res: time.Millisecond}}
	doc, err := EncodeMock(&models.Mock{Version: models.GetVersion(), Name: "mock-0", Kind: kind, Spec: models.MockSpec{Clocks: clocks}}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Clocks, clocks) {
		t.Fatalf("precondition: the mapped kind's document holds clock reads %+v, want %+v", doc.Clocks, clocks)
	}
	back, err := decodeMocks([]*yaml.NetworkTrafficDoc{doc}, zap.NewNop(), false)
	if err != nil || len(back) != 1 {
		t.Fatalf("read back %d mocks, %v", len(back), err)
	}
	if !reflect.DeepEqual(back[0].Spec.Clocks, clocks) {
		t.Fatalf("the mapped mock read back with clock reads %+v, want %+v", back[0].Spec.Clocks, clocks)
	}
}
