package mockdb

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	yamlLib "gopkg.in/yaml.v3"
)

// encode_inplace.go writes a mock to a YAML file in one pass.
//
// EncodeMock builds a yaml.NetworkTrafficDoc whose spec is a yaml.Node, and
// yaml.v3 builds a Node by marshaling the value to text and parsing that text
// back (Node.Encode). A MySQL mock pays that for every wire message first
// (mysql.RequestYaml and ResponseYaml carry theirs as Nodes), then once more
// for the spec that holds them, and the file's encoder then emits the node:
// the text of a message was made three times and parsed twice. That made the
// recording CLI persist the production shape's ~20 KB MySQL mocks at ~300 a
// second on a busy host, against the agent's ~1,000: its queue overflowed and
// mocks were lost (the mysql-capture-starved lane, F7). Marshaled in place,
// the same bytes are written in one pass (TestEncodeMockInPlaceWritesWhatEncodeMockWrites).
//
// The kinds whose spec needs its nodes (Mongo's and Postgres v2's wire
// messages, Postgres v3's sanitized strings) and every kind a MockYAMLMapper
// owns go through EncodeMock as before.

// inPlaceDoc is yaml.NetworkTrafficDoc with the spec marshaled in place: the
// same keys, in the same order.
type inPlaceDoc struct {
	Version      models.Version      `yaml:"version"`
	Kind         models.Kind         `yaml:"kind"`
	Name         string              `yaml:"name"`
	Spec         any                 `yaml:"spec"`
	Async        *models.AsyncMeta   `yaml:"async,omitempty"`
	Noise        *yaml.DocNoise      `yaml:"noise,omitempty"`
	LastUpdated  *models.LastUpdated `yaml:"last_updated,omitempty"`
	Curl         string              `yaml:"curl,omitempty"`
	ConnectionID string              `yaml:"connectionId,omitempty"`
}

// mysqlPacketInPlace is mysql.RequestYaml (and ResponseYaml, the same shape)
// with its wire message marshaled in place.
type mysqlPacketInPlace struct {
	Header  *mysql.PacketInfo `yaml:"header"`
	Meta    map[string]string `yaml:"meta,omitempty"`
	Message any               `yaml:"message"`
}

// mysqlSpecInPlace is mysql.Spec with its packets marshaled in place. Its keys
// are mysql.Spec's: the timestamps have no yaml tag there, so yaml.v3 names
// them after the lowercased field.
type mysqlSpecInPlace struct {
	Metadata         map[string]string    `yaml:"metadata"`
	Requests         []mysqlPacketInPlace `yaml:"requests"`
	Response         []mysqlPacketInPlace `yaml:"responses"`
	CreatedAt        int64                `yaml:"created,omitempty"`
	ReqTimestampMock time.Time            `yaml:"reqtimestampmock"`
	ResTimestampMock time.Time            `yaml:"restimestampmock"`
}

// encodeMockInPlace is what EncodeMock encodes mock to, as a value the YAML
// encoder marshals in one pass; false for a kind it leaves to EncodeMock.
func encodeMockInPlace(mock *models.Mock) (any, bool) {
	if mock == nil || hasMapperForKind(mock.Kind) {
		return nil, false
	}
	doc := inPlaceDoc{
		Version:      mock.Version,
		Kind:         mock.Kind,
		Name:         mock.Name,
		ConnectionID: mock.ConnectionID,
		Async:        mock.Spec.Async,
		Noise:        yaml.NewDocNoise(mock.Noise, mock.Spec.ReqBodyNoise),
	}
	switch mock.Kind {
	case models.MySQL:
		spec := mysqlSpecInPlace{
			Metadata:         mock.Spec.Metadata,
			Requests:         make([]mysqlPacketInPlace, 0, len(mock.Spec.MySQLRequests)),
			Response:         make([]mysqlPacketInPlace, 0, len(mock.Spec.MySQLResponses)),
			CreatedAt:        mock.Spec.Created,
			ReqTimestampMock: mock.Spec.ReqTimestampMock,
			ResTimestampMock: mock.Spec.ResTimestampMock,
		}
		for _, v := range mock.Spec.MySQLRequests {
			spec.Requests = append(spec.Requests, mysqlPacketInPlace{Header: v.Header, Meta: v.Meta, Message: v.Message})
		}
		for _, v := range mock.Spec.MySQLResponses {
			spec.Response = append(spec.Response, mysqlPacketInPlace{Header: v.Header, Meta: v.Meta, Message: v.Message})
		}
		doc.Spec = spec
	case models.HTTP:
		doc.Spec = httpSpecOf(mock)
	case models.DNS:
		doc.Spec = dnsSpecOf(mock)
	case models.GENERIC:
		doc.Spec = genericSpecOf(mock)
	case models.GRPC_EXPORT:
		doc.Spec = grpcSpecOf(mock)
	case models.HTTP2:
		doc.Spec = http2SpecOf(mock)
	default:
		return nil, false
	}
	return &doc, true
}

