package models

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestFormatMockTimestamp(t *testing.T) {
	timestamp := time.Date(2026, time.March, 19, 14, 5, 6, 789123456, time.FixedZone("IST", 5*60*60+30*60))

	if got := FormatMockTimestamp(time.Time{}); got != "" {
		t.Fatalf("FormatMockTimestamp(zero) = %q, want empty string", got)
	}

	if got, want := FormatMockTimestamp(timestamp), timestamp.Format(time.RFC3339Nano); got != want {
		t.Fatalf("FormatMockTimestamp() = %q, want %q", got, want)
	}
}

func TestMappedTestCasePreservesReqResTimestamps(t *testing.T) {
	testCase := MappedTestCase{
		ID: "test-1",
		Mocks: []MockEntry{
			{
				Name:             "mock-1",
				Kind:             "Http",
				Timestamp:        1710837306,
				ReqTimestampMock: "2026-03-19T14:05:06.789123456+05:30",
				ResTimestampMock: "2026-03-19T14:05:06.999123456+05:30",
			},
		},
	}

	data, err := yaml.Marshal(testCase)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}

	if !strings.Contains(string(data), "reqTimestampMock") || !strings.Contains(string(data), "resTimestampMock") {
		t.Fatalf("marshaled YAML missing req/res timestamps:\n%s", data)
	}

	var got MappedTestCase
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	if len(got.Mocks) != 1 {
		t.Fatalf("decoded %d mocks, want 1", len(got.Mocks))
	}

	mock := got.Mocks[0]
	if mock.ReqTimestampMock != testCase.Mocks[0].ReqTimestampMock {
		t.Fatalf("ReqTimestampMock = %q, want %q", mock.ReqTimestampMock, testCase.Mocks[0].ReqTimestampMock)
	}
	if mock.ResTimestampMock != testCase.Mocks[0].ResTimestampMock {
		t.Fatalf("ResTimestampMock = %q, want %q", mock.ResTimestampMock, testCase.Mocks[0].ResTimestampMock)
	}
}
