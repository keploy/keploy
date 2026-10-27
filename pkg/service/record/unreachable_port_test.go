package record

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A keploy.yml that sends replay somewhere other than test.host and each test's
// recorded port makes "can the host reach the recorded port?" the wrong
// question: replay will not dial it, so a warning about it is noise, or worse,
// advice to publish a port nobody uses. Each redirect setting on its own keeps
// the recording quiet and asks the instrumentation nothing; with none of them,
// it asks and warns.
func TestUnreachablePortWarnerAsksNothingWhenReplayIsRedirected(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Test)
	}{
		{"test.port", func(t *config.Test) { t.Port = 8080 }},
		{"test.grpcPort", func(t *config.Test) { t.GRPCPort = 50051 }},
		{"test.ssePort", func(t *config.Test) { t.SSEPort = 8047 }},
		{"a protocol port", func(t *config.Test) { t.Protocol = config.ProtocolConfig{"http": {Port: 8080}} }},
		{"replaceWith.global.url", func(t *config.Test) {
			t.ReplaceWith.Global.URL = map[string]string{"localhost:8096": "app.internal:8096"}
		}},
		{"replaceWith.global.port", func(t *config.Test) { t.ReplaceWith.Global.Port = map[uint32]uint32{8096: 18096} }},
		{"replaceWith.test-sets", func(t *config.Test) {
			t.ReplaceWith.TestSets = map[string]config.ReplaceWithMap{"test-set-0": {Port: map[uint32]uint32{8096: 18096}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			tc.set(&cfg.Test)
			instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
			core, logs := observer.New(zapcore.DebugLevel)

			w := newUnreachablePortWarner(zap.New(core), instr, cfg)
			w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
			w.wait(time.Millisecond)

			if len(instr.asked) != 0 {
				t.Errorf("asked %v; replay does not dial the recorded port", instr.asked)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %v", logs.All())
			}
		})
	}

	// The control: nothing redirected, so it asks, and warns.
	instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := newUnreachablePortWarner(zap.New(core), instr, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	w.wait(time.Millisecond)
	if len(instr.asked) != 1 || logs.FilterMessageSnippet("port 8096 cannot be replayed").Len() != 1 {
		t.Errorf("not redirected: asked %v, logged %v; want one question and one warning", instr.asked, logs.All())
	}
}

// inspectOnly is a docker daemon that answers every ContainerInspect with resp.
type inspectOnly struct {
	kdocker.Client
	resp container.InspectResponse
}

func (f inspectOnly) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return f.resp, nil
}

// The record-time warning from the Docker-mode instrumentation itself, which
// asks about the recorded port at that same host port. Where host port 8095
// already has a publish, to the container's port 9000 where nothing listens,
// a second one is refused ("port is already allocated"): the warning says to
// change that one, or to send these tests where 8095 is published with a port
// map, never to add -p 8095:8095. Where host port 8095 has none, adding it is
// the fix.
func TestUnreachablePortWarnerSaysHowToPublishTheRecordedPort(t *testing.T) {
	both := func(port string) []nat.PortBinding {
		return []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: port}, {HostIP: "::", HostPort: port}}
	}
	const prefix = "test cases recorded on the app's port 8095 cannot be replayed at that port from the host: "
	for _, tc := range []struct {
		name   string
		ports  nat.PortMap
		listen map[string][]string // where the app listens, by container port; nil: on 8095, every address
		want   string
	}{
		{
			name:  "host port 8095 published to another container port, the app's port on two others",
			ports: nat.PortMap{"9000/tcp": both("8095"), "8095/tcp": append(both("18095"), both("28095")...)},
			want: prefix + "host port 8095 is published to the container's port 9000, where nothing listens, instead of the app's port 8095 these tests were recorded on, where the app listens, and keploy sends each test from the host to port 8095, the port it was recorded on. " +
				"Change -p 8095:9000 to -p 8095:8095 in the docker command" +
				", or send these tests to one of host ports 18095, 28095 with a port map in keploy.yml (test.replaceWith.global.port: {8095: 18095})",
		},
		{
			// Replay sends the tests to host port 18095 (pkg.PublishedTarget).
			name:  "host port 8095 published to another container port, the app's port on one other",
			ports: nat.PortMap{"9000/tcp": both("8095"), "8095/tcp": both("18095")},
		},
		{
			name:  "host port 8095 not published, the app's port on two others",
			ports: nat.PortMap{"8095/tcp": append(both("18095"), both("28095")...)},
			want: prefix + "it is published on host port 18095, 28095 instead, and keploy sends each test from the host to port 8095, the port it was recorded on. " +
				"Publish it on the same port (add -p 8095:8095 to the docker command)" +
				", or send these tests to one of host ports 18095, 28095 with a port map in keploy.yml (test.replaceWith.global.port: {8095: 18095})",
		},
		{
			// Replay sends the tests to host port 18095 (pkg.PublishedTarget).
			name:  "host port 8095 not published, the app's port on one other",
			ports: nat.PortMap{"8095/tcp": both("18095")},
		},
		{
			// Replay sends the tests to host port 18095, which docker forwards
			// to the container's own address: an app listening only on
			// loopback never sees them, so that is warned of, at 18095.
			name:   "the app's port on one other host port, the app listening only on loopback",
			ports:  nat.PortMap{"8095/tcp": both("18095")},
			listen: map[string][]string{"8095": {"127.0.0.1"}},
			want: "test cases recorded on the app's port 8095 cannot be replayed from the host at host port 18095, where it is published: " +
				"host port 18095 is published to the app's port 8095, where the app listens only on 127.0.0.1 inside the container, which no published port reaches, " +
				"and keploy sends these tests from the host to port 18095, for the app's port 8095 they were recorded on. " +
				"keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:8095",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The app listens on 8095, on every address; nothing on 9000.
				listen := tc.listen
				if listen == nil {
					listen = map[string][]string{"9000": {}, "8095": {"0.0.0.0"}}
				}
				addrs := listen[r.URL.Query().Get("port")]
				_ = json.NewEncoder(w).Encode(models.AppListenAddrs{Addrs: addrs})
			}))
			defer agent.Close()
			cfg := &config.Config{CommandType: string(utils.DockerRun), KeployContainer: "keploy-v3-test"}
			cfg.Agent.AgentURI = agent.URL + "/agent"
			inspect := container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					State:      &container.State{Running: true},
					HostConfig: &container.HostConfig{NetworkMode: "bridge"},
				},
				NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: tc.ports}},
			}
			core, logs := observer.New(zapcore.WarnLevel)

			w := newUnreachablePortWarner(zap.New(core), agenthttp.New(zap.NewNop(), inspectOnly{resp: inspect}, cfg), cfg)
			w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8095})
			w.wait(time.Millisecond)

			if tc.want == "" {
				if logs.Len() != 0 {
					t.Fatalf("warned %v; replay sends these tests where the app's port is published", logs.All())
				}
				return
			}
			if logs.Len() != 1 || logs.All()[0].Message != tc.want {
				t.Fatalf("warned %v\nwant one warning:\n%q", logs.All(), tc.want)
			}
			if strings.Contains(tc.name, "another container port") && strings.Contains(logs.All()[0].Message, "add -p") {
				t.Errorf("the warning advises a second publish of host port 8095: %q", logs.All()[0].Message)
			}
		})
	}
}

