package replay

import (
	"context"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// These tests run a whole test set through RunTestSet with ids followed, so
// that what the replay asks of the agent, sends to the app, reports and writes
// back is asserted where it is wired — not only in runIDs' own methods.

// riAgent is an agent that can be asked for the ids it bound. When it follows
// the set it binds the recorded order id to the one the app made, as the real
// agent does when the app's create reaches its dependency.
type riAgent struct {
	*prInstr
	follows bool

	mu       sync.Mutex
	outgoing []models.OutgoingOptions
	pairs    map[string]string
	reads    int
}

func (a *riAgent) MockOutgoing(_ context.Context, opts models.OutgoingOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.outgoing = append(a.outgoing, opts)
	return nil
}

func (a *riAgent) GetIDPairs(context.Context) (map[string]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	return maps.Clone(a.pairs), nil
}

func (a *riAgent) appCreated() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.follows {
		a.pairs = map[string]string{idRecorded: idLive}
	}
}

// riApp is an app that mints its order id anew on every run: a create answers
// with idLive, and the order is known under that id alone.
type riApp struct {
	prHooks
	agent *riAgent
	// templated does to a test case what pkg.SimulateHTTP does to one in a
	// templated set: it renders the id's placeholder in the expected response
	// with the template's value before the request goes out, and a create
	// then updates the template with the id it made.
	templated bool
	// wrongID is a regression: the read-back answers with an id the app did
	// not make.
	wrongID bool
	sent    []string
}

func (a *riApp) SimulateRequest(_ context.Context, tc *models.TestCase, _ string) (interface{}, error) {
	a.sent = append(a.sent, tc.HTTPReq.URL)
	if a.templated {
		tc.HTTPResp.Body = strings.ReplaceAll(tc.HTTPResp.Body, "{{string .id}}", utils.TemplatizedValues["id"].(string))
	}
	switch {
	case tc.HTTPReq.Method == "POST":
		a.agent.appCreated()
		if a.templated {
			utils.TemplatizedValues["id"] = idLive
		}
		return &models.HTTPResp{StatusCode: 201, Body: `{"id":"` + idLive + `"}`}, nil
	case strings.Contains(tc.HTTPReq.URL, idLive):
		id := idLive
		if a.wrongID {
			id = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
		}
		return &models.HTTPResp{StatusCode: 200, Body: `{"id":"` + id + `","name":"widget"}`}, nil
	}
	return &models.HTTPResp{StatusCode: 404, Body: `{"error":"no such order"}`}, nil
}

// riConf is a test-set config with a template map, which keeps what is
// written back to it.
type riConf struct {
	prTestSetConf
	template map[string]interface{}
	written  map[string]interface{}
}

func (c *riConf) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{Template: maps.Clone(c.template)}, nil
}

func (c *riConf) Write(_ context.Context, _ string, ts *models.TestSet) error {
	c.written = maps.Clone(ts.Template)
	return nil
}

type riRun struct {
	*prRun
	agent *riAgent
	app   *riApp
	conf  *riConf
	logs  *observer.ObservedLogs
}

