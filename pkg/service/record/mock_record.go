package record

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	yamldoc "go.keploy.io/server/v3/pkg/platform/yaml"
	mockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// RecordMocks runs a mocks-only recording session using the record pipeline.
func (r *Recorder) RecordMocks(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error) {
	if r == nil || r.config == nil {
		return nil, errors.New("record service is not configured")
	}

	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return nil, errors.New("command is required")
	}

	basePath := strings.TrimSpace(opts.Path)
	if basePath == "" {
		basePath = strings.TrimSpace(r.config.Path)
		if basePath == "" {
			basePath = "./keploy"
		}
	}

	sessionID := fmt.Sprintf("mock-%d", time.Now().Unix())
	mockFilePath := filepath.Join(basePath, sessionID, "mocks.yaml")

	// Snapshot config overrides for this call.
	prevCommand := r.config.Command
	prevCommandType := r.config.CommandType
	prevPath := r.config.Path
	prevProxyPort := r.config.ProxyPort
	prevDNSPort := r.config.DNSPort
	prevRecordTimer := r.config.Record.RecordTimer
	prevMetadata := r.config.Record.Metadata
	prevMockDB := r.mockDB
	defer func() {
		r.config.Command = prevCommand
		r.config.CommandType = prevCommandType
		r.config.Path = prevPath
		r.config.ProxyPort = prevProxyPort
		r.config.DNSPort = prevDNSPort
		r.config.Record.RecordTimer = prevRecordTimer
		r.config.Record.Metadata = prevMetadata
		r.mockDB = prevMockDB
	}()

	r.config.Command = command
	r.config.CommandType = string(utils.FindDockerCmd(command))
	r.config.Path = basePath
	if opts.ProxyPort != 0 {
		r.config.ProxyPort = opts.ProxyPort
	}
	if opts.DNSPort != 0 {
		r.config.DNSPort = opts.DNSPort
	}
	// Avoid triggering Stop() via internal timer; use context timeout instead.
	r.config.Record.RecordTimer = 0
	// Skip config.yaml writes for mock-only flows.
	r.config.Record.Metadata = ""

	r.mockDB = mockdb.New(r.logger, basePath, "")
	if resettable, ok := r.mockDB.(interface{ ResetCounterID() }); ok {
		resettable.ResetCounterID()
	}

	recordCtx := ctx
	cancel := func() {}
	if opts.Duration > 0 {
		recordCtx, cancel = context.WithTimeout(ctx, opts.Duration)
	}
	defer cancel()

	if err := r.Start(recordCtx, models.ReRecordCfg{Rerecord: true, TestSet: sessionID}); err != nil {
		return nil, err
	}

	mocks, err := loadMocksFromFile(r.logger, mockFilePath)
	if err != nil {
		return nil, err
	}
	if len(mocks) == 0 {
		if err := ensureMockFile(mockFilePath); err != nil {
			return nil, err
		}
	}

	return &models.RecordResult{
		MockFilePath: mockFilePath,
		MockCount:    len(mocks),
		Mocks:        mocks,
		AppExitCode:  0,
	}, nil
}

func loadMocksFromFile(logger *zap.Logger, filePath string) ([]*models.Mock, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []*models.Mock{}, nil
		}
		return nil, err
	}

	dec := yamlLib.NewDecoder(bytes.NewReader(data))
	var docs []*yamldoc.NetworkTrafficDoc
	for {
		var doc *yamldoc.NetworkTrafficDoc
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}

	if len(docs) == 0 {
		return []*models.Mock{}, nil
	}

	return mockdb.DecodeMocks(docs, logger)
}

func ensureMockFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(utils.GetVersionAsComment()), 0o644)
}
