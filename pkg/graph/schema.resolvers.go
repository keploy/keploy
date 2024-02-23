package graph

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.
// Code generated by github.com/99designs/gqlgen version v0.17.36

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.keploy.io/server/pkg/graph/model"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/proxy"
	"go.keploy.io/server/pkg/service/test"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

// RunTestSet is the resolver for the runTestSet field.
func (r *mutationResolver) RunTestSet(ctx context.Context, testSet string) (*model.RunTestSetResponse, error) {
	if r.Resolver == nil {
		err := fmt.Errorf(Emoji + "failed to get Resolver")
		return nil, err
	}

	tester := r.Resolver.Tester

	if tester == nil {
		r.Logger.Error("failed to get tester from resolver")
		return nil, fmt.Errorf(Emoji+"failed to run testSet:%v", testSet)
	}

	testRunChan := make(chan string, 1)
	pid := r.Resolver.AppPid
	serveTest := r.Resolver.ServeTest
	testCasePath := r.Resolver.Path
	testReportPath := r.Resolver.TestReportPath
	delay := r.Resolver.Delay

	testReportFS := r.Resolver.TestReportFS
	if tester == nil {
		r.Logger.Error("failed to get testReportFS from resolver")
		return nil, fmt.Errorf(Emoji+"failed to run testSet:%v", testSet)
	}

	ys := r.Resolver.Storage
	if ys == nil {
		r.Logger.Error("failed to get ys from resolver")
		return nil, fmt.Errorf(Emoji+"failed to run testSet:%v", testSet)
	}

	loadedHooks := r.LoadedHooks
	if loadedHooks == nil {
		r.Logger.Error("failed to get loadedHooks from resolver")
		return nil, fmt.Errorf(Emoji+"failed to run testSet:%v", testSet)
	}

	resultForTele := make([]int, 2)
	ctx = context.WithValue(ctx, "resultForTele", &resultForTele)
	initialisedValues := test.TestEnvironmentSetup{
		Ctx:                ctx,
		LoadedHooks:        loadedHooks,
		TestReportFS:       testReportFS,
		Storage:            ys,
		IgnoreOrdering:     false,
		GenerateTestReport: true,
	}
	go func() {
		defer utils.HandlePanic()
		r.Logger.Debug("starting testrun...", zap.Any("testSet", testSet))

		// send filtered testcases to run the test-set
		testcaseFilter := utils.ArrayToMap(r.TestFilter[testSet])
		// run the test set with a delay
		tester.RunTestSet(testSet, testCasePath, testReportPath, "", "", "", delay, 30*time.Second, pid, testRunChan, r.ApiTimeout, testcaseFilter, nil, serveTest, initialisedValues)
	}()

	testRunID := <-testRunChan
	r.Logger.Debug("", zap.Any("testRunID", testRunID))

	return &model.RunTestSetResponse{Success: true, TestRunID: testRunID}, nil
}