// The specs EncodeMock and encodeMockInPlace both write, built once.

func httpSpecOf(mock *models.Mock) models.HTTPSchema {
	return models.HTTPSchema{
		Metadata:         mock.Spec.Metadata,
		Request:          *mock.Spec.HTTPReq,
		Response:         *mock.Spec.HTTPResp,
		Created:          mock.Spec.Created,
		ReqTimestampMock: mock.Spec.ReqTimestampMock,
		ResTimestampMock: mock.Spec.ResTimestampMock,
	}
}

func dnsSpecOf(mock *models.Mock) models.DNSSchema {
	var req models.DNSReq
	if mock.Spec.DNSReq != nil {
		req = *mock.Spec.DNSReq
	}
	var resp models.DNSResp
	if mock.Spec.DNSResp != nil {
		resp = *mock.Spec.DNSResp
	}
	return models.DNSSchema{
		Metadata:         mock.Spec.Metadata,
		Request:          req,
		Response:         resp,
		ReqTimestampMock: mock.Spec.ReqTimestampMock,
		ResTimestampMock: mock.Spec.ResTimestampMock,
	}
}

func genericSpecOf(mock *models.Mock) models.GenericSchema {
	return models.GenericSchema{
		Metadata:         mock.Spec.Metadata,
		GenericRequests:  mock.Spec.GenericRequests,
		GenericResponses: mock.Spec.GenericResponses,
		ReqTimestampMock: mock.Spec.ReqTimestampMock,
		ResTimestampMock: mock.Spec.ResTimestampMock,
	}
}

func grpcSpecOf(mock *models.Mock) models.GrpcSpec {
	return models.GrpcSpec{
		Metadata:         mock.Spec.Metadata,
		GrpcReq:          *mock.Spec.GRPCReq,
		GrpcResp:         *mock.Spec.GRPCResp,
		ReqTimestampMock: mock.Spec.ReqTimestampMock,
		ResTimestampMock: mock.Spec.ResTimestampMock,
	}
}

func http2SpecOf(mock *models.Mock) models.HTTP2Schema {
	var req models.HTTP2Req
	if mock.Spec.HTTP2Req != nil {
		req = *mock.Spec.HTTP2Req
	}
	var resp models.HTTP2Resp
	if mock.Spec.HTTP2Resp != nil {
		resp = *mock.Spec.HTTP2Resp
	}
	return models.HTTP2Schema{
		Metadata:         mock.Spec.Metadata,
		Request:          req,
		Response:         resp,
		Created:          mock.Spec.Created,
		ReqTimestampMock: mock.Spec.ReqTimestampMock,
		ResTimestampMock: mock.Spec.ResTimestampMock,
	}
}

// encodeYAMLDoc marshals v into buf as one YAML document. yaml.v3 panics on a
// value it cannot marshal (a channel, a func) rather than failing: that is
// this mock's payload fault, an error, not the end of the recording.
func encodeYAMLDoc(buf *bytes.Buffer, v any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("yaml: %v", r)
		}
	}()
	enc := yamlLib.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		_ = enc.Close()
		return err
	}
	return enc.Close()
}

// yamlBuffers are the buffers InsertMock makes a document in: one per mock
// written at a time, not one allocated per mock.
var yamlBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// yamlBufferKeep is the largest buffer kept for the next mock: a rare
// multi-MB result is not held for the rest of the recording.
const yamlBufferKeep = 1 << 20

func getYAMLBuffer() *bytes.Buffer {
	b := yamlBuffers.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putYAMLBuffer(b *bytes.Buffer) {
	if b.Cap() <= yamlBufferKeep {
		yamlBuffers.Put(b)
	}
}
