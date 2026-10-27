package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// containerPushers holds, by agent URI, what this process gives each agent:
// the pusher of a docker-mode agent's container list, or noPusher for an
// agent that reads Docker itself. By URI, not by client: whichever client of
// this process drives an agent's sessions gives it the list.
var containerPushers sync.Map

var noPusher = struct{}{}

// forgetContainers drops what this process kept for the agent at uri: a new
// agent is set up there, which is asked afresh, and its own container's name
// is the one left out of its list.
func forgetContainers(uri string) {
	containerPushers.Delete(uri)
}

// pushContainers gives a docker-mode agent, under the proven switch, the
// list of the local Docker daemon's containers it names container
// destinations from: it runs in keploy's container, which has no Docker
// socket, and this client drives Docker for the run. Called before each
// session starts and when the agent restarts, so the agent has the list
// before it is asked to start a session; it never waits for one. A list it
// could not take only costs keys, and is said at Warn.
func (a *AgentClient) pushContainers(ctx context.Context) {
	if !provenswitch.On() {
		return
	}
	uri := a.conf.Agent.AgentURI
	v, ok := containerPushers.Load(uri)
	if !ok {
		takes, older, err := a.agentTakesContainers(ctx, uri)
		if err != nil {
			// Asked again at the next session start. Until then a
			// docker-mode agent names container destinations by their
			// address, which is said, not left to Debug.
			a.logger.Warn("keploy could not ask its agent whether it takes the list of the Docker daemon's containers: a docker-mode agent names container destinations by their address until it is given one",
				zap.Error(err), zap.String("next_step", "check that keploy's agent is up; the next session start asks again"))
			return
		}
		if !takes {
			containerPushers.Store(uri, noPusher)
			if older {
				a.logger.Info("keploy's agent predates container keys, so this run names container destinations by their address",
					zap.String("next_step", "run the agent image of this keploy version"))
			}
			return
		}
		// The pusher gives its lists to the agent at uri, the one it was
		// made for, whatever this client is set up for later.
		post := func(ctx context.Context, s containerkey.Snapshot) error { return a.postContainers(ctx, uri, s) }
		pusher := containerkey.NewPusher(post, a.conf.KeployContainer)
		pusher.AnswerSettles(func(ctx context.Context, boot string, after uint64) (containerkey.SettleAsk, error) {
			return a.askSettle(ctx, uri, boot, after)
		})
		v, _ = containerPushers.LoadOrStore(uri, pusher)
	}
	p, ok := v.(*containerkey.Pusher)
	if !ok {
		return
	}
	if err := p.Push(ctx, a.logger); err != nil && ctx.Err() == nil {
		a.logger.Warn("keploy could not give its agent the list of the Docker daemon's containers: until it does, the agent names container destinations from the last list it was given, or by their address if it has none",
			zap.Error(err), zap.String("next_step", "check that keploy's agent is up; the next change, session start or agent restart gives it the list again"))
	}
}

// agentTakesContainers asks the agent at uri whether it names container
// destinations from a list its client gives it, and whether it predates
// container keys (it has no such route).
func (a *AgentClient) agentTakesContainers(ctx context.Context, uri string) (takes, older bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri+"/containers", nil)
	if err != nil {
		return false, false, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	switch res.StatusCode {
	case http.StatusNoContent:
		return true, false, nil
	case http.StatusConflict:
		return false, false, nil // reads Docker itself
	case http.StatusNotFound:
		return false, true, nil
	}
	return false, false, fmt.Errorf("the agent answered %s", res.Status)
}

// postContainers gives the agent at uri a container list.
func (a *AgentClient) postContainers(ctx context.Context, uri string, s containerkey.Snapshot) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uri+"/containers", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("the agent took no container list: %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// askSettle asks the agent at uri for its next settle, held open until it
// has one the pusher was not told of (containerkey.AwaitSettleAsk). An agent
// that predates settles has no such route: containerkey.ErrNoSettles.
func (a *AgentClient) askSettle(ctx context.Context, uri, boot string, after uint64) (containerkey.SettleAsk, error) {
	q := url.Values{"after": {strconv.FormatUint(after, 10)}, "boot": {boot}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri+"/containers/settle?"+q.Encode(), nil)
	if err != nil {
		return containerkey.SettleAsk{}, err
	}
	res, err := a.client.Do(req)
	if err != nil {
		return containerkey.SettleAsk{}, err
	}
	defer func() { _ = res.Body.Close() }()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict:
		// No such route (an agent that predates settles), or an agent that
		// reads Docker itself.
		return containerkey.SettleAsk{}, containerkey.ErrNoSettles
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return containerkey.SettleAsk{}, fmt.Errorf("the agent answered %s: %s", res.Status, bytes.TrimSpace(msg))
	}
	var ask containerkey.SettleAsk
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&ask); err != nil {
		return containerkey.SettleAsk{}, fmt.Errorf("invalid settle ask: %w", err)
	}
	return ask, nil
}
