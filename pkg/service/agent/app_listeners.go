package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"strconv"
	"strings"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
)

// procNetTables are the kernel's socket tables for the reading process's
// network namespace. In Docker mode that namespace is the app's: the app runs
// with --network=container:<agent> (network_mode: service:keploy-agent).
var procNetTables = []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"}

// AppListenAddrs reports the local addresses of the sockets listening on the
// app's port: where the app accepts connections on it. An empty answer means
// nothing listens there yet; an error means it cannot tell.
//
// The CLI asks in Docker mode, where it reaches the app from the host through
// the ports the docker command publishes, and docker forwards a published port
// to the container's own address: a socket listening only on 127.0.0.1 or ::1
// never sees that traffic.
func (a *Agent) AppListenAddrs(_ context.Context, port uint16) ([]netip.Addr, error) {
	listenPort := port
	if a.IncomingProxy != nil {
		lp, ok := a.IncomingProxy.(coreAgent.AppListenPorter)
		if !ok {
			// It may hold the app's port for its own forwarder, and then the
			// socket on it is keploy's, not the app's.
			return nil, errors.New("this agent's ingress cannot say where the app listens")
		}
		if listenPort, ok = lp.AppListenPort(port); !ok {
			return nil, fmt.Errorf("keploy's ingress forwarder holds port %d and the app's own port is not known yet", port)
		}
	}
	var addrs []netip.Addr
	for i, table := range procNetTables {
		f, err := os.Open(table)
		if err != nil {
			if i > 0 && errors.Is(err, fs.ErrNotExist) {
				continue // IPv6 disabled: no tcp6 table
			}
			return nil, err
		}
		found, err := listenAddrsFromProcNet(f, listenPort)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", table, err)
		}
		addrs = append(addrs, found...)
	}
	return addrs, nil
}

// tcpListen is the "st" column value of a listening socket.
const tcpListen = "0A"

// listenAddrsFromProcNet returns the local address of every listening socket on
// port in a /proc/net/tcp or /proc/net/tcp6 table.
//
// Each row's local_address is ADDR:PORT in hex. PORT is the number itself;
// ADDR is the address's 32-bit words, each printed as the value it has in this
// machine's byte order, so writing the words back in that order restores the
// address's bytes.
func listenAddrsFromProcNet(r io.Reader, port uint16) ([]netip.Addr, error) {
	var addrs []netip.Addr
	sc := bufio.NewScanner(r)
	for first := true; sc.Scan(); first = false {
		if first {
			continue // the header
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != tcpListen {
			continue
		}
		hexAddr, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			return nil, fmt.Errorf("malformed local address %q", fields[1])
		}
		p, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("malformed port in %q: %w", fields[1], err)
		}
		if uint16(p) != port {
			continue
		}
		words, err := hex.DecodeString(hexAddr)
		if err != nil || (len(words) != 4 && len(words) != 16) {
			return nil, fmt.Errorf("malformed address in %q", fields[1])
		}
		raw := make([]byte, len(words))
		for i := 0; i < len(words); i += 4 {
			binary.NativeEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(words[i:]))
		}
		addr, _ := netip.AddrFromSlice(raw)
		addrs = append(addrs, addr.Unmap())
	}
	return addrs, sc.Err()
}
