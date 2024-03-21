// Package record provides functionality for recording and managing test cases and mocks.
package record

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type recorder struct {
	logger          *zap.Logger
	testDB          TestDB
	mockDB          MockDB
	telemetry       Telemetry
	instrumentation Instrumentation
	config          config.Config
}

func New(logger *zap.Logger, testDB TestDB, mockDB MockDB, telemetry Telemetry, instrumentation Instrumentation, config config.Config) Service {
	return &recorder{
		logger:          logger,
		testDB:          testDB,
		mockDB:          mockDB,
		telemetry:       telemetry,
		instrumentation: instrumentation,
		config:          config,
	}
}

func (r *recorder) Start(ctx context.Context) error {

	// creating error group to manage proper shutdown of all the go routines and to propagate the error to the caller
	errGrp, _ := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)

	runAppErrGrp, _ := errgroup.WithContext(ctx)
	runAppCtx := context.WithoutCancel(ctx)
	runAppCtx, runAppCtxCancel := context.WithCancel(runAppCtx)

	hookErrGrp, _ := errgroup.WithContext(ctx)
	hookCtx := context.WithoutCancel(ctx)
	hookCtx, hookCtxCancel := context.WithCancel(hookCtx)
	hookCtx = context.WithValue(hookCtx, models.ErrGroupKey, hookErrGrp)

	var stopReason string

	// defining all the channels and variables required for the record
	var runAppError models.AppError
	var appErrChan = make(chan models.AppError, 1)
	var incomingChan <-chan *models.TestCase
	var outgoingChan <-chan *models.Mock
	var insertTestErrChan = make(chan error, 10)
	var insertMockErrChan = make(chan error, 10)
	var appID uint64
	var newTestSetID string
	var testCount = 0
	var mockCountMap = make(map[string]int)

	// defering the stop function to stop keploy in case of any error in record or in case of context cancellation
	defer func() {
		select {
		case <-ctx.Done():
			r.telemetry.RecordedTestSuite(newTestSetID, testCount, mockCountMap)
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}
		runAppCtxCancel()
		err := runAppErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop application")
		}
		hookCtxCancel()
		err = hookErrGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop hooks")
		}
		err = errGrp.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}()

	defer close(appErrChan)
	defer close(insertTestErrChan)
	defer close(insertMockErrChan)

	testSetIDs, err := r.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		stopReason = "failed to get testSetIds"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	newTestSetID = pkg.NewID(testSetIDs, models.TestSetPattern)

	// setting up the environment for recording
	appID, err = r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		stopReason = "failed setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	// checking for context cancellation as we don't want to start the hooks and proxy if the context is cancelled
	select {
	case <-ctx.Done():
		return nil
	default:
		// Starting the hooks and proxy
		err = r.instrumentation.Hook(hookCtx, appID, models.HookOptions{Mode: models.MODE_RECORD})
		if err != nil {
			stopReason = "failed to start the hooks and proxy"
			utils.LogError(r.logger, err, stopReason)
			if err == context.Canceled {
				return err
			}
			return fmt.Errorf(stopReason)
		}
	}

	// fetching test cases and mocks from the application and inserting them into the database
	incomingChan, err = r.instrumentation.GetIncoming(ctx, appID, models.IncomingOptions{})
	if err != nil {
		stopReason = "failed to get incoming frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}

	errGrp.Go(func() error {
		for testCase := range incomingChan {
			err := r.testDB.InsertTestCase(ctx, testCase, newTestSetID)
			if err != nil {
				if err == context.Canceled {
					continue
				}
				insertTestErrChan <- err
			} else {

				testCount++
				r.telemetry.RecordedTestAndMocks()
			}
		}
		return nil
	})

	outgoingChan, err = r.instrumentation.GetOutgoing(ctx, appID, models.OutgoingOptions{})
	if err != nil {
		stopReason = "failed to get outgoing frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}
	errGrp.Go(func() error {
		for mock := range outgoingChan {
			err := r.mockDB.InsertMock(ctx, mock, newTestSetID)
			if err != nil {
				if err == context.Canceled {
					continue
				}
				insertMockErrChan <- err
			} else {
				mockCountMap[mock.GetKind()]++
				r.telemetry.RecordedTestCaseMock(mock.GetKind())
			}
		}
		return nil
	})

	// running the user application
	runAppErrGrp.Go(func() error {
		runAppError = r.instrumentation.Run(runAppCtx, appID, models.RunOptions{})
		if runAppError.AppErrorType == models.ErrCtxCanceled {
			return nil
		}
		appErrChan <- runAppError
		return nil
	})
	time.Sleep(2 * time.Second) // Example sleep, adjust according to your application's startup time

	if len(r.config.ReRecord) != 0 {
		err = r.ReRecord(ctx)
		if err != nil {
			return err
		}
	}

	// setting a timer for recording
	if r.config.Record.RecordTimer != 0 {
		errGrp.Go(func() error {
			r.logger.Info("Setting a timer of " + r.config.Record.RecordTimer.String() + " for recording")
			timer := time.After(r.config.Record.RecordTimer)
			select {
			case <-timer:
				r.logger.Warn("Time up! Stopping keploy")
				err := utils.Stop(r.logger, "Time up! Stopping keploy")
				if err != nil {
					utils.LogError(r.logger, err, "failed to stop recording")
					return errors.New("failed to stop recording")
				}
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}

	// Waiting for the error to occur in any of the go routines
	select {
	case appErr := <-appErrChan:
		switch appErr.AppErrorType {
		case models.ErrCommandError:
			stopReason = "error in running the user application, hence stopping keploy"
		case models.ErrUnExpected:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
		case models.ErrInternal:
			stopReason = "internal error occured while hooking into the application, hence stopping keploy"
		case models.ErrAppStopped:
			stopReason = "user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is not expected"
			r.logger.Warn(stopReason, zap.Error(appErr))
			return nil
		case models.ErrCtxCanceled:
			return nil
		default:
			stopReason = "unknown error recieved from application, hence stopping keploy"
		}

	case err = <-insertTestErrChan:
		stopReason = "error while inserting test case into db, hence stopping keploy"
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}

func (r *recorder) StartMock(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	var stopReason string
	defer func() {
		select {
		case <-ctx.Done():
			break
		default:
			err := utils.Stop(r.logger, stopReason)
			if err != nil {
				utils.LogError(r.logger, err, "failed to stop recording")
			}
		}
		err := g.Wait()
		if err != nil {
			utils.LogError(r.logger, err, "failed to stop recording")
		}
	}()
	var outgoingChan <-chan *models.Mock
	var insertMockErrChan = make(chan error)

	appID, err := r.instrumentation.Setup(ctx, r.config.Command, models.SetupOptions{})
	if err != nil {
		stopReason = "failed to exeute mock record due to error while setting up the environment"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}
	err = r.instrumentation.Hook(ctx, appID, models.HookOptions{Mode: models.MODE_RECORD})
	if err != nil {
		stopReason = "failed to start the hooks and proxy"
		utils.LogError(r.logger, err, stopReason)
		return fmt.Errorf(stopReason)
	}

	outgoingChan, err = r.instrumentation.GetOutgoing(ctx, appID, models.OutgoingOptions{})
	if err != nil {
		stopReason = "failed to get outgoing frames"
		utils.LogError(r.logger, err, stopReason)
		if err == context.Canceled {
			return err
		}
		return fmt.Errorf(stopReason)
	}
	g.Go(func() error {
		for mock := range outgoingChan {
			mock := mock // capture range variable
			g.Go(func() error {
				err := r.mockDB.InsertMock(ctx, mock, "")
				if err != nil {
					insertMockErrChan <- err
				}
				return nil
			})
		}
		return nil
	})

	select {
	case err = <-insertMockErrChan:
		stopReason = "error while inserting mock into db, hence stopping keploy"
	case <-ctx.Done():
		return nil
	}
	utils.LogError(r.logger, err, stopReason)
	return fmt.Errorf(stopReason)
}

func (r *recorder) ReRecord(ctx context.Context) error {
	var httpCommands []*models.TestCase

	// Load HTTP commands from your configuration
	if len(r.config.ReRecord) != 0 {
		for _, testSet := range r.config.ReRecord {
			filepath := path.Join(r.config.Path, testSet, "tests")
			files, err := os.ReadDir(filepath)
			if err != nil {
				r.logger.Error("Failed to read directory", zap.String("filepath", filepath), zap.Error(err))
				return err
			}

			for _, file := range files {
				if file.IsDir() {
					continue
				}
				testCase, err := ReadTestCase(filepath, file) // Assumes ReadTestCase is a predefined function
				if err != nil {
					r.logger.Error("Failed to read test case", zap.String("file", file.Name()), zap.Error(err))
					return err
				}
				httpCommands = append(httpCommands, &testCase)
			}
		}
	}

	if len(httpCommands) == 0 {
		r.logger.Info("No HTTP commands to re-record")
		return nil
	}

	for _, cmd := range httpCommands {
		host, port, err := extractHostAndPort(cmd.Curl)
		if err != nil {
			r.logger.Error("Failed to extract host and port", zap.Error(err))
			continue // Proceed with the next command
		}

		if err := waitForPort(ctx, host, port); err != nil {
			r.logger.Error("Waiting for port failed", zap.String("host", host), zap.String("port", port), zap.Error(err))
			continue // Proceed with the next command
		}

		output, err := exec.Command("sh", "-c", cmd.Curl).CombinedOutput()
		if err != nil {
			r.logger.Error("Failed to execute HTTP command", zap.String("command", cmd.Curl), zap.String("output", string(output)), zap.Error(err))
		} else {
			r.logger.Info("Successfully executed HTTP command", zap.String("command", cmd.Curl), zap.String("output", string(output)))
		}
	}

	return nil
}

// extractHostAndPort parses the CURL command to extract the host and port.
func extractHostAndPort(curlCmd string) (string, string, error) {
    // Split the command string to find the URL
    parts := strings.Split(curlCmd, " ")
    for _, part := range parts {
        if strings.HasPrefix(part, "http") {
            u, err := url.Parse(part)
            if err != nil {
                return "", "", err
            }
            host := u.Hostname()
            port := u.Port()
            if port == "" {
                // Default HTTP/HTTPS ports if not specified
                if u.Scheme == "https" {
                    port = "443"
                } else {
                    port = "80"
                }
            }
            return host, port, nil
        }
    }
    return "", "", fmt.Errorf("no URL found in CURL command")
}

// waitForPort attempts to connect to a given host and port until successful or a context deadline is exceeded.
func waitForPort(ctx context.Context, host, port string) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
            conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 1*time.Second)
            if err == nil {
                conn.Close()
                return nil // Success
            }
            time.Sleep(1 * time.Second) // Wait before trying again
        }
    }
}
