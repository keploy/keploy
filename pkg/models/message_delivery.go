package models

import (
	"encoding/json"
	"encoding/xml"
	"net/url"
	"strings"
)

// RegisterMessageDelivery declares, for mocks of kind, which per-test mocks
// carry a broker message delivery to an application consumer (for example an
// SQS ReceiveMessage response carrying Messages, or a Kafka Fetch response
// carrying records). A delivery consumed before the first test fires is spent
// once and must never be re-armed by the readiness-gate re-arm or re-staged
// into a test window.
//
// Predicates for one kind are OR-ed alongside the built-in SQS and Kafka
// detectors; a nil predicate is ignored. The returned function removes this
// registration; production code ignores it, tests use it to leave the registry
// as they found it.
func RegisterMessageDelivery(kind Kind, isDelivery func(*Mock) bool) (unregister func()) {
	return messageDeliveryHooks.add(kind, isDelivery)
}

var messageDeliveryHooks mockPredicates

const (
	deliveryUnknown uint8 = iota
	deliveryNo
	deliveryYes
)

// cacheMessageDeliveryState computes and caches the built-in broker message
// delivery classification once at ingest (DeriveLifetime) for kinds whose
// detection parses payloads (Kafka frames and SQS ReceiveMessage bodies),
// mirroring cacheReadThroughState by leaving ordinary non-broker mocks at
// deliveryUnknown (0) so hot consume paths never re-parse large Kafka/SQS
// payloads while non-broker TestModeInfo stays untouched.
func (m *Mock) cacheMessageDeliveryState() {
	if m == nil || WindowBound(m.Kind) || m.TestModeInfo.Lifetime != LifetimePerTest || m.IsUndelivered() || m.IsProtocolPhase() {
		return
	}
	if m.onDisk != nil {
		if m.onDisk.messageDelivery {
			m.TestModeInfo.messageDeliveryState = deliveryYes
		}
		return
	}
	switch m.Kind {
	case HTTP:
		if m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil || !isSQSReceiveMessageRequest(m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Header, m.Spec.HTTPReq.Body) {
			return
		}
	case HTTP2:
		if m.Spec.HTTP2Req == nil || m.Spec.HTTP2Resp == nil || !isSQSReceiveMessageRequest(m.Spec.HTTP2Req.URL, m.Spec.HTTP2Req.Headers, m.Spec.HTTP2Req.Body) {
			return
		}
	case KAFKA:
		// Always cache Kafka mocks so hot consume paths never re-parse JSON frames.
	default:
		return
	}
	isDeliv, cacheable := isBuiltInMessageDelivery(m)
	if isDeliv {
		m.TestModeInfo.messageDeliveryState = deliveryYes
	} else if cacheable {
		m.TestModeInfo.messageDeliveryState = deliveryNo
	}
}

// IsMessageDelivery reports whether m is a per-test mock carrying a delivered
// message to a consumer (SQS ReceiveMessage with Messages, Kafka Fetch with
// records, or a mock accepted by RegisterMessageDelivery). The mock's lifetime
// must already be derived.
//
// Empty/idle polls, error responses, non-delivery actions (SendMessage,
// DeleteMessage, Kafka Produce/Metadata), window-bound mocks, undelivered
// calls, protocol-phase exchanges, and non-per-test mocks return false.
func IsMessageDelivery(m *Mock) bool {
	if m == nil || WindowBound(m.Kind) || m.TestModeInfo.Lifetime != LifetimePerTest {
		return false
	}
	if m.IsUndelivered() || m.IsProtocolPhase() {
		return false
	}
	if m.TestModeInfo.messageDeliveryState == deliveryYes {
		return true
	}
	if messageDeliveryHooks.match(m) {
		return true
	}
	if m.TestModeInfo.messageDeliveryState == deliveryNo {
		return false
	}
	if m.onDisk != nil {
		return m.onDisk.messageDelivery
	}
	isDeliv, _ := isBuiltInMessageDelivery(m)
	return isDeliv
}

func isBuiltInMessageDelivery(m *Mock) (isDelivery bool, cacheable bool) {
	if m == nil {
		return false, true
	}
	switch m.Kind {
	case HTTP:
		if m.Spec.HTTPReq == nil || m.Spec.HTTPResp == nil {
			return false, true
		}
		return isSQSReceiveMessageDelivery(
			m.Spec.HTTPReq.URL,
			m.Spec.HTTPReq.Header,
			m.Spec.HTTPReq.Body,
			m.Spec.HTTPResp.StatusCode,
			m.Spec.HTTPResp.Body,
		), true
	case HTTP2:
		if m.Spec.HTTP2Req == nil || m.Spec.HTTP2Resp == nil {
			return false, true
		}
		return isSQSReceiveMessageDelivery(
			m.Spec.HTTP2Req.URL,
			m.Spec.HTTP2Req.Headers,
			m.Spec.HTTP2Req.Body,
			m.Spec.HTTP2Resp.StatusCode,
			m.Spec.HTTP2Resp.Body,
		), true
	case KAFKA:
		return isKafkaFetchDelivery(m)
	default:
		return false, true
	}
}