// TestSets is the resolver for the testSets field.
func (r *queryResolver) TestSets(ctx context.Context) ([]string, error) {
	if r.Resolver == nil {
		err := fmt.Errorf(Emoji + "failed to get Resolver")
		return nil, err
	}
	testPath := r.Resolver.Path

	testSets, err := r.Resolver.Storage.ReadTestSessionIndices()
	if err != nil {
		r.Resolver.Logger.Error("failed to fetch test sets", zap.Any("testPath", testPath), zap.Error(err))
		return nil, err
	}

	// Print debug log for retrieved qualified test sets
	if len(testSets) > 0 {
		r.Resolver.Logger.Debug(fmt.Sprintf("Retrieved test sets: %v", testSets), zap.Any("testPath", testPath))
	} else {
		r.Resolver.Logger.Debug("No test sets found", zap.Any("testPath", testPath))
	}

	// Listen for the interrupt signal
	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, syscall.SIGINT, syscall.SIGTERM)
	r.Logger.Info("logging the pid in the graph handler", zap.Any("pid", r.AppPid))

	// load the ebpf hooks into the kernel
	select {
	case <-stopper:
		return nil, fmt.Errorf("test stopped during execution")
	default:
		if err := r.LoadedHooks.LoadHooks("", "", r.AppPid, context.Background(), nil); err != nil {
			return nil, err
		}

	}

	//sending this graphql server port to be filterd in the eBPF program
	if err := r.LoadedHooks.SendKeployServerPort(r.KeployServerPort); err != nil {
		return nil, err
		// return err
	}

	select {
	case <-stopper:
		r.LoadedHooks.Stop(true)
		return nil, fmt.Errorf("test stopped during execution")
	default:
		// start the proxy
		r.ProxySet = proxy.BootProxy(r.Logger, proxy.Option{Port: r.ProxyPort, MongoPassword: r.MongoPassword}, "", "", r.AppPid, r.Lang, r.PassThroughPorts, r.LoadedHooks, ctx, 0)

	}

	// proxy update its state in the ProxyPorts map
	// Sending Proxy Ip & Port to the ebpf program
	if err := r.LoadedHooks.SendProxyInfo(r.ProxySet.IP4, r.ProxySet.Port, r.ProxySet.IP6); err != nil {
		return nil, err
	}

	// Sending the Dns Port to the ebpf program
	if err := r.LoadedHooks.SendDnsPort(r.ProxySet.DnsPort); err != nil {
		return nil, err
	}

	r.Logger.Info("Adding default jacoco agent port to passthrough", zap.Uint("Port", 36320))
	r.PassThroughPorts = append(r.PassThroughPorts, 36320)
	// filter the required destination ports
	if err := r.LoadedHooks.SendPassThroughPorts(r.PassThroughPorts); err != nil {
		return nil, err
	}

	err = r.LoadedHooks.SendCmdType(false)
	if err != nil {
		return nil, err
	}

	resultTestsets := []string{}
	// filter the test sets to be run based on cmd flag
	for _, testset := range testSets {
		// checking whether the provided testset match with a recorded testset.
		if _, ok := r.TestFilter[testset]; !ok && len(r.TestFilter) != 0 {
			continue
		}
		resultTestsets = append(resultTestsets, testset)
	}
	return resultTestsets, nil
}

// TestSetStatus is the resolver for the testSetStatus field.
func (r *queryResolver) TestSetStatus(ctx context.Context, testRunID string) (*model.TestSetStatus, error) {
	//Initiate the telemetry.
	var store = fs.NewTeleFS(r.Logger)
	var tele = telemetry.NewTelemetry(true, false, store, r.Logger, "", nil)
	if r.Resolver == nil {
		err := fmt.Errorf(Emoji + "failed to get Resolver")
		return nil, err
	}
	testReportFs := r.Resolver.TestReportFS

	if testReportFs == nil {
		r.Logger.Error("failed to get testReportFS from resolver")
		return nil, fmt.Errorf(Emoji+"failed to get the status for testRunID:%v", testRunID)
	}
	testReport, err := testReportFs.Read(ctx, r.Resolver.TestReportPath, testRunID)
	if err != nil {
		r.Logger.Error("failed to fetch testReport", zap.Any("testRunID", testRunID), zap.Error(err))
		return nil, err
	}
	readTestReport, ok := testReport.(*models.TestReport)
	if !ok {
		r.Logger.Error("failed to read testReport from resolver")
		return nil, fmt.Errorf(Emoji+"failed to read the test report for testRunID:%v", testRunID)
	}
	if readTestReport.Status == "PASSED" || readTestReport.Status == "FAILED" {
		tele.Testrun(readTestReport.Success, readTestReport.Failure)
	}

	r.Logger.Debug("", zap.Any("testRunID", testRunID), zap.Any("testSetStatus", readTestReport.Status))
	return &model.TestSetStatus{Status: readTestReport.Status}, nil
}

func (r *Resolver) StopTest(ctx context.Context) (bool, error) {
	r.Logger.Debug("stopping test...")
	r.LoadedHooks.Stop(true)

	// stop the proxy set
	r.ProxySet.StopProxyServer()
	return true, nil
}

// Mutation returns MutationResolver implementation.
func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

// Query returns QueryResolver implementation.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
