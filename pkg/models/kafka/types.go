// Package kafka provides types for Kafka request/response handling.
package kafka

// ApiKey represents a Kafka API key
type ApiKey int16

// Kafka API Keys
const (
	Produce            ApiKey = 0
	Fetch              ApiKey = 1
	ListOffsets        ApiKey = 2
	Metadata           ApiKey = 3
	LeaderAndIsr       ApiKey = 4
	StopReplica        ApiKey = 5
	UpdateMetadata     ApiKey = 6
	ControlledShutdown ApiKey = 7
	OffsetCommit       ApiKey = 8
	OffsetFetch        ApiKey = 9
	FindCoordinator    ApiKey = 10
	JoinGroup          ApiKey = 11
	Heartbeat          ApiKey = 12
	LeaveGroup         ApiKey = 13
	SyncGroup          ApiKey = 14
	DescribeGroups     ApiKey = 15
	ListGroups         ApiKey = 16
	SaslHandshake      ApiKey = 17
	ApiVersions        ApiKey = 18
	CreateTopics       ApiKey = 19
	DeleteTopics       ApiKey = 20
	DeleteRecords      ApiKey = 21
	InitProducerId     ApiKey = 22
	OffsetForLeader    ApiKey = 23
	AddPartitions      ApiKey = 24
	AddOffsets         ApiKey = 25
	EndTxn             ApiKey = 26
	WriteTxnMarkers    ApiKey = 27
	TxnOffsetCommit    ApiKey = 28
	DescribeAcls       ApiKey = 29
	CreateAcls         ApiKey = 30
	DeleteAcls         ApiKey = 31
	DescribeConfigs    ApiKey = 32
	AlterConfigs       ApiKey = 33
)

// ApiKeyToString maps API keys to their string names
var ApiKeyToString = map[ApiKey]string{
	Produce:            "Produce",
	Fetch:              "Fetch",
	ListOffsets:        "ListOffsets",
	Metadata:           "Metadata",
	LeaderAndIsr:       "LeaderAndIsr",
	StopReplica:        "StopReplica",
	UpdateMetadata:     "UpdateMetadata",
	ControlledShutdown: "ControlledShutdown",
	OffsetCommit:       "OffsetCommit",
	OffsetFetch:        "OffsetFetch",
	FindCoordinator:    "FindCoordinator",
	JoinGroup:          "JoinGroup",
	Heartbeat:          "Heartbeat",
	LeaveGroup:         "LeaveGroup",
	SyncGroup:          "SyncGroup",
	DescribeGroups:     "DescribeGroups",
	ListGroups:         "ListGroups",
	SaslHandshake:      "SaslHandshake",
	ApiVersions:        "ApiVersions",
	CreateTopics:       "CreateTopics",
	DeleteTopics:       "DeleteTopics",
	DeleteRecords:      "DeleteRecords",
	InitProducerId:     "InitProducerId",
	OffsetForLeader:    "OffsetForLeaderEpoch",
	AddPartitions:      "AddPartitionsToTxn",
	AddOffsets:         "AddOffsetsToTxn",
	EndTxn:             "EndTxn",
	WriteTxnMarkers:    "WriteTxnMarkers",
	TxnOffsetCommit:    "TxnOffsetCommit",
	DescribeAcls:       "DescribeAcls",
	CreateAcls:         "CreateAcls",
	DeleteAcls:         "DeleteAcls",
	DescribeConfigs:    "DescribeConfigs",
	AlterConfigs:       "AlterConfigs",
}

// RequestHeader represents a Kafka request header
type RequestHeader struct {
	ApiKey        ApiKey `json:"apiKey" yaml:"apiKey"`
	ApiVersion    int16  `json:"apiVersion" yaml:"apiVersion"`
	CorrelationID int32  `json:"correlationId" yaml:"correlationId"`
	ClientID      string `json:"clientId" yaml:"clientId"`
}

// ResponseHeader represents a Kafka response header
type ResponseHeader struct {
	CorrelationID int32 `json:"correlationId" yaml:"correlationId"`
}

// Request represents a Kafka request with header and body
type Request struct {
	Header     RequestHeader `json:"header" yaml:"header"`
	APIKeyName string        `json:"apiKeyName" yaml:"apiKeyName"`
	// Body contains the raw payload as base64-encoded string or structured data
	Body interface{} `json:"body" yaml:"body"`
}

// Response represents a Kafka response with header and body
type Response struct {
	Header ResponseHeader `json:"header" yaml:"header"`
	// Body contains the raw payload as base64-encoded string or structured data
	Body interface{} `json:"body" yaml:"body"`
}

// PacketInfo stores packet metadata
type PacketInfo struct {
	Length int32 `json:"length" yaml:"length"`
}
