//go:build !windows

package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// agentComposeDoc is a compose document keploy runs, its agent on 16789.
const agentComposeDoc = `services:
  keploy-agent:
    image: keploy-agent
    ports:
      - "127.0.0.1:16789:16789"
    command: ["--port", "16789", "--proxy-port", "16790"]
  app:
    image: app
    network_mode: service:keploy-agent
`

// The refusal Docker 29 prints for a host port another process holds.
const agentPortRefusal = "Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-agent (b7eadbc86d7b): failed to bind host port 127.0.0.1:16789/tcp: address already in use"

// composeWriter is a daemon client that writes compose documents as keploy's
// does; any other call panics.
type composeWriter struct{ noContainers }

func (*composeWriter) MarshalCompose(c *docker.Compose) ([]byte, error) { return yaml.Marshal(c) }
func (w *composeWriter) WriteComposeFile(c *docker.Compose, path string) error {
	b, err := w.MarshalCompose(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func agentComposeOf(t *testing.T) *docker.Compose {
	t.Helper()
	var c docker.Compose
	require.NoError(t, yaml.Unmarshal([]byte(agentComposeDoc), &c))
	return &c
}

// movingAgent is the agent client's side of a move, as the App sees it: the
// address it reads, and the port each refusal moves the agent to (from 20001
// up), where said names agentPortRefusal's address.
type movingAgent struct {
	mu    sync.Mutex
	addr  atomic.Pointer[string]
	said  []string
	moves uint32
}

func (m *movingAgent) hand(a *App) {
	first := "http://localhost:16789/agent"
	m.addr.Store(&first)
	a.SetAgentAddress(func() string { return *m.addr.Load() })
	a.SetOnAgentPortRefused(func(_ context.Context, said string) (uint32, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.said = append(m.said, said)
		if !strings.Contains(said, "address already in use") {
			return 0, nil
		}
		m.moves++
		to := 20000 + m.moves
		uri := "http://localhost:" + strconv.FormatUint(uint64(to), 10) + "/agent"
		m.addr.Store(&uri)
		return to, nil
	})
}

// A run whose `docker compose up` compose ended because it could not publish
// keploy's agent's port on the host (another socket took it after keploy
// drew it) runs the up again with the agent moved, before the app started:
// the compose file the second up reads publishes and tells the agent the new
// port, the agent's launch is recorded at its new address, and the run goes
// on as one that never failed. What compose said reaches the agent client
// through keploy's console tee. Readers of the agent's address while it moves
// read the old one or the new one (-race).
func TestARefusedAgentPublishIsUpAgainOnAnotherPort(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	composeFile := filepath.Join(t.TempDir(), "docker-compose-tmp.yaml")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(`#!/bin/sh
printf '%s\n' "$*" >> '`+calls+`'
case "$*" in
  *" up")
    if grep -q '127.0.0.1:16789:16789' '`+composeFile+`'; then echo '`+agentPortRefusal+`' >&2; exit 1; fi
    grep -q '127.0.0.1:20001:20001' '`+composeFile+`' && grep -q '20001' '`+composeFile+`' || exit 3 ;;
esac
`), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := &App{
		logger: zap.NewNop(),
		docker: &composeWriter{},
		kind:   utils.DockerCompose,
		cmd:    utils.NewAppCommand("docker compose -f " + composeFile + " up"),
		opts:   models.SetupOptions{AgentURI: "http://localhost:16789/agent", AgentPort: 16789},
	}
	doc := agentComposeOf(t)
	require.NoError(t, (&composeWriter{}).WriteComposeFile(doc, composeFile))
	a.setComposeSource(composeFile, nil, doc)
	agent := &movingAgent{}
	agent.hand(a)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	var torn atomic.Pointer[string]
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := a.agentAddress(); got != "http://localhost:16789/agent" && got != "http://localhost:20001/agent" {
					torn.Store(&got)
				}
			}
		}()
	}
	appErr := a.run(context.Background())
	close(stop)
	readers.Wait()

	require.Equal(t, models.ErrAppStopped, appErr.AppErrorType, "%v", appErr.Err)
	require.Nil(t, torn.Load(), "a reader read the agent's address torn")
	log, err := os.ReadFile(calls)
	require.NoError(t, err)
	ups := 0
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasSuffix(line, " up") {
			ups++
		}
	}
	require.Equal(t, 2, ups, "docker was asked: %s", log)
	require.Len(t, agent.said, 1)
	require.Contains(t, agent.said[0], agentPortRefusal, "the agent client was not handed what compose said")
	moved, err := os.ReadFile(composeFile)
	require.NoError(t, err)
	require.Contains(t, string(moved), "127.0.0.1:20001:20001")
	require.NotContains(t, string(moved), "16789")
	require.EqualValues(t, 20001, a.opts.AgentPort)
	_, oldLaunched := token.Launched("http://localhost:16789/agent")
	require.False(t, oldLaunched, "the refused port's launch record was left behind after moving the agent")
	launch, launched := token.Launched("http://localhost:20001/agent")
	require.True(t, launched, "the agent's launch was not recorded at its new address, so the client does not check that agent enforces the token")
	t.Cleanup(func() { token.ForgetLaunch("http://localhost:20001/agent", launch) })
}

