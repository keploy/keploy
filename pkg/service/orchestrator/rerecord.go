package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

func (o *Orchestrator) ReRecord(ctx context.Context) error {
	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller

	var stopReason string
	var err error

	defer func() {
		select {
		case <-ctx.Done():
		default:
			err := utils.Stop(o.logger, stopReason)
			if err != nil {
				utils.LogError(o.logger, err, "failed to stop recording")
			}
		}
	}()

	// Get all the testsets
	testSets, err := o.replay.GetAllTestSetIDs(ctx)
	if err != nil {
		errMsg := "Failed to get all testset IDs"
		utils.LogError(o.logger, err, errMsg)
		return err
	}

	// Sort the testsets to ensure that the testcases are re-recorded in the same order
	sort.SliceStable(testSets, func(i, j int) bool {
		return testSets[i] < testSets[j]
	})

	for _, testSet := range testSets {
		if ctx.Err() != nil {
			break
		}

		if _, ok := o.config.Test.SelectedTests[testSet]; !ok && len(o.config.Test.SelectedTests) != 0 {
			continue
		}

		o.logger.Info("Re-recording testcases for the given testset", zap.String("testset", testSet))

		errGrp, _ := errgroup.WithContext(ctx)
		recordCtx := context.WithoutCancel(ctx)
		recordCtx, recordCtxCancel := context.WithCancel(recordCtx)

		var errCh = make(chan error, 1)
		var replayErrCh = make(chan error, 1)

		select {
		case <-ctx.Done():
		default:
			errGrp.Go(func() error {
				defer utils.Recover(o.logger)
				err := o.record.Start(recordCtx, true)
				errCh <- err
				return nil
			})
		}

		select {
		case <-ctx.Done():
		default:
			errGrp.Go(func() error {
				defer utils.Recover(o.logger)
				allRecorded, err := o.replayTests(recordCtx, testSet)

				if allRecorded {
					o.logger.Info("Re-recorded testcases successfully for the given testset", zap.String("testset", testSet))
				} else {
					o.logger.Warn("Failed to re-record some testcases", zap.String("testset", testSet))
				}

				replayErrCh <- err
				return nil
			})
		}

		var err error
		select {
		case err = <-errCh:
			if err != nil {
				stopReason = "error while starting the recording"
				utils.LogError(o.logger, err, stopReason, zap.String("testset", testSet))
			}
		case err = <-replayErrCh:
			if err != nil {
				stopReason = "error while replaying the testcases"
				utils.LogError(o.logger, err, stopReason, zap.String("testset", testSet))
			}
		case <-ctx.Done():
		}

		if err == nil {
			// Sleep for 3 seconds to ensure that the recording has completed
			time.Sleep(3 * time.Second)
		}

		recordCtxCancel()

		// Wait for the recording to stop
		err = errGrp.Wait()
		if err != nil {
			utils.LogError(o.logger, err, "failed to stop re-recording")
		}

		// Check if the global context is done after each iteration
		if ctx.Err() != nil {
			break
		}
	}

	if stopReason == "" {
		stopReason = "Re-recorded all the selected testsets successfully"
		return nil
	}

	utils.LogError(o.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}

func (o *Orchestrator) replayTests(ctx context.Context, testSet string) (bool, error) {
	var err error
	var errMsg string

	//replay the recorded testcases

	tcs, err := o.replay.GetTestCases(ctx, testSet)
	if err != nil {
		errMsg = "Failed to get all testcases"
		utils.LogError(o.logger, err, errMsg, zap.String("testset", testSet))
		return false, fmt.Errorf(errMsg)
	}

	if len(tcs) == 0 {
		o.logger.Warn("No testcases found for the given testset", zap.String("testset", testSet))
		return false, nil
	}

	host, port, err := pkg.ExtractHostAndPort(tcs[0].Curl)
	if err != nil {
		errMsg = "failed to extract host and port"
		utils.LogError(o.logger, err, "")
		o.logger.Debug("", zap.String("curl", tcs[0].Curl))
		return false, fmt.Errorf(errMsg)
	}
	cmdType := utils.CmdType(o.config.CommandType)
	if utils.IsDockerKind(cmdType) {
		host = o.config.ContainerName
	}

	timeout := time.Duration(30) * time.Second

	o.logger.Debug("", zap.String("host", host), zap.String("port", port), zap.Any("WaitTimeout", timeout), zap.Any("CommandType", cmdType))

	if err := pkg.WaitForPort(ctx, host, port, timeout); err != nil {
		utils.LogError(o.logger, err, "Waiting for port failed", zap.String("host", host), zap.String("port", port))
		return false, err
	}

	allTcRecorded := true
	for _, tc := range tcs {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if utils.IsDockerKind(cmdType) {

			userIP, err := o.record.GetContainerIP(ctx, o.config.AppID)
			if err != nil {
				utils.LogError(o.logger, err, "failed to get the app ip")
				break
			}

			tc.HTTPReq.URL, err = utils.ReplaceHostToIP(tc.HTTPReq.URL, userIP)
			if err != nil {
				utils.LogError(o.logger, err, "failed to replace host to docker container's IP")
				break
			}
			o.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HTTPReq.URL))
		}

		resp, err := pkg.SimulateHTTP(ctx, *tc, testSet, o.logger, o.config.Test.APITimeout)
		if err != nil {
			utils.LogError(o.logger, err, "failed to simulate HTTP request")
			allTcRecorded = false
			continue // Proceed with the next command
		}

		o.logger.Info("Re-recorded the testcase successfully", zap.String("curl", tc.Curl), zap.Any("response", (resp)))
	}

	return allTcRecorded, nil
}
