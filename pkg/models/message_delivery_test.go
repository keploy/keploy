package models

import (
	"context"
	"strconv"
	"testing"
)

func sqsJSONMock(name, target string, status int, respBody string) *Mock {
	m := &Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    HTTP,
		Spec: MockSpec{
			Metadata: map[string]string{
				"type":     "HTTP_CLIENT",
				"lifetime": MetadataLifetimePerTest,
			},
			HTTPReq: &HTTPReq{
				Method: Method("POST"),
				URL:    "/000000000000/localstack-queue/",
				Header: map[string]string{
					"Content-Type": "application/x-amz-json-1.0",
					"X-Amz-Target": target,
				},
				Body: `{"MaxNumberOfMessages":10,"QueueUrl":"http://sqs/q","WaitTimeSeconds":4}`,
			},
			HTTPResp: &HTTPResp{
				StatusCode: status,
				Header:     map[string]string{"Content-Type": "application/x-amz-json-1.0"},
				Body:       respBody,
			},
		},
	}
	m.DeriveLifetime()
	return m
}

func sqsXMLMock(name, reqURL, contentType, reqBody string, status int, respBody string) *Mock {
	hdr := map[string]string{}
	if contentType != "" {
		hdr["Content-Type"] = contentType
	}
	m := &Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    HTTP,
		Spec: MockSpec{
			Metadata: map[string]string{
				"type":     "HTTP_CLIENT",
				"lifetime": MetadataLifetimePerTest,
			},
			HTTPReq: &HTTPReq{
				Method: Method("POST"),
				URL:    reqURL,
				Header: hdr,
				Body:   reqBody,
			},
			HTTPResp: &HTTPResp{
				StatusCode: status,
				Header:     map[string]string{"Content-Type": "text/xml"},
				Body:       respBody,
			},
		},
	}
	m.DeriveLifetime()
	return m
}

func kafkaFetchMock(name string, apiKey int, respJSON string) *Mock {
	m := &Mock{
		Version: "api.keploy.io/v1beta1",
		Name:    name,
		Kind:    KAFKA,
		Spec: MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			GenericRequests: []Payload{
				{Message: []OutputBinary{{Data: `{"header":{"apiKey":` + strconv.Itoa(apiKey) + `,"apiVersion":11,"correlationId":4}}`}}},
			},
			GenericResponses: []Payload{
				{Message: []OutputBinary{{Data: respJSON}}},
			},
		},
	}
	m.DeriveLifetime()
	return m
}