// exitWith is the end of a command that exited code.
func exitWith(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run()
	require.Error(t, err)
	return err
}

// upsSaying is an up that prints said and ends as end, counting its runs.
func upsSaying(a *App, said string, end func() utils.CmdError, runs *int) func() utils.CmdError {
	return func() utils.CmdError {
		*runs++
		if out := a.upSaid.Load(); out != nil {
			_, _ = out.Write([]byte(said + "\n"))
		}
		return end()
	}
}

// noRedo is an up that must not run: the agent was not moved.
func noRedo(t *testing.T) func() utils.CmdError {
	return func() utils.CmdError {
		t.Error("the up ran again")
		return utils.CmdError{}
	}
}

func movingApp(t *testing.T, file bool) (*App, *movingAgent) {
	t.Helper()
	a := &App{logger: zap.NewNop(), docker: &composeWriter{}, kind: utils.DockerCompose, opts: models.SetupOptions{AgentPort: 16789}}
	doc := agentComposeOf(t)
	if file {
		path := filepath.Join(t.TempDir(), "docker-compose-tmp.yaml")
		require.NoError(t, (&composeWriter{}).WriteComposeFile(doc, path))
		a.setComposeSource(path, nil, doc)
	} else {
		content, err := yaml.Marshal(doc)
		require.NoError(t, err)
		a.setComposeSource("", content, doc)
	}
	agent := &movingAgent{}
	agent.hand(a)
	return a, agent
}

// Only an up compose itself ended (exit 1, or the compose library's error)
// is read for a refused publish: one that exited otherwise, that keploy could
// not start, or that a signal ended, is returned as it is, the agent client
// not asked.
func TestOnlyComposesOwnFailureIsReadForARefusedPublish(t *testing.T) {
	for name, end := range map[string]func(t *testing.T) utils.CmdError{
		"exit 2":         func(t *testing.T) utils.CmdError { return utils.CmdError{Type: utils.Runtime, Err: exitWith(t, "2")} },
		"not started":    func(t *testing.T) utils.CmdError { return utils.CmdError{Type: utils.Init, Err: exitWith(t, "1")} },
		"no exit code":   func(*testing.T) utils.CmdError { return utils.CmdError{Type: utils.Runtime, Err: errors.New("killed")} },
		"a clean finish": func(*testing.T) utils.CmdError { return utils.CmdError{} },
	} {
		t.Run(name, func(t *testing.T) {
			a, agent := movingApp(t, true)
			runs := 0
			want := end(t)
			got := a.upMovingTheAgent(context.Background(), upsSaying(a, agentPortRefusal, func() utils.CmdError { return want }, &runs), noRedo(t))
			require.Equal(t, want, got)
			require.Equal(t, 1, runs)
			require.Empty(t, agent.said)
		})
	}
	require.True(t, composeUpEnded(utils.CmdError{Type: utils.Runtime, Err: errors.New("Error response from daemon: x")}, true), "the compose library's failed up")
	require.False(t, composeUpEnded(utils.CmdError{Type: utils.Init, Err: errors.New("x")}, true), "the compose library's project that did not load")
	require.True(t, composeUpEnded(utils.CmdError{Type: utils.Runtime, Err: exitWith(t, "1")}, false), "docker compose up's exit 1")
}

