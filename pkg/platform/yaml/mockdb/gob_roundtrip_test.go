// Round-trip tests for the gob mock format. For every supported
// protocol: build a populated *models.Mock, write it through
// InsertMock (async path), Close to drain, readGobMocks, assert
// reflect.DeepEqual. Critical because MySQL/MongoDB/Postgres store
// their command payloads as interface{} — gob rebinds concrete types
// via the gob.Register calls in pkg/models/*.
package mockdb

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/models/postgres"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

func roundTrip(t *testing.T, name string, mock *models.Mock) {
	t.Helper()
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	if err := ys.InsertMock(context.Background(), mock, "set-0"); err != nil {
		t.Fatalf("%s InsertMock: %v", name, err)
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("%s Close: %v", name, err)
	}
	got, err := readGobMocks(filepath.Join(dir, "set-0", "mocks.gob"))
	if err != nil {
		t.Fatalf("%s readGobMocks: %v", name, err)
	}
	if len(got) != 1 {
		t.Fatalf("%s: want 1 mock, got %d", name, len(got))
	}
	expected := *mock
	expected.Name = got[0].Name
	if !reflect.DeepEqual(got[0], &expected) {
		t.Fatalf("%s: round-trip mismatch\nwant %#v\n got %#v", name, &expected, got[0])
	}
}

func TestRoundTrip_HTTP(t *testing.T) {
	roundTrip(t, "HTTP", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata: map[string]string{"src": "test"},
			HTTPReq: &models.HTTPReq{
				Method: "POST", URL: "http://x/y", ProtoMajor: 1, ProtoMinor: 1,
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   `{"a":1}`,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200, StatusMessage: "OK",
				Header: map[string]string{"X-K": "v"}, Body: `{"ok":true}`,
			},
			ReqTimestampMock: time.Unix(1_700_000_000, 0).UTC(),
			ResTimestampMock: time.Unix(1_700_000_001, 0).UTC(),
		},
	})
}

func TestRoundTrip_Redis(t *testing.T) {
	roundTrip(t, "Redis", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.REDIS,
		Spec: models.MockSpec{
			RedisRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "bin", Data: "cGluZwo="}}}},
			RedisResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "bin", Data: "UE9OR1xy"}}}},
		},
	})
}

func TestRoundTrip_Generic(t *testing.T) {
	roundTrip(t, "Generic", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.GENERIC,
		Spec: models.MockSpec{
			GenericRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "utf-8", Data: "hello"}}}},
			GenericResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "utf-8", Data: "world"}}}},
		},
	})
}

func TestRoundTrip_Kafka(t *testing.T) {
	roundTrip(t, "Kafka", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.KAFKA,
		Spec: models.MockSpec{
			KafkaRequests:  []models.Payload{{Origin: models.FromClient, Message: []models.OutputBinary{{Type: "bin", Data: "AAA="}}}},
			KafkaResponses: []models.Payload{{Origin: models.FromServer, Message: []models.OutputBinary{{Type: "bin", Data: "AAE="}}}},
		},
	})
}

func TestRoundTrip_Postgres(t *testing.T) {
	roundTrip(t, "Postgres", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.PostgresV2,
		Spec: models.MockSpec{
			PostgresRequestsV2: []postgres.Request{{
				PacketBundle: postgres.PacketBundle{
					Packets: []postgres.Packet{{
						Header:  &postgres.PacketInfo{Type: "Query", Header: &postgres.Header{PayloadLength: 9, PacketID: "Q"}},
						Message: map[string]interface{}{"query": "SELECT 1"},
					}},
				},
			}},
			PostgresResponsesV2: []postgres.Response{{
				PacketBundle: postgres.PacketBundle{
					Packets: []postgres.Packet{{
						Header:  &postgres.PacketInfo{Type: "CommandComplete", Header: &postgres.Header{PayloadLength: 5, PacketID: "C"}},
						Message: map[string]interface{}{"tag": "SELECT 1"},
					}},
				},
			}},
		},
	})
}

func TestRoundTrip_MySQL(t *testing.T) {
	roundTrip(t, "MySQL", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 13, SequenceID: 0},
						Type:   "COM_QUERY",
					},
					Message: &mysql.QueryPacket{Command: 0x03, Query: "SELECT 1"},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header: &mysql.PacketInfo{
						Header: &mysql.Header{PayloadLength: 7, SequenceID: 1},
						Type:   "OK_PACKET",
					},
					Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
				},
			}},
		},
	})
}

func TestRoundTrip_Mongo(t *testing.T) {
	roundTrip(t, "Mongo", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.Mongo,
		Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{
				Header: &models.MongoHeader{Length: 50, RequestID: 1, ResponseTo: 0, Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{FlagBits: 0, Sections: []string{`{"find":"c"}`}, Checksum: 0},
			}},
			MongoResponses: []models.MongoResponse{{
				Header: &models.MongoHeader{Length: 60, RequestID: 2, ResponseTo: 1, Opcode: wiremessage.OpMsg},
				Message: &models.MongoOpMessage{FlagBits: 0, Sections: []string{`{"ok":1}`}, Checksum: 0},
			}},
		},
	})
}

func TestRoundTrip_DNS(t *testing.T) {
	roundTrip(t, "DNS", &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			DNSReq:  &models.DNSReq{Name: "example.com.", Qtype: 1, Qclass: 1},
			DNSResp: &models.DNSResp{Rcode: 0, Authoritative: true, RecursionAvailable: true, Answers: []string{"example.com.\t60\tIN\tA\t1.2.3.4"}},
		},
	})
}

func TestRoundTrip_MultipleMocksAppend(t *testing.T) {
	// Ensure appending a second mock via the async writer doesn't
	// corrupt the first. Persistent encoder writes one continuous
	// stream; readGobMocks handles both one-per-call and one-per-file
	// layouts by re-creating the decoder on every iteration.
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	dir := t.TempDir()
	ys := New(zap.NewNop(), dir, "mocks")
	a := &models.Mock{Version: "v1", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://a"}}}
	b := &models.Mock{Version: "v1", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://b"}}}
	for _, m := range []*models.Mock{a, b} {
		if err := ys.InsertMock(context.Background(), m, "set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := readGobMocks(filepath.Join(dir, "set-0", "mocks.gob"))
	if err != nil {
		t.Fatalf("readGobMocks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 mocks, got %d", len(got))
	}
	if got[0].Spec.HTTPReq.URL != "http://a" || got[1].Spec.HTTPReq.URL != "http://b" {
		t.Fatalf("unexpected order/content: %s %s", got[0].Spec.HTTPReq.URL, got[1].Spec.HTTPReq.URL)
	}
}
