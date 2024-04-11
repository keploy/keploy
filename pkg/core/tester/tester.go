// Package tester provides functionality for testing keploy with itself
package tester

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type Tester struct {
	logger        *zap.Logger
	testBenchInfo core.TestBenchInfo
}

func New(logger *zap.Logger, testBenchInfo core.TestBenchInfo) *Tester {
	return &Tester{
		logger:        logger,
		testBenchInfo: testBenchInfo,
	}
}

const (
	testPort   = 56789
	recordPort = 36789
)

func (t *Tester) Setup(ctx context.Context, opts models.TestingOptions) error {
	t.logger.Info("🧪 setting up environment for testing keploy with itself")

	if opts.Mode == models.MODE_TEST {
		err := t.setupReplay(ctx)
		if err != nil {
			return err
		}
		return nil
	}

	err := t.setupRecord(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (t *Tester) setupReplay(ctx context.Context) error {
	setUpErr := errors.New("failed to setup the keploy replay testing")

	recordPid, err := utils.GetPIDFromPort(ctx, t.logger, recordPort)
	if err != nil {
		t.logger.Error("failed to get the keployRecord pid", zap.Error(err))
		utils.LogError(t.logger, err, "failed to get the keployRecord pid from port", zap.Any("port", recordPort))
		return setUpErr
	}

	t.logger.Debug(fmt.Sprintf("keployRecord pid:%v", recordPid))

	err = t.testBenchInfo.SendKeployPids(models.RecordKey, recordPid)
	if err != nil {
		utils.LogError(t.logger, err, fmt.Sprintf("failed to send keploy %v server pid to the epbf program", models.MODE_RECORD), zap.Any("Keploy Pid", recordPid))
		return setUpErr
	}

	err = t.testBenchInfo.SendKeployPorts(models.RecordKey, recordPort)
	if err != nil {
		utils.LogError(t.logger, err, fmt.Sprintf("failed to send keploy %v server port to the epbf program", models.MODE_RECORD), zap.Any("Keploy server port", recordPort))
		return setUpErr
	}

	err = t.testBenchInfo.SendKeployPorts(models.TestKey, testPort)
	if err != nil {
		utils.LogError(t.logger, err, fmt.Sprintf("failed to send keploy %v server port to the epbf program", models.MODE_TEST), zap.Any("Keploy server port", testPort))
		return setUpErr
	}

	// to get the pid of keployTest binary in keployRecord binary, we have to wait for some time till the proxy server is started
	// TODO: find other way to filter child process (keployTest) pid in parent process binary (keployRecord)
	time.Sleep(10 * time.Second) // just for test bench.

	return nil
}

func (t *Tester) setupRecord(ctx context.Context) error {

	go func() {
		defer utils.Recover(t.logger)

		timeout := 30 * time.Second
		startTime := time.Now()

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				testPid, err := utils.GetPIDFromPort(ctx, t.logger, testPort)
				if err != nil {
					t.logger.Debug("failed to get the keploytest pid", zap.Error(err))
					continue
				}

				if testPid == 0 {
					continue
				}

				t.logger.Debug("keploytest pid", zap.Uint32("pid", testPid))

				// sending keploytest binary pid in keployrecord binary to filter out ingress/egress calls related to keploytest binary.
				err = t.testBenchInfo.SendKeployPids(models.TestKey, testPid)
				if err != nil {
					utils.LogError(t.logger, err, fmt.Sprintf("failed to send keploy %v server pid to the epbf program", models.MODE_TEST), zap.Any("Keploy Pid", testPid))
				}
				return
			case <-time.After(timeout - time.Since(startTime)):
				t.logger.Info("Timeout reached, exiting loop from setupRecordTesting")
				return // Exit the goroutine

			case <-ctx.Done():
				t.logger.Debug("Context cancelled, exiting loop from setupRecordTesting")
				return // Exit the goroutine
			}
		}
	}()

	return nil
}
