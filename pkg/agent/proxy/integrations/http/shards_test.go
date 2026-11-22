package http

import (
	"fmt"
	"runtime"
	"testing"
)

// inShards runs each(t, i) for every i below n, in one parallel subtest of t
// for each CPU Go runs on: shard s takes i = s, s+shards, s+2*shards and so
// on, in order. The shards run at once, after t's own body returns (code
// after inShards in it runs first) and before its cleanups, so a Setenv on
// t holds for all of them. A row whose cases share no state runs them at
// once this way, every case still checked, and fails at the first case
// that differs, in its shard.
func inShards(t *testing.T, n int, each func(t *testing.T, i int)) {
	t.Helper()
	shards := min(runtime.GOMAXPROCS(0), n)
	for s := range shards {
		t.Run(fmt.Sprintf("shard %d of %d", s, shards), func(t *testing.T) {
			t.Parallel()
			for i := s; i < n; i += shards {
				each(t, i)
			}
		})
	}
}
