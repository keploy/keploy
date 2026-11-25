package replay

import (
	"net/url"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// keploy test, in the last set that runs: KEPLOY-LAST-TESTCASE goes on the
// last test case whose request the set sends, never on one the set fails
// unsent (refused when the set loaded, or a URL the base path cannot
// replace), which sends no request: with the header keyed on the last test
// case by its place, no request carried it. A test case outside -t's named
// ones, or ignored, is left out before the set's first pass, and a test case
// recorded as a stream is sent after the others, so the last stream sent
// carries it, or, when the set sends none of its streams, the last test case
// of the passes before. A retry cycle's pass sends it on its own last. The
// harness's two sets hold the same test cases, so the earlier set fails the
// same ones; only the later set's request carries the header, in both modes.
func TestTheLastTestCaseHeaderGoesOnTheLastTestCaseTheSetSends(t *testing.T) {
	// refuse makes tc's request capture into the set's key id, which only a
	// response may: the set refuses it as it loads, and fails it unsent. The
	// key is defined in the set's config, as a capture's key must be for
	// the check to read it at all.
	refuse := func(h *prRun, tc *models.TestCase) {
		h.replayer.testSetConf = &tmplConf{stored: models.TestSet{Template: map[string]interface{}{"id": "u_rec_0001"}}}
		tc.HTTPReq.Body = `{"id":"{{capture string .id}}"}`
	}
	// unsendable gives tc a URL with no scheme and host for the base path to
	// replace, so it fails unsent once a base path is set.
	unsendable := func(tc *models.TestCase) { tc.HTTPReq.URL = "/" + tc.Name }
	atBasePath := func(t *testing.T, h *prRun) {
		u, err := url.Parse(h.cases[0].HTTPReq.URL)
		if err != nil {
			t.Fatalf("the harness's URL: %v", err)
		}
		h.replayer.config.Test.BasePath = u.Scheme + "://" + u.Host
	}
	// failed: the shape fails a test case of each set (unsent, or its
	// answer), so each set is FAILED; else each is PASSED.
	type shape struct {
		name    string
		n       int
		streams []string
		setup   func(t *testing.T, h *prRun)
		failed  bool
		want    []string
	}
	shapes := []shape{
		{name: "its last test case refused", n: 2, failed: true,
			setup: func(t *testing.T, h *prRun) { refuse(h, h.cases[1]) },
			want:  []string{"test-set-1/test-1"}},
		{name: "its last test case unsent at the base path", n: 2, failed: true,
			setup: func(t *testing.T, h *prRun) { atBasePath(t, h); unsendable(h.cases[1]) },
			want:  []string{"test-set-1/test-1"}},
		{name: "its last test case gRPC, a base path set", n: 2,
			setup: func(t *testing.T, h *prRun) {
				atBasePath(t, h)
				tc := h.cases[1]
				tc.Kind = models.GRPC_EXPORT
				tc.HTTPReq, tc.HTTPResp = models.HTTPReq{}, models.HTTPResp{}
				tc.GrpcReq = models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc/Get"}}}
				tc.GrpcResp = models.GrpcResp{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "200"}}}
			},
			want: []string{"test-set-1/test-2"}},
		{name: "its last test case ignored", n: 2,
			setup: func(t *testing.T, h *prRun) {
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-1": {"test-2"}}
			},
			want: []string{"test-set-1/test-1"}},
		{name: "its last test case not among -t's", n: 2,
			setup: func(t *testing.T, h *prRun) {
				h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {}, "test-set-1": {"test-1"}}
			},
			want: []string{"test-set-1/test-1"}},
		{name: "its last stream refused", n: 3, streams: []string{"test-2", "test-3"}, failed: true,
			setup: func(t *testing.T, h *prRun) { refuse(h, h.cases[2]) },
			want:  []string{"test-set-1/test-2"}},
		{name: "its last stream unsent at the base path", n: 3, streams: []string{"test-2", "test-3"}, failed: true,
			setup: func(t *testing.T, h *prRun) { atBasePath(t, h); unsendable(h.cases[2]) },
			want:  []string{"test-set-1/test-2"}},
		{name: "its one stream refused", n: 2, streams: []string{"test-2"}, failed: true,
			setup: func(t *testing.T, h *prRun) { refuse(h, h.cases[1]) },
			want:  []string{"test-set-1/test-1"}},
		{name: "a retry cycle's pass", n: 3, failed: true,
			setup: func(t *testing.T, h *prRun) {
				h.replayer.config.RetryPassing = true
				h.replayer.hookImpl.(*lastCaseHooks).wrongBody = map[string]bool{"test-3": true}
			},
			want: []string{"test-set-1/test-3", "test-set-1/test-2"}},
	}
	for _, on := range []bool{true, false} {
		for _, s := range shapes {
			name := s.name
			if !on {
				name += "/switch off"
			}
			t.Run(name, func(t *testing.T) {
				provenOn(t, on)
				h := newPartialRunHarness(t, s.n, 0)
				h.streaming(s.streams...)
				reports := &setsReportDB{prReportDB: h.report}
				h.replayer.reportDB = reports
				lastSetShape{ids: []string{"test-set-0", "test-set-1"}}.apply(h)
				h.replayer.config.Command = "./app"
				h.replayer.config.CommandType = string(utils.Native)
				h.replayer.config.Test.MaxFlakyChecks = 1
				h.replayer.config.DisableANSI = true
				h.replayer.hookImpl = &lastCaseHooks{}
				s.setup(t, h)

				_, statuses := startEarlierSetRun(t, h, reports)
				if last, _ := h.replayer.hookImpl.(*lastCaseHooks).seen(); !slices.Equal(last, s.want) {
					t.Fatalf("the test cases sent with KEPLOY-LAST-TESTCASE: %v, want %v (sets %v): the last test case the last set that runs sends, alone", last, s.want, statuses)
				}
				want := string(models.TestSetStatusPassed)
				if s.failed {
					want = string(models.TestSetStatusFailed)
				}
				for _, set := range []string{"test-set-0", "test-set-1"} {
					if statuses[set] != want {
						t.Fatalf("%s is %q (all %v), want %s: the shape's test case did not fail as it is built to", set, statuses[set], statuses, want)
					}
				}
			})
		}
	}
}
