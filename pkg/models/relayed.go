package models

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RelayedLive is the connections keploy's agent relayed, live, to one
// destination instead of answering them from a recording, and why (Reason).
// A replay whose agent relayed any did not run with every dependency
// answered from the recording, whatever else it proved: `keploy mock replay`
// claims isolation only when its agent says it relayed none.
type RelayedLive struct {
	// Reason is what relayed the connections: RelayedRedisEnterpriseOnly,
	// RelayedPassThrough or RelayedReal.
	Reason string `json:"reason" yaml:"reason"`
	// Destination is where the connections were relayed to (host:port): as
	// the application dialled it; or, for a Redis keploy dialled at the
	// address a name has now, by the name keploy's DNS answered for the
	// address the application dialled; or, for a Redis over TLS dialled by
	// its server's name, by that name.
	Destination string `json:"destination" yaml:"destination"`
	// Connections counts them.
	Connections int `json:"connections" yaml:"connections"`
}

// The reasons a connection is relayed live (RelayedLive.Reason).
const (
	// RelayedRedisEnterpriseOnly is a Redis connection the open-source
	// keploy relays to the real server, recording and mocking nothing of it
	// (the proxy's Redis skip). It is the skip's integration name.
	RelayedRedisEnterpriseOnly = "redis_enterprise_only"
	// RelayedPassThrough is a connection keploy's agent passes through to
	// the real service: under its global pass-through, in a session with
	// mocking off, or under its opportunistic TLS intercept. (A bypass
	// rule's connection never reaches keploy's proxy: keploy mock replay
	// names bypass rules from its own configuration.)
	// Not "passthrough", the --on-miss policy (MissPassthrough), whose calls
	// the receipt names by its policy.
	RelayedPassThrough = "agent_pass_through"
	// RelayedReal is a connection to a destination mock.real lists, which
	// every run reaches for real (KEPLOY_PROVEN=1).
	RelayedReal = "mock_real"
)

// ErrRelayedLiveUnsupported reports that an agent does not say which
// connections it relayed live: an agent older than the report, or one whose
// proxy does not count them. It is "unknown", never "none".
var ErrRelayedLiveUnsupported = errors.New("keploy's agent does not report the connections it relays to a live server")

// RelayedLives is an agent's RelayedLive rows, one per reason and
// destination, sorted by reason, then destination.
type RelayedLives []RelayedLive

// Sorted is r in its order (by reason, then destination), with one row per
// reason and destination, their connections added; rows that count no
// connection are left out. r itself is not changed.
func (r RelayedLives) Sorted() RelayedLives {
	type key struct{ reason, dest string }
	sum := map[key]int{}
	for _, row := range r {
		sum[key{row.Reason, row.Destination}] += row.Connections
	}
	out := make(RelayedLives, 0, len(sum))
	for k, n := range sum {
		if n > 0 {
			out = append(out, RelayedLive{Reason: k.reason, Destination: k.dest, Connections: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Destination < out[j].Destination
	})
	return out
}

// Since is what r counts beyond base, an earlier read of the same agent's
// counts: the connections relayed between the two reads. An agent's total
// only grows for its life; one row can shrink by a connection the agent
// counted at match and then named by its dial target (moved to another
// row), which only ever charges the run more. A row base does not have is
// new whole, and so is a row that counts fewer than base did: its count
// started again (another agent answered the later read, or the same one
// restarted), so all of it may be the run's. A count that started again and grew back to base or
// past it reads as less than it is; keploy mock replay reads both counts of
// the one agent it started (under compose it reads no base).
func (r RelayedLives) Since(base RelayedLives) RelayedLives {
	type key struct{ reason, dest string }
	before := map[key]int{}
	for _, row := range base {
		before[key{row.Reason, row.Destination}] += row.Connections
	}
	var out RelayedLives
	for _, row := range r.Sorted() {
		n := row.Connections - before[key{row.Reason, row.Destination}]
		if n < 0 {
			n = row.Connections
		}
		if n > 0 {
			out = append(out, RelayedLive{Reason: row.Reason, Destination: row.Destination, Connections: n})
		}
	}
	return out
}

// Why is what relayed the row's connections, in words.
func (r RelayedLive) Why() string {
	switch r.Reason {
	case RelayedRedisEnterpriseOnly:
		return "open-source keploy does not mock Redis"
	case RelayedPassThrough:
		return "keploy's agent passes them through to the real service (its global pass-through, mocking turned off, or its opportunistic TLS intercept)"
	case RelayedReal:
		return "mock.real lists the destination, which every run reaches for real"
	default:
		return r.Reason
	}
}

// Text is the row in words: what was relayed to the live server, where, how
// many times, and why.
func (r RelayedLive) Text() string {
	what := "calls"
	if r.Reason == RelayedRedisEnterpriseOnly {
		what = "Redis"
	}
	return fmt.Sprintf("%s relayed to the live server at %s (%d connection(s)): %s", what, r.Destination, r.Connections, r.Why())
}

// NextStep is what a user does for the rows to be answered from a recording
// instead, one step per reason they hold, "" for none.
func (r RelayedLives) NextStep() string {
	var steps []string
	seen := map[string]bool{}
	for _, row := range r.Sorted() {
		if seen[row.Reason] {
			continue
		}
		seen[row.Reason] = true
		switch row.Reason {
		case RelayedRedisEnterpriseOnly:
			steps = append(steps, "to record and mock Redis, use Keploy Enterprise: "+RedisEnterpriseURL)
		case RelayedPassThrough:
			steps = append(steps, "to answer them from the recording, run keploy's agent with mocking on and without its global pass-through or opportunistic TLS intercept")
		case RelayedReal:
			steps = append(steps, "to answer it from the recording, take the destination out of mock.real and record it")
		}
	}
	return strings.Join(steps, "; ")
}

// Text is every row in words (RelayedLive.Text), joined by "; ".
func (r RelayedLives) Text() string {
	parts := make([]string, 0, len(r))
	for _, row := range r.Sorted() {
		parts = append(parts, row.Text())
	}
	return strings.Join(parts, "; ")
}