// slowReach is an instrumentation whose question about the app's port is
// answered, with reason, only once release closes, and not at all once its
// context ends (as a docker inspect then fails): ended closes then.
type slowReach struct {
	Instrumentation
	reason  string
	asked   chan struct{}
	release chan struct{}
	ended   chan struct{}
	once    sync.Once
}

func newSlowReach(reason string) *slowReach {
	return &slowReach{reason: reason, asked: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
}

func (s *slowReach) UnreachableAppPort(ctx context.Context, _ string, _, _ uint16) string {
	s.once.Do(func() { close(s.asked) })
	select {
	case <-s.release:
		if ctx.Err() != nil {
			return ""
		}
		return s.reason
	case <-ctx.Done():
		close(s.ended)
		return ""
	}
}

// soon fails the test unless ch closes; the bound is only for a wait that
// never ends, which is what it catches.
func soon(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// logged waits until a line containing snippet is logged; the bound is only
// for one that never is.
func logged(t *testing.T, logs *observer.ObservedLogs, snippet string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for logs.FilterMessageSnippet(snippet).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%q was never logged: %v", snippet, logs.All())
		}
		time.Sleep(time.Millisecond)
	}
}

// A question about an app port holds up no recording, and the stop's end does
// not cut it short: the stop waits for it once its drains are over,
// saying which port it still asks about, and the warning comes before the
// wait ends.
func TestAPortQuestionHoldsNothingUpAndTheStopWaitsForIt(t *testing.T) {
	reach := newSlowReach("it is not published on the host")
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	ctx, stop := context.WithCancel(context.Background())
	checked := make(chan struct{})
	go func() { w.check(ctx, &models.TestCase{Name: "test-1", AppPort: 8096}); close(checked) }()
	soon(t, checked, "check returning while its question is out")
	soon(t, reach.asked, "the question")
	stop() // the stop ends record's context
	waited := make(chan struct{})
	go func() { w.wait(5 * time.Millisecond); close(waited) }()
	logged(t, logs, "Still checking whether the test cases recorded on the app's port(s) can be replayed")
	if got := logs.FilterMessageSnippet("Still checking").All()[0].ContextMap()["appPorts"]; got == nil {
		t.Fatalf("the wait does not say which port it asks about: %v", logs.All())
	}
	select {
	case <-waited:
		t.Fatal("the wait ended while the question was out")
	default:
	}
	close(reach.release)
	soon(t, waited, "the end of the wait")
	if logs.FilterMessageSnippet("port 8096 cannot be replayed").Len() != 1 {
		t.Fatalf("the stop cut the question short, or the wait ended before its warning: %v", logs.All())
	}
}

// stoppedOnce is a run the user has stopped once, as NewCtx's handler marks
// it, put back when the test ends.
func stoppedOnce(t *testing.T) {
	t.Helper()
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted()
}