// newRunIDsHarness is a replay of the canonical set — a create whose response
// carries the id the app made, and a read-back that names it, both with the id
// written out — whose template map names that id.
func newRunIDsHarness(t *testing.T) *riRun {
	t.Helper()
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })

	h := newPartialRunHarness(t, 2, 0)
	addr := strings.TrimPrefix(strings.TrimSuffix(h.cases[0].HTTPReq.URL, "/test-1"), "http://")
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	create, read := h.cases[0], h.cases[1]
	create.Name, read.Name = "create", "read"
	create.HTTPReq = models.HTTPReq{Method: "POST", URL: "http://" + addr + "/orders", Body: `{"name":"widget"}`, Timestamp: at}
	create.HTTPResp = models.HTTPResp{StatusCode: 201, Body: `{"id":"` + idRecorded + `"}`, Timestamp: at}
	read.HTTPReq = models.HTTPReq{Method: "GET", URL: "http://" + addr + "/orders/" + idRecorded, Timestamp: at.Add(time.Second)}
	read.HTTPResp = models.HTTPResp{StatusCode: 200, Body: `{"id":"` + idRecorded + `","name":"widget"}`, Timestamp: at.Add(time.Second)}

	core, logs := observer.New(zap.InfoLevel)
	agent := &riAgent{prInstr: h.instr}
	app := &riApp{agent: agent}
	conf := &riConf{template: map[string]interface{}{"id": idRecorded}}
	h.replayer.logger = zap.New(core)
	h.replayer.instrumentation = agent
	h.replayer.hookImpl = app
	h.replayer.testSetConf = conf
	h.replayer.config.Test.Mocking = true
	h.replayer.config.Test.UpdateTemplate = true
	return &riRun{prRun: h, agent: agent, app: app, conf: conf, logs: logs}
}

func (h *riRun) result(name string) models.TestResult {
	for _, r := range h.report.results {
		if r.TestCaseID == name {
			return r
		}
	}
	return models.TestResult{}
}

// The whole of it, on a set the agent follows: the agent is asked to follow
// the id the templates name, the read-back is sent with the id the app made,
// both test cases pass and are reported with the pair, and --update-template
// leaves the RECORDED id in the templates though the run holds its own.
//
// The read-back expects the id through a placeholder, as `keploy templatize`
// leaves a response, while its request still names it written out: its pair
// is reported for the id its answer names, nothing having been swapped.
func TestRunTestSet_FollowsTheIDsTheAgentBound(t *testing.T) {
	// The agent is asked in two places: a compose app's agent comes up with
	// the app, so it is told what to follow before the mocks are loaded.
	for _, cmdType := range []utils.CmdType{utils.Native, utils.DockerCompose} {
		t.Run(string(cmdType), func(t *testing.T) {
			h := newRunIDsHarness(t)
			h.replayer.config.CommandType = string(cmdType)
			// The stand-in app answers the agent's health probe too.
			h.replayer.config.Agent.AgentURI = strings.TrimSuffix(h.cases[0].HTTPReq.URL, "/orders")
			h.agent.follows = true
			h.app.templated = true
			h.cases[1].HTTPResp.Body = `{"id":"{{string .id}}","name":"widget"}`

			require.Equal(t, models.TestSetStatusPassed, h.run(t))

			require.NotEmpty(t, h.agent.outgoing)
			for _, opts := range h.agent.outgoing {
				require.Equal(t, []string{idRecorded}, opts.Rebind.Named(), "what the agent is asked to follow")
			}
			require.Len(t, h.app.sent, 2)
			require.Contains(t, h.app.sent[1], "/orders/"+idLive, "the read-back is sent with the id the app made")

			pair := map[string]string{idRecorded: idLive}
			require.Equal(t, pair, h.result("create").RunIDs, "swapped into the create's expected response")
			require.Contains(t, h.result("create").Result.BodyResult[0].Expected, idLive)
			require.Equal(t, pair, h.result("read").RunIDs, "named by the read-back's answer")

			require.Equal(t, idLive, utils.TemplatizedValues["id"], "precondition: the run's own template value is the id it made")
			require.Equal(t, map[string]interface{}{"id": idRecorded}, h.conf.written, "an id that was followed is written back as recorded")

			require.Equal(t, 1, h.logs.FilterMessageSnippet("made new ids").Len())
			require.Zero(t, h.logs.FilterMessageSnippet("did not follow").Len())
		})
	}
}

