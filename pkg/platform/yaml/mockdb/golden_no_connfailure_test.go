package mockdb

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// A test set with no connection failure, every document of which decodes, is
// written, read and rewritten byte for byte as the goldens hold it: the mock
// file a recording writes, the file a keep-everything prune rewrites it to
// (every document decoded and encoded again), the pools a replay reads from
// it, and a mocks.gob read back. The goldens pin the encoders' output for such
// a set, so any change to what an encoder writes trips this test; when that
// change is intended, regenerate them.
//
// To regenerate: KEPLOY_GOLDEN_OUT=<dir> go test -run
// TestASetWithoutConnectionFailuresIsUnchanged ./pkg/platform/yaml/mockdb/,
// then copy <dir> to testdata/no_connfailure_golden. To check a change that
// must leave such a set alone, generate them on a checkout from before the
// change, with this file added to it, and run this test after the change.
const goldenNoConnFailureDir = "testdata/no_connfailure_golden"

// goldenCorpus is a mock of every kind keploy OSS reads and writes.
func goldenCorpus(t *testing.T) []*models.Mock {
	t.Helper()
	at := time.Date(2026, 3, 4, 5, 6, 7, 891011000, time.UTC)
	mocks := inPlaceCorpus(t) // MySQL, DNS, HTTP, Generic, HTTP/2
	mocks = append(mocks,
		pruneMongoMock(7),
		pruneDNSConfigMock(8),
		&models.Mock{Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: "mock-grpc",
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "mocks"},
				GRPCReq: &models.GrpcReq{
					Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.Orders/Get", ":method": "POST"}, OrdinaryHeaders: map[string]string{"content-type": "application/grpc"}},
					Body:    models.GrpcLengthPrefixedMessage{MessageLength: 4, DecodedData: "1: 42\n"},
				},
				GRPCResp: &models.GrpcResp{
					Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}, OrdinaryHeaders: map[string]string{"grpc-status": "0"}},
					Body:    models.GrpcLengthPrefixedMessage{MessageLength: 6, DecodedData: "1: \"ok\"\n"},
				},
				ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
			}},
		&models.Mock{Version: models.GetVersion(), Kind: models.PostgresV2, Name: "mock-pg2",
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "mocks", "connID": "1"},
				PostgresRequestsV2: []postgres.Request{{PacketBundle: postgres.PacketBundle{Packets: []postgres.Packet{{
					Header:  &postgres.PacketInfo{Type: "Query", Header: &postgres.Header{PayloadLength: 9, PacketID: "Q"}},
					Message: map[string]interface{}{"query": "SELECT 1"},
				}}}}},
				PostgresResponsesV2: []postgres.Response{{PacketBundle: postgres.PacketBundle{Packets: []postgres.Packet{{
					Header:  &postgres.PacketInfo{Type: "CommandComplete", Header: &postgres.Header{PayloadLength: 5, PacketID: "C"}},
					Message: map[string]interface{}{"tag": "SELECT 1"},
				}}}}},
				ReqTimestampMock: at.Add(time.Second), ResTimestampMock: at.Add(time.Second + time.Millisecond),
			}},
		&models.Mock{Version: models.GetVersion(), Kind: models.PostgresV3, Name: "mock-pg3",
			Spec: models.MockSpec{
				Metadata: map[string]string{"type": "config", "connID": "0"},
				PostgresV3: &models.PostgresV3Spec{
					Type: models.PostgresV3TypeSession,
					Session: &models.PostgresV3SessionSpec{
						ProtocolVersion: "3.0", SSLResponse: "N", ServerVersion: "15.17",
						ParameterStatus:  map[string]string{"DateStyle": "ISO, MDY", "client_encoding": "UTF8"},
						BackendProcessID: 573, BackendSecretKey: -271483429, ObservedAuthMode: "scram",
					},
				},
				ReqTimestampMock: at.Add(2 * time.Second), ResTimestampMock: at.Add(2*time.Second + time.Millisecond),
			}},
	)
	return mocks
}

// checkGolden compares got with the golden file name, or writes it under
// KEPLOY_GOLDEN_OUT when that is set.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if out := os.Getenv("KEPLOY_GOLDEN_OUT"); out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden %s (%d bytes)", name, len(got))
		return
	}
	want, err := os.ReadFile(filepath.Join(goldenNoConnFailureDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("the encoders' output for a set without connection failures changed: %s differs from the golden, first at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
		}
	}
	t.Fatalf("the encoders' output for a set without connection failures changed: %s differs from the golden (%d bytes, want %d)", name, len(got), len(want))
}

func TestASetWithoutConnectionFailuresIsUnchanged(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			corpus := goldenCorpus(t)
			path := writePruneFixture(t, dir, format, corpus, "")
			recorded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "recorded."+format, recorded)

			f := yaml.FormatYAML
			if format == "json" {
				f = yaml.FormatJSON
			}
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", f)
			set, err := ys.GetTestSetMocks(context.Background(), pruneSet, models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			pools := fmt.Sprintf("filtered %s\nunfiltered %s\nallPerTest %s\nallSession %s\n",
				poolString(set.Filtered), poolString(set.Unfiltered), poolString(set.AllPerTest), poolString(set.AllSession))
			checkGolden(t, "pools."+format, []byte(pools))

			// Keep everything: every document is decoded and encoded again.
			all := map[string]models.MockState{}
			for i := range corpus {
				n := fmt.Sprintf("mock-%d", i)
				all[n] = models.MockState{Name: n}
			}
			if err := ys.UpdateMocks(context.Background(), pruneSet, all, pruneT0, time.Time{}); err != nil {
				t.Fatalf("UpdateMocks: %v", err)
			}
			rewritten, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "rewritten."+format, rewritten)
		})
	}

	// gob writes a map in Go's random iteration order, so its bytes are never
	// the same twice. What must hold is that the golden mocks.gob reads back
	// exactly as this keploy's own does.
	t.Run("gob", func(t *testing.T) {
		dir := t.TempDir()
		path := writePruneFixture(t, dir, "gob", goldenCorpus(t), "")
		if out := os.Getenv("KEPLOY_GOLDEN_OUT"); out != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, "recorded.gob", b)
			return
		}
		ours, err := readGobMocks(path)
		if err != nil {
			t.Fatal(err)
		}
		golden, err := readGobMocks(filepath.Join(goldenNoConnFailureDir, "recorded.gob"))
		if err != nil {
			t.Fatalf("read the golden mocks.gob: %v", err)
		}
		if !reflect.DeepEqual(golden, ours) {
			t.Fatalf("the golden mocks.gob for a set without connection failures reads back differently: %d mocks, ours %d", len(golden), len(ours))
		}
	})
}