// A question the daemon never answers holds the stop only until the user's
// next Ctrl+C, which the wait offers once it has said it waits: that press
// ends the wait and the question.
func TestTheNextCtrlCEndsTheWaitForAPortQuestion(t *testing.T) {
	stoppedOnce(t)
	reach := newSlowReach("it is not published on the host")
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(5 * time.Millisecond); close(waited) }()
	logged(t, logs, "press Ctrl+C again")
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the user's press after the offer did not end the wait")
	}
	soon(t, waited, "the end of the wait")
	soon(t, reach.ended, "the end of the question")
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len(); n != 0 {
		t.Fatalf("warned %d times after the user gave the question up: %v", n, logs.All())
	}
}

// With no question out the wait says nothing and ends at once, and it seals
// the warner all the same: a question that would be asked after it (by a
// consumer of a record drain that gave up) is not.
func TestTheWaitWithNoPortQuestionOutSaysNothing(t *testing.T) {
	reach := newSlowReach("") // a question asked stays out until released
	t.Cleanup(func() { close(reach.release) })
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	w.wait(time.Hour) // nothing ever asked
	w.wait(time.Hour)
	if logs.Len() != 0 {
		t.Fatalf("logged %v", logs.All())
	}
	w.check(context.Background(), &models.TestCase{Name: "test-2", AppPort: 8096})
	if ports, _ := w.out(); len(ports) != 0 {
		t.Fatalf("a question was asked after the wait: %v", ports)
	}
}

// deafReach answers its question only once release closes, whatever its
// context: a question that does not end with it.
type deafReach struct {
	Instrumentation
	asked   chan struct{}
	release chan struct{}
}

func (d *deafReach) UnreachableAppPort(context.Context, string, uint16, uint16) string {
	close(d.asked)
	<-d.release
	return ""
}

// The user's next Ctrl+C ends the wait even for a question that does not end
// with its context.
func TestTheNextCtrlCEndsTheWaitForADeafPortQuestion(t *testing.T) {
	stoppedOnce(t)
	reach := &deafReach{asked: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(reach.release) })
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(5 * time.Millisecond); close(waited) }()
	logged(t, logs, "press Ctrl+C again")
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the user's press after the offer did not end the wait")
	}
	soon(t, waited, "the end of the wait")
}

// The wait ends as the last question is answered, not at its next notice:
// here a whole hour away.
func TestTheWaitEndsAsTheLastQuestionIsAnswered(t *testing.T) {
	reach := newSlowReach("")
	w := newUnreachablePortWarner(zap.NewNop(), reach, &config.Config{})
	parked := make(chan struct{}, 1)
	w.parked = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(time.Hour); close(waited) }()
	soon(t, parked, "the wait waiting on the question")
	close(reach.release)
	soon(t, waited, "the end of the wait")
}

// An interrupt that gave an earlier wait up (utils.StopNow) ends this one at
// once, even for a question that does not end with its context.
func TestAnEarlierGiveUpEndsTheWaitAtOnce(t *testing.T) {
	stoppedOnce(t)
	_, withdraw := utils.OfferStopNow() // a drain said another interrupt stops it now
	if !utils.MarkInterruptedAgain() {
		t.Fatal("the press did not answer the drain's offer")
	}
	withdraw()
	reach := &deafReach{asked: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(reach.release) })
	w := newUnreachablePortWarner(zap.NewNop(), reach, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(time.Hour); close(waited) }()
	soon(t, waited, "the end of the wait")
}

// The wait's offer goes with it: once the last question is answered, a
// press is no answer to it.
func TestTheWaitsOfferIsWithdrawnAsItEnds(t *testing.T) {
	stoppedOnce(t)
	reach := newSlowReach("")
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(5 * time.Millisecond); close(waited) }()
	logged(t, logs, "press Ctrl+C again")
	close(reach.release)
	soon(t, waited, "the end of the wait")
	if utils.MarkInterruptedAgain() {
		t.Fatal("the wait's offer still stood after it ended")
	}
}

// The stop's wait seals the warner: a question that would be asked once it
// has begun is not, and the one out is waited for.
func TestTheStopsWaitAsksNoMore(t *testing.T) {
	reach := newSlowReach("it is not published on the host")
	core, logs := observer.New(zapcore.InfoLevel)
	w := newUnreachablePortWarner(zap.New(core), reach, &config.Config{})
	parked := make(chan struct{}, 1)
	w.parked = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	soon(t, reach.asked, "the question")
	waited := make(chan struct{})
	go func() { w.wait(time.Hour); close(waited) }()
	soon(t, parked, "the wait waiting on the question")
	w.check(context.Background(), &models.TestCase{Name: "test-2", AppPort: 8097})
	if ports, _ := w.out(); fmt.Sprint(ports) != "[8096]" {
		t.Fatalf("questions out %v; want the one asked before the wait, and none asked after", ports)
	}
	close(reach.release)
	soon(t, waited, "the end of the wait")
	if logs.FilterMessageSnippet("port 8096 cannot be replayed").Len() != 1 {
		t.Fatalf("the warning of the question out is not said: %v", logs.All())
	}
}
