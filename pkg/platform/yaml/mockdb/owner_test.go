// models.Mock.Owner is the persisted half of a mock's identity: the per-test
// scope that owned the capture. These tests pin the three properties the rest
// of the scheme rests on -- it survives every on-disk format, it is absent from
// a set recorded without scopes, and a set written BEFORE the field existed
// still loads.
package mockdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func ownerTestMock(owner string) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Owner:   owner,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "HTTP_CLIENT"},
			HTTPReq: &models.HTTPReq{
				Method: "GET", URL: "http://api/x", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Accept": "*/*"},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   `{"ok":true}`,
			},
			ReqTimestampMock: time.Unix(1_700_000_000, 0).UTC(),
			ResTimestampMock: time.Unix(1_700_000_001, 0).UTC(),
		},
	}
}

// YAML is the default on-disk format; owner must survive a write/read cycle.
func TestOwnerSurvivesYAMLRoundTrip(t *testing.T) {
	doc, err := EncodeMock(ownerTestMock("spec.ts > describe > test one"), zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	raw, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), "owner: spec.ts > describe > test one") {
		t.Fatalf("owner key missing from the encoded doc:\n%s", raw)
	}
	var back yaml.NetworkTrafficDoc
	if err := yamlLib.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{&back}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocks: %v", err)
	}
	if got := mocks[0].Owner; got != "spec.ts > describe > test one" {
		t.Fatalf("owner lost in the yaml round trip: %q", got)
	}
}

func TestOwnerSurvivesJSONRoundTrip(t *testing.T) {
	doc, _, err := EncodeMockJSON(ownerTestMock("owner-json"), zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMockJSON: %v", err)
	}
	if doc.Owner != "owner-json" {
		t.Fatalf("owner not carried onto the JSON doc: %q", doc.Owner)
	}
	mocks, err := DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, zap.NewNop())
	if err != nil {
		t.Fatalf("DecodeMocksJSON: %v", err)
	}
	if got := mocks[0].Owner; got != "owner-json" {
		t.Fatalf("owner lost in the json round trip: %q", got)
	}
}

// gob keys on exported field NAMES and ignores struct tags, so Owner needs no
// gob-specific work -- this pins that it actually reaches disk, the same way
// SourcePID reaches the CLI over the /outgoing gob stream.
func TestOwnerSurvivesGobRoundTrip(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	if err := ys.InsertMock(context.Background(), ownerTestMock("owner-gob"), "set-0"); err != nil {
		t.Fatalf("InsertMock: %v", err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := readGobMocks(filepath.Join(dir, "set-0", "mocks.gob"))
	if err != nil {
		t.Fatalf("readGobMocks: %v", err)
	}
	if len(got) != 1 || got[0].Owner != "owner-gob" {
		t.Fatalf("owner lost in the gob round trip: %+v", got)
	}
}

// A set recorded with no scopes (every `keploy record` integration set, and any
// `keploy mock record` whose runner declares no boundaries) must be written
// exactly as before -- no owner key at all.
func TestEmptyOwnerWritesNoKey(t *testing.T) {
	doc, err := EncodeMock(ownerTestMock(""), zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMock: %v", err)
	}
	raw, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "owner:") {
		t.Fatalf("an unowned mock must not write an owner key:\n%s", raw)
	}
	jdoc, _, err := EncodeMockJSON(ownerTestMock(""), zap.NewNop())
	if err != nil {
		t.Fatalf("EncodeMockJSON: %v", err)
	}
	jraw, err := yaml.MarshalDoc(yaml.FormatJSON, doc)
	if err != nil {
		t.Fatalf("MarshalDoc: %v", err)
	}
	if jdoc.Owner != "" || strings.Contains(string(jraw), `"owner"`) {
		t.Fatalf("an unowned mock must not write an owner key in json:\n%s", jraw)
	}
}

// Backward compatibility: a mocks.yaml written before Owner existed has no
// owner key. It must still load, with Owner empty -- which routes it down the
// unchanged mock-N naming branch.
func TestOldMocksFileWithoutOwnerStillDecodes(t *testing.T) {
	const legacy = `version: api.keploy.io/v1beta1
kind: Http
name: mock-0
spec:
    metadata:
        type: HTTP_CLIENT
    req:
        method: GET
        proto_major: 1
        proto_minor: 1
        url: http://api/x
        header:
            Accept: '*/*'
        body: ""
        timestamp: 2023-11-14T22:13:20Z
    resp:
        status_code: 200
        header:
            Content-Type: application/json
        body: '{"ok":true}'
        status_message: OK
        timestamp: 2023-11-14T22:13:21Z
    objects: []
    created: 0
    reqTimestampMock: 2023-11-14T22:13:20Z
    resTimestampMock: 2023-11-14T22:13:21Z
connectionId: conn-7
`
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "set-0", "mocks.yaml"), []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ys := New(zap.NewNop(), dir, "mocks")
	// DeriveLifetime routes a loaded mock into exactly one of the two pools, so
	// read both and assert on the union.
	filtered, err := ys.GetFilteredMocks(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("GetFilteredMocks on a pre-Owner mocks.yaml: %v", err)
	}
	unfiltered, err := ys.GetUnFilteredMocks(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("GetUnFilteredMocks on a pre-Owner mocks.yaml: %v", err)
	}
	mocks := append(filtered, unfiltered...)
	if len(mocks) != 1 {
		t.Fatalf("want 1 mock from the legacy file, got %d", len(mocks))
	}
	if mocks[0].Name != "mock-0" || mocks[0].ConnectionID != "conn-7" {
		t.Fatalf("legacy mock decoded wrong: %+v", mocks[0])
	}
	if mocks[0].Owner != "" {
		t.Fatalf("a pre-Owner mock must decode with an empty owner, got %q", mocks[0].Owner)
	}
}