// The agent client's word decides: an up whose failure is not a refused
// publish of the agent's port is returned as it is; one the agent client
// does not move the agent for (words it does not know for a taken port)
// fails closed, its error saying why and still the up's exit; a document the
// agent cannot be moved in whole is not run again; a run that is cut is not
// moved; and with no agent client, or not under compose, the up runs once.
func TestTheAgentIsMovedOnlyWhenTheAgentClientMovesIt(t *testing.T) {
	refused := func(t *testing.T) func() utils.CmdError {
		err := exitWith(t, "1")
		return func() utils.CmdError { return utils.CmdError{Type: utils.Runtime, Err: err} }
	}

	a, agent := movingApp(t, true)
	runs := 0
	end := refused(t)
	got := a.upMovingTheAgent(context.Background(), upsSaying(a, "dependency failed to start", end, &runs), noRedo(t))
	require.Equal(t, end(), got)
	require.Equal(t, 1, runs)
	require.Len(t, agent.said, 1)

	a, _ = movingApp(t, true)
	a.SetOnAgentPortRefused(func(context.Context, string) (uint32, error) {
		return 0, errors.New("in words keploy does not know for a taken port")
	})
	runs = 0
	got = a.upMovingTheAgent(context.Background(), upsSaying(a, agentPortRefusal, refused(t), &runs), noRedo(t))
	require.Equal(t, 1, runs)
	require.ErrorContains(t, got.Err, "does not know")
	var ee *exec.ExitError
	require.True(t, errors.As(got.Err, &ee) && ee.ExitCode() == 1, "the up's exit was lost: %v", got.Err)

	a, agent = movingApp(t, true)
	a.composeDoc = &docker.Compose{}
	runs = 0
	got = a.upMovingTheAgent(context.Background(), upsSaying(a, agentPortRefusal, refused(t), &runs), noRedo(t))
	require.Equal(t, 1, runs)
	require.ErrorContains(t, got.Err, "could not move its agent")
	require.Len(t, agent.said, 1)

	cut, cancel := context.WithCancel(context.Background())
	cancel()
	a, agent = movingApp(t, true)
	runs = 0
	a.upMovingTheAgent(cut, upsSaying(a, agentPortRefusal, refused(t), &runs), noRedo(t))
	require.Equal(t, 1, runs)
	require.Empty(t, agent.said)

	for name, edit := range map[string]func(a *App){
		"no agent client": func(a *App) { a.onAgentPortRefused = nil },
		"docker run":      func(a *App) { a.kind = utils.DockerRun },
	} {
		a, agent = movingApp(t, true)
		edit(a)
		runs = 0
		a.upMovingTheAgent(context.Background(), upsSaying(a, agentPortRefusal, refused(t), &runs), noRedo(t))
		require.Equal(t, 1, runs, name)
		require.Empty(t, agent.said, name)
		require.Nil(t, a.upSaid.Load(), name)
	}
}

// Moves are bounded: an up refused on every port is run composeAgentMoves+1
// times in all, then its failure returned. A document keploy hands compose
// (no file) is moved where compose reads it.
func TestAnAgentMovedOnEveryUpIsMovedABoundedNumberOfTimes(t *testing.T) {
	for _, file := range []bool{true, false} {
		a, agent := movingApp(t, file)
		runs := 0
		err := exitWith(t, "1")
		up := upsSaying(a, "failed to bind host port: address already in use", func() utils.CmdError { return utils.CmdError{Type: utils.Runtime, Err: err} }, &runs)
		got := a.upMovingTheAgent(context.Background(), up, up)
		require.Equal(t, composeAgentMoves+1, runs)
		require.Equal(t, err, got.Err)
		require.Len(t, agent.said, composeAgentMoves)
		var doc []byte
		if file {
			doc, err = os.ReadFile(a.composeFile)
			require.NoError(t, err)
		} else {
			doc = a.composeContent
		}
		last := strconv.Itoa(20000 + composeAgentMoves)
		require.Contains(t, string(doc), "127.0.0.1:"+last+":"+last, "file %v", file)
		require.EqualValues(t, 20000+composeAgentMoves, a.opts.AgentPort)
	}
}
