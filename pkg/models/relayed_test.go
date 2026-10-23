package models

import (
	"reflect"
	"strings"
	"testing"
)

// An agent's counts only grow: what a run relayed is the later read less the
// earlier, per reason and destination, and a row the earlier read did not
// have is the run's whole.
func TestRelayedLivesSinceIsWhatTheRunRelayed(t *testing.T) {
	base := RelayedLives{{Reason: RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 2}}
	now := RelayedLives{
		{Reason: RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 5},
		{Reason: RelayedPassThrough, Destination: "api.example.com:443", Connections: 1},
	}
	got := now.Since(base)
	want := RelayedLives{
		{Reason: RelayedPassThrough, Destination: "api.example.com:443", Connections: 1},
		{Reason: RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Since = %+v, want %+v", got, want)
	}
	if got := base.Since(base); len(got) != 0 {
		t.Fatalf("nothing relayed between two equal reads, got %+v", got)
	}
	// A count that went down started again (another agent, or a restart):
	// all of it may be the run's, and it is taken whole, never as none.
	restarted := RelayedLives{{Reason: RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 1}}
	if got := restarted.Since(now); !reflect.DeepEqual(got, restarted) {
		t.Fatalf("Since of a count that started again = %+v, want it whole %+v", got, restarted)
	}
}

// The next step names what to do for each reason the rows hold: for Redis,
// Keploy Enterprise and its URL.
func TestRelayedLivesNextStepNamesWhatToDo(t *testing.T) {
	redis := RelayedLives{{Reason: RelayedRedisEnterpriseOnly, Destination: "a:6379", Connections: 1}, {Reason: RelayedRedisEnterpriseOnly, Destination: "b:6379", Connections: 1}}
	if got := redis.NextStep(); got != "to record and mock Redis, use Keploy Enterprise: "+RedisEnterpriseURL {
		t.Fatalf("NextStep = %q", got)
	}
	both := append(redis, RelayedLive{Reason: RelayedPassThrough, Destination: "c:443", Connections: 1})
	if got := both.NextStep(); !strings.Contains(got, "Keploy Enterprise") || !strings.Contains(got, "mocking on") {
		t.Fatalf("NextStep = %q, want a step per reason", got)
	}
	if got := (RelayedLives{}).NextStep(); got != "" {
		t.Fatalf("NextStep of nothing = %q", got)
	}
}

// The words a receipt carries name what was relayed, where, how often and
// why: for Redis, that the open-source keploy does not mock it.
func TestRelayedLiveTextNamesWhatAndWhy(t *testing.T) {
	r := RelayedLive{Reason: RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 3}
	if got, want := r.Text(), "Redis relayed to the live server at 10.0.0.5:6379 (3 connection(s)): open-source keploy does not mock Redis"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	for reason, want := range map[string]string{RelayedPassThrough: "keploy's agent passes them through to the real service", RelayedReal: "mock.real lists the destination"} {
		if got := (RelayedLive{Reason: reason, Destination: "d:1", Connections: 1}).Text(); !strings.Contains(got, want) || !strings.HasPrefix(got, "calls relayed to the live server at d:1") {
			t.Errorf("%s: Text = %q, want it to say %q", reason, got, want)
		}
	}
	two := RelayedLives{{Reason: RelayedRedisEnterpriseOnly, Destination: "b:6379", Connections: 1}, {Reason: RelayedRedisEnterpriseOnly, Destination: "a:6379", Connections: 1}}
	if got := two.Text(); !strings.HasPrefix(got, "Redis relayed to the live server at a:6379") || !strings.Contains(got, "; Redis relayed to the live server at b:6379") {
		t.Fatalf("Text = %q, want every row, sorted", got)
	}
}
