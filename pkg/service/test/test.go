package test

import (
	"fmt"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

type tester struct {
	logger *zap.Logger
}

func NewTester (logger *zap.Logger) Tester {
	return &tester{
		logger: logger,
	}
}

func (t *tester) Test(tcsPath, mockPath string, pid uint32) bool  {
	models.SetMode(models.MODE_TEST)


	// fetch the recorded testcases with their mocks
	ys := yaml.NewYamlStore(tcsPath, mockPath, t.logger)
	// start the proxies
	ps := proxy.BootProxies(t.logger, proxy.Option{})
	// Initiate the hooks and update the vaccant ProxyPorts map
	loadedHooks := hooks.NewHook(ps.PortList, ys, t.logger)
	if err := loadedHooks.LoadHooks(pid); err != nil {
		return false
	}
	// proxy update its state in the ProxyPorts map
	ps.SetHook(loadedHooks)

	tcs, _, err := ys.Read(nil)
	if err != nil {
		return false
	}

	for _, tc := range tcs {
		switch tc.Kind {
		case models.HTTP:
			resp, err := pkg.SimulateHttp(tc, t.logger,loadedHooks.GetResp)
			if err!=nil {
				t.logger.Info("result", zap.Any("testcase id", tc.Name), zap.Any("passed", "false"))
				continue
			}
		// println("before blocking simulate")

			// resp := loadedHooks.GetResp()
			// println("after blocking simulate")

			t.logger.Info(fmt.Sprintf(" ----- response from simulation: %v", resp))
	// 		spec := &spec.HttpSpec{}
	// 		err := tc.Spec.Decode(spec)
	// 		if err!=nil {
	// 			t.logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
	// 			return false
	// 		}
	// 		req, err := http.NewRequest(string(spec.Request.Method), "http://localhost"+":"+k.cfg.App.Port+spec.Request.URL, bytes.NewBufferString(spec.Request.Body))
	// 		if err != nil {
	// 			panic(err)
	// 		}
	// 		req.Header = tc.HttpReq.Header
	// 		req.Header.Set("KEPLOY_TEST_ID", tc.ID)
	// 		req.ProtoMajor = tc.HttpReq.ProtoMajor
	// 		req.ProtoMinor = tc.HttpReq.ProtoMinor
	// 		req.Close = true

	// 		// httpresp, err := k.client.Do(req)
	// 		k.client.Do(req)
	// 		if err != nil {
	// 			k.Log.Error("failed sending testcase request to app", zap.Error(err))
	// 			return nil, err
	// 		}
	// 		// defer httpresp.Body.Close()
	// 		println("before blocking simulate")
	
		}
	}


	// stop listening for the eBPF events
	loadedHooks.Stop()
	return true
}

// func (t *tester) testHttp(tc models.Mock) bool {
// 	spec := &spec.HttpSpec{}
// 	err := tc.Spec.Decode(spec)
// 	if err!=nil {
// 		t.logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
// 		return false
// 	}

	
// }