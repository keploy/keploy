package testnet

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// claimPort returns the highest port of [from, below) that it can claim in
// dir and that bind then finds free. A port is claimed by creating its file,
// keploy-testnet-port-<port>, in dir; creation fails when the file exists,
// so no other test, in this process or another that claims in dir, is given
// a port while it is claimed. The claimed port's file is open until the test
// ends, then closed and removed. A port whose file exists already, or that
// bind fails at, is passed over; the claim of a port bind failed at is let
// go at once. bind tries the port and lets it go before it returns. A file
// that a test left behind when it was killed before its cleanup ran keeps
// its port passed over.
func claimPort(t *testing.T, dir string, from, below int, bind func(port int) error) (int, error) {
	t.Helper()
	var last error
	for port := below - 1; port >= from; port-- {
		name := filepath.Join(dir, "keploy-testnet-port-"+strconv.Itoa(port))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue // claimed by another test
		}
		if err != nil {
			return 0, fmt.Errorf("claim port %d: %w", port, err)
		}
		release := func() { _ = f.Close(); _ = os.Remove(name) }
		if err := bind(port); err != nil {
			release()
			last = err
			continue
		}
		t.Cleanup(release)
		return port, nil
	}
	return 0, fmt.Errorf("no port from %d to %d could be claimed in %s and bound (the last bind: %v)", from, below-1, dir, last)
}

// dynamicPortStart returns the lowest port at which the ranges that netsh
// reports in outs start: each out is what "netsh interface ipv4|ipv6 show
// dynamicportrange protocol=tcp" prints, whose first line with a number
// after its colon gives the range's start port.
func dynamicPortStart(outs ...string) (int, error) {
	if len(outs) == 0 {
		return 0, errors.New("no dynamic port range to read")
	}
	lowest := math.MaxInt
	for _, out := range outs {
		start, ok := 0, false
		for _, line := range strings.Split(out, "\n") {
			_, after, _ := strings.Cut(line, ":")
			if n, err := strconv.Atoi(strings.TrimSpace(after)); err == nil {
				start, ok = n, true
				break
			}
		}
		if !ok {
			return 0, fmt.Errorf("no start port in netsh's dynamic port range: %q", out)
		}
		lowest = min(lowest, start)
	}
	return lowest, nil
}
