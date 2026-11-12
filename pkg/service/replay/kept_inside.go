package replay

import (
	"sort"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A replay answers the app's outgoing calls from its mocks, and a call no mock
// answers is not sent anywhere: the proxy answers it itself (an HTTP 502, a
// closed connection, a synthetic DNS answer). keptInside collects, for the
// whole run, each destination the app called that way, so the run's output
// names each one once, with how many calls it took and how real egress would
// be opened for it. The misses themselves are still reported per test.

// KeptInsidePrefix starts each line of that report.
const KeptInsidePrefix = "kept inside the replay: "

// keptInsideOptIn is how a replay is let out to the real network: only by a
// named setting, each of which says so at start (agent.RealEgressPrefix).
const keptInsideOptIn = "test.allowEgress: [dns] (--allow-egress dns) sends an unrecorded DNS name to the host's and public resolvers; " +
	"any other call reaches its real destination only through a bypassRules entry, --global-passthrough or --mocking=false, " +
	"each logged at start as \"real egress opened by ...\". A call the app needs answered belongs in a recording: re-record it"

// keptDest is one destination a replay kept inside.
type keptDest struct {
	protocol    string
	destination string
	// unnamed: the miss's parser reported no destination.
	unnamed bool
	calls   int
}

// keptInside is the run's set of destinations kept inside the replay, in the
// order each was first missed. The zero value is ready to use.
type keptInside struct {
	mu    sync.Mutex
	dests map[string]*keptDest
	order []string
}

// add counts the misses of one read of the agent's misses. An answered call
// (models.UnmatchedCall.Answer, proven switch) is no miss, and a connection
// that sent nothing (models.MatchPhaseNoRequest) asked for nothing.
func (k *keptInside) add(read []models.UnmatchedCall) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range read {
		if c.Answer != "" || c.MatchPhase == models.MatchPhaseNoRequest {
			continue
		}
		protocol, dest, unnamed := keptDestination(c)
		key := protocol + "\x00" + dest
		d, ok := k.dests[key]
		if !ok {
			if k.dests == nil {
				k.dests = map[string]*keptDest{}
			}
			d = &keptDest{protocol: protocol, destination: dest, unnamed: unnamed}
			k.dests[key] = d
			k.order = append(k.order, key)
		}
		d.calls++
	}
}

// keptDestination names a miss's destination: a DNS miss by the name looked
// up (its summary is "<qtype> <name>"), any other by the destination its
// parser reported, and one whose parser named none (unnamed) by its protocol
// alone.
func keptDestination(c models.UnmatchedCall) (protocol, dest string, unnamed bool) {
	protocol = c.Protocol
	if protocol == "" {
		protocol = "unknown"
	}
	if strings.EqualFold(protocol, "DNS") {
		name := c.ActualSummary
		if _, after, ok := strings.Cut(name, " "); ok {
			name = after
		}
		return "DNS", strings.TrimSuffix(strings.TrimSpace(name), "."), false
	}
	if c.Destination != "" {
		return protocol, c.Destination, false
	}
	return protocol, "(the " + protocol + " parser did not name the destination)", true
}

// snapshot returns the destinations in the order each was first missed.
func (k *keptInside) snapshot() []keptDest {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]keptDest, 0, len(k.order))
	for _, key := range k.order {
		out = append(out, *k.dests[key])
	}
	return out
}

// sayKeptInside logs, once for the run, each destination the replay kept
// inside, with how many calls it took, and how real egress is opened.
func (r *Replayer) sayKeptInside() {
	dests := r.keptInside.snapshot()
	if len(dests) == 0 {
		return
	}
	// DNS names first, then the named destinations, then the calls whose
	// parser named none, each group by name: the same run prints them in the
	// same order whatever order its tests ran in.
	group := func(d keptDest) int {
		switch {
		case d.protocol == "DNS":
			return 0
		case d.unnamed:
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(dests, func(i, j int) bool {
		if gi, gj := group(dests[i]), group(dests[j]); gi != gj {
			return gi < gj
		}
		if dests[i].destination != dests[j].destination {
			return dests[i].destination < dests[j].destination
		}
		return dests[i].protocol < dests[j].protocol
	})
	r.logger.Info(KeptInsidePrefix+"no recorded mock answered the app's calls to these destinations, so keploy answered each call itself and relayed none of them to its destination",
		zap.Int("destinations", len(dests)),
		zap.String("opt_in", keptInsideOptIn))
	for _, d := range dests {
		r.logger.Info(KeptInsidePrefix+d.protocol+" "+d.destination, zap.Int("calls", d.calls))
	}
}
