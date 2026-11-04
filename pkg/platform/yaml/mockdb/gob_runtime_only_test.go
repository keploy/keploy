package mockdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// taggedRuntimeOnly is one field of models.Mock, or of a struct it holds by
// value, tagged "-" for both json and bson.
type taggedRuntimeOnly struct {
	name  string
	index []int
}

// runtimeOnlyFieldsOf walks t on its own, apart from the code under test, so a
// walk that misses a field there fails these rows instead of passing them.
func runtimeOnlyFieldsOf(t reflect.Type, prefix string, index []int) []taggedRuntimeOnly {
	var out []taggedRuntimeOnly
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		at := append(append([]int(nil), index...), i)
		if f.Tag.Get("json") == "-" && f.Tag.Get("bson") == "-" {
			out = append(out, taggedRuntimeOnly{prefix + f.Name, at})
		} else if f.Type.Kind() == reflect.Struct {
			out = append(out, runtimeOnlyFieldsOf(f.Type, prefix+f.Name+".", at)...)
		}
	}
	return out
}

// mockRuntimeOnlyFields is every such field of models.Mock. It holds at least
// the fields these tags mark today, so a walk that finds fewer fails here.
func mockRuntimeOnlyFields(t *testing.T) []taggedRuntimeOnly {
	t.Helper()
	fields := runtimeOnlyFieldsOf(reflect.TypeOf(models.Mock{}), "", nil)
	have := map[string]bool{}
	for _, f := range fields {
		have[f.name] = true
	}
	for _, name := range []string{
		"SourcePID", "StartRef",
		"TestModeInfo.Lifetime", "TestModeInfo.LifetimeDerived", "TestModeInfo.Consume",
		"TestModeInfo.IsStartup", "TestModeInfo.ServedIn", "TestModeInfo.StagedSet",
		"TestModeInfo.Concurrent", "TestModeInfo.LoadSeq",
	} {
		if !have[name] {
			t.Fatalf("the walk found no %s among %v", name, fields)
		}
	}
	return fields
}

// setNonZero gives v a value other than its zero value. A field of a kind it
// has no value for fails the row, so a new runtime-only field is never left
// unset.
func setNonZero(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// 1 is LifetimeSession for Lifetime and ConsumeCursorSaturate for
		// Consume: values a recorder or a derivation sets.
		v.SetUint(1)
	case reflect.String:
		v.SetString("runtime-only")
	default:
		t.Fatalf("%s is of kind %s: give it a value here", name, v.Kind())
	}
}

// recordedMock is a mock with its recorded fields set, Start and the
// TestModeInfo fields the text formats leave out among them, and none of its
// runtime-only fields.
func recordedMock(name string) *models.Mock {
	m := &models.Mock{
		Version: "api.keploy.io/v1beta1", Kind: models.HTTP, Name: name,
		Start: "worker-1", ConnectionID: "conn-1", Noise: []string{"n1"},
		Origin: &models.Origin{Clock: "proxy:run-1", Comm: "app"},
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://x/y", Header: map[string]string{"A": "b"}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"X": "v"}, Body: "ok"},
			Created:          1_700_000_000,
			ReqTimestampMock: time.Unix(1_700_000_000, 0).UTC(),
			ResTimestampMock: time.Unix(1_700_000_001, 0).UTC(),
		},
	}
	m.TestModeInfo.ID = 3
	m.TestModeInfo.IsFiltered = true
	m.TestModeInfo.SortOrder = 4
	return m
}

// runtimeOnlyMock is recordedMock with every runtime-only field set as well.
func runtimeOnlyMock(t *testing.T, name string) *models.Mock {
	t.Helper()
	m := recordedMock(name)
	v := reflect.ValueOf(m).Elem()
	for _, f := range mockRuntimeOnlyFields(t) {
		setNonZero(t, f.name, v.FieldByIndex(f.index))
	}
	return m
}

