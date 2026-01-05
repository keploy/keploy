package record

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// StartOptions customizes the recording flow for special cases like mocks-only capture.
type StartOptions struct {
	Command            string
	CommandType        string
	TestSetID          string
	RecordTimer        time.Duration
	UseRecordTimer     bool
	ProxyPort          uint32
	DNSPort            uint32
	ContainerName      string
	CaptureIncoming    bool
	CaptureOutgoing    bool
	WriteTestSetConfig bool
	IgnoreAppError     bool
	MockDB             MockDB
	OnMock             func(*models.Mock) error
}

// StartResult summarizes the recording session.
type StartResult struct {
	TestSetID    string
	TestCount    int
	MockCount    int
	MockCountMap map[string]int
	AppError     models.AppError
}
