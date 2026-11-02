package http

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// namePushers holds, by agent URI, the goroutine that answers a docker-mode
// agent's asks for a fresh list of its networks' names, or noNamePusher for an
// agent that predates the list.
var namePushers sync.Map

var noNamePusher = struct{}{}

// namePusher answers one agent's asks until it is forgotten or the agent goes.
type namePusher struct {
	cancel context.CancelFunc
}

// forgetNetworkNames stops answering the agent at uri: a new agent is set up
// there, which is given its list afresh.
func forgetNetworkNames(uri string) {
	if v, ok := namePushers.LoadAndDelete(uri); ok {
		if np, ok := v.(*namePusher); ok {
			np.cancel()
		}
	}
}

// errNoNameRoute is an agent that predates the name list.
var errNoNameRoute = errors.New("keploy's agent predates the network name list")

// nameInspector is what of Docker the name list is read from.
type nameInspector interface {
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	NetworkInspect(ctx context.Context, networkID string, options network.InspectOptions) (network.Inspect, error)
}

// pushNetworkNames gives a docker-mode agent the names its replay's networks
// answer themselves (netnames), the only names its DNS mock miss is forwarded
// to docker's resolver for, and answers its asks for a fresh list from then
// on. Called before each replay session and when the agent restarts. A list
// it could not give leaves the agent answering such names itself, which is
// said at Warn.
func (a *AgentClient) pushNetworkNames(ctx context.Context) {
	if a.conf == nil || a.dockerClient == nil || a.conf.KeployContainer == "" ||
		!utils.IsDockerCmd(utils.CmdType(a.conf.CommandType)) {
		return
	}
	uri := a.conf.Agent.AgentURI
	agentContainer := a.conf.KeployContainer
	if v, ok := namePushers.Load(uri); ok && v == noNamePusher {
		return
	}
	err := a.postNetworkNames(ctx, uri, agentContainer, 0)
	if errors.Is(err, errNoNameRoute) {
		namePushers.Store(uri, noNamePusher)
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			a.logger.Warn("keploy could not give its agent the names the replay's docker networks answer: until it does, a DNS query no mock answers for a container or service name is answered by keploy, not docker's resolver",
				zap.Error(err), zap.String("next_step", "check that keploy's agent and the Docker daemon are up; the next test set or agent restart gives the list again"))
		}
		return
	}
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	np := &namePusher{cancel: cancel}
	if _, loaded := namePushers.LoadOrStore(uri, np); loaded {
		cancel()
		return
	}
	go a.answerNameAsks(actx, uri, agentContainer, np)
}

// answerNameAsks holds the agent's stream of asks for a fresh list open and
// answers each ask with one, until ctx ends or the agent stops taking asks. A
// list it could not give ends the stream, and the next stream it holds tells
// the agent it is done with that ask, so a DNS miss waiting for its list is
// answered by keploy at once rather than waiting on a clock.
func (a *AgentClient) answerNameAsks(ctx context.Context, uri, agentContainer string, np *namePusher) {
	defer namePushers.CompareAndDelete(uri, np)
	var after uint64
	for ctx.Err() == nil {
		last, err := a.answerNameAskStream(ctx, uri, agentContainer, after)
		after = last
		var listErr *nameListError
		if errors.As(err, &listErr) {
			if ctx.Err() == nil {
				a.logger.Warn("keploy could not give its agent a fresh list of the names the replay's docker networks answer: a DNS query no mock answers for a container started since is answered by keploy",
					zap.Error(listErr.err))
			}
			continue
		}
		if ctx.Err() == nil {
			a.logger.Debug("keploy stopped answering its agent's asks for the replay's network names", zap.Error(err))
		}
		return
	}
}

// nameListError is a list answerNameAskStream could not give for an ask.
type nameListError struct{ err error }

func (e *nameListError) Error() string { return e.err.Error() }

// maxNameAskLine bounds one ask on the agent's stream, a few dozen bytes.
const maxNameAskLine = 4096

