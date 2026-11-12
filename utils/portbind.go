package utils

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"syscall"
)

// BindFreePort binds the first free port it finds with bind, which binds the
// caller's own socket or sockets on the port it is given and fails with
// EADDRINUSE where another socket holds that port. The bind is the check:
// unlike GetAvailablePort, no port is probed and released for a later bind to
// lose, to another keploy run that probed it free at the same moment.
//
// Candidates are GetAvailablePort's: the ports above the ephemeral range, from
// a random start, so that runs started together try different ones. Where that
// range is unknown or reaches 65535, they are the ports from 1024 up, from a
// random start. Any error but EADDRINUSE ends the search with it.
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
		if !errors.Is(err, syscall.EADDRINUSE) {
			return 0, fmt.Errorf("bind port %d: %w", port, err)
		}
		last = err
	}
	return 0, fmt.Errorf("every port from %d to %d is taken: %w", lo, lo+span-1, last)
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
