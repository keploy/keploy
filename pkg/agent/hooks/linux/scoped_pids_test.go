//go:build linux

package linux

import (
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

func TestScopedPidsSharedWithUnixconn(t *testing.T) {
	m := &ebpf.Map{}
	got := sharedMaps(&bpfObjects{bpfMaps: bpfMaps{ScopedPids: m}})
	require.Same(t, m, got["scoped_pids"], "unixconn MapReplacements must share scoped_pids with the main BPF object")
}

func TestScopedPidsMapSpecMatchesBetweenMainAndUnixconnObjects(t *testing.T) {
	mainSpec, err := loadBpf()
	require.NoError(t, err)
	unixSpec, err := loadUnixconn()
	require.NoError(t, err)

	mainMap := mainSpec.Maps["scoped_pids"]
	require.NotNil(t, mainMap, "main BPF object missing scoped_pids map")
	unixMap := unixSpec.Maps["scoped_pids"]
	require.NotNil(t, unixMap, "unixconn BPF object missing scoped_pids map")

	require.Equal(t, uint32(4), mainMap.KeySize, "scoped_pids key must be uint32 nsTgid")
	require.Equal(t, uint32(8), mainMap.ValueSize, "scoped_pids value must be uint64 start_boottime ticks")
	require.Equal(t, mainMap.KeySize, unixMap.KeySize)
	require.Equal(t, mainMap.ValueSize, unixMap.ValueSize)
	require.Equal(t, mainMap.Type, unixMap.Type)
}

func TestScopedPidsActiveAndInputValidation(t *testing.T) {
	h := &Hooks{}
	require.False(t, h.ScopedPidsActive(), "inactive before agent info is sent")

	h.agentInfoSent = true
	h.agentInfo = structs.AgentInfo{IsDocker: 0, Flags: structs.FlagScopedPids}
	require.True(t, h.ScopedPidsActive())

	h.agentInfo.IsDocker = 1
	require.False(t, h.ScopedPidsActive(), "docker mode must ignore FlagScopedPids")

	h.agentInfo = structs.AgentInfo{IsDocker: 0, Flags: 0}
	require.False(t, h.ScopedPidsActive())

	require.Error(t, h.AddScopedPID(0, 123))
	require.Error(t, h.AddScopedPID(100, 0))
	require.ErrorContains(t, h.AddScopedPID(100, 123), "not loaded")
}
