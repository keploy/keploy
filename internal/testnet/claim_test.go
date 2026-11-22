package testnet

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// claimFile is the file that claims port in dir.
func claimFile(dir string, port int) string {
	return filepath.Join(dir, "keploy-testnet-port-"+strconv.Itoa(port))
}

// A claim takes the highest port of its range that no other test claimed
// and that its bind finds free: a port whose file exists is passed over, and
// so is one its bind fails at, whose claim is let go at once. A second claim
// is given the next port, and both files are removed when the test ends.
func TestAClaimTakesTheHighestPortNoTestClaimedThatItsBindFinds(t *testing.T) {
	dir := t.TempDir()
	const below = 30000
	require.NoError(t, os.WriteFile(claimFile(dir, below-1), nil, 0o600)) // another test's
	busy := errors.New("in use")
	var tried []int
	bind := func(port int) error {
		tried = append(tried, port)
		if port == below-2 {
			return busy
		}
		return nil
	}
	t.Run("claims", func(t *testing.T) {
		first, err := claimPort(t, dir, 1024, below, bind)
		require.NoError(t, err)
		require.Equal(t, below-3, first)
		require.Equal(t, []int{below - 2, below - 3}, tried, "the ports the claim bound")
		require.NoFileExists(t, claimFile(dir, below-2), "the claim of a port its bind failed at")
		require.FileExists(t, claimFile(dir, below-3))
		second, err := claimPort(t, dir, 1024, below, bind)
		require.NoError(t, err)
		require.Equal(t, below-4, second)
		require.FileExists(t, claimFile(dir, below-4))
	})
	require.NoFileExists(t, claimFile(dir, below-3), "a claim once its test ended")
	require.NoFileExists(t, claimFile(dir, below-4), "a claim once its test ended")
	require.FileExists(t, claimFile(dir, below-1), "another test's claim")
}

// A claim whose bind fails at every port of its range fails, naming the
// range and the last bind's error, after it bound each port once, and leaves
// no claim behind; one whose file cannot be made fails at once.
func TestAClaimThatFindsNoPortFailsNamingTheRange(t *testing.T) {
	dir := t.TempDir()
	n := 0
	_, err := claimPort(t, dir, 2000, 2004, func(int) error { n++; return errors.New("excluded") })
	require.ErrorContains(t, err, "no port from 2000 to 2003")
	require.ErrorContains(t, err, "excluded")
	require.Equal(t, 4, n, "the binds")
	left, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	require.Empty(t, left, "claims left behind")

	n = 0
	_, err = claimPort(t, filepath.Join(dir, "absent"), 2000, 2004, func(int) error { n++; return nil })
	require.ErrorContains(t, err, "claim port 2003")
	require.Zero(t, n, "binds without a claim")
}

// The dynamic range starts at the lowest start port netsh reports for IPv4
// and IPv6; output with no start port in it, or none at all, is an error.
func TestTheDynamicRangeStartsAtTheLowestStartPortNetshReports(t *testing.T) {
	report := func(start string) string {
		return "\r\nProtocol tcp Dynamic Port Range\r\n---------------------------------\r\n" +
			"Start Port      : " + start + "\r\nNumber of Ports : 16384\r\n"
	}
	for _, outs := range [][]string{{report("49152"), report("50000")}, {report("50000"), report("49152")}} {
		start, err := dynamicPortStart(outs...)
		require.NoError(t, err)
		require.Equal(t, 49152, start)
	}
	_, err := dynamicPortStart(report("49152"), "The requested operation requires elevation.\r\n")
	require.ErrorContains(t, err, "no start port")
	_, err = dynamicPortStart()
	require.Error(t, err)
}