func isSQSReceiveMessageDelivery(rawURL string, headers map[string]string, reqBody string, statusCode int, respBody string) bool {
	if !isSQSReceiveMessageRequest(rawURL, headers, reqBody) {
		return false
	}
	return hasSQSReceiveMessages(statusCode, respBody)
}

func isSQSReceiveMessageRequest(rawURL string, headers map[string]string, reqBody string) bool {
	var contentType string
	for k, v := range headers {
		switch {
		case strings.EqualFold(k, "X-Amz-Target"):
			if strings.TrimSpace(v) == "AmazonSQS.ReceiveMessage" {
				return true
			}
		case strings.EqualFold(k, "Content-Type"):
			contentType = v
		}
	}
	if strings.Contains(rawURL, "Action=") {
		if u, err := url.Parse(rawURL); err == nil && u.Query().Get("Action") == "ReceiveMessage" {
			return true
		}
	}
	if reqBody != "" && (strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") ||
		(contentType == "" && strings.Contains(reqBody, "Action=ReceiveMessage"))) {
		if vals, err := url.ParseQuery(reqBody); err == nil && vals.Get("Action") == "ReceiveMessage" {
			return true
		}
	}
	return false
}

func hasSQSReceiveMessages(statusCode int, respBody string) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	trimmed := strings.TrimSpace(respBody)
	if trimmed == "" {
		return false
	}
	if trimmed[0] == '<' {
		var xmlResp struct {
			Result struct {
				Messages []struct{} `xml:"Message"`
			} `xml:"ReceiveMessageResult"`
		}
		return xml.Unmarshal([]byte(trimmed), &xmlResp) == nil && len(xmlResp.Result.Messages) > 0
	}
	var jsonResp struct {
		Messages []json.RawMessage `json:"Messages"`
	}
	return json.Unmarshal([]byte(trimmed), &jsonResp) == nil && len(jsonResp.Messages) > 0
}

const kafkaFetchAPIKey = 1

func isKafkaFetchDelivery(m *Mock) (bool, bool) {
	if len(m.Spec.GenericRequests) == 0 || len(m.Spec.GenericResponses) == 0 {
		return false, true
	}
	if !isKafkaFetchRequest(m.Spec.GenericRequests) {
		return false, true
	}
	if IsIdleAnswer(m) {
		return false, true
	}
	return hasKafkaFetchRecords(m.Spec.GenericResponses)
}

func isKafkaFetchRequest(reqs []Payload) bool {
	for _, p := range reqs {
		for _, msg := range p.Message {
			data := strings.TrimSpace(msg.Data)
			if data == "" {
				continue
			}
			var frame struct {
				Header struct {
					APIKey *int `json:"apiKey"`
				} `json:"header"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				return false
			}
			return frame.Header.APIKey != nil && *frame.Header.APIKey == kafkaFetchAPIKey
		}
	}
	return false
}

func hasKafkaFetchRecords(resps []Payload) (bool, bool) {
	kafkaIdleRegistered := idleHooks.hasKind(KAFKA)
	sawOpaqueWithoutIdleHook := false
	for _, p := range resps {
		for _, msg := range p.Message {
			data := strings.TrimSpace(msg.Data)
			if data == "" {
				continue
			}
			var frame struct {
				Body json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal([]byte(data), &frame); err != nil {
				continue
			}
			raw := strings.TrimSpace(string(frame.Body))
			if raw == "" || raw == "null" {
				continue
			}
			if raw[0] == '"' {
				var opaque string
				if err := json.Unmarshal(frame.Body, &opaque); err == nil && strings.TrimSpace(opaque) != "" {
					if kafkaIdleRegistered {
						return true, true
					}
					sawOpaqueWithoutIdleHook = true
				}
				continue
			}
			if raw[0] != '{' {
				continue
			}
			var body struct {
				ErrorCode  int16  `json:"errorCode"`
				RawBodyB64 string `json:"rawBodyB64"`
				Topics     []struct {
					Partitions []struct {
						ErrorCode     int16  `json:"errorCode"`
						RawRecords    string `json:"rawRecords"`
						RecordBatches []struct {
							Records []json.RawMessage `json:"records"`
						} `json:"recordBatches"`
					} `json:"partitions"`
				} `json:"topics"`
			}
			if err := json.Unmarshal(frame.Body, &body); err != nil {
				continue
			}
			if strings.TrimSpace(body.RawBodyB64) != "" {
				if kafkaIdleRegistered {
					return true, true
				}
				sawOpaqueWithoutIdleHook = true
			}
			if body.ErrorCode != 0 {
				continue
			}
			for _, topic := range body.Topics {
				for _, part := range topic.Partitions {
					if part.ErrorCode != 0 {
						continue
					}
					if strings.TrimSpace(part.RawRecords) != "" {
						return true, true
					}
					for _, batch := range part.RecordBatches {
						if len(batch.Records) > 0 {
							return true, true
						}
					}
				}
			}
		}
	}
	return false, !sawOpaqueWithoutIdleHook
}
