package utils

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSuggestProxyStartError_PortInUse(t *testing.T) {
	err := errors.New("bind: address already in use")
	out := SuggestProxyStartError(err, 8080)

	require.NotNil(t, out)
	require.Contains(t, out.Error(), "8080")
	require.Contains(t, out.Error(), "already in use")
	require.Contains(t, out.Error(), "lsof")
}

func TestSuggestProxyStartError_PermissionDenied(t *testing.T) {
	err := errors.New("permission denied")
	out := SuggestProxyStartError(err, 80)

	require.NotNil(t, out)
	require.Contains(t, out.Error(), "permission denied")
	require.Contains(t, out.Error(), ">1024")
}

func TestSuggestProxyStartError_CannotAssignAddress(t *testing.T) {
	err := errors.New("can't assign requested address")
	out := SuggestProxyStartError(err, 8080)

	require.NotNil(t, out)
	require.Contains(t, out.Error(), "requested interface")
}

func TestSuggestProxyStartError_NetworkUnreachable(t *testing.T) {
	err := errors.New("network is unreachable")
	out := SuggestProxyStartError(err, 8080)

	require.NotNil(t, out)
	require.Contains(t, out.Error(), "network")
}

func TestSuggestProxyStartError_UnknownError(t *testing.T) {
	err := errors.New("some random error")
	out := SuggestProxyStartError(err, 8080)

	require.NotNil(t, out)
	require.Contains(t, out.Error(), "check system logs")
}

func TestSuggestProxyStartError_NilError(t *testing.T) {
	out := SuggestProxyStartError(nil, 1234)
	require.Nil(t, out)
}
