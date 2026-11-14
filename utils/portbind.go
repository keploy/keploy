package utils

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// BindFreePort binds the first free port it finds with bind, which binds the
// caller's own socket or sockets on the port it is given and fails with
// EADDRINUSE where another socket holds that port. The bind is the check:
// unlike GetAvailablePort, no port is probed and released for a later bind to
// lose, to another keploy run that probed it free at the same moment.
//
// Candidates are the ports above the ephemeral range, as GetAvailablePort's,
// scanned from a random start, so that runs started together try different
// ones; a run of held ports gathers the scan on the port after it, which is
// harmless here, where the bind is the check. Where that range is unknown or
// reaches 65535, they are the ports from 1024 up, from a random start. Any
// error but EADDRINUSE ends the search with it.
//
// The ports in held are never tried: the caller holds them, or will, as the
// agent does the port its server binds once its proxy has started (a Docker
// agent's is above its container's ephemeral range too, among these
// candidates: utils.GetAvailableContainerPort). Bound by the proxy first,
// that port fails the agent's start.
func BindFreePort(bind func(port uint32) error, held ...uint32) (uint32, error) {
	return bindFreePortAbove(ephemeralPortRange, bind, held...)
}

// bindFreePortAbove is BindFreePort with the ephemeral range read by
// ephemeral (ephemeralPortRange's signature).
func bindFreePortAbove(ephemeral func() (lo, hi uint32, known bool), bind func(port uint32) error, held ...uint32) (uint32, error) {
	lo, span := uint32(1024), uint32(65536-1024)
	if _, hi, known := ephemeral(); known && hi < 65535 {
		lo, span = hi+1, 65536-(hi+1)
	}
	start := lo
	if n, err := rand.Int(rand.Reader, big.NewInt(int64(span))); err == nil {
		start = lo + uint32(n.Int64())
	}
	var last error
	for i := uint32(0); i < span; i++ {
		port := lo + ((start - lo + i) % span)
		if slices.Contains(held, port) {
			continue
		}
		err := bind(port)
		if err == nil {
			return port, nil
		}
		if !IsAddrInUse(err) {
			return 0, fmt.Errorf("bind port %d: %w", port, err)
		}
		last = err
	}
	return 0, fmt.Errorf("every port from %d to %d is taken: %w", lo, lo+span-1, last)
}

// ErrPortTaken is the failure of a start of keploy's agent whose server could
// not bind a port another socket took after the CLI found it free and handed
// it over: the proxy's or the DNS server's of a native agent off Linux, which
// cannot move once its hooks are loaded, or the agent's own port, which a
// Docker agent's `docker run` publishes. The CLI starts the agent again at
// once, on ports it picks again (pkg.RetryAgentSetup), a bounded number of
// times; the last start's failure names the port and, where it can, its
// holder (PortTaken).
var ErrPortTaken = errors.New("a port keploy picked for its agent was taken before the agent bound it")

type portTakenError struct{ err error }

func (e *portTakenError) Error() string        { return e.err.Error() }
func (e *portTakenError) Unwrap() error        { return e.err }
func (e *portTakenError) Is(target error) bool { return target == ErrPortTaken }

// PortTakenError tags err as ErrPortTaken, keeping its message. nil stays nil.
func PortTakenError(err error) error {
	if err == nil {
		return nil
	}
	return &portTakenError{err: err}
}

// PortTaken says that port is taken and, where it can, what holds it
// (PortHolder): "port 16789 is taken by pid 4242 (keploy)". Where it cannot,
// it says why (unnamedHolder, the platform's).
func PortTaken(port uint32) string {
	if h := PortHolder(port); h != "" {
		return fmt.Sprintf("port %d is taken by %s", port, h)
	}
	return fmt.Sprintf("port %d is taken (%s)", port, unnamedHolder)
}
