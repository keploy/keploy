//go:build !linux

package containerkey

import (
	"errors"
	"net/netip"
)

// socketCgroup: keploy reads a socket's cgroup on Linux alone, where its
// docker-mode agent runs.
func socketCgroup(netip.AddrPort, netip.AddrPort) (uint64, error) {
	return 0, errors.New("keploy reads a socket's cgroup on Linux alone")
}

// findCgroupDir: keploy reads cgroups on Linux alone.
func findCgroupDir(uint64) (string, bool, error) {
	return "", false, errors.New("keploy reads cgroups on Linux alone")
}