// answerNameAskStream holds one stream of the agent's asks after after open
// and answers each with a list. It returns the last ask it is done with, and
// why the stream ended: a *nameListError for a list it could not give, so the
// next stream settles that ask, or the stream's own end.
func (a *AgentClient) answerNameAskStream(ctx context.Context, uri, agentContainer string, after uint64) (uint64, error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	q := url.Values{"after": {strconv.FormatUint(after, 10)}}
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, uri+models.NetworkNamesAskPath+"?"+q.Encode(), nil)
	if err != nil {
		return after, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return after, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return after, fmt.Errorf("the agent answered %s", res.Status)
	}
	lines := bufio.NewScanner(res.Body)
	lines.Buffer(make([]byte, 0, 256), maxNameAskLine)
	for lines.Scan() {
		var ask netnames.Ask
		if err := json.Unmarshal(lines.Bytes(), &ask); err != nil {
			return after, fmt.Errorf("an ask for a name list: %w", err)
		}
		if err := a.postNetworkNames(ctx, uri, agentContainer, ask.Asked); err != nil {
			return ask.Asked, &nameListError{err: err}
		}
		after = ask.Asked
	}
	if err := lines.Err(); err != nil {
		return after, err
	}
	return after, io.EOF
}

// postNetworkNames lists the names and gives them to the agent at uri, as the
// answer to ask answers (0: none).
func (a *AgentClient) postNetworkNames(ctx context.Context, uri, agentContainer string, answers uint64) error {
	l, err := listNetworkNames(ctx, a.dockerClient, agentContainer)
	if err != nil {
		return err
	}
	l.Answers = answers
	body, err := json.Marshal(l)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri+models.NetworkNamesPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Made beside the run's requests, and cancelled as the run ends
	// (ownConnection).
	client := a.ownConnection()
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return errNoNameRoute
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	return fmt.Errorf("the agent took no name list: %s: %s", res.Status, bytes.TrimSpace(msg))
}

// namelessNetworks are the networks docker's resolver holds no names on.
var namelessNetworks = []string{"host", "none", "bridge"}

// listNetworkNames lists the names docker's embedded resolver holds for the
// networks agentContainer is on: each container's DNSNames there (its name,
// aliases, compose service name and short ID), or, from a daemon that
// predates DNSNames, its name, aliases and short ID; and each of them
// qualified by the network's name, which the resolver answers too. A
// container that went away while it was listed is left out.
func listNetworkNames(ctx context.Context, d nameInspector, agentContainer string) (netnames.List, error) {
	ai, err := d.ContainerInspect(ctx, agentContainer)
	if err != nil {
		return netnames.List{}, fmt.Errorf("inspect keploy's agent container %s: %w", agentContainer, err)
	}
	var l netnames.List
	if ai.NetworkSettings == nil {
		return l, nil
	}
	seen := map[string]bool{}
	add := func(n string) {
		if c := netnames.Canon(n); c != "" && !seen[c] {
			seen[c] = true
			l.Names = append(l.Names, c)
		}
	}
	for netName := range ai.NetworkSettings.Networks {
		if slices.Contains(namelessNetworks, netName) {
			continue
		}
		ni, err := d.NetworkInspect(ctx, netName, network.InspectOptions{})
		if err != nil {
			return netnames.List{}, fmt.Errorf("inspect network %s: %w", netName, err)
		}
		l.Networks = append(l.Networks, netnames.Canon(netName))
		for id, ep := range ni.Containers {
			names := []string{strings.TrimPrefix(ep.Name, "/")}
			ci, err := d.ContainerInspect(ctx, id)
			if err == nil && ci.NetworkSettings != nil {
				if es := ci.NetworkSettings.Networks[netName]; es != nil {
					if len(es.DNSNames) > 0 {
						names = append(names, es.DNSNames...)
					} else {
						names = append(names, es.Aliases...)
						if len(id) >= 12 {
							names = append(names, id[:12])
						}
					}
				}
			}
			for _, n := range names {
				if n == "" {
					continue
				}
				add(n)
				add(n + "." + netName)
			}
		}
	}
	slices.Sort(l.Names)
	slices.Sort(l.Networks)
	return l, nil
}
