package util

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// isBenignReadErr must classify wrapped causes, which is the whole point of the
// %w change, and must NOT swallow a genuine failure.
func TestIsBenignReadErr(t *testing.T) {
	require.True(t, isBenignReadErr(errors.New("read tcp: use of closed network connection")))
	require.True(t, isBenignReadErr(errors.Join(net.ErrClosed)))
	require.True(t, isBenignReadErr(context.Canceled))
	require.True(t, isBenignReadErr(io.EOF))

	require.False(t, isBenignReadErr(nil))
	require.False(t, isBenignReadErr(errors.New("connection refused")))
	require.False(t, isBenignReadErr(context.DeadlineExceeded),
		"a timeout can mean a hung parser and must stay loud")
}