// A followed id is still asserted. In a set with templates keploy otherwise
// takes whatever the app answers where a template's value is expected; an id
// the agent bound is settled, so the app answering the read-back with another
// id than the one it made fails the test case.
func TestRunTestSet_AFollowedIDIsStillAsserted(t *testing.T) {
	h := newRunIDsHarness(t)
	h.agent.follows = true
	h.app.templated = true
	h.app.wrongID = true

	require.Equal(t, models.TestSetStatusFailed, h.run(t))
	require.Equal(t, models.TestStatusPassed, h.result("create").Status)
	require.Equal(t, models.TestStatusFailed, h.result("read").Status)
	require.Equal(t, idLive, utils.TemplatizedValues["id"], "and the template is not taught the wrong id")
}

// A set the agent does not follow (it holds a mock of a kind that cannot be
// followed; the agent predates this) gets no pair, and replays as it does
// without rebinding: the read-back goes out with the recorded id and fails,
// and --update-template writes what the run made, as it always has — the
// recorded id is written back only for an id that was in fact followed.
//
// The one thing said is where to look: the failed test case names an id that
// was not followed.
func TestRunTestSet_ASetTheAgentDoesNotFollowReplaysAsWithoutRebinding(t *testing.T) {
	h := newRunIDsHarness(t)

	require.Equal(t, models.TestSetStatusFailed, h.run(t))

	require.Contains(t, h.app.sent[1], "/orders/"+idRecorded, "nothing was bound: the read-back is sent as recorded")
	require.Equal(t, models.TestStatusPassed, h.result("create").Status, "the template takes the id the create answered, as without rebinding")
	require.Equal(t, models.TestStatusFailed, h.result("read").Status)
	require.Nil(t, h.result("create").RunIDs)
	require.Nil(t, h.result("read").RunIDs)
	require.Equal(t, map[string]interface{}{"id": idLive}, h.conf.written, "what the run made is written back, as without rebinding")

	said := h.logs.FilterMessageSnippet("did not follow").All()
	require.Len(t, said, 1, "one line")
	require.Equal(t, map[string][]string{idRecorded: {"read"}}, said[0].ContextMap()["first"])
	require.Zero(t, h.logs.FilterMessageSnippet("made new ids").Len())
}

// A set the replay itself leaves alone is not named to the agent, the agent is
// never asked for pairs, nothing is said about ids, and its templates are
// written back as the run made them: it is the run it would be with
// test.disableMockRebinding.
func TestRunTestSet_ASetThatIsLeftAloneReplaysAsWithRebindingOff(t *testing.T) {
	for name, leave := range map[string]func(h *riRun){
		"rebinding is off":           func(h *riRun) { h.replayer.config.Test.DisableMockRebinding = true },
		"mocks are matched strictly": func(h *riRun) { h.replayer.config.Test.MockNoiseStrict = true },
		"the app is not run by keploy (--base-path)": func(h *riRun) {
			h.replayer.instrument = false
			h.replayer.config.Test.UpdateTemplate = false
			h.replayer.config.Test.BasePath = strings.TrimSuffix(h.cases[0].HTTPReq.URL, "/orders")
		},
		"only the read-back is selected": func(h *riRun) {
			h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"read"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRunIDsHarness(t)
			// An agent that would bind, were it asked to.
			h.agent.follows = true
			leave(h)

			require.Equal(t, models.TestSetStatusFailed, h.run(t))

			for _, opts := range h.agent.outgoing {
				require.Nil(t, opts.Rebind, "the agent is not asked to follow anything")
			}
			require.Zero(t, h.agent.reads, "and is never asked what it bound")
			for _, url := range h.app.sent {
				require.NotContains(t, url, idLive, "every request is sent as recorded")
			}
			for _, r := range h.report.results {
				require.Nil(t, r.RunIDs, r.TestCaseID)
			}
			require.Zero(t, h.logs.FilterMessageSnippet("did not follow").Len()+h.logs.FilterMessageSnippet("made new ids").Len(), "nothing is said about ids")
			if len(h.replayer.config.Test.SelectedTests) == 0 {
				require.Equal(t, map[string]interface{}{"id": idLive}, h.conf.written, "the templates are written back as the run made them")
			}
		})
	}
}