func TestIsMessageDelivery_SQS(t *testing.T) {
	xmlWithMsg := `<?xml version='1.0' encoding='utf-8'?>
<ReceiveMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/"><ReceiveMessageResult><Message><MessageId>m-1</MessageId><Body>Test message 1</Body></Message></ReceiveMessageResult><ResponseMetadata><RequestId>r1</RequestId></ResponseMetadata></ReceiveMessageResponse>`
	xmlEmpty := `<?xml version='1.0' encoding='utf-8'?>
<ReceiveMessageResponse xmlns="http://queue.amazonaws.com/doc/2012-11-05/"><ReceiveMessageResult /><ResponseMetadata><RequestId>r2</RequestId></ResponseMetadata></ReceiveMessageResponse>`

	cases := []struct {
		name string
		mock *Mock
		want bool
	}{
		{
			name: "SQS JSON ReceiveMessage with messages",
			mock: sqsJSONMock("sqs-json-msg", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"MessageId":"1","Body":"Test message 1"}]}`),
			want: true,
		},
		{
			name: "SQS JSON ReceiveMessage lowercase header",
			mock: func() *Mock {
				m := sqsJSONMock("sqs-json-lc", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"hi"}]}`)
				delete(m.Spec.HTTPReq.Header, "X-Amz-Target")
				m.Spec.HTTPReq.Header["x-amz-target"] = " AmazonSQS.ReceiveMessage "
				return m
			}(),
			want: true,
		},
		{
			name: "SQS JSON ReceiveMessage empty object",
			mock: sqsJSONMock("sqs-json-empty", "AmazonSQS.ReceiveMessage", 200, `{}`),
			want: false,
		},
		{
			name: "SQS JSON ReceiveMessage empty array",
			mock: sqsJSONMock("sqs-json-empty-arr", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[]}`),
			want: false,
		},
		{
			name: "SQS JSON ReceiveMessage null Messages",
			mock: sqsJSONMock("sqs-json-null", "AmazonSQS.ReceiveMessage", 200, `{"Messages":null}`),
			want: false,
		},
		{
			name: "SQS JSON ReceiveMessage 500 error",
			mock: sqsJSONMock("sqs-json-err", "AmazonSQS.ReceiveMessage", 500, `{"Messages":[{"Body":"err"}]}`),
			want: false,
		},
		{
			name: "SQS JSON DeleteMessage is not delivery",
			mock: sqsJSONMock("sqs-del", "AmazonSQS.DeleteMessage", 200, `{}`),
			want: false,
		},
		{
			name: "SQS JSON SendMessage is not delivery",
			mock: sqsJSONMock("sqs-send", "AmazonSQS.SendMessage", 200, `{"MessageId":"1"}`),
			want: false,
		},
		{
			name: "SQS Query form body ReceiveMessage with XML Message",
			mock: sqsXMLMock("sqs-xml-msg", "/", "application/x-www-form-urlencoded; charset=utf-8", "Action=ReceiveMessage&MaxNumberOfMessages=10&WaitTimeSeconds=4", 200, xmlWithMsg),
			want: true,
		},
		{
			name: "SQS Query form body ReceiveMessage empty XML",
			mock: sqsXMLMock("sqs-xml-empty", "/", "application/x-www-form-urlencoded; charset=utf-8", "Action=ReceiveMessage&MaxNumberOfMessages=10&WaitTimeSeconds=4", 200, xmlEmpty),
			want: false,
		},
		{
			name: "SQS Query URL param ReceiveMessage with XML Message",
			mock: sqsXMLMock("sqs-url-msg", "/000000000000/q?Action=ReceiveMessage&Version=2012-11-05", "", "", 200, xmlWithMsg),
			want: true,
		},
		{
			name: "SQS Query DeleteMessage is not delivery",
			mock: sqsXMLMock("sqs-xml-del", "/", "application/x-www-form-urlencoded", "Action=DeleteMessage&ReceiptHandle=abc", 200, xmlWithMsg),
			want: false,
		},
		{
			name: "Non-SQS HTTP returning Messages JSON is not delivery",
			mock: sqsJSONMock("plain-http", "", 200, `{"Messages":[{"Body":"chat"}]}`),
			want: false,
		},
		{
			name: "HTTP2 SQS ReceiveMessage with messages",
			mock: func() *Mock {
				m := &Mock{
					Name: "h2-sqs",
					Kind: HTTP2,
					Spec: MockSpec{
						Metadata: map[string]string{"type": "HTTP2_CLIENT", "lifetime": MetadataLifetimePerTest},
						HTTP2Req: &HTTP2Req{
							Method:  Method("POST"),
							URL:     "/000000000000/q",
							Headers: map[string]string{"x-amz-target": "AmazonSQS.ReceiveMessage"},
						},
						HTTP2Resp: &HTTP2Resp{
							StatusCode: 200,
							Body:       `{"Messages":[{"MessageId":"h2-1","Body":"hello"}]}`,
						},
					},
				}
				m.DeriveLifetime()
				return m
			}(),
			want: true,
		},
		{
			name: "HTTP2 SQS ReceiveMessage empty",
			mock: func() *Mock {
				m := &Mock{
					Name: "h2-sqs-empty",
					Kind: HTTP2,
					Spec: MockSpec{
						Metadata: map[string]string{"type": "HTTP2_CLIENT", "lifetime": MetadataLifetimePerTest},
						HTTP2Req: &HTTP2Req{
							Method:  Method("POST"),
							URL:     "/000000000000/q",
							Headers: map[string]string{"x-amz-target": "AmazonSQS.ReceiveMessage"},
						},
						HTTP2Resp: &HTTP2Resp{StatusCode: 200, Body: `{}`},
					},
				}
				m.DeriveLifetime()
				return m
			}(),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMessageDelivery(tc.mock); got != tc.want {
				t.Fatalf("IsMessageDelivery(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsMessageDelivery_Kafka(t *testing.T) {
	cases := []struct {
		name string
		mock *Mock
		want bool
	}{
		{
			name: "Kafka Fetch with recordBatches records",
			mock: kafkaFetchMock("kf-batches", 1, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"recordBatches":[{"records":[{"key":"k1","value":"v1"}]}]}]}]}}`),
			want: true,
		},
		{
			name: "Kafka Fetch with rawRecords",
			mock: kafkaFetchMock("kf-raw", 1, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"rawRecords":"AAAA"}]}]}}`),
			want: true,
		},
		{
			name: "Kafka Fetch empty partitions",
			mock: kafkaFetchMock("kf-empty", 1, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"recordBatches":[]}]}]}}`),
			want: false,
		},
		{
			name: "Kafka Fetch empty batch records",
			mock: kafkaFetchMock("kf-empty-rec", 1, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"recordBatches":[{"records":[]}]}]}]}}`),
			want: false,
		},
		{
			name: "Kafka Fetch partition error",
			mock: kafkaFetchMock("kf-part-err", 1, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":6,"rawRecords":"AAAA"}]}]}}`),
			want: false,
		},
		{
			name: "Kafka Fetch top-level error",
			mock: kafkaFetchMock("kf-top-err", 1, `{"body":{"errorCode":15,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"rawRecords":"AAAA"}]}]}}`),
			want: false,
		},
		{
			name: "Kafka Produce apiKey 0 is not delivery",
			mock: kafkaFetchMock("kf-produce", 0, `{"body":{"errorCode":0,"topics":[{"name":"orders","partitions":[{"partitionIndex":0,"errorCode":0,"rawRecords":"AAAA"}]}]}}`),
			want: false,
		},
		{
			name: "Kafka Metadata apiKey 3 is not delivery",
			mock: kafkaFetchMock("kf-meta", 3, `{"body":{"errorCode":0}}`),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMessageDelivery(tc.mock); got != tc.want {
				t.Fatalf("IsMessageDelivery(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}

	t.Run("Kafka Fetch opaque body respects RegisterIdleAnswer", func(t *testing.T) {
		unreg := RegisterIdleAnswer(KAFKA, func(m *Mock) bool {
			return m.Name == "kf-opaque-idle"
		})
		defer unreg()

		idle := kafkaFetchMock("kf-opaque-idle", 1, `{"body":{"rawBodyB64":"AAAA"}}`)
		nonIdle := kafkaFetchMock("kf-opaque-msg", 1, `{"body":{"rawBodyB64":"AQID"}}`)
		if IsMessageDelivery(idle) {
			t.Fatal("idle opaque Kafka Fetch must not be classified as a message delivery")
		}
		if !IsMessageDelivery(nonIdle) {
			t.Fatal("non-idle opaque Kafka Fetch must be classified as a message delivery")
		}
	})
}

func TestIsMessageDelivery_ExclusionsAndOnDiskAndSpilled(t *testing.T) {
	if IsMessageDelivery(nil) {
		t.Fatal("nil mock must return false")
	}

	sess := sqsJSONMock("sess-sqs", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"x"}]}`)
	sess.TestModeInfo.Lifetime = LifetimeSession
	if IsMessageDelivery(sess) {
		t.Fatal("session-lifetime mock must return false")
	}

	undeliv := sqsJSONMock("undeliv-sqs", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"x"}]}`)
	undeliv.Spec.Metadata["undelivered"] = "true"
	if IsMessageDelivery(undeliv) {
		t.Fatal("undelivered mock must return false")
	}

	phase := sqsJSONMock("phase-sqs", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"x"}]}`)
	delete(phase.Spec.Metadata, MetadataLifetimeKey)
	phase.Spec.Metadata["type"] = "connection"
	if !phase.IsProtocolPhase() || IsMessageDelivery(phase) {
		t.Fatal("protocol-phase mock must return false")
	}

	connFail := &Mock{Name: "cf", Kind: ConnectionFailure}
	connFail.DeriveLifetime()
	if IsMessageDelivery(connFail) {
		t.Fatal("window-bound ConnectionFailure mock must return false")
	}

	// Spilled response preserved via MarkMessageDelivery.
	spilled := sqsJSONMock("spilled-sqs", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"x"}]}`)
	spilled.MarkMessageDelivery()
	spilled.Spec.HTTPResp = nil
	if !IsMessageDelivery(spilled) {
		t.Fatal("spilled mock marked via MarkMessageDelivery must return true even with nil HTTPResp")
	}

	// LeavePayloadOnDisk + IndexEntryFrom preservation (built-in and custom hook).
	unreg := RegisterMessageDelivery(REDIS, func(m *Mock) bool {
		return m.Spec.Metadata["customDelivery"] == "true"
	})
	defer unreg()

	src := noopDeliveryPayloadSource{}
	onDiskSQS := sqsJSONMock("ondisk-sqs", "AmazonSQS.ReceiveMessage", 200, `{"Messages":[{"Body":"x"}]}`)
	onDiskSQS.LeavePayloadOnDisk(src, PayloadAt{Name: onDiskSQS.Name})
	if !IsMessageDelivery(onDiskSQS) {
		t.Fatal("on-disk SQS delivery must preserve IsMessageDelivery=true after LeavePayloadOnDisk")
	}
	if !IsMessageDelivery(onDiskSQS.IndexEntryFrom(src)) {
		t.Fatal("IndexEntryFrom copy of SQS delivery must preserve IsMessageDelivery=true")
	}

	onDiskEmpty := sqsJSONMock("ondisk-empty", "AmazonSQS.ReceiveMessage", 200, `{}`)
	onDiskEmpty.LeavePayloadOnDisk(src, PayloadAt{Name: onDiskEmpty.Name})
	if IsMessageDelivery(onDiskEmpty) {
		t.Fatal("on-disk empty SQS poll must remain IsMessageDelivery=false")
	}

	onDiskCustom := &Mock{
		Name: "ondisk-custom",
		Kind: REDIS,
		Spec: MockSpec{Metadata: map[string]string{"type": "mocks", "customDelivery": "true"}},
	}
	onDiskCustom.DeriveLifetime()
	onDiskCustom.LeavePayloadOnDisk(src, PayloadAt{Name: onDiskCustom.Name})
	if !IsMessageDelivery(onDiskCustom) {
		t.Fatal("on-disk registered custom delivery must preserve IsMessageDelivery=true")
	}
}

type noopDeliveryPayloadSource struct{}

func (noopDeliveryPayloadSource) LoadPayload(context.Context, PayloadAt) (*Mock, error) {
	return nil, ErrPayloadFileReplaced
}
func (noopDeliveryPayloadSource) SetPayloadMutator(PayloadMutator) {}
func (noopDeliveryPayloadSource) PayloadMutator() PayloadMutator   { return nil }
