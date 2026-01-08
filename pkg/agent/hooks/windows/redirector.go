//go:build windows && amd64

package windows

/*
#cgo windows LDFLAGS: -L${SRCDIR} -l:libwindows_redirector.a -lws2_32 -luserenv -lntdll -ladvapi32 -lole32 -loleaut32
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Rust FFI prototypes (must match the signatures in src/ffi.rs)
typedef struct {
    uint32_t ip_version;
    uint32_t dest_ip4;
    uint32_t dest_ip6[4];
    uint32_t dest_port;
    uint32_t kernel_pid;
} WinDest;

unsigned int start_redirector(unsigned int client_pid, unsigned int agent_pid, unsigned int proxy_port, unsigned int incoming_proxy, unsigned int dns_proxy_port, unsigned int mode);
unsigned int stop_redirector(void);
WinDest get_destination(unsigned int src_port);
unsigned int delete_destination(unsigned int src_port);
*/
import "C"

import (
	"fmt"
)

// StartRedirector initializes and starts the Windows redirector with configuration
// Returns error if already running or startup fails
func StartRedirector(clientPID, agentPID, proxyPort uint32, incomingProxy uint16, dnsPort uint32, mode uint32) error {
	rc := C.start_redirector(C.uint(clientPID), C.uint(agentPID), C.uint(proxyPort), C.uint(incomingProxy), C.uint(dnsPort), C.uint(mode))
	if rc == 0 {
		return fmt.Errorf("start_redirector failed (already running or error)")
	}
	return nil
}

// StopRedirector stops the Windows redirector
// Returns error if not running
func StopRedirector() error {
	rc := C.stop_redirector()
	if rc == 0 {
		return fmt.Errorf("stop_redirector failed (not running)")
	}
	return nil
}

// GetDestination retrieves destination info for a source port
// Returns (destination, true) if found, or (empty, false) if not found
func GetDestination(srcPort uint32) (WinDest, bool) {
	dest := C.get_destination(C.uint(srcPort))
	return WinDest{
		IPVersion: uint32(dest.ip_version),
		DestIP4:   uint32(dest.dest_ip4),
		DestIP6:   [4]uint32{uint32(dest.dest_ip6[0]), uint32(dest.dest_ip6[1]), uint32(dest.dest_ip6[2]), uint32(dest.dest_ip6[3])},
		DestPort:  uint32(dest.dest_port),
		KernelPid: uint32(dest.kernel_pid),
	}, true
}

// DeleteDestination removes a destination mapping for a source port
// Returns error if not found
func DeleteDestination(srcPort uint32) error {
	rc := C.delete_destination(C.uint(srcPort))
	if rc == 0 {
		return fmt.Errorf("delete_destination failed (not found)")
	}
	return nil
}