// checkRuntimeOnlyCleared fails unless m holds none of the runtime-only
// fields and is otherwise the mock it was written from, field for field.
func checkRuntimeOnlyCleared(t *testing.T, where string, m *models.Mock) {
	t.Helper()
	v := reflect.ValueOf(m).Elem()
	for _, f := range mockRuntimeOnlyFields(t) {
		if fv := v.FieldByIndex(f.index); !fv.IsZero() {
			t.Errorf("%s: %s = %v, want its zero value (runtime-only)", where, f.name, fv.Interface())
		}
	}
	if want := recordedMock(m.Name); !reflect.DeepEqual(m, want) {
		t.Errorf("%s: the mock read back differs from the one written in more than its runtime-only fields:\n got %+v\nwant %+v", where, m, want)
	}
}

// writeGobFile writes ms as a gob mock file, as an earlier build wrote one,
// runtime-only fields and all.
func writeGobFile(t *testing.T, path string, ms ...*models.Mock) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteString(gobMockMagic)
	enc := gob.NewEncoder(&buf)
	for _, m := range ms {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// decodeGobFileRaw decodes every mock of a gob mock file with a plain gob
// decoder, past the magic header, so it sees what the writer wrote: the
// store's own reader clears the runtime-only fields as it reads.
func decodeGobFileRaw(t *testing.T, path string) []*models.Mock {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	magic := make([]byte, len(gobMockMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != gobMockMagic {
		t.Fatalf("magic %q: %v", magic, err)
	}
	dec := gob.NewDecoder(br)
	var out []*models.Mock
	for {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, &m)
	}
}

// The code under test finds a field by its tags alone: exported, "-" for both
// json and bson, at any depth of the exported structs held by value, a tagged
// struct whole and not its fields, and nothing behind a pointer.
func TestRuntimeOnlyPathsFollowTheTags(t *testing.T) {
	type inner struct {
		Tagged   int `json:"-" bson:"-"`
		Recorded int `json:"recorded" bson:"recorded"`
	}
	type sample struct {
		Tagged       int    `json:"-" bson:"-"`
		JSONOnly     int    `json:"-" bson:"jsonOnly"`
		BSONOnly     int    `json:"bsonOnly" bson:"-"`
		Untagged     string // a field with no tags is recorded by name
		hidden       inner  // no encoder or setter reaches it
		Inner        inner
		TaggedStruct inner  `json:"-" bson:"-"`
		Pointer      *inner `json:"pointer"`
		Deep         struct{ Inner inner }
	}
	_ = sample{}.hidden
	got := runtimeOnlyPaths(reflect.TypeOf(sample{}))
	want := [][]int{{0}, {5, 0}, {6}, {8, 0, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtimeOnlyPaths = %v, want %v", got, want)
	}
	var names []string
	for _, f := range runtimeOnlyFieldsOf(reflect.TypeOf(sample{}), "", nil) {
		names = append(names, f.name)
	}
	if got := strings.Join(names, " "); got != "Tagged Inner.Tagged TaggedStruct Deep.Inner.Tagged" {
		t.Fatalf("the rows' own walk found %q", got)
	}
}

// The gob writer writes none of a mock's runtime-only fields, and every other
// field as it was, Start among them, as the YAML and JSON stores do: the
// on-disk shape does not depend on the storage format. It leaves the
// caller's mock as it was.
func TestGobWriterWritesNoRuntimeOnlyField(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	m := runtimeOnlyMock(t, "mock-0")
	if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	if want := runtimeOnlyMock(t, "mock-0"); !reflect.DeepEqual(m, want) {
		t.Errorf("InsertMock changed the caller's mock:\n got %+v\nwant %+v", m, want)
	}
	path := filepath.Join(dir, "set-0", "mocks.gob")
	raw := decodeGobFileRaw(t, path)
	if len(raw) != 1 {
		t.Fatalf("file holds %d mocks, want 1", len(raw))
	}
	checkRuntimeOnlyCleared(t, "as written", raw[0])

	read, err := readGobMocks(path)
	if err != nil || len(read) != 1 {
		t.Fatalf("read: %v, %d mocks", err, len(read))
	}
	checkRuntimeOnlyCleared(t, "as read", read[0])
}

// The gob reader clears the runtime-only fields of a file that holds them
// (one an earlier build wrote), so it reads as a file this build writes.
func TestGobReaderClearsRuntimeOnlyFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mocks.gob")
	writeGobFile(t, path, runtimeOnlyMock(t, "mock-0"))
	if raw := decodeGobFileRaw(t, path); len(raw) != 1 || !reflect.DeepEqual(raw[0], runtimeOnlyMock(t, "mock-0")) {
		t.Fatalf("the file under test does not hold the runtime-only fields: %+v", raw)
	}
	read, err := readGobMocks(path)
	if err != nil || len(read) != 1 {
		t.Fatalf("read: %v, %d mocks", err, len(read))
	}
	checkRuntimeOnlyCleared(t, "as read", read[0])
}

// The gob reader clears every mock of a file, not only the first it decodes.
func TestGobReaderClearsEveryMockOfAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mocks.gob")
	writeGobFile(t, path, runtimeOnlyMock(t, "mock-0"), runtimeOnlyMock(t, "mock-1"), runtimeOnlyMock(t, "mock-2"))
	read, err := readGobMocks(path)
	if err != nil || len(read) != 3 {
		t.Fatalf("read: %v, %d mocks", err, len(read))
	}
	for _, m := range read {
		checkRuntimeOnlyCleared(t, "as read "+m.Name, m)
	}
}

type visited struct {
	mock             *models.Mock
	perTest, session bool
}

func walkSet(t *testing.T, dir string) []visited {
	t.Helper()
	ys := New(zap.NewNop(), dir, "mocks")
	var out []visited
	if err := ys.WalkTestSetMocks(context.Background(), "set-0", func(m *models.Mock, perTest, session bool) {
		out = append(out, visited{m, perTest, session})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// The replay's visiting read of a gob file an earlier build wrote, holding a
// stamped lifetime opposite to the one derivation gives, routes the mock as
// the same mock stored in YAML is routed, and hands it on without the
// runtime-only fields.
func TestGobVisitingReadClearsRuntimeOnlyFields(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	yamlDir := t.TempDir()
	ysY := New(zap.NewNop(), yamlDir, "mocks")
	if err := ysY.InsertMock(context.Background(), runtimeOnlyMock(t, "mock-0"), "set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ysY.Close(); err != nil {
		t.Fatal(err)
	}
	ySeen := walkSet(t, yamlDir)
	if len(ySeen) != 1 {
		t.Fatalf("yaml walk saw %d mocks", len(ySeen))
	}

	// Stamp the opposite of what derivation gives, as a recorder's tag would.
	probe := recordedMock("mock-0")
	probe.DeriveLifetime()
	stamped := runtimeOnlyMock(t, "mock-0")
	if probe.TestModeInfo.Lifetime == models.LifetimeSession {
		stamped.TestModeInfo.Lifetime = models.LifetimePerTest
	} else {
		stamped.TestModeInfo.Lifetime = models.LifetimeSession
	}
	stamped.TestModeInfo.LifetimeDerived = true

	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	gobDir := t.TempDir()
	writeGobFile(t, filepath.Join(gobDir, "set-0", "mocks.gob"), stamped)
	gSeen := walkSet(t, gobDir)
	if len(gSeen) != 1 {
		t.Fatalf("gob walk saw %d mocks", len(gSeen))
	}
	g, y := gSeen[0], ySeen[0]
	if g.perTest != y.perTest || g.session != y.session {
		t.Errorf("gob routed perTest=%v session=%v, yaml routed perTest=%v session=%v: the stamped lifetime was kept", g.perTest, g.session, y.perTest, y.session)
	}
	// Both reads derive the lifetime; the rest of the runtime-only fields
	// must read the same from either file.
	gm, ym := g.mock, y.mock
	if gm.TestModeInfo.Lifetime != ym.TestModeInfo.Lifetime || gm.TestModeInfo.Consume != ym.TestModeInfo.Consume {
		t.Errorf("gob read Lifetime=%v Consume=%v, yaml read Lifetime=%v Consume=%v", gm.TestModeInfo.Lifetime, gm.TestModeInfo.Consume, ym.TestModeInfo.Lifetime, ym.TestModeInfo.Consume)
	}
	derived := func(m *models.Mock) *models.Mock {
		c := *m
		c.TestModeInfo.Lifetime, c.TestModeInfo.LifetimeDerived, c.TestModeInfo.Consume = 0, false, 0
		return &c
	}
	checkRuntimeOnlyCleared(t, "visited from gob", derived(gm))
}

// A gob writer that carries an earlier build's file into its new stream
// writes the carried mocks without the runtime-only fields.
func TestGobCarryOverWritesNoRuntimeOnlyField(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	path := filepath.Join(dir, "set-0", "mocks.gob")
	writeGobFile(t, path, runtimeOnlyMock(t, "mock-0"))
	ys := New(zap.NewNop(), dir, "mocks")
	ys.SetCounterID(1)
	if err := ys.InsertMock(context.Background(), runtimeOnlyMock(t, "mock-1"), "set-0"); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	raw := decodeGobFileRaw(t, path)
	// The store names the new mock from its counter.
	if len(raw) != 2 || raw[0].Name != "mock-0" {
		t.Fatalf("file holds %d mocks, want mock-0 carried and the new one", len(raw))
	}
	for _, m := range raw {
		checkRuntimeOnlyCleared(t, "raw "+m.Name, m)
	}
}

// The prune of a gob file an earlier build wrote drops what the prune of the
// same mocks in YAML drops, and writes the mocks it keeps without the
// runtime-only fields. The three mocks are recordings of one HTTP request.
// Read with Consume cursor-saturate left on them (on the two config mocks,
// mock-0 and mock-2, it is stale: a read derives them without it), they were
// one stateful sequence, which the prune keeps whole: it kept mock-1, which
// no test consumed, for the sake of the two a test did.
func TestGobPruneDropsWhatYAMLDropsAndWritesNoRuntimeOnlyField(t *testing.T) {
	ctx := context.Background()
	pruneBefore := time.Unix(1_800_000_000, 0)
	names := map[string]models.MockState{"mock-0": {}, "mock-2": {}}
	mocks := func() []*models.Mock {
		dropped := runtimeOnlyMock(t, "mock-1")
		dropped.Spec.Metadata = nil // not config: the prune may drop it
		return []*models.Mock{runtimeOnlyMock(t, "mock-0"), dropped, runtimeOnlyMock(t, "mock-2")}
	}

	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	yamlDir := t.TempDir()
	ysY := New(zap.NewNop(), yamlDir, "mocks")
	for _, m := range mocks() {
		if err := ysY.InsertMock(ctx, m, "set-0"); err != nil {
			t.Fatal(err)
		}
	}
	if err := ysY.Close(); err != nil {
		t.Fatal(err)
	}
	if err := New(zap.NewNop(), yamlDir, "mocks").UpdateMocks(ctx, "set-0", names, pruneBefore, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var yamlKept []string
	for _, v := range walkSet(t, yamlDir) {
		yamlKept = append(yamlKept, v.mock.Name)
	}
	if strings.Join(yamlKept, " ") != "mock-0 mock-2" {
		t.Fatalf("the yaml prune kept %v, want [mock-0 mock-2]", yamlKept)
	}

	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	path := filepath.Join(dir, "set-0", "mocks.gob")
	writeGobFile(t, path, mocks()...)
	if err := New(zap.NewNop(), dir, "mocks").UpdateMocks(ctx, "set-0", names, pruneBefore, time.Time{}); err != nil {
		t.Fatal(err)
	}
	raw := decodeGobFileRaw(t, path)
	var kept []string
	for _, m := range raw {
		kept = append(kept, m.Name)
	}
	if strings.Join(kept, " ") != strings.Join(yamlKept, " ") {
		t.Fatalf("the gob prune kept %v, the yaml prune of the same mocks kept %v", kept, yamlKept)
	}
	for _, m := range raw {
		checkRuntimeOnlyCleared(t, "pruned file "+m.Name, m)
	}
}
